package indexer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A typed base and a typed sparse pass must agree on unchanged context. An
// untyped comparison retains every caller solely for missing compiler facts.
func TestCommitLayerWithholdsUnchangedTypedContext(t *testing.T) {
	for _, mode := range []string{"shadow", "direct"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "direct" {
				t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
			}
			builderIsolateGit(t)
			t.Setenv("GORTEX_ENRICH_MIN_NODES", "1")
			tree := map[string]string{
				"go.mod":  "module example.com/typedcontext\n\ngo 1.22\n",
				"core.go": "package fixture\nfunc Pivot() int { return 1 }\n",
			}
			for i := range 4 {
				tree[fmt.Sprintf("caller%d.go", i)] = fmt.Sprintf("package fixture\nfunc Caller%d() int {\n total := 0\n%s return total\n}\n", i, strings.Repeat(" total += Pivot()\n", 16))
			}
			repo := builderTempDir(t, "typed-context")
			builderGit(t, repo, "init", "--initial-branch=main")
			builderWriteTree(t, repo, tree)
			builderGit(t, repo, "add", "-A")
			builderGit(t, repo, "commit", "-m", "base")
			baseTree := builderGit(t, repo, "rev-parse", "HEAD^{tree}")
			store := builderOpenStore(t, "typed-context")
			manager := goTypesManager(t)
			builderIndexSemantic(manager)(t, store, repo)
			baseCaller := store.GetNode(builderRepoPrefix + "/caller0.go::Caller0")
			require.NotNil(t, baseCaller)
			require.Equal(t, "go-types", baseCaller.Meta["semantic_source"])
			require.NotEmpty(t, baseCaller.Meta["clone_body"], "exercise parser-only clone transport metadata")

			// The declaration changes, so callers must be re-derived; their own
			// compiler facts and their calls are nevertheless identical at this tree.
			tree["core.go"] = "package fixture\nfunc Pivot(args ...int) int { return 2 }\n"
			builderWriteTree(t, repo, tree)
			builderGit(t, repo, "add", "-A")
			builderGit(t, repo, "commit", "-m", "variadic pivot")
			target := builderGit(t, repo, "rev-parse", "HEAD^{tree}")
			b := builderNewBuilder(store)
			b.Semantic = manager
			id, report, err := b.BuildCommitLayer(context.Background(), CommitLayerRequest{
				Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: "graph-fixture", LayerID: "typed-target", CheckoutID: "typed-checkout"},
				Base:     store, RepoDir: repo, RootPath: repo, BaseTreeOID: baseTree, TargetTreeOID: target,
				RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
			})
			require.NoError(t, err)
			require.True(t, report.CommittedTypecheck.Ran, "%+v", report.CommittedTypecheck)
			require.Equal(t, mode == "shadow", report.ContextHeldInMemory)
			require.Contains(t, report.ClosurePaths, "caller0.go")
			for i := range 4 {
				path := fmt.Sprintf("%s/caller%d.go", builderRepoPrefix, i)
				require.Contains(t, report.ContextPaths, path, "identical typed context was persisted: %+v", report.ContextRetainedPaths)
				require.Empty(t, store.AtGeneration(id).GetFileNodes(path))
			}
			flat := builderOpenStore(t, "typed-flat")
			builderIndexSemantic(goTypesManager(t))(t, flat, repo)
			builderAssertReadersAgree(t, builderComposed(t, store, id), flat)
		})
	}
}
