package indexer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func awaitPromotionCompletionEvent(
	t *testing.T, events <-chan ModeTransitionEvent, checkoutID string,
) ModeTransitionEvent {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.CheckoutID == checkoutID {
				return event
			}
		case <-timer.C:
			t.Fatalf("no transition event arrived for checkout %s", checkoutID)
		}
	}
}

func generationIDsForGraph(
	t *testing.T, catalog *store_sqlite.Catalog, graphID string,
) []int64 {
	t.Helper()
	rows, err := catalog.ListViewGenerations(context.Background(), store_sqlite.ViewGenerationFilter{
		GraphID: graphID,
	})
	require.NoError(t, err)
	ids := make([]int64, len(rows))
	for i := range rows {
		ids[i] = rows[i].GenerationID
	}
	return ids
}

// TestPromotionCompletionFailureRemainsRetryableWithoutRebuild injects the
// post-publication failure that used to be logged and discarded. The durable
// transition is the completion fence: both the cold arm and the already-
// dedicated recovery arm must report failure, retain a pending retry, and
// finally release the same route without rebuilding its physical payload.
func TestPromotionCompletionFailureRemainsRetryableWithoutRebuild(t *testing.T) {
	f := newFamilyFixture(t, "completion-fence")
	defer f.close()
	ctx := context.Background()

	raw, err := sql.Open("sqlite", f.dbPath+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.ExecContext(ctx, `
CREATE TRIGGER fail_promotion_transition_completion
BEFORE DELETE ON intent_transitions
WHEN OLD.cause = 'promote_checkout'
BEGIN
  SELECT RAISE(ABORT, 'injected promotion completion failure');
END`)
	require.NoError(t, err)

	events := make(chan ModeTransitionEvent, 4)
	f.lc.SetModeTransitionObserver(func(event ModeTransitionEvent) { events <- event })
	defer f.lc.SetModeTransitionObserver(nil)

	first, err := f.lc.PromoteCheckout(ctx, f.automatic.CheckoutID, TrackSourceMCP)
	require.ErrorContains(t, err, "injected promotion completion failure")
	assert.True(t, first.Pending)
	assert.True(t, first.Retryable)
	require.NotEmpty(t, first.TransitionID)
	require.NotEmpty(t, first.GraphID)
	assert.True(t, awaitPromotionCompletionEvent(t, events, first.CheckoutID).Failed,
		"cold completion failure was reported as worker success")

	standing, found, err := f.catalog.GetIntentTransition(ctx, first.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, first.TransitionID, standing.TransitionID)
	assert.Equal(t, store_sqlite.IntentTransitionPending, standing.State)
	assert.Contains(t, standing.LastError, "injected promotion completion failure")

	dedicatedBefore, found, err := f.catalog.GetDedicatedGraph(ctx, first.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	require.Positive(t, dedicatedBefore.ActiveGenerationID)
	routeBefore, routed := f.routeOf(first.CheckoutID)
	require.True(t, routed)
	generationsBefore := generationIDsForGraph(t, f.catalog, first.GraphID)
	require.NotEmpty(t, generationsBefore)

	// Retry once while the delete is still blocked. This takes the recovery
	// arm for an already-dedicated checkout and must retain the same fence.
	require.NoError(t, f.lc.ResumePendingTransitions(ctx))
	assert.True(t, awaitPromotionCompletionEvent(t, events, first.CheckoutID).Failed,
		"recovery completion failure was reported as worker success")
	standing, found, err = f.catalog.GetIntentTransition(ctx, first.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.IntentTransitionPending, standing.State)
	assert.Contains(t, standing.LastError, "injected promotion completion failure")
	assert.Equal(t, generationsBefore, generationIDsForGraph(t, f.catalog, first.GraphID))

	_, err = raw.ExecContext(ctx, `DROP TRIGGER fail_promotion_transition_completion`)
	require.NoError(t, err)
	require.NoError(t, f.lc.ResumePendingTransitions(ctx))
	assert.False(t, awaitPromotionCompletionEvent(t, events, first.CheckoutID).Failed,
		"successful journal release was reported as failed")

	_, found, err = f.catalog.GetIntentTransition(ctx, first.CheckoutID)
	require.NoError(t, err)
	assert.False(t, found, "successful retry retained the promotion transition")
	checkout, found, err := f.catalog.GetCheckout(ctx, first.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Empty(t, checkout.ActiveIntentTransitionID)
	dedicatedAfter, found, err := f.catalog.GetDedicatedGraph(ctx, first.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, dedicatedBefore.ActiveGenerationID, dedicatedAfter.ActiveGenerationID)
	routeAfter, routed := f.routeOf(first.CheckoutID)
	require.True(t, routed)
	assert.Equal(t, routeBefore, routeAfter)
	assert.Equal(t, generationsBefore, generationIDsForGraph(t, f.catalog, first.GraphID),
		"completion retry rebuilt physical generations")
}

// BenchmarkPromotionFailureJournalFence measures the only extra work on the
// corrected failure path: restoring the standing transition to pending with
// its durable reason. The successful completion path is unchanged.
func BenchmarkPromotionFailureJournalFence(b *testing.B) {
	store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "promotion-failure.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	catalog := store.Catalog()
	const (
		familyID     = "bench-family"
		checkoutID   = "bench-checkout"
		transitionID = "bench-transition"
	)
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID: familyID, CommonDirIdentity: familyID,
		State: "family_ready", CreatedAt: 1, LastSeen: 1,
	}); err != nil {
		b.Fatal(err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID: checkoutID, Incarnation: "inc-1", FamilyID: familyID,
		RootPath: "/tmp/bench-checkout", GitDir: "/tmp/bench-checkout/.git",
		AdminName: checkoutID, State: store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadCommit:    "c0ffee", HeadTree: "7ee7", LastSeen: 1,
	}); err != nil {
		b.Fatal(err)
	}
	transition := store_sqlite.IntentTransition{
		TransitionID: transitionID, CheckoutID: checkoutID,
		Cause: promotionTransitionCause, RequestedMode: store_sqlite.CheckoutModeDedicated,
		State: store_sqlite.IntentTransitionRunning, CreatedAt: 1, LastProgress: 1,
	}
	if err := catalog.BeginIntentTransition(ctx, transition); err != nil {
		b.Fatal(err)
	}
	lifecycle := &CheckoutLifecycle{
		catalog: catalog,
		logger:  zap.NewNop(),
		now:     func() time.Time { return time.Unix(2, 0) },
	}
	cause := errors.New("benchmark completion failure")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := PromoteResult{CheckoutID: checkoutID}
		if err := lifecycle.promotionFailed(ctx, &out, transition, cause); !errors.Is(err, cause) {
			b.Fatalf("promotionFailed = %v, want %v", err, cause)
		}
		if !out.Pending || !out.Retryable {
			b.Fatalf("promotionFailed result = %+v", out)
		}
	}
}
