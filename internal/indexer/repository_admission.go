package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// DedicatedBaseCleanupRuntime is the shared publisher owner, not a temporary
// per-build facade. Install it before Seed/Register can expose any owner.
type DedicatedBaseCleanupRuntime interface {
	RegisterDedicatedBaseOwner(string, store_sqlite.DedicatedBaseOwner) error
	CloseDedicatedBaseOwner(string, store_sqlite.DedicatedBaseOwner) (<-chan struct{}, error)
	FinalizeDedicatedBaseOwner(string, store_sqlite.DedicatedBaseOwner, <-chan struct{}) error
	CloseDedicatedBaseAdmission() <-chan struct{}
}

func (l *CheckoutLifecycle) SetDedicatedBaseCleanupRuntime(runtime DedicatedBaseCleanupRuntime) error {
	if l == nil {
		return graphview.ErrRepositoryOwnerUnknown
	}
	// A nil installation is never "install nothing later": it would consume the
	// one pre-owner window and leave the publisher half silently unowned.
	if runtime == nil {
		return fmt.Errorf("indexer: publisher cleanup runtime must not be nil")
	}
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	if l.repositoryAdmissionsClosed {
		return graphview.ErrRepositoryAdmissionsStopped
	}
	if len(l.repositoryOwners) != 0 || l.dedicatedBaseCleanupRuntime != nil {
		return fmt.Errorf("indexer: publisher cleanup runtime must be installed before owner registration")
	}
	l.dedicatedBaseCleanupRuntime = runtime
	return nil
}

// DedicatedBasePublisherRuntime reports the installed shared publisher runtime,
// nil while none is installed. It is a read of the installation seam, not an
// installation: owners are still registered through RegisterRepositoryOwner.
func (l *CheckoutLifecycle) DedicatedBasePublisherRuntime() DedicatedBaseCleanupRuntime {
	if l == nil {
		return nil
	}
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	return l.dedicatedBaseCleanupRuntime
}

var repositoryAlreadyDrained = func() <-chan struct{} { done := make(chan struct{}); close(done); return done }()

func (l *CheckoutLifecycle) closeRepositoryPublisher(identity store_sqlite.RepositoryCleanupIdentity, expected *graphview.RepositoryDrain) (<-chan struct{}, error) {
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	if expected == nil || l.repositoryClosing[identity.GraphID] != expected || l.repositoryOwners[identity.GraphID] != repositoryCleanupOwner(identity) {
		return nil, graphview.ErrRepositoryDrainInvalid
	}
	runtime := l.dedicatedBaseCleanupRuntime
	if runtime == nil {
		return repositoryAlreadyDrained, nil
	}
	// Registration/finalization use this same lock. The bounded runtime hook
	// must run inside it so a stale callback cannot close a replacement slot
	// after the handle check but before the tuple-only runtime close call.
	return runtime.CloseDedicatedBaseOwner(identity.GraphID, store_sqlite.DedicatedBaseOwner{CheckoutID: identity.CheckoutID, Incarnation: identity.Incarnation})
}

func (l *CheckoutLifecycle) stopRepositoryPublishers() <-chan struct{} {
	l.repositoryAdmissionMu.Lock()
	runtime := l.dedicatedBaseCleanupRuntime
	l.repositoryAdmissionMu.Unlock()
	if runtime == nil {
		return repositoryAlreadyDrained
	}
	return runtime.CloseDedicatedBaseAdmission()
}

// RegisterRepositoryOwner is the privileged lifecycle registration boundary.
// It revalidates the catalog identity while serialized with closure/finalizing;
// callers must invoke it before making a newly bound corpus visible. It does
// not itself make a catalog graph ready or authorize a build/publication.
func (l *CheckoutLifecycle) RegisterRepositoryOwner(ctx context.Context, graphID string) error {
	if l == nil || l.catalog == nil || l.leases == nil {
		return graphview.ErrRepositoryOwnerUnknown
	}
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	if l.repositoryAdmissionsClosed {
		return graphview.ErrRepositoryAdmissionsStopped
	}
	graph, found, err := l.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: graph %s", graphview.ErrRepositoryOwnerUnknown, graphID)
	}
	if graph.State == store_sqlite.DedicatedGraphClosing {
		return fmt.Errorf("%w: graph %s", graphview.ErrRepositoryAdmissionClosed, graphID)
	}
	if graph.State != store_sqlite.DedicatedGraphReady {
		return fmt.Errorf("%w: graph %s is not ready", graphview.ErrRepositoryOwnerUnknown, graphID)
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, graph.OwnerCheckoutID)
	if err != nil {
		return err
	}
	if !found || checkout.FamilyID != graph.FamilyID || checkout.Incarnation == "" {
		return fmt.Errorf("%w: graph %s has no matching owner", graphview.ErrRepositoryOwnerUnknown, graphID)
	}
	owner := graphview.RepositoryOwner{GraphID: graph.GraphID, CheckoutID: checkout.CheckoutID, Incarnation: checkout.Incarnation, RepoPrefix: graph.RepoPrefix}
	// A new durable owner cannot bypass the old local cleanup tombstone.
	for id, previous := range l.repositoryOwners {
		if id != graphID && previous.RepoPrefix == owner.RepoPrefix {
			return graphview.ErrRepositoryOwnerConflict
		}
		if id == graphID && (previous != owner || l.repositoryClosing[id] != nil) {
			return graphview.ErrRepositoryAdmissionClosed
		}
	}
	if err := l.leases.RegisterRepositoryOwnerPrepared(owner, func() error {
		if l.dedicatedBaseCleanupRuntime == nil {
			return nil
		}
		return l.dedicatedBaseCleanupRuntime.RegisterDedicatedBaseOwner(graphID, store_sqlite.DedicatedBaseOwner{CheckoutID: owner.CheckoutID, Incarnation: owner.Incarnation})
	}); err != nil {
		return err
	}
	if l.repositoryOwners == nil {
		l.repositoryOwners = make(map[string]graphview.RepositoryOwner)
	}
	l.repositoryOwners[graphID] = owner
	return nil
}

// AcquireRepositoryRead acquires the exact registered graph-owner scope before
// a serving catalog snapshot. This lookup does no SQL. An empty explicit scope
// means caller-proven control-only/empty, never an unrestricted Store reader.
func (l *CheckoutLifecycle) AcquireRepositoryRead(graphIDs ...string) (*graphview.RepositoryReadLease, error) {
	if l == nil || l.leases == nil {
		return nil, graphview.ErrRepositoryOwnerUnknown
	}
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	if l.repositoryAdmissionsClosed {
		return nil, graphview.ErrRepositoryAdmissionsStopped
	}
	owners := make([]graphview.RepositoryOwner, 0, len(graphIDs))
	for _, id := range graphIDs {
		owner, found := l.repositoryOwners[id]
		if !found {
			return nil, fmt.Errorf("%w: graph %s", graphview.ErrRepositoryOwnerUnknown, id)
		}
		owners = append(owners, owner)
	}
	return l.leases.AcquireRepositoryRead(owners...)
}

// AcquireAllRepositoryReads covers an unrestricted legacy corpus reader,
// including repositories registered later during that reader's lifetime.
func (l *CheckoutLifecycle) AcquireAllRepositoryReads() (*graphview.RepositoryReadLease, error) {
	if l == nil || l.leases == nil {
		return nil, graphview.ErrRepositoryOwnerUnknown
	}
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	if l.repositoryAdmissionsClosed {
		return nil, graphview.ErrRepositoryAdmissionsStopped
	}
	return l.leases.AcquireAllRepositoryReads()
}

func repositoryCleanupOwner(identity store_sqlite.RepositoryCleanupIdentity) graphview.RepositoryOwner {
	return graphview.RepositoryOwner{GraphID: identity.GraphID, CheckoutID: identity.CheckoutID, Incarnation: identity.Incarnation, RepoPrefix: identity.RepoPrefix}
}

// closeRepositoryAdmission serializes catalog fencing and process admission
// with registration. The durable closing write comes first: any read admitted
// in the narrow interval must still be pinned before payload can be purged.
func (l *CheckoutLifecycle) closeRepositoryAdmission(ctx context.Context, graphID string) (store_sqlite.RepositoryCleanupIdentity, *graphview.RepositoryDrain, bool, error) {
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	if l.repositoryAdmissionsClosed {
		return store_sqlite.RepositoryCleanupIdentity{}, nil, false, graphview.ErrRepositoryAdmissionsStopped
	}
	identity, found, err := l.catalog.BeginRepositoryCleanup(ctx, graphID)
	if err != nil || !found {
		return identity, nil, found, err
	}
	drain, err := l.restoreRepositoryAdmissionLocked(identity)
	return identity, drain, true, err
}

func (l *CheckoutLifecycle) restoreRepositoryAdmissionLocked(identity store_sqlite.RepositoryCleanupIdentity) (*graphview.RepositoryDrain, error) {
	owner := repositoryCleanupOwner(identity)
	if previous, exists := l.repositoryOwners[owner.GraphID]; exists {
		if previous != owner {
			return nil, fmt.Errorf("%w: graph %s cleanup owner changed", graphview.ErrRepositoryOwnerConflict, owner.GraphID)
		}
		// Re-entry re-closes the registration this lifecycle already closed,
		// addressed through the cleanup capability it captured. Resolving the
		// owner tuple again would let a delayed callback close a replacement
		// registration that reused the prefix/graph ID after finalization.
		if drain := l.repositoryClosing[owner.GraphID]; drain != nil {
			return l.leases.CloseRepositoryRegistration(drain.Registration())
		}
	}
	// Registration hands back the identity of the object it opened (idempotent
	// for an already open identical owner), and only that object is closed.
	registration, err := l.leases.RegisterRepositoryOwnerHandle(owner, nil)
	if err != nil {
		return nil, err
	}
	if l.repositoryOwners == nil {
		l.repositoryOwners = make(map[string]graphview.RepositoryOwner)
	}
	l.repositoryOwners[owner.GraphID] = owner
	drain, err := l.leases.CloseRepositoryRegistration(registration)
	if err != nil {
		return nil, err
	}
	if l.repositoryClosing == nil {
		l.repositoryClosing = make(map[string]*graphview.RepositoryDrain)
	}
	l.repositoryClosing[owner.GraphID] = drain
	return drain, nil
}

// RepositoryAdmissionClosed is a registry-only early refusal for Track and
// config restore, before they can revive intent or start physical work. Unknown
// prefixes still require validated registration before serving/publication.
func (l *CheckoutLifecycle) RepositoryAdmissionClosed(prefix string) bool {
	if l == nil {
		return false
	}
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	if l.repositoryAdmissionsClosed {
		return true
	}
	for graphID, owner := range l.repositoryOwners {
		if owner.RepoPrefix == prefix && l.repositoryClosing[graphID] != nil {
			return true
		}
	}
	return false
}

// restoreRepositoryAdmissions runs before Seed resumes any saga or startup
// work. A failed read is fatal to admission restoration, never an empty result.
func (l *CheckoutLifecycle) restoreRepositoryAdmissions(ctx context.Context) error {
	if l.catalog == nil {
		return nil
	}
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	if l.repositoryAdmissionsClosed {
		return graphview.ErrRepositoryAdmissionsStopped
	}
	owners, err := l.catalog.ListRepositoryCleanupOwners(ctx)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		if _, err := l.restoreRepositoryAdmissionLocked(owner); err != nil {
			return err
		}
	}
	return nil
}

func (l *CheckoutLifecycle) stopRepositoryAdmissions() <-chan struct{} {
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	l.repositoryAdmissionsClosed = true
	if l.leases == nil {
		return repositoryAlreadyDrained
	}
	return l.leases.ShutdownRepositoryAdmissions()
}
