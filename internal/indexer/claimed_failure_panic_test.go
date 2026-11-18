package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Additional acceptance for the named-error cleanup seam. This uses the real
// claimed physical runner and checks that its original programmer panic remains
// observable, even though that path has no ordinary named return error.
func TestClaimedPanickedPhysicalLeaderMarksAttemptFailed(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	marker := &struct{ label string }{"private claimed physical panic"}
	var generationID int64
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) {
			observation, err := f.observe(t, ctx)
			if err != nil {
				return observation, err
			}
			observation.PrePublish = func(_ context.Context, id int64) error {
				generationID = id
				panic(marker)
			}
			return observation, nil
		})
		if err != nil {
			t.Fatalf("physical panic fixture returned before panic: %v", err)
		}
	}()
	if recovered != marker || generationID <= 0 {
		t.Fatalf("physical panic was changed or not reached: recovered=%#v generation=%d", recovered, generationID)
	}
	catalog := f.builder.Store.Catalog()
	row, found, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationFailed {
		t.Fatalf("panicked payload did not finish abandonment: %+v found=%v err=%v", row, found, err)
	}
	publication, found, err := catalog.DedicatedBasePublication(ctx, f.publisher.authority.GraphID)
	if err != nil || !found || publication.AttemptState != "failed" || publication.Claim.GenerationID != generationID || publication.Error == "" {
		t.Fatalf("panicked leader did not notify its own attempt: %+v found=%v err=%v", publication, found, err)
	}
	binding, found, err := catalog.GetDedicatedGraph(ctx, f.publisher.authority.GraphID)
	if err != nil || !found || binding.ActiveGenerationID != 0 {
		t.Fatalf("panicked initial build moved active pointer: %+v found=%v err=%v", binding, found, err)
	}
	if f.builder.Store.PayloadBuildFlightActive(generationID) {
		t.Fatal("panic left its physical flight active")
	}
	retry := f.ensure(t, ctx)
	if retry.Claim.GenerationID <= generationID || retry.Adoption.GenerationID != retry.Claim.GenerationID {
		t.Fatalf("panic retry did not allocate and adopt a new payload: %+v", retry)
	}
}
