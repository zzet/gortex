package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

// This pins the two lost derived families, not whole-graph cold parity: the
// legacy EOF path has separate baseline differences in other derived rows.
// Saves select the existing structural reparse mode so this eviction coverage
// remains meaningful when an EOF edit is eligible for metadata-only refresh.
func TestStructuralEOFRestatesValueReadsAndExternalCalls(t *testing.T) {
	t.Setenv("GORTEX_SYNTH_EXTERNAL_CALLS", "1")
	raw, err := os.ReadFile(filepath.Join("..", "analysis", "adjacency.go"))
	require.NoError(t, err)
	files := map[string]string{"adjacency.go": string(raw), "routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	warm := newContractJobFixture(t, files)
	const owner, sibling = "fixture/adjacency.go", "fixture/routes.go"
	baseline := structuralRestatementRows(warm.store, owner)
	require.NotEmpty(t, baseline)
	require.Contains(t, strings.Join(baseline, "\n"), "pprDefaultRestart", "actual constant reads must be present")
	require.Contains(t, strings.Join(baseline, "\n"), "external-call::", "actual synthesized external calls must be present")
	siblingNodes := builderRenderNodes(warm.store.GetFileNodes(sibling))
	siblingEdges := builderRenderEdges(structuralFileEdges(warm.store, sibling))
	require.NotEmpty(t, siblingNodes)
	for round := 0; round < 2; round++ {
		files["adjacency.go"] += "\n// same semantic source, structural EOF refresh\n"
		result := structuralRestatementSave(t, warm, "adjacency.go", files["adjacency.go"], round)
		require.Zero(t, result.DerivedInvalidation.Flags)
		require.Zero(t, result.DerivedInvalidation.MetadataOnlyFiles, "existing structural reparse mode must run")
		require.Equal(t, []string{owner}, result.DerivedInvalidation.structuralFiles)
		require.Equal(t, baseline, structuralRestatementRows(warm.store, owner), "EOF must preserve complete rows of both derived families")
		require.Equal(t, siblingNodes, builderRenderNodes(warm.store.GetFileNodes(sibling)))
		require.Equal(t, siblingEdges, builderRenderEdges(structuralFileEdges(warm.store, sibling)))
	}
	// A position-only edit still needs fresh current rows, not old-row carry.
	files["adjacency.go"] = "\n" + files["adjacency.go"]
	result := structuralRestatementSave(t, warm, "adjacency.go", files["adjacency.go"], 2)
	require.Zero(t, result.DerivedInvalidation.Flags)
	require.Equal(t, []string{owner}, result.DerivedInvalidation.structuralFiles)
	shifted := structuralRestatementRows(warm.store, owner)
	require.NotEqual(t, baseline, shifted)
	cold := newContractJobFixture(t, files)
	require.Equal(t, structuralRestatementRows(cold.store, owner), shifted, "complete current read/external rows must equal a completed cold index")
}

func structuralRestatementSave(t *testing.T, f *contractJobFixture, name, source string, round int) *IndexResult {
	t.Helper()
	path := filepath.Join(f.root, name)
	writeFile(t, path, source)
	mtime := time.Now().Add(time.Duration(round+1) * time.Minute)
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	// Reuse the real per-file delta's structural mode, retaining normal prior
	// binding reuse rather than clearing resolutions or adding a test hook.
	idx := f.mi.indexers["fixture"]
	previous := idx.reparseKeepingResolutions
	idx.reparseKeepingResolutions = map[string]struct{}{filepath.Clean(path): {}}
	defer func() { idx.reparseKeepingResolutions = previous }()
	result, err := f.mi.IncrementalReindexRepo("fixture", []string{path})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	return result
}

func structuralFileEdges(g graph.Reader, file string) []*graph.Edge {
	var edges []*graph.Edge
	for _, edge := range g.AllEdges() {
		if edge != nil && edge.FilePath == file {
			edges = append(edges, edge)
		}
	}
	return edges
}

func structuralRestatementRows(g graph.Reader, file string) []string {
	var edges []*graph.Edge
	for _, edge := range structuralFileEdges(g, file) {
		via, _ := edge.Meta["via"].(string)
		if (edge.Kind == graph.EdgeReads && (via == "value_ref" || via == "value_ref_candidate")) ||
			strings.HasPrefix(edge.To, "dep::") || strings.HasPrefix(edge.To, "external::") || strings.HasPrefix(edge.To, "external-call::") {
			edges = append(edges, edge)
		}
	}
	return builderRenderEdges(edges)
}

func TestStructuralRestatementRequiresReparseAndConfiguredFamilies(t *testing.T) {
	t.Setenv("GORTEX_SYNTH_EXTERNAL_CALLS", "")
	for _, tc := range []struct {
		name            string
		flags           DerivedInvalidationFlags
		structural      bool
		valueEnabled    bool
		externalEnabled bool
	}{
		{name: "artifact_flag", flags: DerivedInvalidatesArtifacts, structural: true, valueEnabled: true, externalEnabled: true},
		{name: "test_flag", flags: DerivedInvalidatesTests, structural: true, valueEnabled: true, externalEnabled: true},
		{name: "metadata_only", valueEnabled: true, externalEnabled: true},
		{name: "value_ref_disabled", structural: true, externalEnabled: true},
		{name: "external_disabled", structural: true, valueEnabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			const file, from, target = "repo/a.go", "repo/a.go::Run", "repo/a.go::LIMIT_X"
			g.AddBatch([]*graph.Node{
				{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "repo", Language: "go"},
				{ID: from, Kind: graph.KindFunction, Name: "Run", FilePath: file, RepoPrefix: "repo", Language: "go", StartLine: 4},
				{ID: target, Kind: graph.KindConstant, Name: "LIMIT_X", FilePath: file, RepoPrefix: "repo", Language: "go", StartLine: 2},
			}, []*graph.Edge{
				{From: from, To: "unresolved::valueref::LIMIT_X", Kind: graph.EdgeReads, FilePath: file, Line: 5, Meta: map[string]any{"name": "LIMIT_X", "via": "value_ref_candidate"}},
				{From: from, To: "dep::example.com/client::Send", Kind: graph.EdgeCalls, FilePath: file, Line: 6},
			})
			cfg := config.Default().Index
			cfg.SynthesizeExternalCalls = &tc.externalEnabled
			selected := []string{resolver.SynthValueRefName}
			if !tc.valueEnabled {
				selected = nil
			}
			cfg.FrameworkSynthesizers = &selected
			mi := &MultiIndexer{graph: g, indexers: map[string]*Indexer{"repo": {config: cfg}}, logger: zap.NewNop()}
			plan := DerivedInvalidationPlan{Flags: tc.flags, Files: []string{file}}
			if tc.structural {
				plan.structuralFiles = []string{file}
			} else {
				plan.MetadataOnlyFiles = 1
			}
			mi.RunIncrementalDerivedPasses(context.Background(), map[string]DerivedInvalidationPlan{"repo": plan})
			var readBound, externalBound bool
			for _, edge := range g.GetOutEdges(from) {
				readBound = readBound || (edge.Kind == graph.EdgeReads && edge.To == target)
				externalBound = externalBound || strings.HasPrefix(edge.To, "external-call::")
			}
			require.Equal(t, tc.structural && tc.valueEnabled, readBound)
			require.Equal(t, tc.structural && tc.externalEnabled, externalBound)
		})
	}
}
