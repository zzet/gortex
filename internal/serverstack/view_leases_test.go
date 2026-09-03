package serverstack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// The lease fixture: one routed checkout over one published generation, which
// is the smallest state a request can materialize a view from.
const (
	leaseTestFamily   = "fam-lease"
	leaseTestCheckout = "wt-lease"
	leaseTestPrimary  = "co-lease-primary"
	leaseTestGraph    = "graph-lease"
	leaseTestLayer    = "layer-lease-commit"
)

// newLeaseStack builds a whole server stack on a private store, with the
// per-user directories pointed at temp paths so the constructor reads and
// writes nothing of the developer's.
func newLeaseStack(t *testing.T) *SharedServer {
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
	// No language servers: the stack under test is the lease wiring, and
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
	t.Cleanup(func() { _ = stack.Close() })
	return stack
}

// seedLeaseRoute publishes one commit generation and routes a checkout at it,
// returning the generation id.
func seedLeaseRoute(t *testing.T, store *store_sqlite.Store) int64 {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          leaseTestFamily,
		CommonDirIdentity: "/lease/.git",
		State:             reconcile.FamilyStateReady,
		CreatedAt:         100,
		LastSeen:          100,
	}); err != nil {
		t.Fatalf("UpsertRepositoryFamily: %v", err)
	}
	for id, mode := range map[string]store_sqlite.CheckoutMode{
		leaseTestPrimary:  store_sqlite.CheckoutModeDedicated,
		leaseTestCheckout: store_sqlite.CheckoutModeAutomatic,
	} {
		if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
			CheckoutID:    id,
			Incarnation:   "inc-" + id,
			FamilyID:      leaseTestFamily,
			RootPath:      filepath.Join("/lease", id),
			GitDir:        "/lease/.git",
			AdminName:     id,
			State:         store_sqlite.CheckoutStateReady,
			DesiredMode:   mode,
			EffectiveMode: mode,
			LastSeen:      101,
		}); err != nil {
			t.Fatalf("UpsertCheckout(%s): %v", id, err)
		}
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         leaseTestGraph,
		OwnerCheckoutID: leaseTestPrimary,
		RepoPrefix:      "repo",
		FamilyID:        leaseTestFamily,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}

	generationID, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        leaseTestGraph,
		LayerID:        leaseTestLayer,
		CheckoutID:     leaseTestCheckout,
		GenerationKind: "commit",
		TreeOID:        "tree-lease-commit",
		CreatedAt:      1000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	handle.AddBatch([]*graph.Node{{
		ID:         "repo/lease.go",
		Kind:       graph.KindFile,
		Name:       "lease.go",
		FilePath:   "repo/lease.go",
		RepoPrefix: "repo",
		Language:   "go",
		EndLine:    4,
	}}, nil)
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "repo", FilePath: "repo/lease.go", Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 2000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}
	if err := catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         leaseTestCheckout,
		GraphID:            leaseTestGraph,
		CommitGenerationID: generationID,
		State:              store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("UpsertCheckoutRoute: %v", err)
	}
	return generationID
}

// TestRequestLeaseRefusesLifecycleRetirement pins the wiring the sweep depends
// on: the MCP request path and the checkout coordinators must pin generations
// with one lease manager. With two, retirement never sees a request's lease
// and can delete the payload a live reader is composing over.
func TestRequestLeaseRefusesLifecycleRetirement(t *testing.T) {
	stack := newLeaseStack(t)
	if stack.CheckoutLifecycle == nil {
		t.Fatal("the stack grew no checkout lifecycle, so nothing owns retirement")
	}
	store, ok := stack.Graph.(*store_sqlite.Store)
	if !ok {
		t.Fatalf("the stack opened a %T, not the sqlite store", stack.Graph)
	}
	materializer := stack.MCP.Materializer()
	if materializer == nil {
		t.Fatal("the server reads through no materializer")
	}
	ctx := context.Background()
	generationID := seedLeaseRoute(t, store)

	view, err := materializer.MaterializeCheckout(ctx, leaseTestCheckout)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	// Retirement also refuses a generation a route still points at, so the
	// route is dropped first: what is under test is the lease alone.
	if err := store.Catalog().DeleteCheckoutRoute(ctx, leaseTestCheckout); err != nil {
		t.Fatalf("DeleteCheckoutRoute: %v", err)
	}

	// The predicate every coordinator hands to retirement.
	inUse := stack.CheckoutLifecycle.ViewLeases().InUse
	if err := store.RetirePayloadGeneration(ctx, generationID, inUse); !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire while the view is live = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}

	view.Close()
	if err := store.RetirePayloadGeneration(ctx, generationID, inUse); err != nil {
		t.Fatalf("retire after the view closed: %v", err)
	}
}
