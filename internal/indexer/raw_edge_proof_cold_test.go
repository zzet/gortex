package indexer

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A completed cold installation already carries the raw extraction proof.
// Its first metadata refresh must preserve the same full state another cold
// installation derives independently from the edited source.
func TestRawEdgeProofColdInstallEOFEqualsCold(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "analysis", "adjacency.go"))
	require.NoError(t, err)
	files := map[string]string{"adjacency.go": string(raw), "routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	idx := f.mi.indexers["fixture"]
	const owner = "fixture/adjacency.go"
	prior := f.store.GetFileNodes(owner)
	require.NotEmpty(t, storedExtractionGraphFingerprints(prior).rawEdges, "cold indexing must install the proof without a structural warm save")
	require.True(t, idx.rawEdgesPriorCompleted(owner, prior))
	before, found, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", owner)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, before.Accepted)
	// The metadata refresh changes accepted source bytes, but leaves symbol
	// documents equal. The combined path must preserve exact score inputs and
	// ordered results, not invalidate shared prefix counts for this save.
	require.NoError(t, f.store.EnsureRowCounters(t.Context()))
	prefixes := []string{"adj", "build", "missing"}
	beforeStats, beforeCounts, err := f.store.SymbolFTSScoringSnapshot(t.Context(), prefixes)
	require.NoError(t, err)
	require.Positive(t, beforeStats.Rows)
	require.Positive(t, beforeStats.Tokens)
	require.NotEmpty(t, beforeStats.Stamp)
	require.Positive(t, beforeCounts["adj"], "prefix parity must include actual matching documents")
	beforeHits, err := f.store.SearchSymbols("adjacency", 100)
	require.NoError(t, err)
	require.NotEmpty(t, beforeHits, "ordered score parity must exercise actual matches")
	files["adjacency.go"] += "\n// contract AG first EOF save\n"
	path := filepath.Join(f.root, "adjacency.go")
	writeFile(t, path, files["adjacency.go"])
	_, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	prepared, ok := idx.takePreparedSnapshot(path)
	require.True(t, ok)
	defer prepared.release()
	idx.applyRepoPrefix(prepared.result.Nodes, prepared.result.Edges)
	require.True(t, unchangedRawEdgesAndSpans(prior, prepared.result.Nodes))
	prepared.release()
	result := rawEdgeProofSave(t, f, "adjacency.go", files["adjacency.go"])
	require.Equal(t, 1, result.DerivedInvalidation.MetadataOnlyFiles, "the first actual post-install save must use the new door")
	after, found, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", owner)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, after.Accepted)
	require.NotEqual(t, before.SourceFingerprint, after.SourceFingerprint)
	require.Equal(t, contractInputHash([]byte(files["adjacency.go"])), after.SourceFingerprint)
	require.True(t, idx.rawEdgesPriorCompleted(owner, f.store.GetFileNodes(owner)))
	afterStats, afterCounts, err := f.store.SymbolFTSScoringSnapshot(t.Context(), prefixes)
	require.NoError(t, err)
	require.Equal(t, beforeStats.Rows, afterStats.Rows)
	require.Equal(t, beforeStats.Tokens, afterStats.Tokens)
	require.Equal(t, beforeStats.Stamp, afterStats.Stamp)
	require.Equal(t, beforeCounts, afterCounts)
	afterHits, err := f.store.SearchSymbols("adjacency", 100)
	require.NoError(t, err)
	require.Len(t, afterHits, len(beforeHits))
	for i, hit := range beforeHits {
		require.Equal(t, hit.NodeID, afterHits[i].NodeID, "ordered symbol identity")
		require.Equal(t, math.Float64bits(hit.Score), math.Float64bits(afterHits[i].Score), "exact symbol score bits")
	}

	cold := newContractJobFixture(t, files)
	require.Equal(t, builderRenderNodes(cold.store.AllNodes()), builderRenderNodes(f.store.AllNodes()), "every persisted node field and metadata must match a completed cold index")
	require.Equal(t, builderRenderEdges(cold.store.AllEdges()), builderRenderEdges(f.store.AllEdges()), "every persisted resolved/derived edge and metadata must match")
	require.Equal(t, rawEdgeProofRepoFactRows(t, cold.store), rawEdgeProofRepoFactRows(t, f.store), "all repository reference facts must match")

	// Independent accepted source captures must also produce the same complete
	// attachment payload rather than merely the same contract identifiers.
	fastReport, err := f.job(nil, nil)
	require.NoError(t, err)
	require.True(t, fastReport.Published)
	require.Positive(t, fastReport.PayloadGeneration)
	coldReport, err := cold.job(nil, nil)
	require.NoError(t, err)
	require.True(t, coldReport.Published)
	require.Positive(t, coldReport.PayloadGeneration)
	fastPayload := f.store.AtGeneration(fastReport.PayloadGeneration)
	coldPayload := cold.store.AtGeneration(coldReport.PayloadGeneration)
	require.NotEmpty(t, f.contracts())
	require.Equal(t, builderRenderNodes(coldPayload.AllNodes()), builderRenderNodes(fastPayload.AllNodes()), "complete contract payload nodes")
	require.Equal(t, builderRenderEdges(coldPayload.AllEdges()), builderRenderEdges(fastPayload.AllEdges()), "complete contract payload edges")
	require.Equal(t, contractMeta(cold.contracts()), contractMeta(f.contracts()))
}
