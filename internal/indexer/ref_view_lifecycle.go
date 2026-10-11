package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrRefViewManagerClosed = errors.New("indexer: ref view manager is closed")

// refViewLifetime owns selections until return and detached builds until their
// heartbeat and physical runner have both stopped. Closing admission and
// observing zero work are one mutex transaction, so an allocated-before-Join
// producer cannot slip past cleanup. No waiter goroutine is needed.
type refViewLifetime struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	closed bool
	active int
}

func (l *refViewLifetime) initLocked() {
	if l.ctx == nil {
		l.ctx, l.cancel = context.WithCancel(context.Background())
		l.done = make(chan struct{})
	}
}

func (l *refViewLifetime) admit(ctx context.Context, detached bool) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	l.initLocked()
	if l.closed {
		l.mu.Unlock()
		return nil, nil, ErrRefViewManagerClosed
	}
	l.active++
	ownerCtx := l.ctx
	l.mu.Unlock()
	if detached {
		ctx = context.WithoutCancel(ctx)
	}
	workCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ownerCtx, cancel)
	var once sync.Once
	release := func() {
		once.Do(func() {
			stop()
			cancel()
			l.mu.Lock()
			defer l.mu.Unlock()
			l.active--
			if l.closed && l.active == 0 {
				close(l.done)
			}
		})
	}
	return workCtx, release, nil
}

func (l *refViewLifetime) closeAdmission() <-chan struct{} {
	l.mu.Lock()
	l.initLocked()
	if !l.closed {
		l.closed = true
		if l.active == 0 {
			close(l.done)
		}
	}
	cancel, done := l.cancel, l.done
	l.mu.Unlock()
	cancel()
	return done
}

// CloseAdmission refuses future selections and detached builds, cancels work
// already owned by this manager, and returns one cached join channel. It does
// not wait for request-owned payload views; those use RepositoryReadLease.
func (m *RefViewManager) CloseAdmission() <-chan struct{} {
	if m == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return m.lifetime.closeAdmission()
}

// Close joins every admitted selection, detached build and its heartbeat.
// Calls are idempotent and the manager cannot be reopened.
func (m *RefViewManager) Close() error {
	<-m.CloseAdmission()
	return nil
}

// closeRepositoryRefViews reserves the prefix even if no manager was created.
// Keep this tombstone until the graph and MI continuation are both finalized.
type repositoryRefViewDrain struct {
	owner     *CheckoutLifecycle
	prefix    string
	manager   *RefViewManager
	done      <-chan struct{}
	finalized bool
}

func (l *CheckoutLifecycle) closeRepositoryRefViews(prefix string) *repositoryRefViewDrain {
	l.refViewMu.Lock()
	defer l.refViewMu.Unlock()
	if drain := l.closingRefViews[prefix]; drain != nil {
		return drain
	}
	if l.closingRefViews == nil {
		l.closingRefViews = make(map[string]*repositoryRefViewDrain)
	}
	manager := l.refViews[prefix]
	drain := &repositoryRefViewDrain{owner: l, prefix: prefix, manager: manager, done: manager.CloseAdmission()}
	l.closingRefViews[prefix] = drain
	return drain
}

func (l *CheckoutLifecycle) finalizeRepositoryRefViews(drain *repositoryRefViewDrain) error {
	l.refViewMu.Lock()
	defer l.refViewMu.Unlock()
	if drain == nil || drain.owner != l {
		return errors.New("indexer: missing ref-view cleanup handle")
	}
	if drain.finalized {
		return nil
	}
	if l.closingRefViews[drain.prefix] != drain || l.refViews[drain.prefix] != drain.manager {
		return fmt.Errorf("indexer: ref-view cleanup identity changed for %s", drain.prefix)
	}
	select {
	case <-drain.done:
	default:
		return fmt.Errorf("indexer: ref-view cleanup still running for %s", drain.prefix)
	}
	drain.finalized = true
	delete(l.refViews, drain.prefix)
	delete(l.closingRefViews, drain.prefix)
	if len(l.refViews) == 0 {
		l.refViews = nil
	}
	if len(l.closingRefViews) == 0 {
		l.closingRefViews = nil
	}
	return nil
}

func (l *CheckoutLifecycle) closeAllRefViews() {
	l.refViewMu.Lock()
	l.refViewsClosed = true
	drains := make([]<-chan struct{}, 0, len(l.refViews))
	for _, manager := range l.refViews {
		drains = append(drains, manager.CloseAdmission())
	}
	l.refViewMu.Unlock()
	for _, done := range drains {
		<-done
	}
}
