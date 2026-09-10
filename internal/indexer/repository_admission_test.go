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
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{GraphID: "graph", OwnerCheckoutID: checkout.CheckoutID, RepoPrefix: "repo", FamilyID: checkout.FamilyID, State: "ready"}); err != nil {
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
