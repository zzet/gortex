package indexer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// An accepted raw-edge-preserving refresh and the structural path must
// produce identical full state from the same completed source history.
func TestRawEdgeProofSQLiteEOFMatchesLegacy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "analysis", "adjacency.go"))
	require.NoError(t, err)
	files := map[string]string{"adjacency.go": string(raw), "routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	optimized, legacy := newContractJobFixture(t, files), newContractJobFixture(t, files)
	const owner = "fixture/adjacency.go"
	assertMatched := func(label string) {
		t.Helper()
		require.Equal(t, builderRenderNodes(legacy.store.AllNodes()), builderRenderNodes(optimized.store.AllNodes()), label+" all persisted node fields/Meta")
		require.Equal(t, builderRenderEdges(legacy.store.AllEdges()), builderRenderEdges(optimized.store.AllEdges()), label+" all persisted edge fields/Meta")
		require.Equal(t, rawEdgeProofRepoFactRows(t, legacy.store), rawEdgeProofRepoFactRows(t, optimized.store), label+" all repository RefFacts")
	}
	assertMatched("independent completed baselines")
	// Establish the SAME existing structural history on both arms. Removing
	// only the old proof marker changes no semantic/contract/resolution input.
	files["adjacency.go"] += "\n// contract AG warm\n"
	for _, f := range []*contractJobFixture{optimized, legacy} {
		rawEdgeProofRemoveMarker(f, owner)
		result := rawEdgeProofSave(t, f, "adjacency.go", files["adjacency.go"])
		require.Zero(t, result.DerivedInvalidation.MetadataOnlyFiles)
		require.True(t, f.mi.indexers["fixture"].rawEdgesPriorCompleted(owner, f.store.GetFileNodes(owner)))
	}
	assertMatched("identical completed structural warm")
	for round := 0; round < 2; round++ {
		files["adjacency.go"] += fmt.Sprintf("// contract AG tick %d\n", round)
		// This is the only arm difference. Do not force reparse/drop resolution,
		// disable the runtime, change source, or filter any oracle row.
		rawEdgeProofRemoveMarker(legacy, owner)
		before := make([]*graph.ContractBoundaryReceipt, 2)
		for i, f := range []*contractJobFixture{optimized, legacy} {
			row, found, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", owner)
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, row.Accepted)
			before[i] = row
		}
		fast := rawEdgeProofSave(t, optimized, "adjacency.go", files["adjacency.go"])
		old := rawEdgeProofSave(t, legacy, "adjacency.go", files["adjacency.go"])
		require.Equal(t, 1, fast.DerivedInvalidation.MetadataOnlyFiles, "actual new door must be used")
		require.Zero(t, old.DerivedInvalidation.MetadataOnlyFiles, "missing proof must use the actual legacy structural path")
		for i, f := range []*contractJobFixture{optimized, legacy} {
			row, found, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", owner)
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, row.Accepted)
			require.NotEqual(t, before[i].SourceFingerprint, row.SourceFingerprint)
			require.Equal(t, contractInputHash([]byte(files["adjacency.go"])), row.SourceFingerprint)
			require.True(t, f.mi.indexers["fixture"].rawEdgesPriorCompleted(owner, f.store.GetFileNodes(owner)))
		}
		assertMatched(fmt.Sprintf("matched EOF round %d", round))
	}
	// Independent actual captures/jobs must publish the same complete payload,
	// including bridge/owner edges and full node metadata, not merely IDs.
	reports := make([]ContractFollowupReport, 2)
	for i, f := range []*contractJobFixture{optimized, legacy} {
		report, err := f.job(nil, nil)
		require.NoError(t, err)
		require.True(t, report.Published)
		require.Positive(t, report.PayloadGeneration)
		reports[i] = report
		require.NotEmpty(t, f.contracts())
	}
	fastPayload := optimized.store.AtGeneration(reports[0].PayloadGeneration)
	oldPayload := legacy.store.AtGeneration(reports[1].PayloadGeneration)
	require.Equal(t, builderRenderNodes(oldPayload.AllNodes()), builderRenderNodes(fastPayload.AllNodes()), "complete independent contract payload nodes")
	require.Equal(t, builderRenderEdges(oldPayload.AllEdges()), builderRenderEdges(fastPayload.AllEdges()), "complete independent contract payload edges")
	require.Equal(t, contractMeta(legacy.contracts()), contractMeta(optimized.contracts()))
	assertMatched("core graph after independent payload publication")
}

func rawEdgeProofRemoveMarker(f *contractJobFixture, path string) {
	for _, node := range f.store.GetFileNodes(path) {
		if node.Kind == graph.KindFile {
			copied := *node
			copied.Meta = map[string]any{}
			for k, v := range node.Meta {
				if k != sourceRawEdgesFingerprintMeta {
					copied.Meta[k] = v
				}
			}
			f.store.AddNode(&copied)
		}
	}
}
func rawEdgeProofSave(t *testing.T, f *contractJobFixture, name, source string) *IndexResult {
	t.Helper()
	path := filepath.Join(f.root, name)
	writeFile(t, path, source)
	result, err := f.mi.IncrementalReindexRepo("fixture", []string{path})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	return result
}
func rawEdgeProofRepoFactRows(t *testing.T, reader graph.RefFactsReader) []string {
	t.Helper()
	facts, err := reader.LoadRefFactsByFiles("fixture", nil)
	require.NoError(t, err)
	rows := make([]string, 0, len(facts))
	for _, fact := range facts {
		data, err := json.Marshal(fact)
		require.NoError(t, err)
		rows = append(rows, string(data))
	}
	sort.Strings(rows)
	return rows
}
func rawEdgeProofFixture(t *testing.T, edges []*graph.Edge) []*graph.Node {
	t.Helper()
	nodes := metadataRegressionNodes()
	for _, n := range nodes {
		n.StartLine = 1
		n.EndLine = 2
		n.StartColumn = 1
		n.EndColumn = 3
	}
	result := &parser.ExtractionResult{Nodes: nodes, Edges: edges}
	fp, _, ok := extractionFingerprints(result)
	require.True(t, ok)
	stampExtractionGraphFingerprints(result, fp)
	return nodes
}

func TestRawEdgeProofIncludesCompleteOrderedEdgePayload(t *testing.T) {
	edge := func() *graph.Edge {
		return &graph.Edge{From: "sample.go::Caller", To: "sample.go::A", Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 2, Confidence: .8, Origin: "ast_inferred", Meta: map[string]any{"proof": "old"}}
	}
	proof := func(rows ...*graph.Edge) string {
		return storedExtractionGraphFingerprints(rawEdgeProofFixture(t, rows)).rawEdges
	}
	base := proof(edge())
	require.NotEmpty(t, base)
	for _, tc := range []struct {
		name   string
		change func(*graph.Edge)
	}{
		{"from", func(e *graph.Edge) { e.From = "other" }}, {"to", func(e *graph.Edge) { e.To = "other" }},
		{"kind", func(e *graph.Edge) { e.Kind = graph.EdgeReferences }}, {"file", func(e *graph.Edge) { e.FilePath = "other.go" }},
		{"line", func(e *graph.Edge) { e.Line++ }}, {"confidence", func(e *graph.Edge) { e.Confidence = .9 }},
		{"label", func(e *graph.Edge) { e.ConfidenceLabel = "high" }}, {"origin", func(e *graph.Edge) { e.Origin = "resolver" }},
		{"tier", func(e *graph.Edge) { e.Tier = "ast" }}, {"crossrepo", func(e *graph.Edge) { e.CrossRepo = true }},
		{"alias", func(e *graph.Edge) { e.Alias = "renamed" }}, {"metadata", func(e *graph.Edge) { e.Meta["proof"] = "new" }},
		{"context", func(e *graph.Edge) { e.Context = "call" }}, {"return", func(e *graph.Edge) { e.ReturnUsage = "returned" }},
		{"via", func(e *graph.Edge) { e.Via = "bridge" }}, {"nameonly", func(e *graph.Edge) { e.NameOnly = true }},
	} {
		t.Run(tc.name, func(t *testing.T) { e := edge(); tc.change(e); require.NotEqual(t, base, proof(e)) })
	}
	first, second := edge(), edge()
	second.Meta["proof"] = "conflicting same storage key"
	require.NotEqual(t, proof(first, second), proof(second, first), "SQLite insertion winners can depend on duplicate order")
	left, _, ok := extractionFingerprints(&parser.ExtractionResult{Nodes: metadataRegressionNodes(), Edges: []*graph.Edge{first, second}})
	require.True(t, ok)
	right, _, ok := extractionFingerprints(&parser.ExtractionResult{Nodes: metadataRegressionNodes(), Edges: []*graph.Edge{second, first}})
	require.True(t, ok)
	require.Equal(t, left.metadata, right.metadata, "established metadata remains order-canonical")
	require.Equal(t, left.semantic, right.semantic)
	require.NotEqual(t, left.rawEdges, right.rawEdges, "only the extra raw vector proof is order-sensitive")
	canonical := edge()
	canonical.Meta = map[string]any{"z": 2, "a": 1}
	reverseKeys := edge()
	reverseKeys.Meta = map[string]any{"a": 1, "z": 2}
	require.Equal(t, proof(canonical), proof(reverseKeys))
	unsupported := edge()
	unsupported.Meta["unsupported"] = make(chan int)
	_, _, ok = extractionFingerprints(&parser.ExtractionResult{Nodes: metadataRegressionNodes(), Edges: []*graph.Edge{unsupported}})
	require.False(t, ok, "unsupported metadata still refuses the complete fingerprint")
	require.NotEqual(t, base, proof(edge(), edge()), "duplicate multiplicity is part of the raw proof")
	require.NotEqual(t, base, proof(nil, edge()), "raw nil positions cannot silently reorder the vector")
	foreign := edge()
	foreign.FilePath = "foreign.go"
	require.Empty(t, proof(foreign), "foreign recording ownership retains the old refusal")
	pathlessPseudo := edge()
	pathlessPseudo.From = "unresolved::pseudo"
	pathlessPseudo.FilePath = ""
	require.Empty(t, proof(pathlessPseudo), "pathless pseudo ownership cannot bypass sibling protection")
	pathlessOwned := edge()
	pathlessOwned.FilePath = ""
	require.NotEmpty(t, proof(pathlessOwned), "actual source-node adjacency still proves pathless ownership")
	// Reserved stamps are excluded by the established metadata digest. Such
	// raw edge payloads cannot claim the stronger complete-raw-edge proof.
	same := edge()
	same.Meta[sourceRawEdgesFingerprintMeta] = "internal"
	require.Empty(t, proof(same))
}

func TestRawEdgeProofRefusesMovedSymbolsAndUnknownProof(t *testing.T) {
	makeNodes := func() []*graph.Node { return rawEdgeProofFixture(t, nil) }
	old, fresh := makeNodes(), makeNodes()
	require.True(t, unchangedRawEdgesAndSpans(old, fresh))
	fresh[0].EndLine++
	require.True(t, unchangedRawEdgesAndSpans(old, fresh), "EOF can extend only the file anchor")
	for _, tc := range []struct {
		name   string
		change func([]*graph.Node)
	}{
		{"file_start", func(n []*graph.Node) { n[0].StartLine++ }},
		{"symbol_start", func(n []*graph.Node) { n[1].StartLine++ }},
		{"symbol_end", func(n []*graph.Node) { n[1].EndLine++ }},
		{"symbol_start_column", func(n []*graph.Node) { n[1].StartColumn++ }},
		{"symbol_end_column", func(n []*graph.Node) { n[1].EndColumn++ }},
		{"missing", func(n []*graph.Node) { delete(n[0].Meta, sourceRawEdgesFingerprintMeta) }},
		{"unknown_version", func(n []*graph.Node) { n[0].Meta[sourceRawEdgesFingerprintMeta] = "v2:" + string(make([]byte, 64)) }},
		{"malformed", func(n []*graph.Node) { n[0].Meta[sourceRawEdgesFingerprintMeta] = "v1:" + string(make([]byte, 64)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := makeNodes(), makeNodes()
			tc.change(b)
			require.False(t, unchangedRawEdgesAndSpans(a, b))
		})
	}
	// A real derived relation can price its source's StartLine even with no
	// raw calls. The new door must refuse a span move rather than retain it.
	g := graph.New()
	g.AddBatch(old, []*graph.Edge{{From: old[1].ID, To: old[2].ID, Kind: graph.EdgeSimilarTo, FilePath: "sample.go", Line: old[1].StartLine, Meta: map[string]any{"via": "clone"}}})
	moved := makeNodes()
	moved[1].StartLine++
	require.False(t, unchangedRawEdgesAndSpans(g.GetFileNodes("sample.go"), moved))
}

func TestRawEdgeProofRefusesPseudoSourceStructuralSibling(t *testing.T) {
	nodes := rawEdgeProofFixture(t, nil)
	stage := &incrementalBatchStage{graphPath: "sample.go", priorNodes: nodes, result: &parser.ExtractionResult{Nodes: rawEdgeProofFixture(t, nil)}}
	incoming := map[string][]*graph.Edge{"sibling.go::Target": {{From: "unresolved::pseudo", To: "sibling.go::Target", Kind: graph.EdgeValueFlow, FilePath: "sample.go", Line: 8}}}
	evicted := map[string]struct{}{"sibling.go::Target": {}}
	require.True(t, metadataStageIndependent(stage, nil, evicted), "the original node-only guard cannot see this pseudo source")
	require.False(t, metadataStageIndependent(stage, nil, evicted, incoming))
	incoming["sibling.go::Target"][0].FilePath = "foreign.go"
	require.True(t, metadataStageIndependent(stage, nil, evicted, incoming), "foreign recording ownership does not implicate this source")
	outgoing := map[string][]*graph.Edge{"sibling.go::Target": {{From: "sibling.go::Target", To: "unresolved::pseudo", Kind: graph.EdgeValueFlow, FilePath: "sample.go", Line: 8}}}
	require.False(t, metadataStageIndependent(stage, outgoing, evicted), "sibling eviction also deletes source-recorded rows FROM the sibling")
	outgoing["sibling.go::Target"][0].FilePath = "foreign.go"
	require.True(t, metadataStageIndependent(stage, outgoing, evicted))
}

func TestRawEdgeProofKeepsResolvedAndDerivedRowsUntouched(t *testing.T) {
	raw := []*graph.Edge{{From: "unresolved::pseudo", To: "unresolved::target", Kind: graph.EdgeValueFlow, FilePath: "sample.go", Line: 9}}
	prior := rawEdgeProofFixture(t, raw)
	fresh := rawEdgeProofFixture(t, raw)
	fresh[0].EndLine++
	resolved := []*graph.Edge{
		{From: "unresolved::pseudo", To: "sample.go::A", Kind: graph.EdgeValueFlow, FilePath: "sample.go", Line: 9, Origin: "resolver", Confidence: .9, Meta: map[string]any{"proof": "resolved"}},
		{From: "sample.go::Caller", To: "sample.go::Z", Kind: graph.EdgeSimilarTo, FilePath: "sample.go", Line: 1, Meta: map[string]any{"similarity": .8}},
	}
	g := graph.New()
	g.AddBatch(prior, resolved)
	before := g.AllEdges()
	stage := &incrementalBatchStage{graphPath: "sample.go", priorNodes: prior, priorRawEdgesCompleted: true, result: &parser.ExtractionResult{Nodes: fresh, Edges: raw}}
	require.True(t, prepareMetadataRefreshFromView(stage, g.GetOutEdgesByNodeIDs([]string{prior[1].ID}), nil), "proof handles real pseudo-source extraction without rebuilding resolution")
	require.Empty(t, stage.edgeRefreshes)
	idx := &Indexer{graph: g}
	idx.commitMetadataIncrementalBatch([]*incrementalBatchStage{stage})
	require.Equal(t, before, g.AllEdges(), "existing resolved/derived rows and all metadata remain byte-equivalent")
	require.Equal(t, fresh[0].EndLine, g.GetNode("sample.go").EndLine)
	// Without accepted prior proof this pseudo-source cannot silently use the
	// new door; the old conservative pairing contract remains authoritative.
	delete(prior[0].Meta, sourceRawEdgesFingerprintMeta)
	stage = &incrementalBatchStage{graphPath: "sample.go", priorNodes: prior, priorRawEdgesCompleted: true, result: &parser.ExtractionResult{Nodes: rawEdgeProofFixture(t, raw), Edges: raw}}
	require.False(t, prepareMetadataRefreshFromView(stage, nil, nil))
	// An accepted old proof must never substitute for a missing fresh proof
	// when preparation merges previously stored node metadata.
	prior = rawEdgeProofFixture(t, raw)
	missingFresh := rawEdgeProofFixture(t, raw)
	delete(missingFresh[0].Meta, sourceRawEdgesFingerprintMeta)
	stage = &incrementalBatchStage{graphPath: "sample.go", priorNodes: prior, priorRawEdgesCompleted: true, result: &parser.ExtractionResult{Nodes: missingFresh, Edges: raw}}
	require.False(t, prepareMetadataRefreshFromView(stage, nil, nil))
	require.Empty(t, storedExtractionGraphFingerprints(stage.result.Nodes).rawEdges)
}

func TestRawEdgeProofRefusesInterruptedRawPrefix(t *testing.T) {
	const owner = "fixture/routes.go"
	f := newRouteConstantFixture(t)
	idx := f.mi.indexers["fixture"]
	prior := f.store.GetFileNodes(owner)
	require.True(t, idx.rawEdgesPriorCompleted(owner, prior), "completed installed-runtime source supplies authority")
	bad := append([]*graph.Node(nil), prior...)
	for i, n := range bad {
		if n.Kind == graph.KindFile {
			copyNode := *n
			copyNode.Meta = map[string]any{}
			for k, v := range n.Meta {
				copyNode.Meta[k] = v
			}
			var input contractDependencyInputs
			require.NoError(t, json.Unmarshal([]byte(copyNode.Meta[contractDependencyInputMeta].(string)), &input))
			input.Source = "different"
			encoded, err := json.Marshal(input)
			require.NoError(t, err)
			copyNode.Meta[contractDependencyInputMeta] = string(encoded)
			bad[i] = &copyNode
		}
	}
	require.False(t, idx.rawEdgesPriorCompleted(owner, bad))
	// A raw marker can be durable while its apply has NOT completed. Use the
	// actual MI output/journal owner and stop after the real raw write prefix,
	// before resolution/derived catch-up or the outer acceptance fence.
	mutation := newContractLaneMutation(t, f, "multi_indexer")
	src := jobRoutes + "\n// interrupted raw prefix\n"
	path := filepath.Join(f.root, "routes.go")
	writeFile(t, path, src)
	err := mutation.run(t.Context(), func(func(*OutputSourceContent)) error {
		probe, ok := idx.prepareFileDelta(path)
		require.True(t, ok)
		require.NotEmpty(t, probe.fingerprints.rawEdges)
		prepared, ok := idx.takePreparedSnapshot(path)
		require.True(t, ok)
		defer prepared.release()
		require.NoError(t, idx.contractCoreInputs.prepare(t.Context(), idx, owner, "go", prepared.src, prepared.result, false))
		require.NoError(t, idx.contractCoreInputs.begin([]string{owner}))
		idx.applyRepoPrefix(prepared.result.Nodes, prepared.result.Edges)
		f.store.EvictFile(owner)
		f.store.AddBatch(prepared.result.Nodes, prepared.result.Edges)
		return errors.New("interrupted after raw edge prefix")
	})
	require.ErrorContains(t, err, "interrupted after raw edge prefix")
	incomplete := f.store.GetFileNodes(owner)
	require.NotEmpty(t, storedExtractionGraphFingerprints(incomplete).rawEdges)
	row, found, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", owner)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)
	require.False(t, row.Accepted)
	require.NotNil(t, row.Previous)
	require.True(t, row.Previous.Accepted)
	require.False(t, idx.rawEdgesPriorCompleted(owner, incomplete), "accepted predecessor cannot certify the interrupted current prefix")
	// Another equal-edge EOF edit still has raw equality; that is intentionally
	// insufficient authority for retaining the incomplete resolver/derived graph.
	writeFile(t, path, src+"// next EOF edit\n")
	_, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	prepared, ok := idx.takePreparedSnapshot(path)
	require.True(t, ok)
	defer prepared.release()
	idx.applyRepoPrefix(prepared.result.Nodes, prepared.result.Edges)
	require.True(t, unchangedRawEdgesAndSpans(incomplete, prepared.result.Nodes))
	require.False(t, idx.rawEdgesPriorCompleted(owner, incomplete))
	// Unsupported readers retain the old structural/pairing path rather
	// than minting completion.
	require.False(t, (&Indexer{graph: graph.New()}).rawEdgesPriorCompleted(owner, incomplete))

}
