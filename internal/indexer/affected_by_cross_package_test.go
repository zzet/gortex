package indexer

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// A package-qualified call (p.F() from package q) whose target is renamed
// away is parked under its import-qualified stub, which the changed file's
// incoming leg never enumerates: when the rename is undone only the
// referrer's own outgoing re-resolution binds it again, and that
// re-resolution is the affected-by pass. Checked on the whole-root and the
// per-file entrypoint; without the affected-by pass the call stays pending.
func TestAffectedByRebindsAPackageQualifiedCaller(t *testing.T) {
	for _, entry := range []string{"reindex_paths", "index_file"} {
		t.Run(entry, func(t *testing.T) {
			builderIsolateGit(t)
			root := builderTempDir(t, "affected-cross-package-"+entry)
			builderGit(t, root, "init", "--initial-branch=main")
			builderWriteTree(t, root, map[string]string{
				"go.mod":   "module example.test/m\n\ngo 1.23\n",
				"p/a.go":   "package p\n\nfunc F(x int) int { return x }\n",
				"q/b.go":   "package q\n\nimport \"example.test/m/p\"\n\nfunc Caller() int { return p.F(1) }\n",
				"q/doc.go": "package q\n",
			})
			builderGit(t, root, "add", "-A")
			builderGit(t, root, "commit", "-m", "fixture")

			g := graph.New()
			cfg := config.Default().Index
			cfg.Workers = 1
			idx := New(g, builderRegistry(), cfg, zap.NewNop())
			idx.SetRepoPrefix(builderRepoPrefix)
			_, err := idx.Index(root)
			require.NoError(t, err)
			aFile, bFile := builderRepoPrefix+"/p/a.go", builderRepoPrefix+"/q/b.go"
			callerID := fnNodeID(t, g, bFile, "Caller")
			require.Equal(t, fnNodeID(t, g, aFile, "F"), callTargetFrom(t, g, callerID),
				"fixture precondition: the package-qualified call must resolve to F")

			aPath := filepath.Join(root, "p", "a.go")
			save := func(content string) {
				t.Helper()
				bumpMtime(t, aPath, content)
				if entry == "reindex_paths" {
					_, err = idx.IncrementalReindexPaths(root, []string{aPath})
				} else {
					err = idx.IndexFile(aPath)
				}
				require.NoError(t, err)
			}
			// Rename F away: the caller's reference is parked under its
			// import-qualified stub.
			save("package p\n\nfunc G(x int) int { return x }\n")
			require.True(t, graph.IsUnresolvedTarget(callTargetFrom(t, g, callerID)),
				"with F renamed away the caller must be pending, got %s", callTargetFrom(t, g, callerID))
			// Undo: F is declared again, and only the caller's own outgoing
			// re-resolution can find the parked reference.
			save("package p\n\nfunc F(x int) int { return x }\n")
			require.Equal(t, fnNodeID(t, g, aFile, "F"), callTargetFrom(t, g, callerID),
				"after the rename was undone, q's package-qualified call must be bound to F again")
		})
	}
}

// Renaming a parameter changes its owner's contract: the argument another
// package passes into it (q's Caller#param:v -arg_of-> F#param:x) loses its
// target, and no pass of the save re-parses the caller. The referrer's rows
// must still equal a whole index of the edited tree (the argument bound to
// F#param:y). The eviction moves the argument back to F
// (argOfIntoSurvivingOwner) and the affected-by pass re-materializes it;
// without either the argument is lost.
func TestAffectedByRematerializesAReferrersArgumentIntoARenamedParameter(t *testing.T) {
	for _, entry := range []string{"reindex_paths", "index_file"} {
		t.Run(entry, func(t *testing.T) {
			builderIsolateGit(t)
			root := builderTempDir(t, "affected-param-rename-"+entry)
			builderGit(t, root, "init", "--initial-branch=main")
			builderWriteTree(t, root, map[string]string{
				"go.mod": "module example.test/m\n\ngo 1.23\n",
				"p/a.go": "package p\n\nfunc F(x int) int { return x }\n",
				"q/b.go": "package q\n\nimport \"example.test/m/p\"\n\nfunc Caller(v int) int { return p.F(v) }\n",
			})
			builderGit(t, root, "add", "-A")
			builderGit(t, root, "commit", "-m", "fixture")
			index := func() (*graph.Graph, *Indexer) {
				g := graph.New()
				cfg := config.Default().Index
				cfg.Workers = 1
				idx := New(g, builderRegistry(), cfg, zap.NewNop())
				idx.SetRepoPrefix(builderRepoPrefix)
				_, err := idx.Index(root)
				require.NoError(t, err)
				return g, idx
			}
			referrerRows := func(g *graph.Graph) []string {
				var rows []string
				for _, e := range g.AllEdges() {
					if e.FilePath == builderRepoPrefix+"/q/b.go" {
						rows = append(rows, e.From+" -"+string(e.Kind)+"-> "+e.To)
					}
				}
				sort.Strings(rows)
				return rows
			}
			g, idx := index()
			require.Contains(t, referrerRows(g), builderRepoPrefix+"/q/b.go::Caller#param:v -arg_of-> "+builderRepoPrefix+"/p/a.go::F#param:x",
				"fixture precondition: the caller's argument is bound to F's parameter")

			aPath := filepath.Join(root, "p", "a.go")
			bumpMtime(t, aPath, "package p\n\nfunc F(y int) int { return y }\n")
			var err error
			if entry == "reindex_paths" {
				_, err = idx.IncrementalReindexPaths(root, []string{aPath})
			} else {
				err = idx.IndexFile(aPath)
			}
			require.NoError(t, err)
			fresh, _ := index()
			require.Equal(t, referrerRows(fresh), referrerRows(g),
				"the referrer's rows after the parameter rename must equal a whole index of the edited tree")
		})
	}
}
