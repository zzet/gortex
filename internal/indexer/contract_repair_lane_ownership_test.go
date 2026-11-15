package indexer

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
)

// reindexUnderLane gives a direct fixture invocation the same batch admission
// and stable repository lane that ContractStuckRepair.pass owns in production.
// realReindex itself is a raw executor whose caller must already own that lane.
func (f *contractRepairFixture) reindexUnderLane(ctx context.Context, repo string, paths []string) error {
	return f.mi.repositoryMutationCoordinator(repo).runExclusive(ctx, func() error {
		return f.r.realReindex(withHeldRepositoryLanes(ctx, []string{repo}), repo, paths)
	})
}

type contractRepairLaneExtractor struct {
	parser.Extractor
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *contractRepairLaneExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	return e.ExtractWithOptions(path, raw, parser.ExtractionOptions{})
}

func (e *contractRepairLaneExtractor) ExtractWithOptions(path string, raw []byte, options parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	if filepath.Base(path) == "helper.go" {
		e.once.Do(func() { close(e.entered) })
		<-e.release
	}
	return parser.Extract(e.Extractor, path, raw, options)
}

// A real foreground extraction keeps its installed journal's entire lifetime
// inside the same lane the real background repair tries to take. The background
// must report busy, not enter a second raw executor or supersede that receipt.
func TestContractRepairDirectReindexOwnsLaneThroughJournalCleanup(t *testing.T) {
	const helper = "fixture/helper.go"
	allowBackground := make(chan struct{})
	var backgroundCalls atomic.Int64
	var barrier *contractRepairLaneExtractor
	f := newContractRepairFixture(t, func(f *contractRepairFixture, r *ContractStuckRepair) {
		extractor, ok := f.mi.registry.GetByLanguage("go")
		require.True(t, ok)
		barrier = &contractRepairLaneExtractor{Extractor: extractor, entered: make(chan struct{}), release: make(chan struct{})}
		f.mi.registry.Register(barrier)
		r.beforeLane = func(string) {
			select {
			case <-allowBackground:
			case <-r.ctx.Done():
			}
		}
		r.reindex = func(ctx context.Context, repo string, paths []string) error {
			backgroundCalls.Add(1)
			return r.realReindex(ctx, repo, paths)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	driftContractFile(t, f.contractJobFixture, "helper.go", jobHelper+"// foreground ownership\n")
	store := f.store.AtGeneration(0)
	require.True(t, f.r.driftedRow(ctx, store, "fixture", helper))
	published := f.publishedCount("fixture")
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	finished := make(chan struct{})
	var reindexErr error
	go func() {
		defer close(finished)
		reindexErr = f.reindexUnderLane(ctx, "fixture", []string{helper})
	}()
	t.Cleanup(func() {
		release()
		<-finished
	})
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("foreground real extraction did not enter: ", ctx.Err())
	}
	close(allowBackground)
	f.r.Suspect("fixture", []string{helper})
	require.Eventually(t, func() bool {
		return f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("outcome", "busy")).Len() > 0
	}, 10*time.Second, time.Millisecond, "the real background pass must try and find the foreground lane busy")
	require.Zero(t, backgroundCalls.Load(), "no background raw executor may borrow the foreground journal")
	release()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("foreground real reindex did not finish: ", ctx.Err())
	}
	require.NoError(t, reindexErr)
	require.False(t, f.r.driftedRow(ctx, store, "fixture", helper))
	require.Equal(t, published+1, f.publishedCount("fixture"), "the foreground receipt really accepted and published")
	require.Eventually(t, func() bool {
		return f.logs.FilterMessage("contract stuck repair").FilterField(zap.String("outcome", "healed")).Len() > 0
	}, 10*time.Second, time.Millisecond, "after lane release the real background pass must see the repaired row")
	f.r.Close()
	require.Zero(t, backgroundCalls.Load(), "the now-current row needs no second raw reindex")
	require.Empty(t, f.store.OpenContractCoreWindows("fixture"))
	t.Log("ACTUAL_REPAIR_LANE foreground_extractor_entered=true background_busy=true background_executor_calls=0 foreground_accepted=true background_healed=true foreground_and_background_joined=true")
}
