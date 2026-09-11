package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func repositoryAdmissionFixture(t *testing.T) (*CheckoutLifecycle, store_sqlite.RepositoryCleanupIdentity) {
	t.Helper()
	root := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(root, "admissions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{FamilyID: "family", CommonDirIdentity: filepath.Join(root, ".git"), State: "active"}); err != nil {
		t.Fatal(err)
	}
	checkout := store_sqlite.Checkout{CheckoutID: "owner", Incarnation: "incarnation", FamilyID: "family", RootPath: root, GitDir: filepath.Join(root, ".git"), AdminName: "main", State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated}
	if err := catalog.UpsertCheckout(ctx, checkout); err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{GraphID: "graph", OwnerCheckoutID: checkout.CheckoutID, RepoPrefix: "repo", FamilyID: checkout.FamilyID, State: store_sqlite.DedicatedGraphReady}); err != nil {
		t.Fatal(err)
	}
	lifecycle := &CheckoutLifecycle{catalog: catalog, leases: graphview.NewLeaseManager()}
	identity := store_sqlite.RepositoryCleanupIdentity{GraphID: "graph", CheckoutID: checkout.CheckoutID, Incarnation: checkout.Incarnation, RepoPrefix: "repo", FamilyID: checkout.FamilyID, RootPath: root}
	return lifecycle, identity
}

func TestLifecycleRepositoryAdmissionPinsGenerationZeroAndClosesExactly(t *testing.T) {
	lifecycle, identity := repositoryAdmissionFixture(t)
	ctx := context.Background()
	if _, err := lifecycle.AcquireRepositoryRead(identity.GraphID); !errors.Is(err, graphview.ErrRepositoryOwnerUnknown) {
		t.Fatalf("unknown graph implicitly opened: %v", err)
	}
	// An unrestricted raw-store reader can see a repository registered later.
	broad, err := lifecycle.AcquireAllRepositoryReads()
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RegisterRepositoryOwner(ctx, identity.GraphID); err != nil {
		t.Fatal(err)
	}
	explicit, err := lifecycle.AcquireRepositoryRead(identity.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	closed, drain, found, err := lifecycle.closeRepositoryAdmission(ctx, identity.GraphID)
	if err != nil || !found || closed != identity {
		t.Fatalf("close identity=%+v found=%v err=%v", closed, found, err)
	}
	if !lifecycle.RepositoryAdmissionClosed(identity.RepoPrefix) {
		t.Fatal("closing graph remains query-admissible")
	}
	if _, err := lifecycle.AcquireRepositoryRead(identity.GraphID); !errors.Is(err, graphview.ErrRepositoryAdmissionClosed) {
		t.Fatalf("late query admitted: %v", err)
	}
	explicit.Release()
	select {
	case <-drain.Done():
		t.Fatal("later-registered graph escaped broad generation-zero reader")
	default:
	}
	broad.Release()
	<-drain.Done()
	if err := lifecycle.RegisterRepositoryOwner(ctx, identity.GraphID); !errors.Is(err, graphview.ErrRepositoryAdmissionClosed) {
		t.Fatalf("closing tombstone reopened before finalization: %v", err)
	}
}

func TestLifecycleRepositoryAdmissionRestoresClosingBeforeSeed(t *testing.T) {
	lifecycle, identity := repositoryAdmissionFixture(t)
	ctx := context.Background()
	if _, found, err := lifecycle.catalog.BeginRepositoryCleanup(ctx, identity.GraphID); err != nil || !found {
		t.Fatalf("durable close: %v %v", found, err)
	}
	restarted := &CheckoutLifecycle{catalog: lifecycle.catalog, leases: graphview.NewLeaseManager()}
	if err := restarted.restoreRepositoryAdmissions(ctx); err != nil {
		t.Fatal(err)
	}
	if !restarted.RepositoryAdmissionClosed(identity.RepoPrefix) {
		t.Fatal("startup restored closing corpus as open")
	}
	if _, err := restarted.AcquireRepositoryRead(identity.GraphID); !errors.Is(err, graphview.ErrRepositoryAdmissionClosed) {
		t.Fatalf("startup acquisition=%v", err)
	}
	if err := restarted.RegisterRepositoryOwner(ctx, identity.GraphID); !errors.Is(err, graphview.ErrRepositoryAdmissionClosed) {
		t.Fatalf("stale startup config revived closing graph: %v", err)
	}
}

// TestLifecycleRepositoryCloseAddressesTheCapturedRegistration traces the
// production close entrypoint (closeRepositoryAdmission ->
// restoreRepositoryAdmissionLocked) onto the handle-identity primitive. Closing
// must address the registration this lifecycle opened and captured, never
// whatever registration currently serves the prefix: once a registration is
// finalized and an identical owner tuple is registered again, a delayed cleanup
// re-entry must be refused instead of closing the replacement (gate 7).
func TestLifecycleRepositoryCloseAddressesTheCapturedRegistration(t *testing.T) {
	lifecycle, identity := repositoryAdmissionFixture(t)
	ctx := context.Background()
	if err := lifecycle.RegisterRepositoryOwner(ctx, identity.GraphID); err != nil {
		t.Fatal(err)
	}
	owner := repositoryCleanupOwner(identity)
	_, drain, found, err := lifecycle.closeRepositoryAdmission(ctx, identity.GraphID)
	if err != nil || !found {
		t.Fatalf("close found=%v err=%v", found, err)
	}
	if got := drain.Registration().Owner(); got != owner {
		t.Fatalf("captured registration owner=%+v want %+v", got, owner)
	}
	// A repeated cleanup attempt re-closes the same registration object.
	_, again, found, err := lifecycle.closeRepositoryAdmission(ctx, identity.GraphID)
	if err != nil || !found || again != drain {
		t.Fatalf("second close drain=%p want %p found=%v err=%v", again, drain, found, err)
	}
	// The delayed-callback window: the captured registration is finalized and
	// the exact same owner tuple is registered again by a later track. The
	// lifecycle still names the old cleanup capability for this graph.
	<-drain.Done()
	if err := lifecycle.leases.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.leases.RegisterRepositoryOwner(owner); err != nil {
		t.Fatal(err)
	}
	if _, stale, _, err := lifecycle.closeRepositoryAdmission(ctx, identity.GraphID); !errors.Is(err, graphview.ErrRepositoryOwnerUnknown) || stale != nil {
		t.Fatalf("delayed cleanup closed a replacement registration: drain=%p err=%v", stale, err)
	}
	read, err := lifecycle.leases.AcquireRepositoryRead(owner)
	if err != nil {
		t.Fatalf("replacement registration did not survive the delayed cleanup: %v", err)
	}
	read.Release()
}

type repositoryAdmissionPublisher struct {
	registered []store_sqlite.DedicatedBaseOwner
	closed     []store_sqlite.DedicatedBaseOwner
	stopped    bool
}

func (p *repositoryAdmissionPublisher) RegisterDedicatedBaseOwner(_ string, owner store_sqlite.DedicatedBaseOwner) error {
	p.registered = append(p.registered, owner)
	return nil
}
func (p *repositoryAdmissionPublisher) CloseDedicatedBaseOwner(_ string, owner store_sqlite.DedicatedBaseOwner) (<-chan struct{}, error) {
	p.closed = append(p.closed, owner)
	return repositoryAlreadyDrained, nil
}
func (p *repositoryAdmissionPublisher) CloseDedicatedBaseAdmission() <-chan struct{} {
	p.stopped = true
	return repositoryAlreadyDrained
}
func (p *repositoryAdmissionPublisher) FinalizeDedicatedBaseOwner(_ string, _ store_sqlite.DedicatedBaseOwner, _ <-chan struct{}) error {
	return nil
}

func TestLifecycleRepositoryPublisherUsesAuthorizedOwnerAndOneSharedRuntime(t *testing.T) {
	lifecycle, identity := repositoryAdmissionFixture(t)
	runtime := &repositoryAdmissionPublisher{}
	if err := lifecycle.SetDedicatedBaseCleanupRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RegisterRepositoryOwner(context.Background(), "missing"); !errors.Is(err, graphview.ErrRepositoryOwnerUnknown) {
		t.Fatal(err)
	}
	if len(runtime.registered) != 0 {
		t.Fatal("unverified graph bound publisher owner")
	}
	if err := lifecycle.RegisterRepositoryOwner(context.Background(), identity.GraphID); err != nil {
		t.Fatal(err)
	}
	want := store_sqlite.DedicatedBaseOwner{CheckoutID: identity.CheckoutID, Incarnation: identity.Incarnation}
	if len(runtime.registered) != 1 || runtime.registered[0] != want {
		t.Fatalf("runtime owner=%+v", runtime.registered)
	}
	if err := lifecycle.SetDedicatedBaseCleanupRuntime(&repositoryAdmissionPublisher{}); err == nil {
		t.Fatal("live publisher owner silently replaced")
	}
	_, drain, _, err := lifecycle.closeRepositoryAdmission(context.Background(), identity.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.closeRepositoryPublisher(identity, drain); err != nil {
		t.Fatal(err)
	}
	if len(runtime.closed) != 1 || runtime.closed[0] != want {
		t.Fatalf("closed owner=%+v", runtime.closed)
	}
	<-lifecycle.stopRepositoryPublishers()
	if !runtime.stopped {
		t.Fatal("shared publisher runtime not owned by shutdown")
	}
}

func TestLifecycleRepositoryShutdownRefusesRegistrationAndWaitsBroadReader(t *testing.T) {
	lifecycle, identity := repositoryAdmissionFixture(t)
	broad, err := lifecycle.AcquireAllRepositoryReads()
	if err != nil {
		t.Fatal(err)
	}
	done := lifecycle.stopRepositoryAdmissions()
	if err := lifecycle.RegisterRepositoryOwner(context.Background(), identity.GraphID); !errors.Is(err, graphview.ErrRepositoryAdmissionsStopped) {
		t.Fatalf("registration after shutdown=%v", err)
	}
	if _, err := lifecycle.AcquireAllRepositoryReads(); !errors.Is(err, graphview.ErrRepositoryAdmissionsStopped) {
		t.Fatalf("query after shutdown=%v", err)
	}
	select {
	case <-done:
		t.Fatal("shutdown passed empty broad raw-store reader")
	default:
	}
	broad.Release()
	<-done
}

// TestLifecycleShutdownFencesTheDivergentCleanupRetryState settles the one
// reachability question left open by the move to handle identity.
//
// restoreRepositoryAdmissionLocked re-registers unconditionally
// (repository_admission.go:213) and RegisterRepositoryOwnerPrepared refuses a
// registration whose graphview state is already closing
// (repository_registration.go:25-27). So a graph that is present in
// l.repositoryOwners, closing in graphview, and ABSENT from l.repositoryClosing
// would get an error where a cleanup retry previously got its drain back.
//
// Every writer of both maps runs under repositoryAdmissionMu and writes them
// together (:213-228, repository_cleanup.go:459-466), so the only producer of
// that divergence is shutdown: stopRepositoryAdmissions (:275-283) closes every
// graphview registration and records nothing in l.repositoryClosing. This test
// creates exactly that state and shows it can never be fed to
// restoreRepositoryAdmissionLocked: both of its entrypoints refuse on
// repositoryAdmissionsClosed first, BEFORE the durable BeginRepositoryCleanup
// write. The refusal is therefore unreachable, and the fail-closed ordering
// that makes it unreachable is what this test pins.
func TestLifecycleShutdownFencesTheDivergentCleanupRetryState(t *testing.T) {
	lifecycle, identity := repositoryAdmissionFixture(t)
	ctx := context.Background()
	if err := lifecycle.RegisterRepositoryOwner(ctx, identity.GraphID); err != nil {
		t.Fatal(err)
	}
	lifecycle.repositoryAdmissionMu.Lock()
	_, owned := lifecycle.repositoryOwners[identity.GraphID]
	_, closing := lifecycle.repositoryClosing[identity.GraphID]
	lifecycle.repositoryAdmissionMu.Unlock()
	if !owned || closing {
		t.Fatalf("registration owned=%v closing=%v", owned, closing)
	}

	<-lifecycle.stopRepositoryAdmissions()
	lifecycle.repositoryAdmissionMu.Lock()
	_, owned = lifecycle.repositoryOwners[identity.GraphID]
	_, closing = lifecycle.repositoryClosing[identity.GraphID]
	lifecycle.repositoryAdmissionMu.Unlock()
	if !owned || closing {
		t.Fatalf("shutdown did not produce the divergent state: owned=%v closing=%v", owned, closing)
	}
	// Shutdown really did close the graphview registration underneath it.
	if err := lifecycle.leases.RegisterRepositoryOwner(repositoryCleanupOwner(identity)); err == nil {
		t.Fatal("shutdown left the graphview registration open")
	}

	if _, _, _, err := lifecycle.closeRepositoryAdmission(ctx, identity.GraphID); !errors.Is(err, graphview.ErrRepositoryAdmissionsStopped) {
		t.Fatalf("cleanup entered restoration after shutdown: %v", err)
	}
	if err := lifecycle.restoreRepositoryAdmissions(ctx); !errors.Is(err, graphview.ErrRepositoryAdmissionsStopped) {
		t.Fatalf("startup restore entered restoration after shutdown: %v", err)
	}
	// The refusal precedes the durable close, so a shutdown race cannot leave a
	// graph marked closing in the catalog with no cleanup capability for it.
	graph, found, err := lifecycle.catalog.GetDedicatedGraph(ctx, identity.GraphID)
	if err != nil || !found {
		t.Fatalf("graph found=%v err=%v", found, err)
	}
	if graph.State == store_sqlite.DedicatedGraphClosing {
		t.Fatal("refused cleanup still performed the durable closing write")
	}
}

// TestLifecycleRefusesNilPublisherRuntimeAndReportsTheInstalledOne pins the
// installation seam a server stack writes through. Nil must not consume the one
// pre-owner window (it would leave the publisher/drain half silently unowned),
// and a caller must be able to read back the runtime it installed — that read
// is how an entry point proves it installed before the first owner instead of
// discovering the refusal later.
func TestLifecycleRefusesNilPublisherRuntimeAndReportsTheInstalledOne(t *testing.T) {
	lifecycle, identity := repositoryAdmissionFixture(t)
	if got := lifecycle.DedicatedBasePublisherRuntime(); got != nil {
		t.Fatalf("uninstalled lifecycle reports runtime %v", got)
	}
	if err := lifecycle.SetDedicatedBaseCleanupRuntime(nil); err == nil {
		t.Fatal("a nil publisher runtime consumed the installation window")
	}
	if got := lifecycle.DedicatedBasePublisherRuntime(); got != nil {
		t.Fatalf("refused nil installation still installed %v", got)
	}
	runtime := &repositoryAdmissionPublisher{}
	if err := lifecycle.SetDedicatedBaseCleanupRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if got := lifecycle.DedicatedBasePublisherRuntime(); got != DedicatedBaseCleanupRuntime(runtime) {
		t.Fatalf("installed runtime reads back as %v", got)
	}
	// The refusal after the first owner registration stays the seam's guard:
	// an entry point installing late gets an error, not a second authority.
	if err := lifecycle.RegisterRepositoryOwner(context.Background(), identity.GraphID); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.SetDedicatedBaseCleanupRuntime(&repositoryAdmissionPublisher{}); err == nil {
		t.Fatal("a runtime installed after owner registration")
	}
	if got := lifecycle.DedicatedBasePublisherRuntime(); got != DedicatedBaseCleanupRuntime(runtime) {
		t.Fatalf("late installation replaced the installed runtime with %v", got)
	}
}
