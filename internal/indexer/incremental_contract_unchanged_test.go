package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const constantRouteConsumer = "package fixture\nfunc setup(r Router) { r.GET(Route, handler) }\nfunc handler() {}\n"

func constantProviderPaths(store graph.Store, repo string) []string {
	var paths []string
	registry := contracts.LoadRegistryFromGraph(store, repo)
	if registry == nil {
		return nil
	}
	for _, record := range registry.ByRepo(repo) {
		if record.Type == contracts.ContractHTTP && record.Role == contracts.RoleProvider {
			value, _ := record.Meta["path"].(string)
			paths = append(paths, value)
		}
	}
	return paths
}

func TestSharedConstantChangeRefreshesDependentContracts(t *testing.T) {
	for _, name := range []string{"same_shape_literal_change", "remove_provider_without_import_edge", "remove_legacy_provider", "resolve_previous_empty_consumer"} {
		remove := name == "remove_provider_without_import_edge" || name == "remove_legacy_provider"
		add := name == "resolve_previous_empty_consumer"
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "constant.go")
			initialSource := "package fixture\nconst Route = \"/before\"\n"
			if add {
				initialSource = "package fixture\n// Route not declared yet.\n"
			}
			writeFile(t, path, initialSource)
			writeFile(t, filepath.Join(root, "routes.go"), constantRouteConsumer)
			g := graph.New()
			initial := newTestIndexer(g)
			initial.SetRepoPrefix("fixture")
			t.Cleanup(initial.Close)
			_, err := initial.Index(root)
			require.NoError(t, err)
			initial.Close()
			if add {
				require.Empty(t, constantProviderPaths(g, "fixture"))
			} else {
				require.Contains(t, constantProviderPaths(g, "fixture"), "/before")
			}
			idx := newTestIndexer(g)
			idx.SetRepoPrefix("fixture")
			idx.storeRootPath(root)
			idx.SetFileMtimes(initial.FileMtimes())
			t.Cleanup(idx.Close)
			if remove {
				require.NoError(t, os.Remove(path))
				_, err = idx.IncrementalReindexPaths(root, []string{"constant.go"})
			} else {
				writeFile(t, path, "package fixture\nconst Route = \"/after\"\n")
				err = idx.IndexFile(path)
			}
			require.NoError(t, err)
			paths := constantProviderPaths(g, "fixture")
			require.NotContains(t, paths, "/before", "a source with no route of its own must invalidate the dependent route")
			if !remove {
				require.Contains(t, paths, "/after")
			}
		})
	}
}

func TestEditDeltaSharedConstantContractsMatchWholeIndex(t *testing.T) {
	for _, name := range []string{"same_shape_literal_change", "remove_provider_without_import_edge", "remove_legacy_provider", "resolve_previous_empty_consumer"} {
		remove := name == "remove_provider_without_import_edge" || name == "remove_legacy_provider"
		add := name == "resolve_previous_empty_consumer"
		t.Run(name, func(t *testing.T) {
			builderIsolateGit(t)
			repo := builderTempDir(t, "repo")
			builderGit(t, repo, "init", "--initial-branch=main")
			initialSource := "package fixture\nconst Route = \"/before\"\n"
			if add {
				initialSource = "package fixture\n// Route not declared yet.\n"
			}
			builderWriteTree(t, repo, map[string]string{
				"constant.go": initialSource, "routes.go": constantRouteConsumer,
			})
			builderGit(t, repo, "add", "-A")
			builderGit(t, repo, "commit", "-m", "base")
			store := builderOpenStore(t, "delta")
			builderIndex(t, store, repo)
			if add {
				require.Empty(t, constantProviderPaths(store, builderRepoPrefix))
			} else {
				require.Contains(t, constantProviderPaths(store, builderRepoPrefix), "/before", "cold reference must produce an actual constant-based route")
			}
			if name == "remove_legacy_provider" {
				require.NoError(t, store.DeleteFileMetasByFiles(builderRepoPrefix, []string{builderRepoPrefix + "/constant.go"}))
			}
			if remove {
				require.NoError(t, os.Remove(filepath.Join(repo, "constant.go")))
			} else {
				builderWriteFile(t, repo, "constant.go", "package fixture\nconst Route = \"/after\"\n")
			}
			recordLastEditDelta(nil)
			generation, _, _ := newDirtyChainBuilder(t, builderNewBuilder(store), store, repo, true).build()
			require.NotNil(t, LastEditDeltaReport(), "exercise the actual delta, not sparse fallback")
			if remove {
				rows, err := store.AtGeneration(generation).FileMetasByPaths(builderRepoPrefix, []string{builderRepoPrefix + "/constant.go"})
				require.NoError(t, err)
				require.Empty(t, rows, "deleted delta file must not retain physical accepted inventory")
			}
			view := builderComposed(t, store, generation)
			clean := builderOpenStore(t, "clean")
			builderIndex(t, clean, repo)
			// Registry restoration accepts Store; the empty delta supplies its
			// composed read facade without mutating the published ancestry.
			var actual, expected []contracts.Contract
			if registry := contracts.LoadRegistryFromGraph(graph.NewDeltaWriter(view, graph.New()), builderRepoPrefix); registry != nil {
				actual = registry.ByRepo(builderRepoPrefix)
			}
			if registry := contracts.LoadRegistryFromGraph(clean, builderRepoPrefix); registry != nil {
				expected = registry.ByRepo(builderRepoPrefix)
			}
			require.True(t, contractSetsEqual(expected, actual), "full owner payloads must match the independent whole index: expected=%#v actual=%#v", expected, actual)
			for _, record := range actual {
				require.NotEqual(t, "/before", record.Meta["path"])
			}
			if !remove {
				require.Contains(t, constantProviderPaths(clean, builderRepoPrefix), "/after")
				require.NotEmpty(t, actual, "matching empty outputs is not success")
			}
		})
	}
}

const unchangedTypedRouteSource = `package fixture

import "github.com/gin-gonic/gin"

type LoginReq struct{ Email string }
type LoginResp struct{ Token string }

func register(r *gin.Engine) { r.POST("/login", login) }

func login(c *gin.Context) {
	var req LoginReq
	if err := c.ShouldBindJSON(&req); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
	resp := LoginResp{Token: req.Email}
	c.JSON(200, resp)
}

func Value() int { return 1 }
`

func TestUnchangedNonemptyTypedContractsAvoidGlobalHydration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "routes.go")
	writeFile(t, path, unchangedTypedRouteSource)
	writeFile(t, filepath.Join(root, "consumer.go"), "package fixture\nfunc Consumer() {}\n")
	g := graph.New()
	initial := newTestIndexer(g)
	initial.SetRepoPrefix("fixture")
	initial.SetWorkspaceID("workspace")
	initial.SetProjectID("project")
	t.Cleanup(initial.Close)
	_, err := initial.Index(root)
	require.NoError(t, err)
	initial.Close()
	prior := contracts.LoadRegistryFromGraphWithScope(g, "fixture", "workspace", "project").ByFile("fixture/routes.go")
	require.Len(t, prior, 1)
	require.Equal(t, "fixture/routes.go::login", prior[0].SymbolID, "require a real handler binding")
	require.Contains(t, prior[0].Meta["request_type"], "LoginReq")
	require.Contains(t, prior[0].Meta["response_type"], "LoginResp")
	require.NotNil(t, prior[0].Meta["request_shape"])
	require.NotNil(t, prior[0].Meta["response_shape"])
	consumer := g.FindNodesByNameInRepo("Consumer", "fixture")
	require.Len(t, consumer, 1)
	sibling := contracts.Contract{ID: prior[0].ID, Type: prior[0].Type, Role: contracts.RoleConsumer,
		SymbolID: consumer[0].ID, FilePath: consumer[0].FilePath, RepoPrefix: "fixture", WorkspaceID: "workspace", ProjectID: "project",
		Line: 2, Confidence: 0.8, Meta: map[string]any{"method": "POST", "path": "/login", "client": "unrelated"}}
	_, edges, missing := contractGraphRows(g, []contracts.Contract{sibling}, true)
	require.Zero(t, missing)
	g.AddBatch(nil, edges)
	before := contracts.LoadRegistryFromGraphWithScope(g, "fixture", "workspace", "project").ByRepo("fixture")
	require.Len(t, before, 2, "same-ID sibling must remain distinct")
	canonicalBefore := *g.GetNode(prior[0].ID)
	canonicalBefore.Meta = copyContractMetaMap(canonicalBefore.Meta)
	probe := &contractHydrationProbe{Graph: g}
	idx := newTestIndexer(probe)
	idx.SetRepoPrefix("fixture")
	idx.SetWorkspaceID("workspace")
	idx.SetProjectID("project")
	idx.storeRootPath(root)
	t.Cleanup(idx.Close)
	writeFile(t, path, strings.Replace(unchangedTypedRouteSource, "return 1", "return 2 /* local body edit */", 1))
	require.NoError(t, idx.IndexFile(path))
	after := contracts.LoadRegistryFromGraphWithScope(g, "fixture", "workspace", "project").ByRepo("fixture")
	require.True(t, contractSetsEqual(before, after), "all enriched local and sibling owner payloads must survive")
	require.Equal(t, canonicalBefore, *g.GetNode(prior[0].ID), "shared canonical must be preserved, not selected from a partial registry")
	require.Zero(t, probe.ownerReads, "typed nonempty routes must not read global ownership")
	require.Zero(t, probe.scalarReads, "typed nonempty routes must not read global contract nodes")
	require.Nil(t, idx.contractRegistry)
	require.Equal(t, 1, idx.contractUnchangedFiles)
	t.Logf("shortcut reasons=%v file_projection=%s owner_reads=%d scalar_reads=%d", idx.contractShortcutReasons, idx.contractProjectionTime, probe.ownerReads, probe.scalarReads)
}

func TestUnchangedContractSnapshotRejectsOffFileOnlyCanonicalOwners(t *testing.T) {
	canonical := &graph.Node{ID: "shared", Kind: graph.KindContract, FilePath: "repo/removed-owner.go", RepoPrefix: "repo",
		Meta: map[string]any{"contract_owner_record": true}}
	sibling := contracts.Contract{ID: "shared", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		SymbolID: "repo/live.go::Handler", FilePath: "repo/live.go", RepoPrefix: "repo"}
	projection := graph.ContractFileProjection{
		ScalarNodes: []*graph.Node{canonical}, Targets: map[string]*graph.Node{"shared": canonical},
		OwnerRows: []graph.RepoEdgeRow{{RepoPrefix: "repo", Edge: &graph.Edge{From: sibling.SymbolID, To: sibling.ID,
			Kind: graph.EdgeProvides, FilePath: sibling.FilePath, Meta: contractOwnerEdgeMeta(sibling)}}},
	}
	_, complete := contractRecordsForUnchangedFile(projection, "repo", "", "", canonical.FilePath)
	require.False(t, complete, "a canonical attributed to a now-ownerless file must not be lost by the empty-file optimization")
}

func TestContractDependencyStampTracksConstantsAndRemovedCrossFileMarkers(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	stamp := func(language, source string, constants string) []*graph.Node {
		file := &graph.Node{ID: "file", Kind: graph.KindFile}
		result := &parser.ExtractionResult{Nodes: []*graph.Node{file}}
		if constants != "" {
			result.ConstValues = []parser.ConstValue{{NodeID: "Path", FilePath: "routes.go", Value: constants}}
		}
		stampExtractionGraphFingerprint(result)
		idx.stampContractDependencyInputs("routes.go", language, []byte(source), result)
		return result.Nodes
	}
	before := stamp("go", "old source", "/before")
	after := stamp("go", "new source", "/after")
	require.False(t, sameContractDependencyInputs(before, after), "constant sidecars are outside ordinary graph fingerprints")
	idx.noteChangedContractDependencyInputs(before, after)
	require.True(t, idx.contractDependenciesChanged, "a shared constant change must re-extract existing contract files")
	mounted := stamp("python", "app.include_router(router)", "")
	unmounted := stamp("python", "app = make_app()", "")
	require.False(t, sameContractDependencyInputs(mounted, unmounted), "removed mounts must be detected from the accepted old stamp")
	for _, test := range []struct{ language, mount string }{
		{"python", "app.register_blueprint(routes)"}, {"python", "path('', include('views'))"},
		{"typescript", "RouterModule.register(routes)"}, {"typescript", "app.use ('/api', router)"},
		{"rust", "router.nest(\"/api\", routes)"}, {"rust", "router.merge(routes)"},
		{"rust", "app.configure(routes)"}, {"rust", "app.service(routes)"},
	} {
		t.Run(test.mount, func(t *testing.T) {
			old := stamp(test.language, test.mount, "")
			fresh := stamp(test.language, "mount removed", "")
			idx.contractDependenciesChanged, idx.contractSharedInputsChanged = false, false
			require.False(t, sameContractDependencyInputs(old, fresh))
			idx.noteChangedContractDependencyInputs(old, fresh)
			require.True(t, idx.contractDependenciesChanged)
			require.True(t, idx.contractSharedInputsChanged, "previous-empty dependents must be eligible for the conservative source frontier")
		})
	}
}

func TestLegacyContractInputsRequireAcceptedContentHash(t *testing.T) {
	builderIsolateGit(t)
	root := builderTempDir(t, "legacy-inputs")
	builderGit(t, root, "init", "--initial-branch=main")
	path := filepath.Join(root, "constant.go")
	before := "package fixture\nconst Route = \"/before\"\n"
	builderWriteFile(t, root, "constant.go", before)
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "accepted")
	sha := strings.TrimSpace(builderGit(t, root, "rev-parse", "HEAD"))
	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("fixture")
	idx.storeRootPath(root)
	t.Cleanup(idx.Close)
	_, err := idx.Index(root)
	require.NoError(t, err)
	stage := &incrementalBatchStage{graphPath: "fixture/constant.go", prepared: &preparedExtraction{absPath: path, relPath: "constant.go"}}
	// The production delta exposes composed accepted inventory rather than
	// FileMetaPathReader. Exercise that exact capability shape.
	idx.graph = graph.NewDeltaWriter(g, graph.New())
	read := idx.verifiedHeadContractInputs(root, sha)
	receipt, ok := read(stage)
	require.True(t, ok, "exact accepted file receipt must permit bounded reconstruction")
	require.NotEmpty(t, receipt.Constants)
	require.NoError(t, g.BulkSetConstantValues("fixture", []graph.ConstantValueRow{{NodeID: "fixture/constant.go::Route", FilePath: stage.graphPath, Value: "/diverged"}}))
	_, ok = read(stage)
	require.False(t, ok, "accepted source hash does not certify independently corrected constant sidecars")
	require.NoError(t, g.BulkSetConstantValues("fixture", []graph.ConstantValueRow{{NodeID: "fixture/constant.go::Route", FilePath: stage.graphPath, Value: "/before"}}))
	builderWriteFile(t, root, "constant.go", "package fixture\nconst Route = \"/after\"\n")
	// Change accepted inventory in the underlying source to a same-shaped
	// source that no longer matches the selected Git blob.
	require.NoError(t, g.SetFileMetas("fixture", []graph.FileMetaRow{{FilePath: stage.graphPath, ContentHash: contentHashForSource([]byte("package fixture\nconst Route = \"/after\"\n")), Size: len("package fixture\nconst Route = \"/after\"\n")}}))
	_, ok = read(stage)
	require.False(t, ok, "same-shaped graph and HEAD blob cannot replace a mismatched accepted source receipt")
	// Unknown provenance keeps the full dependency frontier, including consumers
	// that previously emitted no contract. It is not a one-time repository backfill.
	idx.contractDependenciesChanged, idx.contractSharedInputsChanged = false, false
	idx.noteChangedContractDependencyInputs(nil, g.GetFileNodes(stage.graphPath))
	require.True(t, idx.contractDependenciesChanged)
	require.True(t, idx.contractSharedInputsChanged)
}

func TestMixedContractBatchAndLineChangesMatchWholeIndex(t *testing.T) {
	for _, mode := range []string{"mixed_batch", "route_line_shift"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "a_routes.go"), unchangedTypedRouteSource)
			other := "package fixture\nfunc Other(r Router) { r.GET(\"/old\", otherHandler) }\nfunc otherHandler() {}\n"
			writeFile(t, filepath.Join(root, "b_routes.go"), other)
			g := graph.New()
			initial := newTestIndexer(g)
			initial.SetRepoPrefix("fixture")
			t.Cleanup(initial.Close)
			_, err := initial.Index(root)
			require.NoError(t, err)
			probe := &contractHydrationProbe{Graph: g}
			idx := newTestIndexer(probe)
			idx.SetRepoPrefix("fixture")
			idx.storeRootPath(root)
			idx.SetFileMtimes(initial.FileMtimes())
			t.Cleanup(idx.Close)
			paths := []string{"a_routes.go"}
			if mode == "mixed_batch" {
				writeFile(t, filepath.Join(root, "a_routes.go"), strings.Replace(unchangedTypedRouteSource, "return 1", "return 2", 1))
				writeFile(t, filepath.Join(root, "b_routes.go"), strings.Replace(other, "/old", "/new", 1))
				paths = append(paths, "b_routes.go")
			} else {
				writeFile(t, filepath.Join(root, "a_routes.go"), strings.Replace(unchangedTypedRouteSource, "func register", "\nfunc register", 1))
			}
			_, err = idx.IncrementalReindexPaths(root, paths)
			require.NoError(t, err)
			clean := graph.New()
			full := newTestIndexer(clean)
			full.SetRepoPrefix("fixture")
			t.Cleanup(full.Close)
			_, err = full.Index(root)
			require.NoError(t, err)
			actual := contracts.LoadRegistryFromGraph(g, "fixture").ByRepo("fixture")
			expected := contracts.LoadRegistryFromGraph(clean, "fixture").ByRepo("fixture")
			require.True(t, contractSetsEqual(expected, actual), "full normalized owner payloads and route locations must match: expected=%#v actual=%#v", expected, actual)
			if mode == "mixed_batch" {
				require.Equal(t, 1, idx.contractUnchangedFiles)
			} else {
				require.Zero(t, idx.contractUnchangedFiles)
				require.Positive(t, probe.ownerReads, "Full/location change must retain the correct broad fallback")
			}
		})
	}
}

type failingContractProjection struct {
	*graph.Graph
	err error
}

func (g failingContractProjection) LoadContractFileProjectionContext(context.Context, string, []string) (graph.ContractFileProjection, error) {
	return graph.ContractFileProjection{}, g.err
}

func TestContractProjectionFailureDoesNotAcceptFileReceipt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.go")
	writeFile(t, path, "package fixture\nfunc Value() int { return 1 }\n")
	g := graph.New()
	initial := newTestIndexer(g)
	initial.SetRepoPrefix("fixture")
	t.Cleanup(initial.Close)
	_, err := initial.Index(root)
	require.NoError(t, err)
	before, err := g.FileMetasByPaths("fixture", []string{"fixture/value.go"})
	require.NoError(t, err)
	idx := newTestIndexer(failingContractProjection{Graph: g, err: context.Canceled})
	idx.SetRepoPrefix("fixture")
	idx.storeRootPath(root)
	idx.SetFileMtimes(initial.FileMtimes())
	t.Cleanup(idx.Close)
	mtimes := idx.FileMtimes()
	writeFile(t, path, "package fixture\nfunc Value() int { return 2 }\n")
	require.ErrorIs(t, idx.IndexFile(path), context.Canceled)
	after, err := g.FileMetasByPaths("fixture", []string{"fixture/value.go"})
	require.NoError(t, err)
	require.Equal(t, before, after, "failed pre-eviction proof must not accept new source inventory")
	require.Equal(t, mtimes, idx.FileMtimes(), "unapplied source must not be marked current")
}
