package serverstack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// The smallest catalog state RegisterRepositoryOwner accepts: one family, one
// dedicated checkout, one ready dedicated graph owned by it. That call is the
// one Seed reaches through bindDedicatedGraph, so registering through it here
// is the same admission the daemon performs during warmup.
const (
	publisherTestFamily   = "fam-publisher"
	publisherTestCheckout = "co-publisher-primary"
	publisherTestGraph    = "graph-publisher"
	publisherTestInc      = "inc-publisher-primary"
)

// newPublisherStack builds a whole server stack on a private store, with the
// per-user directories pointed at temp paths so the constructor reads and
// writes nothing of the developer's. The returned function closes the stack
// exactly once, whether the test calls it or the cleanup does.
func newPublisherStack(t *testing.T) (*SharedServer, func()) {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	repoRoot := filepath.Join(base, "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	conf := config.Default()
	// No language servers: the stack under test is the publisher wiring, and
	// enrichment would spawn subprocesses that have nothing to do with it.
	conf.Semantic.Enabled = false

	stack, err := NewSharedServer(SharedServerConfig{
		Lifecycle:         LifecycleOneshot,
		Index:             repoRoot,
		BackendPath:       filepath.Join(base, "store.sqlite"),
		Config:            conf,
		Logger:            zap.NewNop(),
		SavingsPath:       filepath.Join(base, "savings.sqlite"),
		SavingsLegacyJSON: filepath.Join(base, "savings.json"),
	})
	if err != nil {
		t.Fatalf("NewSharedServer: %v", err)
	}
	var once sync.Once
	closeStack := func() { once.Do(func() { _ = stack.Close() }) }
	t.Cleanup(closeStack)
	return stack, closeStack
}

func seedPublisherOwner(t *testing.T, store *store_sqlite.Store, root string) {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          publisherTestFamily,
		CommonDirIdentity: filepath.Join(root, ".git"),
		State:             "active",
		CreatedAt:         100,
		LastSeen:          100,
	}); err != nil {
		t.Fatalf("UpsertRepositoryFamily: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    publisherTestCheckout,
		Incarnation:   publisherTestInc,
		FamilyID:      publisherTestFamily,
		RootPath:      root,
		GitDir:        filepath.Join(root, ".git"),
		AdminName:     "main",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      101,
	}); err != nil {
		t.Fatalf("UpsertCheckout: %v", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         publisherTestGraph,
		OwnerCheckoutID: publisherTestCheckout,
		RepoPrefix:      "repo",
		FamilyID:        publisherTestFamily,
		IsPrimaryBase:   true,
		State:           store_sqlite.DedicatedGraphReady,
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
}

// TestSharedServerInstallsPublisherRuntimeBeforeOwnerRegistration is the
// production-entrypoint trace for the install: the one constructor both the
// daemon (cmd/gortex/daemon_state.go) and the embedded one-shot server
// (cmd/gortex/mcp.go) build their stack through must leave the lifecycle
// owning a publisher runtime BEFORE the first owner registration, because
// SetDedicatedBaseCleanupRuntime refuses the installation afterwards and Seed
// registers owners as soon as the daemon warms up.
func TestSharedServerInstallsPublisherRuntimeBeforeOwnerRegistration(t *testing.T) {
	stack, _ := newPublisherStack(t)
	lifecycle := stack.CheckoutLifecycle
	if lifecycle == nil {
		t.Fatal("the stack grew no checkout lifecycle, so nothing owns publisher admission")
	}
	if stack.DedicatedBaseRuntime == nil {
		t.Fatal("the stack constructed no dedicated base publisher runtime")
	}
	installed := lifecycle.DedicatedBasePublisherRuntime()
	if installed == nil {
		t.Fatal("the lifecycle carries no publisher runtime: the publisher/drain half has no owner")
	}
	if installed != indexer.DedicatedBaseCleanupRuntime(stack.DedicatedBaseRuntime) {
		t.Fatalf("the lifecycle holds %p, the stack published %p: two runtimes, not one", installed, stack.DedicatedBaseRuntime)
	}
	// The lease domain must be the lifecycle's own — advancement through a
	// private manager would be invisible to the retirement sweep.
	if stack.DedicatedBaseRuntime.ViewLeases() != lifecycle.ViewLeases() {
		t.Fatal("the publisher runtime was installed with a lease manager the lifecycle does not share")
	}

	store, ok := stack.Graph.(*store_sqlite.Store)
	if !ok {
		t.Fatalf("the stack opened a %T, not the sqlite store", stack.Graph)
	}
	seedPublisherOwner(t, store, t.TempDir())

	ctx := context.Background()
	if err := lifecycle.RegisterRepositoryOwner(ctx, publisherTestGraph); err != nil {
		t.Fatalf("RegisterRepositoryOwner: %v", err)
	}

	// The owner reached the installed runtime's counted admission: a different
	// incarnation for the same graph is refused as a live-owner replacement,
	// which a runtime holding no owner for that graph would have accepted.
	rogue := store_sqlite.DedicatedBaseOwner{CheckoutID: publisherTestCheckout, Incarnation: "inc-rogue"}
	if err := stack.DedicatedBaseRuntime.RegisterDedicatedBaseOwner(publisherTestGraph, rogue); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("registering a rival owner = %v, want %v: the lifecycle's owner never reached the runtime",
			err, store_sqlite.ErrCatalogStaleGuard)
	}
	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: publisherTestCheckout, Incarnation: publisherTestInc}
	if err := stack.DedicatedBaseRuntime.RegisterDedicatedBaseOwner(publisherTestGraph, owner); err != nil {
		t.Fatalf("re-registering the live owner is not idempotent: %v", err)
	}

	// And the seam is now closed: this is the state the plan's revert-red
	// describes — an install attempted after the first bindDedicatedGraph.
	late, err := indexer.NewDedicatedBaseRuntime(store, lifecycle.ViewLeases())
	if err != nil {
		t.Fatalf("NewDedicatedBaseRuntime: %v", err)
	}
	if err := lifecycle.SetDedicatedBaseCleanupRuntime(late); err == nil {
		t.Fatal("a second publisher runtime was installed after an owner was registered")
	}
	if now := lifecycle.DedicatedBasePublisherRuntime(); now != installed {
		t.Fatal("the refused installation still replaced the installed runtime")
	}
}

// TestSharedServerCloseDrainsPublisherRuntime pins the other end: the stack's
// teardown chain reaches the runtime it installed. CheckoutLifecycle.Close
// stops publisher admission through stopRepositoryPublishers and waits on the
// returned drain, so after Close the runtime admits nothing more.
func TestSharedServerCloseDrainsPublisherRuntime(t *testing.T) {
	stack, closeStack := newPublisherStack(t)
	runtime := stack.DedicatedBaseRuntime
	if runtime == nil {
		t.Fatal("the stack constructed no dedicated base publisher runtime")
	}
	store, ok := stack.Graph.(*store_sqlite.Store)
	if !ok {
		t.Fatalf("the stack opened a %T, not the sqlite store", stack.Graph)
	}
	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: publisherTestCheckout, Incarnation: publisherTestInc}

	closeStack()

	if err := runtime.RegisterDedicatedBaseOwner(publisherTestGraph, owner); err == nil {
		t.Fatal("the publisher runtime still admits owners after the stack closed")
	}
	select {
	case <-runtime.CloseDedicatedBaseAdmission():
	default:
		t.Fatal("shutdown left the publisher runtime's admitted actors unjoined")
	}
	// Control: closure is what refused the registration above, not the tuple.
	fresh, err := indexer.NewDedicatedBaseRuntime(store, graphview.NewLeaseManager())
	if err != nil {
		t.Fatalf("NewDedicatedBaseRuntime: %v", err)
	}
	if err := fresh.RegisterDedicatedBaseOwner(publisherTestGraph, owner); err != nil {
		t.Fatalf("a runtime nothing closed refused the same owner: %v", err)
	}
}
