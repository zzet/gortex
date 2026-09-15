package graphview

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func preparedOwner() RepositoryOwner {
	return RepositoryOwner{GraphID: "graph", CheckoutID: "checkout", Incarnation: "incarnation", RepoPrefix: "repo"}
}

func TestRepositoryPreparedRegistrationValidatesBeforePublisherMutation(t *testing.T) {
	m := NewLeaseManager()
	owner := preparedOwner()
	if err := m.RegisterRepositoryOwner(owner); err != nil {
		t.Fatal(err)
	}
	conflict := owner
	conflict.Incarnation = "other"
	called := false
	if err := m.RegisterRepositoryOwnerPrepared(conflict, func() error { called = true; return nil }); !errors.Is(err, ErrRepositoryOwnerConflict) || called {
		t.Fatalf("lease conflict mutated publisher: called=%v err=%v", called, err)
	}
	drain, err := m.CloseRepositoryAdmission(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterRepositoryOwnerPrepared(owner, func() error { called = true; return nil }); !errors.Is(err, ErrRepositoryAdmissionClosed) || called {
		t.Fatalf("closing owner replaced publisher: called=%v err=%v", called, err)
	}
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterRepositoryOwnerPrepared(owner, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("authorized retrack failed: called=%v err=%v", called, err)
	}
}

func TestRepositoryPreparedRegistrationFailureDoesNotPublishOwner(t *testing.T) {
	m := NewLeaseManager()
	owner := preparedOwner()
	prepareErr := errors.New("publisher refused")
	if err := m.RegisterRepositoryOwnerPrepared(owner, func() error { return prepareErr }); !errors.Is(err, prepareErr) {
		t.Fatal(err)
	}
	if _, err := m.AcquireRepositoryRead(owner); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("failed preparation published read capability: %v", err)
	}
	if err := m.RegisterRepositoryOwner(owner); err != nil {
		t.Fatalf("failed preparation poisoned owner registration: %v", err)
	}
	if err := m.RegisterRepositoryOwnerPrepared(owner, func() error { return prepareErr }); !errors.Is(err, prepareErr) {
		t.Fatal(err)
	}
	read, err := m.AcquireRepositoryRead(owner)
	if err != nil {
		t.Fatalf("failed idempotent preparation revoked healthy owner: %v", err)
	}
	read.Release()
}

func TestRepositoryPreparedRegistrationDoesNotExposeBeforePreparation(t *testing.T) {
	m := NewLeaseManager()
	owner := preparedOwner()
	allow := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(allow) })
	registered := make(chan error, 1)
	finished := make(chan struct{})
	type registrationState struct {
		mutexHeld    bool
		prefixAbsent bool
		graphAbsent  bool
	}
	observed := make(chan registrationState, 1)
	// Install failure cleanup before the worker or any assertion. The lease API
	// has no context parameter; releasing the test-only callback pause lets the
	// worker finish and its production defer unlock the registry before joining.
	t.Cleanup(func() { unblock(); <-finished })
	go func() {
		defer close(finished)
		registered <- m.RegisterRepositoryOwnerPrepared(owner, func() error {
			// No other worker touches m yet. A failed TryLock therefore proves
			// this callback is inside the actual registration critical section,
			// not merely that a spawned reader has not been scheduled.
			acquiredHere := m.repositories.mu.TryLock()
			state := registrationState{
				mutexHeld:    !acquiredHere,
				prefixAbsent: m.repositories.byPrefix[owner.RepoPrefix] == nil,
				graphAbsent:  m.repositories.byGraph[owner.GraphID] == nil,
			}
			if acquiredHere {
				m.repositories.mu.Unlock()
			}
			observed <- state
			<-allow
			return nil
		})
	}()
	var state registrationState
	select {
	case state = <-observed:
	case <-finished:
		t.Fatalf("registration returned before preparation barrier: %v", <-registered)
	case <-time.After(5 * time.Second):
		t.Fatal("preparation callback was not reached")
	}
	if !state.mutexHeld || !state.prefixAbsent || !state.graphAbsent {
		t.Fatalf("preparation was not isolated before owner publication: %+v", state)
	}
	// AcquireRepositoryRead takes this same verified mutex before looking up
	// the owner. The locked, unpublished snapshot is the ordering oracle; there
	// is deliberately no scheduler-dependent negative select on a reader.
	unblock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("registration did not finish after preparation was released")
	}
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	read, err := m.AcquireRepositoryRead(owner)
	if err != nil {
		t.Fatalf("completed preparation did not publish the owner: %v", err)
	}
	defer read.Release()
}

func BenchmarkRepositoryPreparedRegistration(b *testing.B) {
	for _, test := range []struct {
		name    string
		prepare func() error
	}{
		{"owner-only", nil}, {"bounded-publisher-callback", func() error { return nil }},
	} {
		b.Run(test.name, func(b *testing.B) {
			m := NewLeaseManager()
			owner := preparedOwner()
			b.ReportAllocs()
			for b.Loop() {
				if err := m.RegisterRepositoryOwnerPrepared(owner, test.prepare); err != nil {
					b.Fatal(err)
				}
				drain, err := m.CloseRepositoryAdmission(owner)
				if err != nil {
					b.Fatal(err)
				}
				if err := m.FinalizeRepositoryCleanup(drain); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
