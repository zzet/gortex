package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

const (
	derivedShadowGraphID    = "graph-derived-shadow"
	derivedShadowLayerID    = "layer-derived-shadow"
	derivedShadowCheckoutID = "checkout-derived-shadow"
)

// derivedGenerationSnapshot is the strategy-independent surface a completed
// payload build must preserve. Generation ids and timings are intentionally
// absent: separate stores mint different ids, and direct SQLite versus a
// bounded shadow is allowed to differ only in how it reaches this payload.
type derivedGenerationSnapshot struct {
	Nodes          []string
	Edges          []string
	Files          []graph.FileMetaRow
	FileMasks      []store_sqlite.FileMask
	NodeTombstones []string
	EdgeMasks      []store_sqlite.EdgeSourceMask
	Producers      []store_sqlite.ProducerCompleteness
	SearchHits     map[string][]string
	ContentHits    []string

	IndexedPaths     []string
	NodeCount        int
	EdgeCount        int
	ReplaceMasks     int
	DeleteMasks      int
	NodeTombstoneCnt int
	EdgeMarkerCnt    int
}

func derivedShadowCorpus(files int) map[string]string {
	if files < 2 {
		files = 2
	}
	tree := make(map[string]string, files+2)
	tree["types.go"] = `package shadowfixture

type Options struct{ Value int }
`
	tree["guide.txt"] = "zzderivedcontent verifies the generation-scoped content index\n"
	for i := 0; i < files; i++ {
		next := (i + 1) % files
		name := fmt.Sprintf("Func%03d", i)
		nextName := fmt.Sprintf("Func%03d", next)
		tree[fmt.Sprintf("pkg/file_%03d.go", i)] = fmt.Sprintf(`package shadowfixture

func %s(o Options) Options {
	if o.Value > 0 {
		return %s(Options{})
	}
	return o
}
`, name, nextName)
	}
	return tree
}

func derivedShadowChanges(tree map[string]string) []LayerPathChange {
	changes := make([]LayerPathChange, 0, len(tree))
	for filePath := range tree {
		changes = append(changes, LayerPathChange{Path: filePath, Kind: LayerPathAdded})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

func derivedShadowBuildRequest(
	root string,
	target source.ContentSource,
	tree map[string]string,
) BuildRequest {
	return BuildRequest{
		Identity: GenerationIdentity{
			OwnerKind:            dedicatedBaseGenerationKind,
			GraphID:              derivedShadowGraphID,
			LayerID:              derivedShadowLayerID,
			CheckoutID:           derivedShadowCheckoutID,
			GenerationKind:       dedicatedBaseGenerationKind,
			LowerViewFingerprint: "derived-shadow-tree",
			TreeOID:              "derived-shadow-tree",
			ConfigHash:           "derived-shadow-config",
			ExtractorVersions:    "derived-shadow-extractors",
			ResolverVersion:      "derived-shadow-resolver",
			CreatedAt:            100,
		},
		Base:        graph.New(),
		Target:      target,
		Changes:     derivedShadowChanges(tree),
		RootPath:    root,
		RepoPrefix:  builderRepoPrefix,
		WorkspaceID: builderRepoPrefix,
		ProjectID:   builderRepoPrefix,
	}
}

func observedShadowDecision(t testing.TB, logs *observer.ObservedLogs) bool {
	t.Helper()
	entries := logs.FilterMessage("indexer: shadow-swap decision").All()
	if len(entries) != 1 {
		t.Fatalf("shadow decisions = %d, want 1", len(entries))
	}
	taken, ok := entries[0].ContextMap()["shadow_taken"].(bool)
	if !ok {
		t.Fatalf("shadow decision has no boolean shadow_taken: %#v", entries[0].ContextMap())
	}
	return taken
}

func canonicalJSONRows[T any](t testing.TB, rows []T) []string {
	t.Helper()
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal graph row: %v", err)
		}
		out = append(out, string(encoded))
	}
	sort.Strings(out)
	return out
}

func firstDifferentRow(left, right []string) (int, string, string) {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		if left[i] != right[i] {
			return i, left[i], right[i]
		}
	}
	return limit, "", ""
}

func snapshotDerivedGeneration(
	t testing.TB,
	handle *store_sqlite.Store,
	report BuildReport,
) derivedGenerationSnapshot {
	t.Helper()
	nodes := make([]graph.Node, 0, handle.NodeCount())
	for _, node := range handle.AllNodes() {
		if node != nil {
			nodes = append(nodes, *node)
		}
	}
	edges := make([]graph.Edge, 0, handle.EdgeCount())
	for _, edge := range handle.AllEdges() {
		if edge != nil {
			edges = append(edges, *edge)
		}
	}
	files, err := handle.FileMetasForRepo(builderRepoPrefix)
	if err != nil {
		t.Fatalf("read generation files: %v", err)
	}
	fileMasks, err := handle.FileMasks()
	if err != nil {
		t.Fatalf("read generation file masks: %v", err)
	}
	nodeTombstones, err := handle.NodeTombstones()
	if err != nil {
		t.Fatalf("read generation node tombstones: %v", err)
	}
	edgeMasks, err := handle.EdgeSourceMasks()
	if err != nil {
		t.Fatalf("read generation edge masks: %v", err)
	}
	producers, err := handle.ProducerStates()
	if err != nil {
		t.Fatalf("read generation producers: %v", err)
	}
	searchHits := make(map[string][]string)
	for _, query := range []string{"Func000", "Func007", "Options"} {
		hits, err := handle.SearchSymbols(query, 64)
		if err != nil {
			t.Fatalf("search generation for %q: %v", query, err)
		}
		ids := make([]string, 0, len(hits))
		for _, hit := range hits {
			ids = append(ids, hit.NodeID)
		}
		sort.Strings(ids)
		searchHits[query] = ids
	}
	contentHits, err := handle.SearchContent("zzderivedcontent", builderRepoPrefix, 64)
	if err != nil {
		t.Fatalf("search generation content: %v", err)
	}
	indexedPaths := append([]string(nil), report.IndexedPaths...)
	sort.Strings(indexedPaths)
	return derivedGenerationSnapshot{
		Nodes: canonicalJSONRows(t, nodes), Edges: canonicalJSONRows(t, edges),
		Files: files, FileMasks: fileMasks, NodeTombstones: nodeTombstones,
		EdgeMasks: edgeMasks, Producers: producers, SearchHits: searchHits,
		ContentHits:  canonicalJSONRows(t, contentHits),
		IndexedPaths: indexedPaths, NodeCount: report.NodeCount, EdgeCount: report.EdgeCount,
		ReplaceMasks: report.ReplaceMasks, DeleteMasks: report.DeleteMasks,
		NodeTombstoneCnt: report.NodeTombstones, EdgeMarkerCnt: report.EdgeSourceMarkers,
	}
}

func buildDerivedGenerationWithStrategy(
	t *testing.T,
	root string,
	tree map[string]string,
	shadow bool,
) derivedGenerationSnapshot {
	t.Helper()
	if shadow {
		t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
		t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
	} else {
		t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	}
	store := builderOpenStore(t, fmt.Sprintf("derived-shadow-%t", shadow))
	target, err := source.NewFilesystemSource(root)
	if err != nil {
		t.Fatalf("open derived shadow source: %v", err)
	}
	defer target.Close() //nolint:errcheck // read-only test source

	core, logs := observer.New(zapcore.InfoLevel)
	builder := builderNewBuilder(store)
	builder.Config.Workers = 2
	builder.Logger = zap.New(core)
	generationID, report, err := builder.Build(
		context.Background(), derivedShadowBuildRequest(root, target, tree))
	if err != nil {
		t.Fatalf("build derived generation (shadow=%t): %v", shadow, err)
	}
	if taken := observedShadowDecision(t, logs); taken != shadow {
		t.Fatalf("shadow_taken = %t, want %t", taken, shadow)
	}
	return snapshotDerivedGeneration(t, store.AtGeneration(generationID), report)
}

func TestDerivedGenerationShadowMatchesDirectSQLite(t *testing.T) {
	tree := derivedShadowCorpus(24)
	root := builderTempDir(t, "derived-shadow-parity-source")
	builderWriteTree(t, root, tree)

	direct := buildDerivedGenerationWithStrategy(t, root, tree, false)
	shadow := buildDerivedGenerationWithStrategy(t, root, tree, true)
	if !reflect.DeepEqual(shadow.Nodes, direct.Nodes) {
		i, shadowRow, directRow := firstDifferentRow(shadow.Nodes, direct.Nodes)
		t.Errorf("nodes differ: shadow=%d direct=%d first[%d]:\nshadow=%s\ndirect=%s",
			len(shadow.Nodes), len(direct.Nodes), i, shadowRow, directRow)
	}
	if !reflect.DeepEqual(shadow.Edges, direct.Edges) {
		i, shadowRow, directRow := firstDifferentRow(shadow.Edges, direct.Edges)
		t.Errorf("edges differ: shadow=%d direct=%d first[%d]:\nshadow=%s\ndirect=%s",
			len(shadow.Edges), len(direct.Edges), i, shadowRow, directRow)
	}
	if !reflect.DeepEqual(shadow.Files, direct.Files) {
		t.Errorf("files differ: shadow=%+v direct=%+v", shadow.Files, direct.Files)
	}
	if !reflect.DeepEqual(shadow.FileMasks, direct.FileMasks) {
		t.Errorf("file masks differ: shadow=%+v direct=%+v", shadow.FileMasks, direct.FileMasks)
	}
	if !reflect.DeepEqual(shadow.NodeTombstones, direct.NodeTombstones) {
		t.Errorf("node tombstones differ: shadow=%v direct=%v", shadow.NodeTombstones, direct.NodeTombstones)
	}
	if !reflect.DeepEqual(shadow.EdgeMasks, direct.EdgeMasks) {
		t.Errorf("edge masks differ: shadow=%+v direct=%+v", shadow.EdgeMasks, direct.EdgeMasks)
	}
	if !reflect.DeepEqual(shadow.Producers, direct.Producers) {
		t.Errorf("producers differ: shadow=%+v direct=%+v", shadow.Producers, direct.Producers)
	}
	if !reflect.DeepEqual(shadow.SearchHits, direct.SearchHits) {
		t.Errorf("symbol FTS differs: shadow=%v direct=%v", shadow.SearchHits, direct.SearchHits)
	}
	if !reflect.DeepEqual(shadow.ContentHits, direct.ContentHits) {
		t.Errorf("content FTS differs: shadow=%v direct=%v", shadow.ContentHits, direct.ContentHits)
	}
	if !reflect.DeepEqual(shadow.IndexedPaths, direct.IndexedPaths) {
		t.Errorf("indexed paths differ: shadow=%v direct=%v", shadow.IndexedPaths, direct.IndexedPaths)
	}
	if shadow.NodeCount != direct.NodeCount || shadow.EdgeCount != direct.EdgeCount ||
		shadow.ReplaceMasks != direct.ReplaceMasks || shadow.DeleteMasks != direct.DeleteMasks ||
		shadow.NodeTombstoneCnt != direct.NodeTombstoneCnt || shadow.EdgeMarkerCnt != direct.EdgeMarkerCnt {
		t.Errorf("report differs: shadow=(nodes=%d edges=%d replace=%d delete=%d tombstones=%d edge_masks=%d) "+
			"direct=(nodes=%d edges=%d replace=%d delete=%d tombstones=%d edge_masks=%d)",
			shadow.NodeCount, shadow.EdgeCount, shadow.ReplaceMasks, shadow.DeleteMasks,
			shadow.NodeTombstoneCnt, shadow.EdgeMarkerCnt,
			direct.NodeCount, direct.EdgeCount, direct.ReplaceMasks, direct.DeleteMasks,
			direct.NodeTombstoneCnt, direct.EdgeMarkerCnt)
	}
	if len(shadow.Nodes) == 0 || len(shadow.Edges) == 0 || len(shadow.Files) == 0 {
		t.Fatal("parity fixture produced no graph payload")
	}
	for query, hits := range shadow.SearchHits {
		if len(hits) == 0 {
			t.Fatalf("shadow FTS has no hit for %q", query)
		}
	}
	if len(shadow.ContentHits) == 0 {
		t.Fatal("shadow content FTS has no hit for zzderivedcontent")
	}
}

func TestAdoptedDerivedShadowReplacesOnlyItsGeneration(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
	t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
	ctx := context.Background()
	store := builderOpenStore(t, "derived-shadow-recovery")
	seedReindexScopeControlPlane(t, store)

	base := seedReadyReindexPayload(t, store, 1, "dedicated_graph", "base-layer", "base", 0)
	commit := seedReadyReindexPayload(t, store, 2, "checkout", "commit-layer", "commit", base.generationID)
	dirty := seedReadyReindexPayload(t, store, 3, "checkout", "dirty-layer", "dirty", commit.generationID)
	ref := seedReadyReindexPayload(t, store, 4, "ref_view", "ref-layer", "ref", base.generationID)
	installReindexScopePointers(t, store, base, commit, dirty, ref)
	siblings := []readyReindexPayload{base, commit, dirty, ref}

	zeroNode := &graph.Node{
		ID: reindexScopeRepo + "/zero.go::ZeroSentinel", Kind: graph.KindFunction,
		Name: "ZeroSentinel", FilePath: reindexScopeRepo + "/zero.go",
		RepoPrefix: reindexScopeRepo, Language: "go",
	}
	store.AddNode(zeroNode)
	if err := store.SetFileMetas(reindexScopeRepo, []graph.FileMetaRow{{
		FilePath: zeroNode.FilePath, ContentHash: "zero-hash", Size: 1, NodeCount: 1,
	}}); err != nil {
		t.Fatalf("seed generation-zero file: %v", err)
	}
	if err := store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{
		NodeID: zeroNode.ID, Tokens: "zerosentinel",
	}}); err != nil {
		t.Fatalf("seed generation-zero FTS: %v", err)
	}
	if err := store.AppendContent(reindexScopeRepo, []graph.ContentFTSItem{{
		NodeID: zeroNode.ID, FilePath: zeroNode.FilePath, Body: "zerogenerationcontent",
	}}); err != nil {
		t.Fatalf("seed generation-zero content FTS: %v", err)
	}
	if err := store.BuildContentIndex(); err != nil {
		t.Fatalf("finalize generation-zero content FTS: %v", err)
	}

	graphBefore, graphFound, err := store.Catalog().GetDedicatedGraph(ctx, reindexScopeGraphID)
	if err != nil || !graphFound {
		t.Fatalf("read dedicated graph before recovery: found=%t err=%v", graphFound, err)
	}
	routeBefore, routeFound, err := store.Catalog().GetCheckoutRoute(ctx, reindexScopeCheckoutID)
	if err != nil || !routeFound {
		t.Fatalf("read checkout route before recovery: found=%t err=%v", routeFound, err)
	}
	refBefore, refFound, err := store.Catalog().GetRefView(ctx, reindexScopeRefViewID)
	if err != nil || !refFound {
		t.Fatalf("read ref route before recovery: found=%t err=%v", refFound, err)
	}

	tree := derivedShadowCorpus(12)
	root := builderTempDir(t, "derived-shadow-recovery-source")
	builderWriteTree(t, root, tree)
	target, err := source.NewFilesystemSource(root)
	if err != nil {
		t.Fatalf("open recovery source: %v", err)
	}
	defer target.Close() //nolint:errcheck // read-only test source
	request := derivedShadowBuildRequest(root, target, tree)
	request.Identity.GraphID = reindexScopeGraphID
	request.Identity.LayerID = "recovery-layer"
	request.Identity.CheckoutID = reindexScopeCheckoutID
	request.RepoPrefix = reindexScopeRepo
	request.WorkspaceID = reindexScopeRepo
	request.ProjectID = reindexScopeRepo

	generationID, recovery, err := store.BeginPayloadGeneration(ctx, payloadRequestForBuild(request))
	if err != nil {
		t.Fatalf("seed recoverable generation: %v", err)
	}
	staleNode := &graph.Node{
		ID: reindexScopeRepo + "/stale.go::StaleDerived", Kind: graph.KindFunction,
		Name: "StaleDerived", FilePath: reindexScopeRepo + "/stale.go",
		RepoPrefix: reindexScopeRepo, Language: "go",
	}
	recovery.AddNode(staleNode)
	if err := recovery.SetFileMetas(reindexScopeRepo, []graph.FileMetaRow{{
		FilePath: staleNode.FilePath, ContentHash: "stale-hash", Size: 1, NodeCount: 1,
	}}); err != nil {
		t.Fatalf("seed stale derived file: %v", err)
	}
	if err := recovery.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{
		NodeID: staleNode.ID, Tokens: "stalederived",
	}}); err != nil {
		t.Fatalf("seed stale derived FTS: %v", err)
	}

	core, logs := observer.New(zapcore.InfoLevel)
	builder := builderNewBuilder(store)
	builder.Config.Workers = 2
	builder.Logger = zap.New(core)
	builtGeneration, report, err := builder.Build(ctx, request)
	if err != nil {
		t.Fatalf("recover adopted derived generation: %v", err)
	}
	if builtGeneration != generationID {
		t.Fatalf("recovery generation = %d, want adopted %d", builtGeneration, generationID)
	}
	if !observedShadowDecision(t, logs) {
		t.Fatal("adopted derived generation did not use the bounded shadow")
	}
	started := logs.FilterMessage(sparsePhysicalBuildStartedMessage).All()
	if len(started) != 1 || started[0].ContextMap()["adopted"] != true {
		t.Fatalf("physical recovery did not report adoption: %#v", started)
	}
	if report.NodeCount == 0 || report.EdgeCount == 0 {
		t.Fatalf("recovered payload is empty: nodes=%d edges=%d", report.NodeCount, report.EdgeCount)
	}

	if recovery.GetNode(staleNode.ID) != nil {
		t.Fatal("adopted generation retained its stale pre-recovery node")
	}
	if got := recovery.FindNodesByNameInRepo("Func000", reindexScopeRepo); len(got) == 0 {
		t.Fatal("adopted generation did not receive the replacement payload")
	}
	if hits, err := recovery.SearchSymbols("StaleDerived", 8); err != nil || len(hits) != 0 {
		t.Fatalf("stale generation FTS survived recovery: hits=%v err=%v", hits, err)
	}
	if store.GetNode(zeroNode.ID) == nil {
		t.Fatal("derived shadow drain erased generation zero")
	}
	if hits, err := store.SearchSymbols("ZeroSentinel", 8); err != nil || len(hits) == 0 {
		t.Fatalf("generation-zero FTS after recovery: hits=%v err=%v", hits, err)
	}
	if hits, err := store.SearchContent("zerogenerationcontent", reindexScopeRepo, 8); err != nil || len(hits) == 0 {
		t.Fatalf("generation-zero content FTS after recovery: hits=%v err=%v", hits, err)
	}
	zeroFiles, err := store.FileMetasForRepo(reindexScopeRepo)
	if err != nil || len(zeroFiles) != 1 || zeroFiles[0].FilePath != zeroNode.FilePath {
		t.Fatalf("generation-zero files after recovery = %+v err=%v", zeroFiles, err)
	}
	for _, sibling := range siblings {
		requireReadyReindexPayload(t, sibling)
	}

	graphAfter, graphFound, err := store.Catalog().GetDedicatedGraph(ctx, reindexScopeGraphID)
	if err != nil || !graphFound || !reflect.DeepEqual(graphAfter, graphBefore) {
		t.Fatalf("dedicated graph changed during recovery:\nbefore=%+v\nafter=%+v found=%t err=%v",
			graphBefore, graphAfter, graphFound, err)
	}
	routeAfter, routeFound, err := store.Catalog().GetCheckoutRoute(ctx, reindexScopeCheckoutID)
	if err != nil || !routeFound || !reflect.DeepEqual(routeAfter, routeBefore) {
		t.Fatalf("checkout route changed during recovery:\nbefore=%+v\nafter=%+v found=%t err=%v",
			routeBefore, routeAfter, routeFound, err)
	}
	refAfter, refFound, err := store.Catalog().GetRefView(ctx, reindexScopeRefViewID)
	if err != nil || !refFound || !reflect.DeepEqual(refAfter, refBefore) {
		t.Fatalf("ref route changed during recovery:\nbefore=%+v\nafter=%+v found=%t err=%v",
			refBefore, refAfter, refFound, err)
	}
}
