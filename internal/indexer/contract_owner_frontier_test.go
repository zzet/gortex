package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestContractRegistryPreloadSkipsEmptyIncrementalWork(t *testing.T) {
	idx := New(graph.New(), newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix("fixture")
	idx.storeRootPath(t.TempDir())
	require.Nil(t, idx.contractRegistry)
	require.NoError(t, idx.coordinateRepositoryMutation(context.Background(), OutputEntryIndexFile, func() error {
		plan, reparsed, failed, raced := idx.reindexIncrementalFilesBatched(nil, nil, &reparsePendingEnrichmentBatch{}, false)
		assert.Empty(t, plan.Files)
		assert.Empty(t, reparsed)
		assert.Empty(t, failed)
		assert.Empty(t, raced)
		assert.Nil(t, idx.contractRegistry, "a zero-input batch must not materialize the scoped registry")
		result := idx.evictFileIncrementalRaw("")
		assert.NotNil(t, result.result)
		assert.Nil(t, idx.contractRegistry, "invalid/empty explicit deletion must return before registry hydration")
		return nil
	}))
}

func bridgeDeletionOwnerSnapshot(t *testing.T, store graph.Store, consumer contracts.Contract) string {
	t.Helper()
	var snapshots []struct {
		Edge *graph.Edge    `json:"edge"`
		Meta map[string]any `json:"meta"`
	}
	for _, row := range graph.ReadRepoEdgesByKinds(store, []string{consumer.RepoPrefix}, []graph.EdgeKind{graph.EdgeConsumes}) {
		if row.Edge.From == consumer.SymbolID && row.Edge.To == consumer.ID {
			snapshots = append(snapshots, struct {
				Edge *graph.Edge    `json:"edge"`
				Meta map[string]any `json:"meta"`
			}{row.Edge, row.Edge.Meta})
		}
	}
	encoded, err := json.Marshal(snapshots)
	require.NoError(t, err)
	return string(encoded)
}

func TestBridgeRegistryRestartFileDeletionRetainsOtherRepositoryOwner(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sameRepo bool
	}{
		{name: "cross_repository"},
		{name: "same_repository", sameRepo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testBridgeRegistryRestartFileDeletion(t, tc.sameRepo)
		})
	}
}

func testBridgeRegistryRestartFileDeletion(t *testing.T, sameRepo bool) {
	t.Helper()
	rootA, rootB := t.TempDir(), t.TempDir()
	if sameRepo {
		rootB = rootA
	}
	const relativeA = "provider.go"
	fileA := filepath.Join(rootA, relativeA)
	require.NoError(t, os.WriteFile(fileA, []byte("package fixture\nfunc Provide() {}\n"), 0o600))
	fileB := filepath.Join(rootB, "consumer.go")
	require.NoError(t, os.WriteFile(fileB, []byte("package fixture\nfunc Consume() {}\n"), 0o600))
	storePath := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(storePath)
	require.NoError(t, err)
	t.Cleanup(func() {
		if store != nil {
			require.NoError(t, store.Close())
		}
	})
	provider := contracts.Contract{
		ID: "env::FILE_DELETE_SHARED", Type: contracts.ContractType("env"), Role: contracts.RoleProvider,
		RepoPrefix: "repo-a", WorkspaceID: "workspace-a", ProjectID: "project-a",
		FilePath: "repo-a/provider.go", SymbolID: "repo-a/provider.go::Provide", Line: 2, Confidence: 1,
		Meta: map[string]any{"var": "FILE_DELETE_SHARED", "owner": "a"},
	}
	consumer := contracts.Contract{
		ID: provider.ID, Type: contracts.ContractType("env"), Role: contracts.RoleConsumer,
		RepoPrefix: "repo-b", WorkspaceID: "workspace-b", ProjectID: "project-b",
		FilePath: "repo-b/consumer.go", SymbolID: "repo-b/consumer.go::Consume", Line: 2, Confidence: 0.75,
		Meta: map[string]any{"var": "FILE_DELETE_SHARED", "owner": "b", "nested": map[string]any{"values": []any{"survives", "deletion"}}},
	}
	if sameRepo {
		consumer.RepoPrefix, consumer.WorkspaceID, consumer.ProjectID = provider.RepoPrefix, provider.WorkspaceID, provider.ProjectID
		consumer.FilePath = "repo-a/consumer.go"
		consumer.SymbolID = consumer.FilePath + "::Consume"
	}
	sourceA := &graph.Node{ID: provider.SymbolID, Kind: graph.KindFunction, FilePath: provider.FilePath, RepoPrefix: provider.RepoPrefix, WorkspaceID: provider.WorkspaceID, ProjectID: provider.ProjectID}
	sourceB := &graph.Node{ID: consumer.SymbolID, Kind: graph.KindFunction, FilePath: consumer.FilePath, RepoPrefix: consumer.RepoPrefix, WorkspaceID: consumer.WorkspaceID, ProjectID: consumer.ProjectID}
	canonical := &graph.Node{ID: provider.ID, Kind: graph.KindContract, FilePath: provider.FilePath, RepoPrefix: provider.RepoPrefix, WorkspaceID: provider.WorkspaceID, ProjectID: provider.ProjectID,
		Meta: map[string]any{"type": string(provider.Type), "role": string(provider.Role), "symbol_id": provider.SymbolID, "line": provider.Line, "confidence": provider.Confidence, "contract_meta": provider.Meta}}
	edgeA := &graph.Edge{From: provider.SymbolID, To: provider.ID, Kind: graph.EdgeProvides, FilePath: provider.FilePath, Line: provider.Line, Meta: contractOwnerEdgeMeta(provider)}
	edgeB := &graph.Edge{From: consumer.SymbolID, To: consumer.ID, Kind: graph.EdgeConsumes, FilePath: consumer.FilePath, Line: consumer.Line, Meta: contractOwnerEdgeMeta(consumer)}
	store.AddBatch([]*graph.Node{
		{ID: provider.FilePath, Kind: graph.KindFile, FilePath: provider.FilePath, RepoPrefix: provider.RepoPrefix},
		{ID: consumer.FilePath, Kind: graph.KindFile, FilePath: consumer.FilePath, RepoPrefix: consumer.RepoPrefix},
		sourceA, sourceB, canonical,
	}, []*graph.Edge{edgeA, edgeB})
	require.Len(t, store.GetInEdges(provider.ID), 2, "persist both actual ownership edges before simulating restart")
	require.Equal(t, provider.FilePath, store.GetNode(provider.ID).FilePath, "canonical scalar must be attributed to the file being deleted")
	ownerRowsB := graph.ReadRepoEdgesByKinds(store, []string{consumer.RepoPrefix}, []graph.EdgeKind{graph.EdgeConsumes})
	require.Len(t, ownerRowsB, 1)
	require.Equal(t, consumer.Meta, ownerRowsB[0].Edge.Meta["contract_owner_meta"], "fixture comparison must include real hydrated owner metadata")
	beforeB := bridgeDeletionOwnerSnapshot(t, store, consumer)
	require.NoError(t, store.Close())
	store = nil
	store, err = store_sqlite.Open(storePath)
	require.NoError(t, err)
	idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix(provider.RepoPrefix)
	idx.SetWorkspaceID(provider.WorkspaceID)
	idx.SetProjectID(provider.ProjectID)
	idx.storeRootPath(rootA)
	t.Cleanup(func() {
		if idx != nil {
			idx.Close()
		}
	})
	require.Nil(t, idx.contractRegistry, "restart fixture must not hide lost durable records behind an in-memory registry")
	require.Len(t, store.GetOutEdges(sourceB.ID), 1, "B must still have its persisted owner after reopen")
	require.NoError(t, os.Remove(fileA))
	require.NoError(t, idx.coordinateRepositoryMutation(context.Background(), OutputEntryIndexFile, func() error {
		idx.evictFileIncrementalRaw(relativeA)
		return nil
	}))
	assert.Nil(t, store.GetNode(provider.SymbolID), "real raw deletion removes A's source symbol")
	assert.Nil(t, store.GetNode(provider.FilePath), "real raw deletion removes A's file node")
	assert.NotNil(t, store.GetNode(consumer.SymbolID), "unrelated B source remains")
	assert.NotNil(t, store.GetNode(provider.ID), "B still requires the shared canonical contract node")
	afterB := bridgeDeletionOwnerSnapshot(t, store, consumer)
	assert.Equal(t, beforeB, afterB, "deleting A must retain B's complete persisted ownership edge, including metadata")
	checkRegistryB := func(stage string) {
		t.Helper()
		var got []contracts.Contract
		if registry := contracts.LoadRegistryFromGraph(store, consumer.RepoPrefix); registry != nil {
			got = registry.All()
		}
		assert.Equal(t, bridgeRoundtripRecordMultiset(t, []contracts.Contract{consumer}), bridgeRoundtripRecordMultiset(t, got), "%s: surviving B must hydrate without another repository reparse", stage)
	}
	checkRegistryB("after deletion")
	idx.Close()
	idx = nil
	require.NoError(t, store.Close())
	store = nil
	store, err = store_sqlite.Open(storePath)
	require.NoError(t, err)
	assert.NotNil(t, store.GetNode(provider.ID), "shared canonical preservation is durable")
	afterReopenB := bridgeDeletionOwnerSnapshot(t, store, consumer)
	assert.Equal(t, beforeB, afterReopenB, "B ownership preservation is durable")
	checkRegistryB("after deletion and second reopen")
	_, err = os.Stat(fileB)
	require.NoError(t, err, "B's physical source is outside the deletion scope")

	// A preservation fix must not keep every contract forever. Delete the last
	// actual owner through another fresh, nil-registry indexer and audit durable
	// node/edge rows, not only endpoint-filtered graph projections.
	idx = New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix(consumer.RepoPrefix)
	idx.SetWorkspaceID(consumer.WorkspaceID)
	idx.SetProjectID(consumer.ProjectID)
	idx.storeRootPath(rootB)
	require.Nil(t, idx.contractRegistry, "last-owner deletion also models restart")
	require.NoError(t, os.Remove(fileB))
	require.NoError(t, idx.coordinateRepositoryMutation(context.Background(), OutputEntryIndexFile, func() error {
		idx.evictFileIncrementalRaw("consumer.go")
		return nil
	}))
	assert.Nil(t, store.GetNode(consumer.SymbolID), "last owner's source symbol is removed")
	assert.Nil(t, store.GetNode(consumer.FilePath), "last owner's file node is removed")
	assert.Nil(t, store.GetNode(consumer.ID), "canonical node must not leak after final owner removal")
	assert.Empty(t, store.GetInEdges(consumer.ID))
	assert.Empty(t, store.GetOutEdges(consumer.ID))
	idx.Close()
	idx = nil
	require.NoError(t, store.Close())
	store = nil
	db, err := sql.Open("sqlite", storePath)
	require.NoError(t, err)
	defer db.Close()
	var canonicalNodes, incidentEdges int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM nodes WHERE view_gen = 0 AND id = ?", consumer.ID).Scan(&canonicalNodes))
	require.NoError(t, db.QueryRow("SELECT count(*) FROM edges WHERE view_gen = 0 AND (from_id = ? OR to_id = ?)", consumer.ID, consumer.ID).Scan(&incidentEdges))
	assert.Zero(t, canonicalNodes, "last-owner canonical pruning is durable")
	assert.Zero(t, incidentEdges, "no orphan incident rows remain after last-owner pruning")
}

func TestBridgeRegistryRestartOffFileCanonicalDeletionPlansContractReconcile(t *testing.T) {
	root := t.TempDir()
	const relativeA = "provider.go"
	require.NoError(t, os.WriteFile(filepath.Join(root, relativeA), []byte("package fixture\nfunc Provide() {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "consumer.go"), []byte("package fixture\nfunc Consume() {}\n"), 0o600))
	storePath := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(storePath)
	require.NoError(t, err)
	var idx *Indexer
	t.Cleanup(func() {
		if idx != nil {
			idx.Close()
		}
		if store != nil {
			require.NoError(t, store.Close())
		}
	})
	provider := contracts.Contract{
		ID: "env::OFF_FILE_DELETE_PLAN", Type: contracts.ContractType("env"), Role: contracts.RoleProvider,
		RepoPrefix: "fixture", WorkspaceID: "workspace", ProjectID: "project",
		FilePath: "fixture/provider.go", SymbolID: "fixture/provider.go::Provide", Line: 2, Confidence: 1,
		Meta: map[string]any{"var": "OFF_FILE_DELETE_PLAN"},
	}
	consumer := provider
	consumer.Role = contracts.RoleConsumer
	consumer.FilePath, consumer.SymbolID = "fixture/consumer.go", "fixture/consumer.go::Consume"
	// The stored contract is complete by the actual production predicate. Seed
	// deterministic fingerprints explicitly; no parser probe is being tested.
	fileA := &graph.Node{ID: provider.FilePath, Kind: graph.KindFile, FilePath: provider.FilePath, RepoPrefix: provider.RepoPrefix,
		Meta: map[string]any{
			sourceDerivedDeclFingerprintMeta:     "fixture-declarations",
			sourceDerivedImportFingerprintMeta:   "fixture-imports",
			sourceDerivedRuntimeFingerprintMeta:  "fixture-runtime",
			sourceDerivedArtifactFingerprintMeta: "fixture-artifacts",
		}}
	store.AddBatch([]*graph.Node{
		fileA,
		{ID: consumer.FilePath, Kind: graph.KindFile, FilePath: consumer.FilePath, RepoPrefix: consumer.RepoPrefix},
		{ID: provider.SymbolID, Name: "Provide", Kind: graph.KindFunction, FilePath: provider.FilePath, RepoPrefix: provider.RepoPrefix, WorkspaceID: provider.WorkspaceID, ProjectID: provider.ProjectID},
		{ID: consumer.SymbolID, Name: "Consume", Kind: graph.KindFunction, FilePath: consumer.FilePath, RepoPrefix: consumer.RepoPrefix, WorkspaceID: consumer.WorkspaceID, ProjectID: consumer.ProjectID},
	}, nil)
	idx = New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix(provider.RepoPrefix)
	idx.SetWorkspaceID(provider.WorkspaceID)
	idx.SetProjectID(provider.ProjectID)
	idx.storeRootPath(root)
	registry := contracts.NewRegistry()
	registry.Add(provider)
	registry.Add(consumer)
	matched := contracts.Match(registry)
	require.Len(t, matched.Matched, 1, "fixture must produce a real matched provider/consumer pair")
	idx.commitContracts(registry)
	canonical := store.GetNode(provider.ID)
	require.NotNil(t, canonical)
	require.Equal(t, consumer.FilePath, canonical.FilePath, "shared canonical scalar must belong to surviving B, outside deleted A")
	require.Equal(t, 1, MaterializeContractBridges(store, matched.Matched), "fixture must materialize a real bridge")
	var bridgeID string
	for _, node := range store.GetFileNodes(ContractBridgeFilePath) {
		if node.Kind == graph.KindContractBridge {
			require.Empty(t, bridgeID, "fixture has exactly one bridge")
			bridgeID = node.ID
		}
	}
	require.NotEmpty(t, bridgeID)
	var bridgeEdges int
	for _, edge := range store.GetInEdges(provider.ID) {
		if edge.Kind == graph.EdgeBridges && edge.From == bridgeID {
			bridgeEdges++
		}
	}
	require.Equal(t, 1, bridgeEdges, "shared-ID bridge must have its actual canonical attachment")
	idx.Close()
	idx = nil
	require.NoError(t, store.Close())
	store = nil
	store, err = store_sqlite.Open(storePath)
	require.NoError(t, err)
	idx = New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix(provider.RepoPrefix)
	idx.SetWorkspaceID(provider.WorkspaceID)
	idx.SetProjectID(provider.ProjectID)
	idx.storeRootPath(root)
	require.Nil(t, idx.contractRegistry, "restarted indexer must not use the cold in-memory registry")
	prior := store.GetFileNodes(provider.FilePath)
	require.True(t, storedDerivedFingerprints(prior).complete(), "durable complete fingerprints disable broad legacy fallback")
	for _, node := range prior {
		require.NotEqual(t, graph.KindContract, node.Kind, "A's by-file node set deliberately excludes the off-file canonical")
	}
	require.NotNil(t, store.GetNode(bridgeID), "matched bridge survives the fixture restart")
	require.Equal(t, consumer.FilePath, store.GetNode(provider.ID).FilePath)
	require.NoError(t, os.Remove(filepath.Join(root, relativeA)))
	var eviction forcedFileEviction
	require.NoError(t, idx.coordinateRepositoryMutation(context.Background(), OutputEntryIndexFile, func() error {
		eviction = idx.evictFileIncrementalRaw(relativeA)
		return nil
	}))
	require.NotNil(t, eviction.result, "actual raw path must return its IndexResult before inspecting the plan")
	invalidation := eviction.result.DerivedInvalidation
	require.False(t, invalidation.LegacyFallback, "complete fingerprints must not accidentally make this regression pass through broad fallback")
	assert.Nil(t, store.GetNode(provider.SymbolID), "A's source was physically and structurally removed")
	assert.NotNil(t, store.GetNode(consumer.SymbolID), "B's source remains outside deletion scope")
	assert.NotNil(t, store.GetNode(provider.ID), "B still owns the shared canonical")
	assert.True(t, invalidation.Flags.Has(DerivedInvalidatesContracts), "deleting A's ownership must schedule contract reconciliation even though its canonical scalar is off-file")
	assert.Contains(t, invalidation.ContractGroups, ContractGroupFrontier{WorkspaceID: provider.WorkspaceID, ProjectID: provider.ProjectID, ContractID: provider.ID})
	assert.Contains(t, invalidation.ContractSymbolIDs, provider.SymbolID)
	// Explicit bridge IDs are optional: the actual frontier consumer derives
	// bridge cleanup from scoped contract groups and incoming bridge edges.
	t.Logf("complete_fingerprints=true initial_matches=%d initial_bridges=1 contract_flag=%t groups=%d symbols=%d bridge_ids=%d legacy_fallback=%t",
		len(matched.Matched), invalidation.Flags.Has(DerivedInvalidatesContracts), len(invalidation.ContractGroups), len(invalidation.ContractSymbolIDs), len(invalidation.ContractBridgeNodeIDs), invalidation.LegacyFallback)
}

func registeredBridgeNodeSnapshot(t *testing.T, store graph.Store, id string) string {
	t.Helper()
	node := store.GetNode(id)
	require.NotNil(t, node)
	encoded, err := json.Marshal(struct {
		Node *graph.Node
		Meta map[string]any
	}{node, node.Meta})
	require.NoError(t, err)
	return string(encoded)
}

func registeredBridgeOwnerSnapshot(t *testing.T, store graph.Store, source string) []string {
	t.Helper()
	var rows []string
	for _, edge := range store.GetOutEdges(source) {
		if edge.Kind != graph.EdgeProvides && edge.Kind != graph.EdgeConsumes {
			continue
		}
		encoded, err := json.Marshal(struct {
			Edge *graph.Edge
			Meta map[string]any
		}{edge, edge.Meta})
		require.NoError(t, err)
		rows = append(rows, string(encoded))
	}
	sort.Strings(rows)
	return rows
}

func registeredBridgeAttachment(t *testing.T, store graph.Store, contractID string) string {
	t.Helper()
	var id string
	for _, edge := range store.GetInEdges(contractID) {
		if edge.Kind != graph.EdgeBridges {
			continue
		}
		require.Empty(t, id, "one canonical attachment per fixture bridge")
		id = edge.From
	}
	require.NotEmpty(t, id)
	return id
}

func registeredBridgeMatchSnapshot(t *testing.T, store graph.Store, ids ...string) map[string]struct{} {
	t.Helper()
	rows := make(map[string]struct{})
	for _, id := range ids {
		edges := append([]*graph.Edge(nil), store.GetInEdges(id)...)
		edges = append(edges, store.GetOutEdges(id)...)
		for _, edge := range edges {
			if edge.Kind != graph.EdgeMatches {
				continue
			}
			encoded, err := json.Marshal(struct {
				Edge *graph.Edge
				Meta map[string]any
			}{edge, edge.Meta})
			require.NoError(t, err)
			rows[string(encoded)] = struct{}{}
		}
	}
	return rows
}

func TestBridgeRegistryRegisteredRestartDispatchPreservesSurvivors(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, name := range []string{"provider.go", "consumer.go", "other_provider.go", "other_consumer.go"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("package fixture\nfunc "+map[string]string{"provider.go": "Provide", "consumer.go": "Consume", "other_provider.go": "OtherProvide", "other_consumer.go": "OtherConsume"}[name]+"() {}\n"), 0o600))
	}
	entry := config.RepoEntry{Path: root, Name: "fixture"}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	global := &config.GlobalConfig{Repos: []config.RepoEntry{entry}}
	global.SetConfigPath(configPath)
	require.NoError(t, global.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	var idx *Indexer
	t.Cleanup(func() {
		if idx != nil {
			idx.Close()
		}
		if store != nil {
			require.NoError(t, store.Close())
		}
	})
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
	_, err = mi.ReconcileRepoCtx(ctx, entry, nil)
	require.NoError(t, err)
	idx = mi.indexers["fixture"]
	require.NotNil(t, idx, "real registration must create the Indexer and metadata")
	idx.SetWorkspaceID("workspace")
	idx.SetProjectID("project")
	record := func(id, file, symbol string, role contracts.Role) contracts.Contract {
		return contracts.Contract{ID: id, Type: contracts.ContractType("env"), Role: role, RepoPrefix: "fixture", WorkspaceID: "workspace", ProjectID: "project",
			FilePath: "fixture/" + file, SymbolID: "fixture/" + file + "::" + symbol, Line: 2, Confidence: 1,
			Meta: map[string]any{"var": id, "payload": map[string]any{"source": file}}}
	}
	provider := record("env::REGISTERED_DELETE", "provider.go", "Provide", contracts.RoleProvider)
	consumer := record(provider.ID, "consumer.go", "Consume", contracts.RoleConsumer)
	otherProvider := record("env::REGISTERED_UNRELATED", "other_provider.go", "OtherProvide", contracts.RoleProvider)
	otherConsumer := record(otherProvider.ID, "other_consumer.go", "OtherConsume", contracts.RoleConsumer)
	records := []contracts.Contract{provider, consumer, otherProvider, otherConsumer}
	var sources []*graph.Node
	for _, c := range records {
		sources = append(sources, &graph.Node{ID: c.SymbolID, Name: c.SymbolID, Kind: graph.KindFunction, FilePath: c.FilePath, RepoPrefix: c.RepoPrefix, WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID})
	}
	// Complete real deletion predicate, independent of the parser's own
	// graph ID convention. Persisted mtimes come from actual registration.
	sources = append(sources, &graph.Node{ID: provider.FilePath, Kind: graph.KindFile, FilePath: provider.FilePath, RepoPrefix: provider.RepoPrefix,
		Meta: map[string]any{sourceDerivedDeclFingerprintMeta: "decl", sourceDerivedImportFingerprintMeta: "imports", sourceDerivedRuntimeFingerprintMeta: "runtime", sourceDerivedArtifactFingerprintMeta: "artifacts"}})
	store.AddBatch(sources, nil)
	registry := contracts.NewRegistry()
	for _, c := range records {
		registry.Add(c)
	}
	idx.commitContracts(registry)
	matched := contracts.Match(registry)
	require.Len(t, matched.Matched, 2)
	require.Equal(t, 2, MaterializeContractBridges(store, matched.Matched))
	// Model the cold complete extraction registry, then persist match edges
	// through the actual public derived dispatcher, not synthetic edge rows.
	idx.contractRegistry = registry
	initialPlan := DerivedInvalidationPlan{Files: []string{provider.FilePath, consumer.FilePath, otherProvider.FilePath, otherConsumer.FilePath}}
	initialPlan.Flags |= DerivedInvalidatesContracts
	initialPlan.ContractGroups = []ContractGroupFrontier{
		{WorkspaceID: provider.WorkspaceID, ProjectID: provider.ProjectID, ContractID: provider.ID},
		{WorkspaceID: otherProvider.WorkspaceID, ProjectID: otherProvider.ProjectID, ContractID: otherProvider.ID},
	}
	initialReport := mi.RunIncrementalDerivedPasses(ctx, map[string]DerivedInvalidationPlan{"fixture": initialPlan})
	require.Positive(t, initialReport.Contracts, "actual initial contract dispatcher must process the real matches")
	require.NotEmpty(t, registeredBridgeMatchSnapshot(t, store, provider.ID, provider.SymbolID, consumer.SymbolID), "fixture persists a real match edge")
	require.Equal(t, consumer.FilePath, store.GetNode(provider.ID).FilePath)
	staleBridge := registeredBridgeAttachment(t, store, provider.ID)
	unrelatedBridge := registeredBridgeAttachment(t, store, otherProvider.ID)
	require.NotEqual(t, staleBridge, unrelatedBridge)
	require.Len(t, store.LoadFileMtimes("fixture"), 4, "real registration persisted the unchanged restart census")
	idx.Close()
	idx = nil
	require.NoError(t, store.Close())
	store = nil
	store, err = store_sqlite.Open(dbPath)
	require.NoError(t, err)
	mi = NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
	_, err = mi.ReconcileRepoCtx(ctx, entry, store.LoadFileMtimes("fixture"))
	require.NoError(t, err)
	idx = mi.indexers["fixture"]
	require.NotNil(t, idx)
	idx.SetWorkspaceID("workspace")
	idx.SetProjectID("project")
	t.Logf("natural registered restart contract_registry_nil=%t", idx.contractRegistry == nil)
	require.NotNil(t, store.GetNode(staleBridge), "registration must not erase the positive bridge fixture")
	require.True(t, storedDerivedFingerprints(store.GetFileNodes(provider.FilePath)).complete())
	require.Equal(t, consumer.FilePath, store.GetNode(provider.ID).FilePath)
	beforeConsumer := registeredBridgeNodeSnapshot(t, store, provider.ID)
	beforeOwner := registeredBridgeOwnerSnapshot(t, store, consumer.SymbolID)
	require.Len(t, beforeOwner, 1)
	beforeUnrelated := registeredBridgeNodeSnapshot(t, store, unrelatedBridge)
	beforeUnrelatedOwner := registeredBridgeOwnerSnapshot(t, store, otherConsumer.SymbolID)
	beforeUnrelatedMatches := registeredBridgeMatchSnapshot(t, store, otherProvider.ID, otherProvider.SymbolID, otherConsumer.SymbolID)
	require.NotEmpty(t, beforeUnrelatedMatches, "unrelated persisted matches survive registration")
	require.NotEmpty(t, registeredBridgeMatchSnapshot(t, store, provider.ID, provider.SymbolID, consumer.SymbolID), "deleted pair has a positive persisted match before eviction")
	require.NoError(t, os.Remove(filepath.Join(root, "provider.go")))
	var eviction forcedFileEviction
	require.NoError(t, idx.coordinateRepositoryMutation(ctx, OutputEntryIndexFile, func() error { eviction = idx.evictFileIncrementalRaw("provider.go"); return nil }))
	require.NotNil(t, eviction.result)
	plan := eviction.result.DerivedInvalidation
	require.False(t, plan.LegacyFallback)
	assert.True(t, plan.Flags.Has(DerivedInvalidatesContracts))
	assert.Contains(t, plan.ContractGroups, ContractGroupFrontier{WorkspaceID: provider.WorkspaceID, ProjectID: provider.ProjectID, ContractID: provider.ID})
	assert.Contains(t, plan.ContractSymbolIDs, provider.SymbolID)
	// Actual public dispatcher enforces the flag gate and reconciles groups.
	// The group itself derives/discovers bridge IDs; an explicit ID list is
	// not required and is intentionally not asserted here.
	report := mi.RunIncrementalDerivedPasses(ctx, map[string]DerivedInvalidationPlan{"fixture": plan})
	t.Logf("derived_contracts=%d repos=%d files=%d", report.Contracts, report.Repos, report.Files)
	assert.Nil(t, store.GetNode(staleBridge), "the stale matched bridge must disappear through actual gated dispatch")
	assert.Equal(t, beforeConsumer, registeredBridgeNodeSnapshot(t, store, provider.ID), "surviving B canonical metadata stays exact")
	assert.Equal(t, beforeOwner, registeredBridgeOwnerSnapshot(t, store, consumer.SymbolID))
	assert.Equal(t, beforeUnrelated, registeredBridgeNodeSnapshot(t, store, unrelatedBridge), "unrelated bridge group remains exact")
	assert.Equal(t, beforeUnrelatedOwner, registeredBridgeOwnerSnapshot(t, store, otherConsumer.SymbolID))
	assert.Equal(t, unrelatedBridge, registeredBridgeAttachment(t, store, otherProvider.ID))
	assert.Equal(t, beforeUnrelatedMatches, registeredBridgeMatchSnapshot(t, store, otherProvider.ID, otherProvider.SymbolID, otherConsumer.SymbolID))
	assert.Empty(t, registeredBridgeMatchSnapshot(t, store, provider.ID, provider.SymbolID, consumer.SymbolID), "deleted pair has no stale match")
	for _, edge := range store.GetInEdges(provider.ID) {
		assert.NotEqual(t, graph.EdgeBridges, edge.Kind, "deleted pair has no surviving bridge attachment")
		assert.NotEqual(t, graph.EdgeMatches, edge.Kind, "deleted pair has no stale match")
	}
}

// Inspect the actual temp-store FTS row schema without assuming a column name.
// The fixture IDs are unique and matched exactly, never against token substrings.
func contractFTSRowsForID(t *testing.T, db *sql.DB, id string) []string {
	t.Helper()
	rows, err := db.Query("SELECT * FROM symbol_fts")
	require.NoError(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	require.NoError(t, err)
	var found []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(t, rows.Scan(pointers...))
		match := false
		for i, value := range values {
			if raw, ok := value.([]byte); ok {
				value = string(raw)
				values[i] = value
			}
			if value == id {
				match = true
			}
		}
		if match {
			encoded, marshalErr := json.Marshal(values)
			require.NoError(t, marshalErr)
			found = append(found, string(encoded))
		}
	}
	require.NoError(t, rows.Err())
	return found
}

type contractFTSEvictionFixture struct {
	store              *store_sqlite.Store
	db                 *sql.DB
	idx                *Indexer
	root               string
	provider, consumer contracts.Contract
}

func newContractFTSEvictionFixture(t *testing.T) contractFTSEvictionFixture {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "provider.go"), []byte("package fixture\nfunc Provide() {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "consumer.go"), []byte("package fixture\nfunc Consume() {}\n"), 0o600))
	storePath := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(storePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	db, err := sql.Open("sqlite", storePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix("fixture")
	idx.SetWorkspaceID("workspace")
	idx.SetProjectID("project")
	idx.storeRootPath(root)
	provider := contracts.Contract{
		ID: "env::FTS_FILE_EVICTION", Type: contracts.ContractType("env"), Role: contracts.RoleProvider,
		RepoPrefix: "fixture", WorkspaceID: "workspace", ProjectID: "project",
		FilePath: "fixture/provider.go", SymbolID: "fixture/provider.go::Provide", Line: 2, Confidence: 1,
		Meta: map[string]any{"var": "FTS_FILE_EVICTION"},
	}
	consumer := provider
	consumer.Role = contracts.RoleConsumer
	consumer.FilePath, consumer.SymbolID = "fixture/consumer.go", "fixture/consumer.go::Consume"
	store.AddBatch([]*graph.Node{
		{ID: provider.FilePath, Kind: graph.KindFile, FilePath: provider.FilePath, RepoPrefix: provider.RepoPrefix},
		{ID: consumer.FilePath, Kind: graph.KindFile, FilePath: consumer.FilePath, RepoPrefix: consumer.RepoPrefix},
		{ID: provider.SymbolID, Name: "Provide", Kind: graph.KindFunction, FilePath: provider.FilePath, RepoPrefix: provider.RepoPrefix, Language: "go"},
		{ID: consumer.SymbolID, Name: "Consume", Kind: graph.KindFunction, FilePath: consumer.FilePath, RepoPrefix: consumer.RepoPrefix, Language: "go"},
	}, nil)
	registry := contracts.NewRegistry()
	registry.Add(consumer)
	registry.Add(provider)
	idx.commitContracts(registry)
	canonical := store.GetNode(provider.ID)
	require.NotNil(t, canonical)
	require.Equal(t, provider.FilePath, canonical.FilePath, "canonical is in A's node-file eviction frontier")
	require.Len(t, store.GetInEdges(provider.ID), 2, "surviving B owner must exist")
	require.Empty(t, contractFTSRowsForID(t, db, provider.ID), "cold contract writer alone does not seed the FTS control")
	require.Empty(t, contractFTSRowsForID(t, db, consumer.SymbolID), "ordinary positive control starts without synthetic FTS rows")
	require.NoError(t, idx.populateSymbolFTS(nil), "invoke the actual supported store rebuild, not a direct FTS upsert")
	require.Len(t, contractFTSRowsForID(t, db, consumer.SymbolID), 1, "actual ordinary-symbol rebuild proves the path is non-vacuous")
	require.Len(t, contractFTSRowsForID(t, db, provider.ID), 1, "actual rebuild legitimately indexes the persisted contract")
	return contractFTSEvictionFixture{store: store, db: db, idx: idx, root: root, provider: provider, consumer: consumer}
}

func TestContractFTSRebuildLegitimatelyIndexesPersistedContract(t *testing.T) {
	fixture := newContractFTSEvictionFixture(t)
	t.Logf("actual_rebuild ordinary_rows=%d contract_rows=%d", len(contractFTSRowsForID(t, fixture.db, fixture.consumer.SymbolID)), len(contractFTSRowsForID(t, fixture.db, fixture.provider.ID)))
}

func TestContractFTSRemoveFromSearchActualBackend(t *testing.T) {
	f := newContractFTSEvictionFixture(t)
	beforeB := contractFTSRowsForID(t, f.db, f.consumer.SymbolID)
	require.Len(t, contractFTSRowsForID(t, f.db, f.provider.ID), 1)
	f.idx.removeFromSearch(f.store.GetNode(f.provider.ID))
	after := contractFTSRowsForID(t, f.db, f.provider.ID)
	assert.Equal(t, beforeB, contractFTSRowsForID(t, f.db, f.consumer.SymbolID), "targeted search removal must leave B's ordinary document unchanged")
	assert.NotNil(t, f.store.GetNode(f.provider.ID), "search removal is not graph-node removal")
	_, churn := any(f.store).(graph.ChurnEnrichmentWriter)
	_, coverage := any(f.store).(graph.CoverageEnrichmentWriter)
	_, releases := any(f.store).(graph.ReleaseEnrichmentWriter)
	_, blame := any(f.store).(graph.BlameEnrichmentWriter)
	t.Logf("backend=%T before_contract_fts=1 after_remove_from_search=%d churn_writer=%t coverage_writer=%t release_writer=%t blame_writer=%t",
		f.idx.search, len(after), churn, coverage, releases, blame)
}

func TestContractFTSFileEvictionPreservesRetainedCanonicalRows(t *testing.T) {
	for _, rawIndexer := range []bool{false, true} {
		name := "store_batch"
		if rawIndexer {
			name = "indexer_raw"
		}
		t.Run(name, func(t *testing.T) {
			f := newContractFTSEvictionFixture(t)
			beforeCanonical := contractFTSRowsForID(t, f.db, f.provider.ID)
			beforeB := contractFTSRowsForID(t, f.db, f.consumer.SymbolID)
			beforeA := contractFTSRowsForID(t, f.db, f.provider.SymbolID)
			require.Len(t, beforeA, 1, "deleted ordinary A symbol also starts indexed")
			require.NoError(t, os.Remove(filepath.Join(f.root, "provider.go")))
			if rawIndexer {
				// This fixture isolates sidecar accounting; reset the registry so
				// a cold in-memory registry cannot hide the deletion boundary.
				f.idx.contractRegistry = nil
				require.NoError(t, f.idx.coordinateRepositoryMutation(context.Background(), OutputEntryIndexFile, func() error {
					f.idx.evictFileIncrementalRaw("provider.go")
					return nil
				}))
			} else {
				f.store.EvictFiles([]string{f.provider.FilePath})
			}
			assert.Nil(t, f.store.GetNode(f.provider.SymbolID))
			if rawIndexer {
				assert.Empty(t, contractFTSRowsForID(t, f.db, f.provider.SymbolID), "indexer removes A's ordinary-symbol FTS row")
			} else {
				assert.Equal(t, beforeA, contractFTSRowsForID(t, f.db, f.provider.SymbolID), "raw graph primitive leaves FTS accounting with its caller, as before")
			}
			assert.NotNil(t, f.store.GetNode(f.provider.ID), "surviving B still requires the canonical")
			assert.Equal(t, beforeCanonical, contractFTSRowsForID(t, f.db, f.provider.ID), "retain the exact legitimately rebuilt canonical search row")
			assert.Equal(t, beforeB, contractFTSRowsForID(t, f.db, f.consumer.SymbolID), "B ordinary symbol search row is outside deletion scope")
			if rawIndexer {
				// On final-owner deletion the canonical still names the old A
				// scalar path. Its now-orphaned FTS row must not leak just because
				// B's initial by-file node set does not include the canonical.
				f.idx.contractRegistry = nil
				require.NoError(t, os.Remove(filepath.Join(f.root, "consumer.go")))
				require.NoError(t, f.idx.coordinateRepositoryMutation(context.Background(), OutputEntryIndexFile, func() error {
					f.idx.evictFileIncrementalRaw("consumer.go")
					return nil
				}))
				assert.Nil(t, f.store.GetNode(f.provider.ID))
				assert.Empty(t, contractFTSRowsForID(t, f.db, f.provider.ID), "final-owner removal cleans actually orphaned off-file canonical FTS")
				assert.Empty(t, contractFTSRowsForID(t, f.db, f.consumer.SymbolID))
			}
		})
	}
}

func TestContractFTSStructuralReparsePreservesRetainedCanonicalRows(t *testing.T) {
	f := newContractFTSEvictionFixture(t)
	beforeCanonical := contractFTSRowsForID(t, f.db, f.provider.ID)
	beforeB := contractFTSRowsForID(t, f.db, f.consumer.SymbolID)
	require.Len(t, beforeCanonical, 1)
	require.Len(t, beforeB, 1)
	require.NotNil(t, f.store.GetNode(f.provider.SymbolID))
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "provider.go"),
		[]byte("package fixture\nfunc ReparsedProvider() {}\n"), 0o600))

	core, observed := observer.New(zap.DebugLevel)
	f.idx.logger = zap.New(core)
	f.idx.contractRegistry = nil
	// Reuse the receipt-aware entry and relative-path convention exercised by
	// SIZE's applyCensus; do not add another mutation-coordinator wrapper.
	_, _, _, err := f.idx.incrementalReindexPathsWithReceiptMode(f.root,
		[]string{"provider.go"}, incrementalPathMode{forceExplicitFiles: true, detectDeletions: true})
	require.NoError(t, err)

	var structuralFiles int64
	for _, entry := range observed.All() {
		for _, field := range entry.Context {
			if field.Key == "structural_files" {
				structuralFiles += field.Integer
			}
		}
	}
	require.Positive(t, structuralFiles, "actual chunk graph apply must enter its nonempty structural branch")
	assert.Nil(t, f.store.GetNode(f.provider.SymbolID), "the replaced A declaration must leave the graph")
	var reparsedDeclaration bool
	for _, node := range f.store.GetFileNodes(f.provider.FilePath) {
		if node != nil && node.Kind == graph.KindFunction && node.Name == "ReparsedProvider" {
			reparsedDeclaration = true
		}
	}
	require.True(t, reparsedDeclaration, "the new A declaration must be parsed and committed")
	require.NotNil(t, f.store.GetNode(f.provider.ID), "B still owns the canonical after A's structural reparse")
	assert.Equal(t, beforeCanonical, contractFTSRowsForID(t, f.db, f.provider.ID),
		"the real rebuilt canonical FTS row must survive structural eviction exactly")
	assert.Equal(t, beforeB, contractFTSRowsForID(t, f.db, f.consumer.SymbolID),
		"B's ordinary FTS row stays outside A's structural frontier")
}
