package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Ordinary MultiIndexer saves defer their resolver work to a new master.
// The master's evidence must include the old full-pass terminal proof, not
// just the shape that tells it to skip that same unresolved reference.
func TestMultiIndexerStructuralReplayRetainsTerminalProofThroughMaster(t *testing.T) {
	const name = "caller.go"
	const file = "fixture/" + name
	const caller = file + "::Run"
	const source = "package fixture\nfunc Run() { Missing() }\n"
	f := newContractJobFixture(t, map[string]string{name: source})
	calls := func(fixture *contractJobFixture) []*graph.Edge {
		var result []*graph.Edge
		for _, edge := range fixture.store.GetOutEdges(caller) {
			if edge.Kind == graph.EdgeCalls && edge.FilePath == file {
				result = append(result, edge)
			}
		}
		return result
	}
	prior := calls(f)
	require.Len(t, prior, 1)
	require.Equal(t, "unresolved::Missing", prior[0].To)
	require.Equal(t, true, prior[0].Meta[graph.EdgeMetaResolveTerminal], "completed cold indexing must supply the proof being inherited")
	require.Equal(t, "no_definition", prior[0].Meta[graph.EdgeMetaResolveTerminalReason])
	path := filepath.Join(f.root, name)
	mtime := time.Now().Add(time.Minute)
	save := func(text string) *IndexResult {
		rawEdgeProofRemoveMarker(f, file)
		writeFile(t, path, text)
		mtime = mtime.Add(time.Minute)
		require.NoError(t, os.Chtimes(path, mtime, mtime))
		result, err := f.mi.IncrementalReindexRepo("fixture", []string{path})
		require.NoError(t, err)
		require.Empty(t, result.FailedFiles)
		require.Zero(t, result.DerivedInvalidation.MetadataOnlyFiles, "exercise the ordinary evict/raw-add/deferred master path")
		return result
	}
	// A real new declaration makes this an ordinary structural mutation.
	// The old Missing call itself keeps its exact line and payload.
	structural := source + "func Added() {}\n"
	save(structural)
	cold := newContractJobFixture(t, map[string]string{name: structural})
	require.Equal(t, builderRenderEdges(prior), builderRenderEdges(calls(f)), "the entire unchanged row, including its proof, survives real deferred replay")
	require.Equal(t, builderRenderEdges(calls(cold)), builderRenderEdges(calls(f)))

	// The same old proof cannot carry across the arrival of a real definition.
	defined := structural + "func Missing() {}\n"
	save(defined)
	after := calls(f)
	require.Len(t, after, 1)
	require.Equal(t, file+"::Missing", after[0].To)
	require.NotContains(t, after[0].Meta, graph.EdgeMetaResolveTerminal)
	require.NotContains(t, after[0].Meta, graph.EdgeMetaResolveTerminalReason)
	definedCold := newContractJobFixture(t, map[string]string{name: defined})
	require.Equal(t, builderRenderEdges(calls(definedCold)), builderRenderEdges(after), "new definitions retain the normal incoming rebind and full-row cold result")
}
