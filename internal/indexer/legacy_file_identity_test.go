package indexer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// These are real native-Windows reindex regressions, not path simulation on
// POSIX. The old extraction input matches v0.63.8's filepath.Rel graph key.
func TestWindowsReindexHealsLegacyOrdinaryIdentities(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires native Windows path semantics")
	}
	for _, language := range []string{"python", "javascript"} {
		for _, backend := range []string{"memory", "sqlite"} {
			for _, changed := range []bool{false, true} {
				name := language + "/" + backend + "/unchanged"
				if changed {
					name = language + "/" + backend + "/changed"
				}
				t.Run(name, func(t *testing.T) { testWindowsLegacyReindex(t, language, backend, changed) })
			}
		}
	}
}

func testWindowsLegacyReindex(t *testing.T, language, backend string, changed bool) {
	ext, source, callerSource, rootSource, extractor := legacyIdentityFixture(language)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	rel := "sub/sample" + ext
	path := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "caller"+ext), []byte(callerSource), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "root"+ext), []byte(rootSource), 0o644))
	var store graph.Store = graph.New()
	if backend == "sqlite" {
		disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, disk.Close()) })
		store = disk
	}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(store, registry, cfg.Index, zap.NewNop())
	idx.SetRepoPrefix("repo")
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	canonical := "repo/" + rel
	native := "repo/" + filepath.FromSlash(rel)
	require.NotEqual(t, canonical, native)
	var helper *graph.Node
	for _, node := range store.GetFileNodes(canonical) {
		if node.Name == "Helper" && node.Kind == graph.KindFunction {
			helper = node
		}
	}
	require.NotNil(t, helper)
	legacyResult, err := extractor.Extract(filepath.FromSlash(rel), []byte(source))
	require.NoError(t, err)
	defer legacyResult.ReleaseTree()
	idx.applyRepoPrefix(legacyResult.Nodes, legacyResult.Edges)
	store.AddBatch(legacyResult.Nodes, legacyResult.Edges)
	oldHelper := native + strings.TrimPrefix(helper.ID, canonical)
	require.NotNil(t, store.GetNode(oldHelper))
	// Move an actual caller's already resolved edge to the retained old ID.
	// Requiring that edge afterward prevents a mere duplicate-search fix from
	// silently losing foreign callers during endpoint eviction.
	var callerEdge *graph.Edge
	for _, edge := range store.GetInEdges(helper.ID) {
		if edge.Kind == graph.EdgeCalls && edge.FilePath == "repo/caller"+ext {
			callerEdge = edge
			break
		}
	}
	require.NotNil(t, callerEdge, "fixture must resolve the caller before corruption")
	moved := *callerEdge
	moved.To = oldHelper
	store.ReindexEdges([]graph.EdgeReindex{{Edge: &moved, OldTo: helper.ID}})
	for _, edge := range store.GetInEdges(helper.ID) {
		require.False(t, edge.Kind == graph.EdgeCalls && edge.From == moved.From, "canonical helper must have no surviving caller to mask a lost legacy edge")
	}
	foreign := "repo/caller" + ext
	targets := []*graph.Node{
		{ID: "repo/license::MIT", Kind: graph.KindLicense, FilePath: native},
		{ID: "repo/team::core", Kind: graph.KindTeam, FilePath: native},
		{ID: "repo/module::pkg", Kind: graph.KindModule, FilePath: native},
	}
	foreignEdges := []*graph.Edge{
		{From: foreign, To: targets[0].ID, Kind: graph.EdgeLicensedAs, FilePath: foreign},
		{From: foreign, To: targets[1].ID, Kind: graph.EdgeOwns, FilePath: foreign},
		{From: foreign, To: targets[2].ID, Kind: graph.EdgeImports, FilePath: foreign},
	}
	store.AddBatch(targets, foreignEdges)
	sibling := `repo/other\untouched` + ext
	store.AddBatch([]*graph.Node{{ID: sibling, Kind: graph.KindFile, FilePath: sibling}}, nil)
	if fts, ok := store.(graph.SymbolFTSBatchUpserter); ok {
		require.NoError(t, fts.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: oldHelper, Tokens: "Helper"}}))
	}
	rootIDs := fileIdentityIDs(store.GetFileNodes("repo/root" + ext))
	require.False(t, idx.IsStale(rel), "unchanged discovery must reach repair before mtime shortcut")
	if changed {
		require.NoError(t, os.WriteFile(path, []byte(strings.Replace(source, "return 1", "return 2", 1)), 0o644))
	}
	for attempt := 0; attempt < 3; attempt++ {
		_, err = idx.IncrementalReindexPaths(dir, []string{path})
		require.NoError(t, err)
		require.Nil(t, store.GetNode(native))
		require.Nil(t, store.GetNode(oldHelper))
		require.NotNil(t, store.GetNode(helper.ID))
		require.Equal(t, rootIDs, fileIdentityIDs(store.GetFileNodes("repo/root"+ext)))
		require.NotNil(t, store.GetNode(sibling), "scoped repair must leave another file alone")
		for _, target := range targets {
			require.Equal(t, native, store.GetNode(target.ID).FilePath)
			require.Len(t, store.GetInEdges(target.ID), 1, "foreign coverage edges must survive")
		}
		var calls int
		for _, edge := range store.GetOutEdges(moved.From) {
			if edge.Kind == graph.EdgeCalls && edge.To == helper.ID {
				calls++
			}
		}
		require.Equal(t, 1, calls, "foreign caller must resolve to exactly one canonical symbol")
		if searcher, ok := store.(graph.SymbolSearcher); ok {
			hits, err := searcher.SearchSymbols("Helper", 20)
			require.NoError(t, err)
			var symbols int
			for _, hit := range hits {
				require.NotEqual(t, oldHelper, hit.NodeID, "stale FTS document must be deleted")
				if hit.NodeID == helper.ID {
					symbols++
				}
			}
			require.Equal(t, 1, symbols)
		}
	}
}

func legacyIdentityFixture(language string) (string, string, string, string, parser.Extractor) {
	if language == "python" {
		return ".py", "def Helper():\n    return 1\n", "from sub.sample import Helper\n\ndef caller():\n    return Helper()\n", "def RootOnly():\n    return 3\n", languages.NewPythonExtractor()
	}
	return ".js", "export function Helper() { return 1; }\n", "import { Helper } from './sub/sample.js';\nexport function caller() { return Helper(); }\n", "export function RootOnly() { return 3; }\n", languages.NewJavaScriptExtractor()
}

func fileIdentityIDs(nodes []*graph.Node) map[string]bool {
	ids := make(map[string]bool)
	for _, node := range nodes {
		ids[node.ID] = true
	}
	return ids
}

func TestPOSIXReindexPreservesLiteralBackslashFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a path separator on Windows")
	}
	for _, language := range []string{"python", "javascript"} {
		t.Run(language, func(t *testing.T) {
			ext, source, _, _, extractor := legacyIdentityFixture(language)
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
			canonical, native := "sub/sample"+ext, `sub\sample`+ext
			for _, rel := range []string{canonical, native} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(source), 0o644))
			}
			store := graph.New()
			registry := parser.NewRegistry()
			registry.Register(extractor)
			idx := New(store, registry, config.Default().Index, zap.NewNop())
			idx.SetRootPath(dir)
			_, err := idx.Index(dir)
			require.NoError(t, err)
			literalIDs := fileIdentityIDs(store.GetFileNodes(native))
			require.NotEmpty(t, literalIDs)
			for attempt := 0; attempt < 2; attempt++ {
				require.NoError(t, idx.IndexFile(filepath.Join(dir, canonical)))
				require.Equal(t, literalIDs, fileIdentityIDs(store.GetFileNodes(native)))
				require.NotNil(t, store.GetNode(canonical))
				require.NotNil(t, store.GetNode(native))
			}
		})
	}
}

// This portable storage/frontier test exercises the mutation order only. The
// Windows test above separately proves native discovery and reindex behavior.
func TestLegacyIdentityRepairRebindsForeignCaller(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite", "delta"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, disk.Close()) })
				store = disk
			}
			path, legacy, callerPath := "repo/sub/sample.py", `repo/sub\sample.py`, "repo/caller.py"
			fresh := []*graph.Node{
				{ID: path, Kind: graph.KindFile, Name: "sample.py", FilePath: path, RepoPrefix: "repo", Language: "python"},
				{ID: path + "::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: path, RepoPrefix: "repo", Language: "python"},
			}
			old := []*graph.Node{
				{ID: legacy, Kind: graph.KindFile, Name: "sample.py", FilePath: legacy, RepoPrefix: "repo", Language: "python"},
				{ID: legacy + "::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: legacy, RepoPrefix: "repo", Language: "python"},
			}
			caller := &graph.Node{ID: callerPath + "::caller", Kind: graph.KindFunction, Name: "caller", FilePath: callerPath, RepoPrefix: "repo", Language: "python"}
			store.AddBatch(append(append(append([]*graph.Node{}, fresh...), old...), caller), []*graph.Edge{
				{From: caller.ID, To: old[1].ID, Kind: graph.EdgeCalls, FilePath: callerPath, Line: 3},
				{From: caller.ID, To: legacy, Kind: graph.EdgeImports, FilePath: callerPath, Line: 1},
			})
			if backend == "delta" {
				store = graph.NewDeltaWriter(store, nil)
			}
			prior, ids, native := mergeLegacyFilePrior(legacy, fresh, old)
			stage := &incrementalBatchStage{graphPath: path, legacyPath: native, legacyIDs: ids, priorNodes: prior, result: &parser.ExtractionResult{Nodes: fresh}}
			stages := []*incrementalBatchStage{stage}
			carried, _ := restubIncomingRefsFromView(store, stages, loadIncrementalPriorView(store, stages))
			idx := New(store, parser.NewRegistry(), config.Default().Index, zap.NewNop())
			idx.SetRepoPrefix("repo")
			idx.evictFileIdentities([]string{path}, ids, []string{native})
			store.AddBatch(fresh, carried)
			idx.resolver.ResolveFilesAndIncoming([]string{path})
			require.Nil(t, store.GetNode(legacy))
			require.Nil(t, store.GetNode(old[1].ID))
			var calls, imports int
			for _, edge := range store.GetOutEdges(caller.ID) {
				if edge.Kind == graph.EdgeCalls && edge.To == fresh[1].ID {
					calls++
				}
				if edge.Kind == graph.EdgeImports && edge.To == path {
					imports++
				}
			}
			require.Equal(t, 1, calls, "normal resolution must rescue the foreign caller")
			require.Equal(t, 1, imports, "imports must follow the equivalent canonical file identity")
		})
	}
}

func TestLegacyIdentityRepairRespectsDataflowFrontier(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite", "delta"} {
		for _, edit := range []string{"unchanged", "signature", "visibility", "field"} {
			t.Run(backend+"/"+edit, func(t *testing.T) {
				var store graph.Store = graph.New()
				if backend == "sqlite" {
					disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, disk.Close()) })
					store = disk
				}
				path, legacy, callerPath := "repo/sub/sample.py", `repo/sub\sample.py`, "repo/caller.py"
				makeNodes := func(file string) []*graph.Node {
					return []*graph.Node{
						{ID: file, Kind: graph.KindFile, Name: "sample.py", FilePath: file},
						{ID: file + "::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: file, Meta: map[string]any{"signature": "Helper(x: int)", "visibility": "public"}},
						{ID: file + "::Helper#param:x", Kind: graph.KindParam, Name: "x", FilePath: file, Meta: map[string]any{"position": 0, "type": "int"}},
						{ID: file + "::Thing.value", Kind: graph.KindField, Name: "Thing.value", FilePath: file, Meta: map[string]any{"signature": "value: int", "visibility": "public"}},
					}
				}
				prior, fresh := makeNodes(legacy), makeNodes(path)
				ownerChanged := edit == "signature" || edit == "visibility"
				switch edit {
				case "signature":
					fresh[1].Meta["signature"] = "Helper(y: str)"
					fresh[2].ID, fresh[2].Name = path+"::Helper#param:y", "y"
					fresh[2].Meta["type"] = "str"
				case "visibility":
					fresh[1].Meta["visibility"] = "private"
				case "field":
					fresh[3].Meta["signature"] = "value: str"
				}
				caller, result := callerPath+"::caller", callerPath+"::result"
				edges := []*graph.Edge{
					{From: prior[2].ID, To: prior[1].ID, Kind: graph.EdgeParamOf, FilePath: legacy},
					{From: caller, To: prior[1].ID, Kind: graph.EdgeCalls, FilePath: callerPath, Line: 3},
					{From: caller, To: prior[2].ID, Kind: graph.EdgeArgOf, FilePath: callerPath, Line: 3, Meta: map[string]any{"arg_position": 0}},
					{From: caller, To: prior[2].ID, Kind: graph.EdgeCaptures, FilePath: callerPath, Line: 3},
					{From: caller, To: prior[3].ID, Kind: graph.EdgeAccessesField, FilePath: callerPath, Line: 4},
					{From: prior[1].ID, To: result, Kind: graph.EdgeValueFlow, FilePath: callerPath, Line: 3},
				}
				store.AddBatch(append(append([]*graph.Node{}, prior...),
					&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "caller", FilePath: callerPath},
					&graph.Node{ID: result, Kind: graph.KindVariable, Name: "result", FilePath: callerPath}), edges)
				if backend == "delta" {
					store = graph.NewDeltaWriter(store, nil)
				}
				var ids []string
				for _, node := range prior {
					ids = append(ids, node.ID)
				}
				stage := &incrementalBatchStage{graphPath: path, legacyPath: legacy, legacyIDs: ids, priorNodes: prior, result: &parser.ExtractionResult{
					Nodes: fresh, Edges: []*graph.Edge{{From: fresh[2].ID, To: fresh[1].ID, Kind: graph.EdgeParamOf, FilePath: path}},
				}}
				stages := []*incrementalBatchStage{stage}
				carried, _ := restubIncomingRefsFromView(store, stages, loadIncrementalPriorView(store, stages))
				store.(graph.FileIdentityBatchEvicter).EvictFilesWithNodeIDs([]string{path}, ids)
				store.AddBatch(fresh, append(stage.result.Edges, carried...))
				find := func(from, to string, kind graph.EdgeKind) bool {
					for _, edge := range store.GetOutEdges(from) {
						if edge.To == to && edge.Kind == kind {
							return true
						}
					}
					return false
				}
				if ownerChanged {
					require.True(t, find(caller, graph.UnresolvedMarker+"Helper", graph.EdgeCalls), "changed declarations must still restub callers")
					require.True(t, find(caller, path+"::Helper", graph.EdgeArgOf), "changed parameter goes back to its canonical owner for rematerialization")
					require.False(t, find(caller, path+"::Helper#param:x", graph.EdgeCaptures), "changed parameters must not retain stale captures")
					require.True(t, find("repo/"+graph.UnresolvedMarker+"Helper", result, graph.EdgeValueFlow), "outgoing caller-owned dataflow must follow the parked call placeholder")
					require.False(t, find(path+"::Helper", result, graph.EdgeValueFlow))
				} else {
					require.True(t, find(caller, path+"::Helper", graph.EdgeCalls))
					require.True(t, find(caller, path+"::Helper#param:x", graph.EdgeArgOf))
					require.True(t, find(caller, path+"::Helper#param:x", graph.EdgeCaptures))
					require.True(t, find(path+"::Helper", result, graph.EdgeValueFlow), "unchanged outgoing caller-owned dataflow must be rekeyed to the canonical definition")
				}
				require.Equal(t, edit != "field", find(caller, path+"::Thing.value", graph.EdgeAccessesField), "field contract checks must still control carry")
				for _, edge := range store.AllEdges() {
					require.False(t, strings.HasPrefix(edge.From, legacy) || strings.HasPrefix(edge.To, legacy), "no carried row may retain a retired ordinary endpoint")
				}
			})
		}
	}
}
