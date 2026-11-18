package mcp

import (
	"context"
	"sync"
)

type sourceMutationWriteIntentKey struct{}

// sourceMutationWriteIntent belongs to one store-writing checkout handler.
// Publication waits suspend its announcement. The selected route is still
// revalidated by mutation admission after each wait.
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
	// Canonical source edits only write the working file; the watcher owns
	// its store writes. Checkout mutation/capture paths do write the view
	// store and retain foreground preemption during their admission.
	view := requestViewFromContext(ctx)
	storeWriting := checkoutMutationFromContext(ctx) != nil ||
		(view.routed() && view.rider != nil && view.rider.Exact && view.viewRoot != "" &&
			view.files == nil && view.materialized != nil && s.lifecycle != nil)
	if !storeWriting {
		return ctx, func() {}
	}
	switch tool {
	case "edit_file", "write_file", "edit_symbol", "batch_edit":
	default:
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
