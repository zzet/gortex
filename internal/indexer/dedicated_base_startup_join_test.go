package indexer

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Done is first evaluated after JoinScheduled captures its boundary. This
// barrier lets the test enqueue later demand without scheduling sleeps.
type startupJoinEnteredContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *startupJoinEnteredContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestInitialBasePublisherJoinScheduledStopsAtCapturedQueue(t *testing.T) {
	p := &InitialBasePublisher{ctx: t.Context(), logger: zap.NewNop(), changed: make(chan struct{}), drainReleased: true}
	p.enqueueLocked(basePublishRequest{prefix: "first"})
	p.enqueueLocked(basePublishRequest{prefix: "second"})
	ctx := &startupJoinEnteredContext{Context: t.Context(), entered: make(chan struct{})}
	type result struct {
		out []InitialBasePublication
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := p.JoinScheduled(ctx); done <- result{out, err} }()
	<-ctx.entered
	p.mu.Lock()
	p.enqueueLocked(basePublishRequest{prefix: "second", live: true, root: "newer"})
	p.enqueueLocked(basePublishRequest{prefix: "later", live: true})
	first, ok := p.popLocked()
	p.mu.Unlock()
	require.True(t, ok)
	require.Equal(t, "first", first.prefix)
	p.record(InitialBasePublication{RepoPrefix: first.prefix, GenerationID: 1}, nil)
	p.mu.Lock()
	second, ok := p.popLocked()
	p.mu.Unlock()
	require.True(t, ok)
	require.Equal(t, "second", second.prefix)
	require.True(t, second.live)
	require.Equal(t, "newer", second.root)
	failure := context.DeadlineExceeded
	p.record(InitialBasePublication{RepoPrefix: second.prefix, Err: failure}, nil)
	joined := <-done
	require.NoError(t, joined.err)
	require.Len(t, joined.out, 2)
	require.Equal(t, "first", joined.out[0].RepoPrefix)
	require.ErrorIs(t, joined.out[1].Err, failure)
	require.Equal(t, 1, p.Pending(), "later live demand must remain outside the startup join")
	waitCtx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, p.Wait(waitCtx), context.Canceled, "shutdown Wait still includes later demand")
}

func TestInitialBasePublisherJoinScheduledCancellationAndRelease(t *testing.T) {
	publisherCtx, stop := context.WithCancel(t.Context())
	defer stop()
	p := &InitialBasePublisher{ctx: publisherCtx, logger: zap.NewNop(), changed: make(chan struct{})}
	p.enqueueLocked(basePublishRequest{prefix: "held"})
	_, err := p.JoinScheduled(t.Context())
	require.ErrorContains(t, err, "has not been released")
	p.drainReleased = true
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = p.JoinScheduled(ctx)
	require.ErrorIs(t, err, context.Canceled)
	stop()
	_, err = p.JoinScheduled(t.Context())
	require.ErrorIs(t, err, context.Canceled, "runtime shutdown must release the join even with abandoned work")
}
