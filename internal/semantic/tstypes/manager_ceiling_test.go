package tstypes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/semantic"
)

// A pass queued on the resolve mutex for longer than the Manager's whole
// outer ceiling is not cut the moment it gets the mutex: the wait is another
// pass's work, so neither the provider's budget nor the ceiling counts it.
// Before, the ceiling expired during the wait and the pass applied nothing.
func TestManagerPassQueuedPastOuterCeilingStillApplies(t *testing.T) {
	const ceiling = 2500 * time.Millisecond
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", ceiling.String())
	g, dir := buildFixture(t, mixedFixture())
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	defer func() { _ = mgr.Close() }()
	mgr.RegisterProvider(NewProvider(CSharpSpec(), zap.NewNop()))

	// Another pass holds the mutex across the whole ceiling and then some.
	mu := g.ResolveMutex()
	mu.Lock()
	held := 2 * ceiling
	release := time.AfterFunc(held, mu.Unlock)
	defer release.Stop()

	results, _, err := mgr.EnrichAll(g, map[string]string{"": dir}, semantic.EnrichOptions{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	r := results[0]
	assert.GreaterOrEqual(t, r.LockWaitMs, (held - ceiling).Milliseconds(), "the pass must actually have queued")
	assert.False(t, r.Partial, "the queue wait consumed the outer ceiling: %s", r.AbortReason)
	assert.Positive(t, r.EdgesConfirmed+r.EdgesAdded, "the queued pass must still land its work")
}

// A pass cut while it queues records why it was cut (context.Cause), so the
// Manager's outer ceiling, the pass's own budget and a shutdown stay distinct
// in the abort reason instead of all reading "context canceled".
func TestPassCutWhileQueuedRecordsTheCause(t *testing.T) {
	g, dir := buildFixture(t, mixedFixture())
	p := NewProvider(CSharpSpec(), zap.NewNop())
	cause := errors.New("outer bound reached")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	mu := g.ResolveMutex()
	mu.Lock()
	time.AfterFunc(200*time.Millisecond, func() { cancel(cause) })
	release := time.AfterFunc(600*time.Millisecond, mu.Unlock)
	defer release.Stop()

	r, err := p.EnrichRepoContext(ctx, g, "", dir, nil)
	require.NoError(t, err)
	assert.True(t, r.Partial)
	assert.Equal(t, cause.Error(), r.AbortReason)
}
