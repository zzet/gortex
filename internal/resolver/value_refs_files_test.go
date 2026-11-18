package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestValueRefsFileRestatementPreservesScopeAndCurrentRows(t *testing.T) {
	for _, prefix := range []string{"", "repo/"} {
		t.Run(prefix, func(t *testing.T) {
			g := graph.New()
			file, sibling := prefix+"a.go", prefix+"b.go"
			addFile := func(path, name string, line int) {
				g.AddBatch([]*graph.Node{
					{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo", Language: "go"},
					{ID: path + "::" + name, Kind: graph.KindConstant, Name: name, FilePath: path, RepoPrefix: "repo", Language: "go", StartLine: 2},
					{ID: path + "::Run", Kind: graph.KindFunction, Name: "Run", FilePath: path, RepoPrefix: "repo", Language: "go", StartLine: line - 1},
				}, nil)
				valueRefCandidate(g, path+"::Run", name, path, line)
			}
			addFile(file, "RETRY_LIMIT", 8)
			addFile(sibling, "RETRY_LIMIT", 12)
			trap := &frameworkScopeTrapStore{Store: g}
			selection := AllFrameworkSynthesizers()
			require.Equal(t, 1, ResolveValueRefsForFilesWithSelection(trap, nil, []string{file}, selection))
			require.NotNil(t, readsEdge(g, file+"::Run", file+"::RETRY_LIMIT"))
			require.Nil(t, readsEdge(g, sibling+"::Run", sibling+"::RETRY_LIMIT"), "sibling candidates are not restated")
			require.Zero(t, ResolveValueRefsForFilesWithSelection(trap, nil, []string{file}, selection), "already-bound reads are idempotent")
			// A later structural parse can change both the target and position.
			// Restate its fresh candidate, rather than reinsert the old bound row.
			g.EvictFile(file)
			addFile(file, "NEXT_LIMIT", 18)
			require.Equal(t, 1, ResolveValueRefsForFilesWithSelection(trap, nil, []string{file}, selection))
			require.Nil(t, readsEdge(g, file+"::Run", file+"::RETRY_LIMIT"))
			current := readsEdge(g, file+"::Run", file+"::NEXT_LIMIT")
			require.NotNil(t, current)
			require.Equal(t, 18, current.Line)
			g.EvictFile(file)
			require.Zero(t, ResolveValueRefsForFilesWithSelection(trap, nil, []string{file}, selection), "deleted sources do not recreate reads")
			require.Zero(t, trap.globalNodeScans+trap.globalEdgeScans+trap.allNodeScans+trap.allEdgeScans+trap.repoEdgeScans)
		})
	}
}

func TestValueRefsFileRestatementHonorsDisabledSelection(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a.go::LIMIT_X", Kind: graph.KindConstant, Name: "LIMIT_X", FilePath: "a.go", StartLine: 2},
		{ID: "a.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a.go", StartLine: 4},
	}, nil)
	valueRefCandidate(g, "a.go::Run", "LIMIT_X", "a.go", 5)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthFnValue})
	require.NoError(t, err)
	trap := &frameworkScopeTrapStore{Store: g}
	require.Zero(t, ResolveValueRefsForFilesWithSelection(trap, nil, []string{"a.go"}, selection))
	require.Zero(t, trap.fileBatchReads+trap.scopedEdgeScans)
	require.Nil(t, readsEdge(g, "a.go::Run", "a.go::LIMIT_X"))
}

func TestValueRefSuccessfulBindingClearsTerminalMetadata(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var g graph.Store = graph.New()
			var disk *store_sqlite.Store
			path := filepath.Join(t.TempDir(), "value-refs.sqlite")
			if backend == "sqlite" {
				var err error
				disk, err = store_sqlite.Open(path)
				require.NoError(t, err)
				g = disk
				t.Cleanup(func() {
					if disk != nil {
						_ = disk.Close()
					}
				})
			}
			const file, target = "repo/a.go", "repo/a.go::LIMIT_X"
			nodes := []*graph.Node{
				{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "repo", Language: "go"},
				{ID: target, Kind: graph.KindConstant, Name: "LIMIT_X", FilePath: file, RepoPrefix: "repo", Language: "go", StartLine: 2},
				{ID: file + "::Shadow#LIMIT_X", Kind: graph.KindParam, Name: "LIMIT_X", FilePath: file, RepoPrefix: "repo", Language: "go", StartLine: 4},
			}
			for _, name := range []string{"Positive", "Missing", "Shadow", "Already"} {
				nodes = append(nodes, &graph.Node{ID: file + "::" + name, Kind: graph.KindFunction, Name: name, FilePath: file, RepoPrefix: "repo", Language: "go", StartLine: 4})
			}
			g.AddBatch(nodes, nil)
			candidate := func(reader, name, to string) *graph.Edge {
				edge := &graph.Edge{From: file + "::" + reader, To: to, Kind: graph.EdgeReads, FilePath: file, Line: 5,
					Meta: map[string]any{"name": name, "via": valueRefCandidateVia, "retained": "user metadata"}}
				setEdgeTerminal(edge, terminalReasonNoDefinition)
				return edge
			}
			g.AddBatch(nil, []*graph.Edge{
				candidate("Positive", "LIMIT_X", "unresolved::valueref::LIMIT_X"),
				candidate("Missing", "MISSING_X", "unresolved::valueref::MISSING_X"),
				candidate("Shadow", "LIMIT_X", "unresolved::valueref::LIMIT_X"),
			})
			require.Equal(t, 1, ResolveValueRefs(g), "full/cold resolver clears the old terminal claim on successful binding")
			// The file projection can also see a candidate already at the chosen
			// target; its metadata-only normalization must be durably persisted.
			g.AddEdge(candidate("Already", "LIMIT_X", target))
			require.Equal(t, 1, ResolveValueRefsForFilesWithSelection(g, nil, []string{file}, AllFrameworkSynthesizers()))
			check := func() {
				t.Helper()
				for _, reader := range []string{"Positive", "Already"} {
					edges := g.GetOutEdges(file + "::" + reader)
					require.Len(t, edges, 1)
					require.Equal(t, target, edges[0].To)
					require.NotContains(t, edges[0].Meta, metaResolveTerminal)
					require.NotContains(t, edges[0].Meta, metaResolveTerminalReason)
					require.Equal(t, "user metadata", edges[0].Meta["retained"])
				}
				for _, reader := range []string{"Missing", "Shadow"} {
					edges := g.GetOutEdges(file + "::" + reader)
					require.Len(t, edges, 1)
					require.True(t, graph.IsUnresolvedTarget(edges[0].To))
					require.True(t, edgeTerminalFlag(edges[0]))
					require.Equal(t, terminalReasonNoDefinition, edges[0].Meta[metaResolveTerminalReason])
					require.Equal(t, "user metadata", edges[0].Meta["retained"])
				}
			}
			check()
			before := frameworkEdgeSnapshot(g, "repo")
			require.Equal(t, 1, ResolveValueRefsForFilesWithSelection(g, nil, []string{file}, AllFrameworkSynthesizers()))
			require.Equal(t, before, frameworkEdgeSnapshot(g, "repo"), "replay preserves exact rows")
			if disk != nil {
				require.NoError(t, disk.Close())
				disk = nil
				var err error
				disk, err = store_sqlite.Open(path)
				require.NoError(t, err)
				g = disk
				check()
				require.Equal(t, before, frameworkEdgeSnapshot(g, "repo"), "terminal normalization survives reopen")
			}
		})
	}
}
