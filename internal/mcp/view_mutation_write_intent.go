package mcp

import (
	"context"
	"sync"
)

type sourceMutationWriteIntentKey struct{}

// sourceMutationWriteIntent belongs to one handler. Publication waits suspend
// its store announcement; mutation lane/cycle admission and disk writes retain
// ordinary writer preemption. The selected route is still revalidated by the
// existing mutation admission after each wait.
type sourceMutationWriteIntent struct {
	mu        sync.Mutex
	announce  func() func()
	release   func()
	suspended int
	closed    bool
}

func (s *Server) withSourceMutationWriteIntent(ctx context.Context, tool string) (context.Context, func()) {
	if s == nil || s.facades == nil || !s.facades.mutatesSource(tool) ||
		s.materializer == nil || s.materializer.Store == nil {
		return ctx, func() {}
	}
	intent := &sourceMutationWriteIntent{announce: s.materializer.Store.AnnounceWrite}
	intent.release = intent.announce()
	return context.WithValue(ctx, sourceMutationWriteIntentKey{}, intent), intent.close
}

func (intent *sourceMutationWriteIntent) close() {
	intent.mu.Lock()
	defer intent.mu.Unlock()
	intent.closed = true
	if intent.release != nil {
		intent.release()
		intent.release = nil
	}
}

// Suspend only a wait on publication, never the mutation lane. Nested waits
// cannot restore another wait's intent, and canceled/closed requests acquire no
// new announcement. Each resume is idempotent for failure/cleanup paths.
func suspendSourceMutationWriteIntent(ctx context.Context) func() {
	intent, _ := ctx.Value(sourceMutationWriteIntentKey{}).(*sourceMutationWriteIntent)
	if intent == nil {
		return func() {}
	}
	intent.mu.Lock()
	if intent.closed {
		intent.mu.Unlock()
		return func() {}
	}
	intent.suspended++
	if intent.release != nil {
		intent.release()
		intent.release = nil
	}
	intent.mu.Unlock()
	return sync.OnceFunc(func() {
		intent.mu.Lock()
		defer intent.mu.Unlock()
		intent.suspended--
		if intent.suspended == 0 && !intent.closed && ctx.Err() == nil {
			intent.release = intent.announce()
		}
	})
}
