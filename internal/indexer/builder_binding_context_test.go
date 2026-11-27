package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

func TestCommitLayerPreservesBindingTypesAcrossReopen(t *testing.T) {
	for _, mode := range []string{"shadow", "direct"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
			builderIsolateGit(t)
			t.Setenv("GORTEX_ENRICH_MIN_NODES", "1")
			repo := builderTempDir(t, "binding-review")
			builderGit(t, repo, "init", "--initial-branch=main")
			tree := map[string]string{"go.mod": "module example.com/bindingreview\n\ngo 1.22\n", "core.go": "package fixture\ntype Answer struct { Count int }\nfunc Pivot() int {\n value := Answer{Count: 1}\n return value.Count\n}\n"}
			builderWriteTree(t, repo, tree)
			builderGit(t, repo, "add", "-A")
			builderGit(t, repo, "commit", "-m", "base")
			baseTree := builderGit(t, repo, "rev-parse", "HEAD^{tree}")
			store := builderOpenStore(t, "binding-review")
			manager := goTypesManager(t)
			builderIndexSemantic(manager)(t, store, repo)
			site := graph.SemanticBindingSite{RepoPrefix: builderRepoPrefix, FilePath: builderRepoPrefix + "/core.go", Line: 4, Name: "value"}
			baseBindings, e := store.SemanticBindingTypes([]graph.SemanticBindingSite{site})
			require.NoError(t, e)
			require.Equal(t, "Answer", baseBindings[site])
			if mode == "shadow" {
				t.Setenv("GORTEX_SHADOW_MAX_FILES", "10000")
			}
			tree["core.go"] = "package fixture\ntype Answer struct { Count int }\nfunc Pivot() int {\n renamed := Answer{Count: 2}\n return renamed.Count\n}\n"
			builderWriteTree(t, repo, tree)
			builderGit(t, repo, "add", "-A")
			builderGit(t, repo, "commit", "-m", "target")
			target := builderGit(t, repo, "rev-parse", "HEAD^{tree}")
			b := builderNewBuilder(store)
			b.Semantic = manager
			id, report, e := b.BuildCommitLayer(context.Background(), CommitLayerRequest{Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: "graph-fixture", LayerID: "binding-target", CheckoutID: "binding-checkout"}, Base: store, RepoDir: repo, RootPath: repo, BaseTreeOID: baseTree, TargetTreeOID: target, RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix})
			require.NoError(t, e)
			require.True(t, report.CommittedTypecheck.Ran)
			targetSite := site
			targetSite.Name = "renamed"
			got, e := store.AtGeneration(id).SemanticBindingTypes([]graph.SemanticBindingSite{site, targetSite})
			require.NoError(t, e)

			composed := builderComposed(t, store, id).(graph.SemanticBindingTypeReader)
			served, e := composed.SemanticBindingTypes([]graph.SemanticBindingSite{site, targetSite})
			require.NoError(t, e)
			expected := "Answer"
			require.Equal(t, expected, got[targetSite])
			require.NotContains(t, got, site)
			require.Equal(t, expected, served[targetSite])
			require.NotContains(t, served, site)
			file := store.Path()
			require.NoError(t, store.Close())
			reopened := builderOpenStoreAt(t, file)
			defer reopened.Close()
			selected := builderComposed(t, reopened, id)
			durable, e := selected.(graph.SemanticBindingTypeReader).SemanticBindingTypes([]graph.SemanticBindingSite{site, targetSite})
			require.NoError(t, e)
			require.Equal(t, expected, durable[targetSite])
			require.NotContains(t, durable, site)
			projection, e := (commitLayerBase{Reader: selected}).SemanticBindingTypesForFiles(builderRepoPrefix, []string{site.FilePath})
			require.NoError(t, e)
			require.Equal(t, "Answer", projection[targetSite])
			require.NotContains(t, projection, site)
			baseAgain, e := reopened.SemanticBindingTypes([]graph.SemanticBindingSite{site})
			require.NoError(t, e)
			require.Equal(t, "Answer", baseAgain[site])
			t.Logf("mode=%s composed=%v reopened-composed=%v base-still-present=%v producer-ran=%t", mode, served, durable, baseAgain, report.CommittedTypecheck.Ran)

		})
	}
}
func TestCommitLayerPreservesChangedContextBindings(t *testing.T) {
	for _, mode := range []string{"shadow", "direct"} {
		t.Run(mode, func(t *testing.T) {
			builderIsolateGit(t)
			t.Setenv("GORTEX_ENRICH_MIN_NODES", "1")
			t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
			repo := builderTempDir(t, "binding-context")
			builderGit(t, repo, "init", "--initial-branch=main")
			tree := map[string]string{
				"go.mod":    "module example.com/bindingcontext\n\ngo 1.22\n",
				"types.go":  "package fixture\ntype First struct{}\ntype Second struct{}\nfunc Make() First { return First{} }\n",
				"caller.go": "package fixture\nfunc Use() int {\n value := Make()\n _ = value\n return 1\n}\n",
			}
			builderWriteTree(t, repo, tree)
			builderGit(t, repo, "add", "-A")
			builderGit(t, repo, "commit", "-m", "base")
			baseTree := builderGit(t, repo, "rev-parse", "HEAD^{tree}")
			store := builderOpenStore(t, "binding-context")
			manager := goTypesManager(t)
			builderIndexSemantic(manager)(t, store, repo)
			site := graph.SemanticBindingSite{RepoPrefix: builderRepoPrefix, FilePath: builderRepoPrefix + "/caller.go", Line: 3, Name: "value"}
			before, err := store.SemanticBindingTypes([]graph.SemanticBindingSite{site})
			require.NoError(t, err)
			require.Equal(t, "First", before[site])
			if mode == "shadow" {
				t.Setenv("GORTEX_SHADOW_MAX_FILES", "10000")
			}
			tree["types.go"] = "package fixture\ntype First struct{}\ntype Second struct{}\nfunc Make() Second { return Second{} }\n"
			builderWriteTree(t, repo, tree)
			builderGit(t, repo, "add", "-A")
			builderGit(t, repo, "commit", "-m", "target")
			target := builderGit(t, repo, "rev-parse", "HEAD^{tree}")
			b := builderNewBuilder(store)
			b.Semantic = manager
			id, report, err := b.BuildCommitLayer(context.Background(), CommitLayerRequest{
				Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: "graph-fixture", LayerID: "binding-context", CheckoutID: "binding-checkout"},
				Base:     store, RepoDir: repo, RootPath: repo, BaseTreeOID: baseTree, TargetTreeOID: target,
				RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
			})
			require.NoError(t, err)
			require.True(t, report.CommittedTypecheck.Ran)
			got, err := builderComposed(t, store, id).(graph.SemanticBindingTypeReader).SemanticBindingTypes([]graph.SemanticBindingSite{site})
			require.NoError(t, err)
			require.Equal(t, "Second", got[site], "unchanged source has a changed compiler binding")
		})
	}
}

func TestContextWithdrawalComparesCompleteBindingProjection(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		withhold     bool
	}{
		{"equal", "Answer", true}, {"changed", "Reply", false}, {"removed", "", false}, {"unknown", "Answer", false}, {"incomplete", "Answer", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "binding-withdrawal")
			file := builderRepoPrefix + "/caller.go"
			store.AddBatch([]*graph.Node{{ID: file + "::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: file, RepoPrefix: builderRepoPrefix, Language: "go"}}, nil)
			site := graph.SemanticBindingSite{RepoPrefix: builderRepoPrefix, FilePath: file, Line: 3, Name: "value"}
			require.NoError(t, store.ReplaceSemanticBindingTypes(builderRepoPrefix, []graph.SemanticBindingType{{Site: site, TypeName: "Answer"}}))
			corpus := &committedBindingCorpus{Graph: graph.New()}
			corpus.AddBatch(store.GetFileNodes(file), nil)
			var bindings []graph.SemanticBindingType
			if tc.target != "" {
				bindings = append(bindings, graph.SemanticBindingType{Site: site, TypeName: tc.target})
			}
			require.NoError(t, corpus.ReplaceSemanticBindingTypes(builderRepoPrefix, bindings))
			b := builderNewBuilder(store)
			b.Semantic = goTypesManager(t)
			var base LayerBase = store
			if tc.name == "unknown" {
				base = bindinglessContextBase{LayerBase: store}
			}
			if tc.name == "incomplete" {
				base = incompleteBindingContextBase{LayerBase: store}
			}
			out, err := b.withholdContextPayload(context.Background(), BuildRequest{RepoPrefix: builderRepoPrefix, Base: base, committedTypecheck: &committedTypecheckStage{}}, buildPlan{indexed: []string{"changed.go", "caller.go"}, context: []string{"caller.go"}}, corpus)
			require.NoError(t, err)
			require.Equal(t, tc.withhold, len(out.withheldPaths) > 0, "node equality alone cannot prove compiler bindings equal")
		})
	}
}

type bindinglessContextBase struct{ LayerBase }
type incompleteBindingContextBase struct{ LayerBase }

func TestDeclaredGoContextPreservesInheritedSecondHopWithUnknownBindings(t *testing.T) {
	for _, projection := range []string{"unsupported", "incomplete", "unsupported_corpus"} {
		t.Run(projection, func(t *testing.T) {
			builderIsolateGit(t)
			t.Setenv("GORTEX_ENRICH_MIN_NODES", "1")
			repo := builderTempDir(t, "declared-go-binding")
			builderGit(t, repo, "init", "--initial-branch=main")
			tree := map[string]string{
				"go.mod":      "module example.com/declaredcontext\n\ngo 1.22\n",
				"changed.go":  "package fixture\nfunc Changed() int { return Declared() }\n",
				"declared.go": "package fixture\nfunc Declared() int { return Leaf() }\n",
				"leaf.go":     "package fixture\nfunc Leaf() int { return 1 }\n",
			}
			builderWriteTree(t, repo, tree)
			builderGit(t, repo, "add", "-A")
			builderGit(t, repo, "commit", "-m", "base")
			baseTree := builderGit(t, repo, "rev-parse", "HEAD^{tree}")
			store := builderOpenStore(t, "declared-go-binding")
			manager := goTypesManager(t)
			builderIndexSemantic(manager)(t, store, repo)
			declaredPath := builderRepoPrefix + "/declared.go"
			declaredID, leafID := declaredPath+"::Declared", builderRepoPrefix+"/leaf.go::Leaf"
			hasInheritedCall := func(reader graph.Reader) bool {
				for _, edge := range reader.GetOutEdges(declaredID) {
					if edge.To == leafID && edge.Kind == graph.EdgeCalls {
						return true
					}
				}
				return false
			}
			require.True(t, hasInheritedCall(store), "the authoritative base must contain the second-hop call")
			tree["changed.go"] = "package fixture\nfunc Changed() int { return Declared() + 1 }\n"
			builderWriteTree(t, repo, tree)
			builderGit(t, repo, "add", "-A")
			builderGit(t, repo, "commit", "-m", "change caller")
			var base LayerBase = store
			if projection == "unsupported" {
				base = bindinglessContextBase{LayerBase: store}
			}
			if projection == "incomplete" {
				base = incompleteBindingContextBase{LayerBase: store}
			}
			b := builderNewBuilder(store)
			b.Semantic = manager
			targetTree := builderGit(t, repo, "rev-parse", "HEAD^{tree}")
			var id int64
			var report BuildReport
			var err error
			if projection == "unsupported_corpus" {
				// Dedicated deltas request committed type checking but their
				// pre-drain corpus is an ordinary Graph, not the commit-layer
				// compiler-binding sidecar. Exercise that production request
				// shape through the real sparse publication and reader.
				target, openErr := source.NewGitTreeSource(t.Context(), repo, targetTree)
				require.NoError(t, openErr)
				defer target.Close()
				changes, diffErr := diffTreeChanges(t.Context(), repo, baseTree, targetTree)
				require.NoError(t, diffErr)
				id, report, err = b.Build(t.Context(), BuildRequest{
					Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: "graph-fixture", LayerID: "declared-context", CheckoutID: "declared-checkout", GenerationKind: DedicatedBaseGenerationKind, TreeOID: targetTree},
					Base:     base, Target: target, Changes: changes, RootPath: repo,
					RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
					committedTypecheck: &committedTypecheckStage{},
				})
			} else {
				id, report, err = b.BuildCommitLayer(t.Context(), CommitLayerRequest{
					Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: "graph-fixture", LayerID: "declared-context", CheckoutID: "declared-checkout"},
					Base:     base, RepoDir: repo, RootPath: repo, BaseTreeOID: baseTree, TargetTreeOID: targetTree,
					RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
				})
			}
			require.NoError(t, err)
			require.Contains(t, report.ClosureDeclaredPaths, "declared.go")
			require.NotContains(t, report.ClosurePaths, "leaf.go", "the sparse corpus must omit the second hop")
			require.True(t, hasInheritedCall(builderComposed(t, store, id)), "the composed SQLite reader lost the authoritative second-hop edge")
			require.Contains(t, report.ContextPaths, declaredPath)
			require.Empty(t, store.AtGeneration(id).GetFileNodes(declaredPath), "declared context must not publish replacement ownership")
		})
	}
}

func (incompleteBindingContextBase) SemanticBindingTypesForFiles(string, []string) (map[graph.SemanticBindingSite]string, error) {
	return nil, graph.ErrSemanticBindingProjectionIncomplete
}

// Isolate the withdrawal/publication boundary: the sparse pass read Declared
// to bind Changed, but deliberately did not read what Declared calls. No later
// compiler pass may accidentally repair this fixture's missing second hop.
func TestDeclaredContextPublicationKeepsAuthoritativeInheritedEdge(t *testing.T) {
	store := builderOpenStore(t, "declared-context-publication")
	changedPath, declaredPath, leafPath := builderRepoPrefix+"/changed.go", builderRepoPrefix+"/declared.go", builderRepoPrefix+"/leaf.go"
	changed := &graph.Node{ID: changedPath + "::Changed", Kind: graph.KindFunction, Name: "Changed", Language: "go", FilePath: changedPath, RepoPrefix: builderRepoPrefix}
	declared := &graph.Node{ID: declaredPath + "::Declared", Kind: graph.KindFunction, Name: "Declared", Language: "go", FilePath: declaredPath, RepoPrefix: builderRepoPrefix}
	leaf := &graph.Node{ID: leafPath + "::Leaf", Kind: graph.KindFunction, Name: "Leaf", Language: "go", FilePath: leafPath, RepoPrefix: builderRepoPrefix}
	intoDeclared := &graph.Edge{From: changed.ID, To: declared.ID, Kind: graph.EdgeCalls, FilePath: changedPath}
	intoLeaf := &graph.Edge{From: declared.ID, To: leaf.ID, Kind: graph.EdgeCalls, FilePath: declaredPath}
	store.AddBatch([]*graph.Node{changed, declared, leaf}, []*graph.Edge{intoDeclared, intoLeaf})
	corpus := graph.New()
	corpus.AddBatch([]*graph.Node{changed, declared}, []*graph.Edge{intoDeclared})
	b := builderNewBuilder(store)
	b.Semantic = goTypesManager(t)
	plan := buildPlan{indexed: []string{"changed.go", "declared.go"}, context: []string{"declared.go"}, declared: map[string]struct{}{"declared.go": {}}}
	req := BuildRequest{
		Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: "graph-fixture", LayerID: "declared-publication", CheckoutID: "declared-checkout", GenerationKind: CommitLayerGenerationKind},
		Base:     store, RepoPrefix: builderRepoPrefix, committedTypecheck: &committedTypecheckStage{},
	}
	separation, err := b.withholdContextPayload(t.Context(), req, plan, corpus)
	require.NoError(t, err)
	id, handle, _, err := store.BeginPayloadGenerationWithStatus(t.Context(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: req.Identity.OwnerKind, GraphID: req.Identity.GraphID, LayerID: req.Identity.LayerID, CheckoutID: req.Identity.CheckoutID, GenerationKind: req.Identity.GenerationKind,
	})
	require.NoError(t, err)
	handle.AddBatch(corpus.AllNodes(), corpus.AllEdges())
	var report BuildReport
	separation.record(&plan, &report)
	require.NoError(t, b.writeMasks(req, plan, handle, &report))
	require.NoError(t, store.PublishPayloadGeneration(t.Context(), id, 1))
	served := builderComposed(t, store, id)
	found := false
	for _, edge := range served.GetOutEdges(declared.ID) {
		if edge.To == leaf.ID && edge.Kind == graph.EdgeCalls {
			found = true
		}
	}
	require.True(t, found, "the composed SQLite reader must inherit Declared -> Leaf from the authoritative base")
	require.Empty(t, handle.GetFileNodes(declaredPath), "read-only declared context must not publish OwnershipReplace")
}
