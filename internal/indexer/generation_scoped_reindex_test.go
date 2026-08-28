package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
)

const (
	reindexScopeRepo       = "reindex-scope-repo"
	reindexScopeFamilyID   = "reindex-scope-family"
	reindexScopeCheckoutID = "reindex-scope-checkout"
	reindexScopeGraphID    = "reindex-scope-graph"
	reindexScopeRefViewID  = "reindex-scope-ref"
)

type readyReindexPayload struct {
	generationID int64
	handle       *store_sqlite.Store
	nodeID       string
	filePath     string
	bindingSite  graph.SemanticBindingSite
}

func seedReindexScopeControlPlane(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          reindexScopeFamilyID,
		CommonDirIdentity: "identity/" + reindexScopeFamilyID,
		DisplayRemote:     "git@example.invalid:reindex-scope.git",
		State:             "family_ready",
		CreatedAt:         100,
		LastSeen:          100,
	}); err != nil {
		t.Fatalf("upsert repository family: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    reindexScopeCheckoutID,
		Incarnation:   "incarnation-1",
		FamilyID:      reindexScopeFamilyID,
		RootPath:      "/tmp/" + reindexScopeCheckoutID,
		GitDir:        "/tmp/" + reindexScopeCheckoutID + "/.git",
		AdminName:     reindexScopeCheckoutID,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadRef:       "refs/heads/main",
		HeadCommit:    "commit-base",
		HeadTree:      "tree-base",
		LastSeen:      101,
	}); err != nil {
		t.Fatalf("upsert checkout: %v", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         reindexScopeGraphID,
		OwnerCheckoutID: reindexScopeCheckoutID,
		RepoPrefix:      reindexScopeRepo,
		FamilyID:        reindexScopeFamilyID,
		IsPrimaryBase:   true,
		State:           "graph_ready",
	}); err != nil {
		t.Fatalf("upsert dedicated graph: %v", err)
	}
}

func seedReadyReindexPayload(
	t *testing.T,
	store *store_sqlite.Store,
	ordinal int,
	ownerKind, layerID, generationKind string,
	baseGenerationID int64,
) readyReindexPayload {
	t.Helper()
	ctx := context.Background()
	generationID, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:            ownerKind,
		GraphID:              reindexScopeGraphID,
		LayerID:              layerID,
		CheckoutID:           reindexScopeCheckoutID,
		GenerationKind:       generationKind,
		BaseGenerationID:     baseGenerationID,
		LowerViewFingerprint: fmt.Sprintf("lower-%d", ordinal),
		TreeOID:              fmt.Sprintf("tree-%d", ordinal),
		ProvenanceCommitOID:  fmt.Sprintf("commit-%d", ordinal),
		ConfigHash:           "config",
		ExtractorVersions:    `{"go":"1"}`,
		ResolverVersion:      "resolver",
		CreatedAt:            int64(200 + ordinal),
	})
	if err != nil {
		t.Fatalf("begin %s generation: %v", generationKind, err)
	}
	nodeID := fmt.Sprintf("%s::payload/%s.go::Ready%s", reindexScopeRepo, generationKind, generationKind)
	filePath := fmt.Sprintf("%s::payload/%s.go", reindexScopeRepo, generationKind)
	handle.AddBatch([]*graph.Node{{
		ID: nodeID, Kind: graph.KindFunction, Name: "Ready" + generationKind,
		FilePath: filePath, RepoPrefix: reindexScopeRepo, Language: "go",
	}}, nil)
	if err := handle.SetFileMetas(reindexScopeRepo, []graph.FileMetaRow{{
		FilePath: filePath, ContentHash: fmt.Sprintf("hash-%d", ordinal), Size: ordinal + 1, NodeCount: 1,
	}}); err != nil {
		t.Fatalf("set %s file metadata: %v", generationKind, err)
	}
	bindingSite := graph.SemanticBindingSite{
		RepoPrefix: reindexScopeRepo, FilePath: filePath, Line: ordinal, Name: "Ready" + generationKind,
	}
	if err := handle.ReplaceSemanticBindingTypes(reindexScopeRepo, []graph.SemanticBindingType{{
		Site: bindingSite, TypeName: "type-" + generationKind,
	}}); err != nil {
		t.Fatalf("set %s semantic binding: %v", generationKind, err)
	}
	if err := handle.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: nodeID, Tokens: "ready " + generationKind}}); err != nil {
		t.Fatalf("set %s symbol FTS: %v", generationKind, err)
	}
	if err := handle.SetProducerState(store_sqlite.ProducerCompleteness{
		Producer: "extractor", State: store_sqlite.ProducerStateComplete,
	}); err != nil {
		t.Fatalf("complete %s producer: %v", generationKind, err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, int64(300+ordinal)); err != nil {
		t.Fatalf("publish %s generation: %v", generationKind, err)
	}
	return readyReindexPayload{
		generationID: generationID, handle: handle, nodeID: nodeID,
		filePath: filePath, bindingSite: bindingSite,
	}
}

func installReindexScopePointers(
	t *testing.T,
	store *store_sqlite.Store,
	base, commit, dirty, ref readyReindexPayload,
) {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID: reindexScopeGraphID, OwnerCheckoutID: reindexScopeCheckoutID,
		RepoPrefix: reindexScopeRepo, FamilyID: reindexScopeFamilyID,
		IsPrimaryBase: true, ActiveGenerationID: base.generationID, State: "graph_ready",
	}); err != nil {
		t.Fatalf("point dedicated graph at base: %v", err)
	}
	if err := catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID: reindexScopeCheckoutID, GraphID: reindexScopeGraphID,
		CommitGenerationID: commit.generationID, DirtyGenerationID: dirty.generationID,
		RouteEpoch: 7, State: store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("install checkout route: %v", err)
	}
	if err := catalog.UpsertRefView(ctx, store_sqlite.RefView{
		RefViewID: reindexScopeRefViewID, GraphID: reindexScopeGraphID,
		SelectorKind: "git_ref", SelectorValue: "refs/heads/feature",
		DesiredRef: "refs/heads/feature", DesiredCommit: "commit-ref", DesiredTree: "tree-ref",
		ActiveGenerationID: ref.generationID, ActiveRef: "refs/heads/feature",
		ActiveCommit: "commit-ref", ActiveTree: "tree-ref",
		EnrichmentProfile: "structural", DesiredBuildFingerprint: "fingerprint-ref",
		ActiveBuildFingerprint: "fingerprint-ref", RouteEpoch: 3,
		State: store_sqlite.RefViewReady, ExactView: true, LastResolved: 400, LastSelected: 400,
	}); err != nil {
		t.Fatalf("install ref view: %v", err)
	}
}

func requireReadyReindexPayload(t *testing.T, payload readyReindexPayload) {
	t.Helper()
	if got := payload.handle.GetNode(payload.nodeID); got == nil {
		t.Fatalf("ready generation %d lost node %s", payload.generationID, payload.nodeID)
	}
	fileRows, err := payload.handle.FileMetasForRepo(reindexScopeRepo)
	if err != nil || len(fileRows) != 1 || fileRows[0].FilePath != payload.filePath {
		t.Fatalf("ready generation %d file sidecars = %+v err=%v", payload.generationID, fileRows, err)
	}
	bindings, err := payload.handle.SemanticBindingTypes([]graph.SemanticBindingSite{payload.bindingSite})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("ready generation %d semantic sidecars = %+v err=%v", payload.generationID, bindings, err)
	}
}

type allGenerationCapabilityHidingStore struct {
	graph.Store
}

var _ graph.Store = allGenerationCapabilityHidingStore{}

func TestEvictRepoAllGenerationsFailsClosedForCapabilityHidingStore(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "capability-hiding.sqlite"))
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedReindexScopeControlPlane(t, store)
	payload := seedReadyReindexPayload(t, store, 1, "dedicated_graph", "hidden-layer", "base", 0)
	baseNodeID := reindexScopeRepo + "::base.go::Base"
	store.AddNode(&graph.Node{
		ID: baseNodeID, Kind: graph.KindFunction, Name: "Base",
		FilePath: reindexScopeRepo + "::base.go", RepoPrefix: reindexScopeRepo, Language: "go",
	})

	hidden := allGenerationCapabilityHidingStore{Store: store}
	nodesRemoved, edgesRemoved, err := evictRepoAllGenerations(hidden, reindexScopeRepo)
	if err == nil || nodesRemoved != 0 || edgesRemoved != 0 {
		t.Fatalf("capability-hiding eviction = nodes:%d edges:%d err:%v, want 0/0/error", nodesRemoved, edgesRemoved, err)
	}
	if store.GetNode(baseNodeID) == nil || payload.handle.GetNode(payload.nodeID) == nil {
		t.Fatal("fail-closed eviction mutated a generation-aware hidden store")
	}
	if _, _, err := evictRepoAllGenerations(store, ""); err == nil {
		t.Fatal("all-generation helper accepted an empty repo prefix")
	}
}

func TestFullBaseReindexPreservesReadyPayloadGenerationsAndPointers(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
	t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "generation-scoped-reindex.sqlite"))
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedReindexScopeControlPlane(t, store)

	base := seedReadyReindexPayload(t, store, 1, "dedicated_graph", "base-layer", "base", 0)
	commit := seedReadyReindexPayload(t, store, 2, "checkout", "commit-layer", "commit", base.generationID)
	dirty := seedReadyReindexPayload(t, store, 3, "checkout", "dirty-layer", "dirty", commit.generationID)
	ref := seedReadyReindexPayload(t, store, 4, "ref_view", "ref-layer", "ref", base.generationID)
	installReindexScopePointers(t, store, base, commit, dirty, ref)
	payloads := []readyReindexPayload{base, commit, dirty, ref}
	for _, payload := range payloads {
		requireReadyReindexPayload(t, payload)
	}
	leases := graphview.NewLeaseManager()
	lease := leases.Acquire(base.generationID, commit.generationID, dirty.generationID, ref.generationID)
	t.Cleanup(lease.Release)
	if got := leases.Held(); got != len(payloads) {
		t.Fatalf("live generation leases = %d, want %d", got, len(payloads))
	}

	oldBaseID := reindexScopeRepo + "::old.go::OldBaseSymbol"
	store.AddNode(&graph.Node{
		ID: oldBaseID, Kind: graph.KindFunction, Name: "OldBaseSymbol",
		FilePath: reindexScopeRepo + "::old.go", RepoPrefix: reindexScopeRepo, Language: "go",
	})

	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "fresh.go"), `package sample

func FreshBaseSymbol() int { return 1 }
`)
	idx := newTestIndexer(store)
	idx.SetRepoPrefix(reindexScopeRepo)
	t.Cleanup(idx.Close)
	if _, err := idx.Index(repo); err != nil {
		t.Fatalf("full base reindex: %v", err)
	}
	if got := idx.indexCount.Load(); got != 1 {
		t.Fatalf("index count = %d, want the cold-shadow persistence path", got)
	}

	if got := store.GetNode(oldBaseID); got != nil {
		t.Fatalf("generation 0 retained stale node: %+v", got)
	}
	if got := store.FindNodesByNameInRepo("FreshBaseSymbol", reindexScopeRepo); len(got) == 0 {
		t.Fatal("generation 0 did not receive the replacement corpus")
	}
	for _, payload := range payloads {
		requireReadyReindexPayload(t, payload)
	}

	ctx := context.Background()
	graphRow, found, err := store.Catalog().GetDedicatedGraph(ctx, reindexScopeGraphID)
	if err != nil || !found || graphRow.ActiveGenerationID != base.generationID {
		t.Fatalf("dedicated graph pointer = %+v found=%v err=%v", graphRow, found, err)
	}
	route, found, err := store.Catalog().GetCheckoutRoute(ctx, reindexScopeCheckoutID)
	if err != nil || !found || route.CommitGenerationID != commit.generationID || route.DirtyGenerationID != dirty.generationID || route.State != store_sqlite.RouteActive {
		t.Fatalf("checkout route = %+v found=%v err=%v", route, found, err)
	}
	refView, found, err := store.Catalog().GetRefView(ctx, reindexScopeRefViewID)
	if err != nil || !found || refView.ActiveGenerationID != ref.generationID || refView.State != store_sqlite.RefViewReady {
		t.Fatalf("ref view = %+v found=%v err=%v", refView, found, err)
	}
	for _, generationID := range lease.IDs() {
		if !leases.InUse(generationID) {
			t.Fatalf("full reindex dropped live lease for generation %d", generationID)
		}
	}

	lease.Release()
	if got := leases.Held(); got != 0 {
		t.Fatalf("released generation leases = %d, want 0", got)
	}
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
	mi.repos[reindexScopeRepo] = &RepoMetadata{
		RepoPrefix: reindexScopeRepo, RootPath: repo,
		NodeCount: len(store.GetRepoNodes(reindexScopeRepo)),
	}
	mi.indexers[reindexScopeRepo] = idx
	nodesRemoved, _ := mi.UntrackRepo(reindexScopeRepo)
	if nodesRemoved == 0 {
		t.Fatal("authoritative untrack reported no removed base nodes")
	}
	if got := store.GetRepoNodes(reindexScopeRepo); len(got) != 0 {
		t.Fatalf("authoritative untrack retained %d generation-0 nodes", len(got))
	}
	for _, payload := range payloads {
		if got := payload.handle.GetNode(payload.nodeID); got != nil {
			t.Fatalf("authoritative untrack retained generation %d node %s", payload.generationID, payload.nodeID)
		}
		fileRows, fileErr := payload.handle.FileMetasForRepo(reindexScopeRepo)
		if fileErr != nil || len(fileRows) != 0 {
			t.Fatalf("generation %d file sidecar rows = %d err=%v, want empty", payload.generationID, len(fileRows), fileErr)
		}
		bindings, bindingErr := payload.handle.SemanticBindingTypes([]graph.SemanticBindingSite{payload.bindingSite})
		if bindingErr != nil || len(bindings) != 0 {
			t.Fatalf("generation %d semantic sidecar rows = %d err=%v, want empty", payload.generationID, len(bindings), bindingErr)
		}
	}
}
