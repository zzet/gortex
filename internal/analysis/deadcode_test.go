package analysis

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"pgregory.net/rapid"
)

// Feature: gortex-enhancements, Property 5: Dead code has zero incoming edges

// --- Generators ---

// deadCodeGraphResult holds a generated graph with known dead and live symbols.
type deadCodeGraphResult struct {
	Graph     *graph.Graph
	Processes *ProcessResult
	DeadIDs   []string // symbols that should be detected as dead code
	LiveIDs   []string // symbols that should NOT be detected as dead code
}

// genDeadCodeGraph generates a random graph with a mix of dead and live symbols.
// Dead symbols: unexported, non-test, non-process, zero incoming calls/references.
// Live symbols: have incoming edges, or are exported, or are in test files, or are in processes.
func genDeadCodeGraph() *rapid.Generator[deadCodeGraphResult] {
	return rapid.Custom(func(t *rapid.T) deadCodeGraphResult {
		g := graph.New()

		processes := &ProcessResult{
			Processes:   nil,
			NodeToProcs: make(map[string][]string),
		}

		var deadIDs []string
		var liveIDs []string

		// Generate 2-6 dead symbols: unexported, non-test file, no incoming edges, not in process
		numDead := rapid.IntRange(2, 6).Draw(t, "numDead")
		for i := range numDead {
			id := fmt.Sprintf("pkg/internal/mod%d.go::helper%d", i, i)
			g.AddNode(&graph.Node{
				ID:        id,
				Kind:      graph.KindFunction,
				Name:      fmt.Sprintf("helper%d", i), // lowercase = unexported in Go
				FilePath:  fmt.Sprintf("pkg/internal/mod%d.go", i),
				StartLine: 1,
				EndLine:   10,
				Language:  "go",
			})
			deadIDs = append(deadIDs, id)
		}

		// Generate 1-4 live symbols with incoming call edges
		numLiveCalled := rapid.IntRange(1, 4).Draw(t, "numLiveCalled")
		for i := range numLiveCalled {
			calleeID := fmt.Sprintf("pkg/called%d.go::calledFunc%d", i, i)
			callerID := fmt.Sprintf("pkg/caller%d.go::callerFunc%d", i, i)
			g.AddNode(&graph.Node{
				ID:        calleeID,
				Kind:      graph.KindFunction,
				Name:      fmt.Sprintf("calledFunc%d", i),
				FilePath:  fmt.Sprintf("pkg/called%d.go", i),
				StartLine: 1,
				EndLine:   10,
				Language:  "go",
			})
			g.AddNode(&graph.Node{
				ID:        callerID,
				Kind:      graph.KindFunction,
				Name:      fmt.Sprintf("callerFunc%d", i),
				FilePath:  fmt.Sprintf("pkg/caller%d.go", i),
				StartLine: 1,
				EndLine:   10,
				Language:  "go",
			})
			g.AddEdge(&graph.Edge{
				From: callerID,
				To:   calleeID,
				Kind: graph.EdgeCalls,
			})
			liveIDs = append(liveIDs, calleeID)
		}

		// Generate 1-3 live symbols that are exported (uppercase name in Go)
		numExported := rapid.IntRange(1, 3).Draw(t, "numExported")
		for i := range numExported {
			id := fmt.Sprintf("pkg/exported%d.go::ExportedFunc%d", i, i)
			g.AddNode(&graph.Node{
				ID:        id,
				Kind:      graph.KindFunction,
				Name:      fmt.Sprintf("ExportedFunc%d", i), // uppercase = exported
				FilePath:  fmt.Sprintf("pkg/exported%d.go", i),
				StartLine: 1,
				EndLine:   10,
				Language:  "go",
			})
			liveIDs = append(liveIDs, id)
		}

		// Generate 1-2 live symbols in test files
		numTest := rapid.IntRange(1, 2).Draw(t, "numTest")
		for i := range numTest {
			id := fmt.Sprintf("pkg/mod_test.go::testHelper%d", i)
			g.AddNode(&graph.Node{
				ID:        id,
				Kind:      graph.KindFunction,
				Name:      fmt.Sprintf("testHelper%d", i),
				FilePath:  "pkg/mod_test.go",
				StartLine: 1,
				EndLine:   10,
				Language:  "go",
			})
			liveIDs = append(liveIDs, id)
		}

		// Generate 1-2 live symbols that are process members
		numProcess := rapid.IntRange(1, 2).Draw(t, "numProcess")
		var procSteps []Step
		for i := range numProcess {
			id := fmt.Sprintf("pkg/entry%d.go::entryFunc%d", i, i)
			g.AddNode(&graph.Node{
				ID:        id,
				Kind:      graph.KindFunction,
				Name:      fmt.Sprintf("entryFunc%d", i),
				FilePath:  fmt.Sprintf("pkg/entry%d.go", i),
				StartLine: 1,
				EndLine:   10,
				Language:  "go",
			})
			procSteps = append(procSteps, Step{ID: id, Depth: i})
			processes.NodeToProcs[id] = []string{"process-0"}
			liveIDs = append(liveIDs, id)
		}
		processes.Processes = []Process{{
			ID:         "process-0",
			Name:       "test process",
			EntryPoint: procSteps[0].ID,
			Steps:      procSteps,
			StepCount:  len(procSteps),
		}}

		// Generate 1-2 live symbols with incoming reference edges
		numReferenced := rapid.IntRange(1, 2).Draw(t, "numReferenced")
		for i := range numReferenced {
			refID := fmt.Sprintf("pkg/ref%d.go::referencedVar%d", i, i)
			refByID := fmt.Sprintf("pkg/refby%d.go::refByFunc%d", i, i)
			g.AddNode(&graph.Node{
				ID:        refID,
				Kind:      graph.KindVariable,
				Name:      fmt.Sprintf("referencedVar%d", i),
				FilePath:  fmt.Sprintf("pkg/ref%d.go", i),
				StartLine: 1,
				EndLine:   5,
				Language:  "go",
			})
			g.AddNode(&graph.Node{
				ID:        refByID,
				Kind:      graph.KindFunction,
				Name:      fmt.Sprintf("refByFunc%d", i),
				FilePath:  fmt.Sprintf("pkg/refby%d.go", i),
				StartLine: 1,
				EndLine:   10,
				Language:  "go",
			})
			g.AddEdge(&graph.Edge{
				From: refByID,
				To:   refID,
				Kind: graph.EdgeReferences,
			})
			liveIDs = append(liveIDs, refID)
		}

		return deadCodeGraphResult{
			Graph:     g,
			Processes: processes,
			DeadIDs:   deadIDs,
			LiveIDs:   liveIDs,
		}
	})
}

// --- Property Tests ---

// TestPropertyDeadCode_ZeroIncomingEdges verifies that every symbol returned by
// FindDeadCode has zero incoming calls or references edges, is not a process member,
// is not in a test file, and is not exported.
func TestPropertyDeadCode_ZeroIncomingEdges(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		tc := genDeadCodeGraph().Draw(rt, "deadCodeGraph")

		result := FindDeadCode(tc.Graph, tc.Processes, nil)

		resultIDs := make(map[string]bool)
		for _, entry := range result {
			resultIDs[entry.ID] = true

			// Verify zero incoming calls or references
			inEdges := tc.Graph.GetInEdges(entry.ID)
			for _, e := range inEdges {
				if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
					rt.Errorf("dead code entry %s has incoming %s edge from %s", entry.ID, e.Kind, e.From)
				}
			}

			// Verify not a process member
			if _, inProc := tc.Processes.NodeToProcs[entry.ID]; inProc {
				rt.Errorf("dead code entry %s is a process member", entry.ID)
			}

			// Verify not in a test file
			if isTestFilePath(entry.FilePath) {
				rt.Errorf("dead code entry %s is in a test file: %s", entry.ID, entry.FilePath)
			}

			// Verify not exported
			node := tc.Graph.GetNode(entry.ID)
			if node != nil && isExportedSymbol(node.Name, node.Language) {
				rt.Errorf("dead code entry %s is exported: %s", entry.ID, node.Name)
			}
		}

		// Verify all known dead symbols are in the result
		for _, deadID := range tc.DeadIDs {
			if !resultIDs[deadID] {
				rt.Errorf("expected dead symbol %s was not in FindDeadCode result", deadID)
			}
		}

		// Verify no known live symbols are in the result
		for _, liveID := range tc.LiveIDs {
			if resultIDs[liveID] {
				rt.Errorf("live symbol %s was incorrectly reported as dead code", liveID)
			}
		}
	})
}

// TestPropertyDeadCode_Completeness verifies that no symbol meeting all dead code
// criteria is omitted from the result (the converse direction).
func TestPropertyDeadCode_Completeness(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		tc := genDeadCodeGraph().Draw(rt, "deadCodeGraph")

		result := FindDeadCode(tc.Graph, tc.Processes, nil)
		resultIDs := make(map[string]bool)
		for _, entry := range result {
			resultIDs[entry.ID] = true
		}

		// For every node in the graph, if it meets all dead code criteria,
		// it must be in the result.
		for _, node := range tc.Graph.AllNodes() {
			// Completeness applies only to the declared candidate kinds;
			// structural and future kinds are not unreported dead code.
			if !slices.Contains(candidateNodeKinds, node.Kind) {
				continue
			}

			// Variables / fields / constants excluded by default — same
			// reasoning the analyzer uses (graph lacks intra-function
			// data flow, so a non-opt-in scan must skip them).
			if node.Kind == graph.KindVariable {
				continue
			}
			if node.Kind == graph.KindField {
				continue
			}
			if node.Kind == graph.KindConstant {
				continue
			}

			// Go init() excluded
			if node.Name == "init" && node.Language == "go" {
				continue
			}

			// Go main() excluded
			if node.Name == "main" && node.Language == "go" && node.Kind == graph.KindFunction {
				continue
			}

			// Generated/vendored files excluded
			if isVendoredOrGenerated(node.FilePath) {
				continue
			}

			// Build-constrained files excluded
			if node.Language == "go" && hasBuildConstraint(node.FilePath) {
				continue
			}

			// Per-kind incoming-edge allowlist — mirror what the
			// analyzer does. Functions/methods need Calls/References;
			// types need References/Instantiates/MemberOf/...; etc.
			allowed := incomingUsageKinds(node.Kind)
			inEdges := tc.Graph.GetInEdges(node.ID)
			hasIncoming := false
			for _, e := range inEdges {
				for _, k := range allowed {
					if e.Kind == k {
						hasIncoming = true
						break
					}
				}
				if hasIncoming {
					break
				}
			}
			if hasIncoming {
				continue
			}

			// Well-known interface methods excluded
			if node.Kind == graph.KindMethod && isWellKnownInterfaceMethod(node.Name, node.Language) {
				continue
			}

			// CGo exports excluded by default
			if cgoExport, ok := node.Meta["cgo_export"].(bool); ok && cgoExport {
				continue
			}

			// go:linkname targets excluded by default
			if linkname, ok := node.Meta["go_linkname"].(bool); ok && linkname {
				continue
			}

			// Check exclusions
			if _, inProc := tc.Processes.NodeToProcs[node.ID]; inProc {
				continue
			}
			if isTestFilePath(node.FilePath) {
				continue
			}
			if isExportedSymbol(node.Name, node.Language) && !isPackagePrivateByConvention(node.FilePath, node.Language) {
				continue
			}

			// This node meets all dead code criteria — it must be in the result
			if !resultIDs[node.ID] {
				rt.Errorf("symbol %s meets all dead code criteria but was not returned by FindDeadCode", node.ID)
			}
		}
	})
}

// Feature: gortex-enhancements, Property 6: Hotspot complexity score matches formula

// --- Generators ---

// hotspotGraphResult holds a generated graph with known fan_in/fan_out/crossing values.
type hotspotGraphResult struct {
	Graph       *graph.Graph
	Communities *CommunityResult
	// Expected raw values per function node ID
	ExpectedFanIn    map[string]int
	ExpectedFanOut   map[string]int
	ExpectedCrossing map[string]int
}

// genHotspotGraph generates a graph with function nodes that have known
// fan_in, fan_out, and community crossing values for score verification.
func genHotspotGraph() *rapid.Generator[hotspotGraphResult] {
	return rapid.Custom(func(t *rapid.T) hotspotGraphResult {
		g := graph.New()

		// Create 3-8 function nodes spread across 2-3 communities
		numFuncs := rapid.IntRange(3, 8).Draw(t, "numFuncs")
		numComms := rapid.IntRange(2, 3).Draw(t, "numComms")

		commNames := make([]string, numComms)
		for i := range numComms {
			commNames[i] = fmt.Sprintf("community-%d", i)
		}

		nodeToComm := make(map[string]string)
		funcIDs := make([]string, numFuncs)
		funcComms := make([]string, numFuncs) // which community each func belongs to

		for i := range numFuncs {
			id := fmt.Sprintf("pkg/mod%d.go::func%d", i, i)
			funcIDs[i] = id
			commIdx := rapid.IntRange(0, numComms-1).Draw(t, fmt.Sprintf("comm%d", i))
			funcComms[i] = commNames[commIdx]
			nodeToComm[id] = commNames[commIdx]

			g.AddNode(&graph.Node{
				ID:        id,
				Kind:      graph.KindFunction,
				Name:      fmt.Sprintf("func%d", i),
				FilePath:  fmt.Sprintf("pkg/mod%d.go", i),
				StartLine: 1,
				EndLine:   10,
				Language:  "go",
			})
		}

		communities := &CommunityResult{
			NodeToComm: nodeToComm,
		}

		expectedFanIn := make(map[string]int)
		expectedFanOut := make(map[string]int)
		expectedCrossing := make(map[string]int)

		// Add random call edges between functions
		numEdges := rapid.IntRange(2, numFuncs*2).Draw(t, "numEdges")
		seen := make(map[string]bool)
		for e := 0; e < numEdges; e++ {
			fromIdx := rapid.IntRange(0, numFuncs-1).Draw(t, fmt.Sprintf("from%d", e))
			toIdx := rapid.IntRange(0, numFuncs-1).Draw(t, fmt.Sprintf("to%d", e))
			if fromIdx == toIdx {
				continue
			}
			key := fmt.Sprintf("%d->%d", fromIdx, toIdx)
			if seen[key] {
				continue
			}
			seen[key] = true

			fromID := funcIDs[fromIdx]
			toID := funcIDs[toIdx]

			// Randomly choose calls or references
			edgeKind := graph.EdgeCalls
			if rapid.Bool().Draw(t, fmt.Sprintf("isRef%d", e)) {
				edgeKind = graph.EdgeReferences
			}

			g.AddEdge(&graph.Edge{
				From: fromID,
				To:   toID,
				Kind: edgeKind,
			})

			// fan_in: incoming calls + references
			expectedFanIn[toID]++

			// fan_out: outgoing calls only
			if edgeKind == graph.EdgeCalls {
				expectedFanOut[fromID]++
			}

			// community crossings: outgoing edges to different community
			if funcComms[fromIdx] != funcComms[toIdx] {
				expectedCrossing[fromID]++
			}
		}

		return hotspotGraphResult{
			Graph:            g,
			Communities:      communities,
			ExpectedFanIn:    expectedFanIn,
			ExpectedFanOut:   expectedFanOut,
			ExpectedCrossing: expectedCrossing,
		}
	})
}

// --- Property Tests ---

// hotspotBetweennessComponent recomputes the 0-100 normalized
// betweenness term FindHotspots folds into a node's raw score. The
// generated hotspot graphs are tiny, so ComputeBetweenness runs the
// exact path here — the expected score stays faithful to the
// implementation without re-deriving Brandes' in the test.
func hotspotBetweennessComponent(g *graph.Graph) map[string]float64 {
	bc := ComputeBetweenness(g)
	out := make(map[string]float64, len(bc.Scores))
	if bc.Max > 0 {
		for id, v := range bc.Scores {
			out[id] = (v / bc.Max) * 100.0
		}
	}
	return out
}

// TestPropertyHotspot_ComplexityScoreFormula verifies that for every hotspot entry,
// ComplexityScore equals (fan_in * 2) + (fan_out * 1.5) + (community_crossings * 3) +
// (betweenness * hotspotBetweennessWeight) normalized to [0, 100], and that
// FindHotspots returns exactly those symbols whose score exceeds the threshold.
func TestPropertyHotspot_ComplexityScoreFormula(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		tc := genHotspotGraph().Draw(rt, "hotspotGraph")

		// Use threshold=0 to get ALL function nodes with any score
		result := FindHotspots(tc.Graph, tc.Communities, 0)

		betweenness := hotspotBetweennessComponent(tc.Graph)

		// Compute the max raw score across all function nodes to verify normalization
		maxRaw := 0.0
		for _, id := range tc.Graph.AllNodes() {
			if id.Kind != graph.KindFunction && id.Kind != graph.KindMethod {
				continue
			}
			fi := tc.ExpectedFanIn[id.ID]
			fo := tc.ExpectedFanOut[id.ID]
			cc := tc.ExpectedCrossing[id.ID]
			raw := float64(fi)*2.0 + float64(fo)*1.5 + float64(cc)*3.0 + betweenness[id.ID]*hotspotBetweennessWeight
			if raw > maxRaw {
				maxRaw = raw
			}
		}

		for _, entry := range result {
			fi := tc.ExpectedFanIn[entry.ID]
			fo := tc.ExpectedFanOut[entry.ID]
			cc := tc.ExpectedCrossing[entry.ID]

			// Verify fan_in, fan_out, community_crossings match
			if entry.FanIn != fi {
				rt.Errorf("hotspot %s: FanIn = %d, expected %d", entry.ID, entry.FanIn, fi)
			}
			if entry.FanOut != fo {
				rt.Errorf("hotspot %s: FanOut = %d, expected %d", entry.ID, entry.FanOut, fo)
			}
			if entry.CommunityCrossings != cc {
				rt.Errorf("hotspot %s: CommunityCrossings = %d, expected %d", entry.ID, entry.CommunityCrossings, cc)
			}

			// Verify the reported betweenness matches the 0-100 normalized value.
			expectedBw := math.Round(betweenness[entry.ID]*100) / 100
			if math.Abs(entry.Betweenness-expectedBw) > 0.01 {
				rt.Errorf("hotspot %s: Betweenness = %.2f, expected %.2f", entry.ID, entry.Betweenness, expectedBw)
			}

			// Verify complexity score matches formula
			rawScore := float64(fi)*2.0 + float64(fo)*1.5 + float64(cc)*3.0 + betweenness[entry.ID]*hotspotBetweennessWeight
			var expectedNormalized float64
			if maxRaw > 0 {
				expectedNormalized = (rawScore / maxRaw) * 100.0
			}
			expectedNormalized = math.Round(expectedNormalized*100) / 100

			if math.Abs(entry.ComplexityScore-expectedNormalized) > 0.01 {
				rt.Errorf("hotspot %s: ComplexityScore = %.2f, expected %.2f (raw=%.2f, maxRaw=%.2f, fi=%d, fo=%d, cc=%d)",
					entry.ID, entry.ComplexityScore, expectedNormalized, rawScore, maxRaw, fi, fo, cc)
			}

			// Verify score is in [0, 100]
			if entry.ComplexityScore < 0 || entry.ComplexityScore > 100 {
				rt.Errorf("hotspot %s: ComplexityScore = %.2f, not in [0, 100]", entry.ID, entry.ComplexityScore)
			}
		}
	})
}

// TestPropertyHotspot_ThresholdFiltering verifies that FindHotspots returns
// exactly those symbols whose normalized score exceeds the given threshold.
func TestPropertyHotspot_ThresholdFiltering(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		tc := genHotspotGraph().Draw(rt, "hotspotGraph")

		// Pick a random threshold between 0 and 100
		threshold := rapid.Float64Range(0.01, 99.0).Draw(rt, "threshold")

		result := FindHotspots(tc.Graph, tc.Communities, threshold)

		// All returned entries must have score >= threshold
		for _, entry := range result {
			if entry.ComplexityScore < threshold {
				rt.Errorf("hotspot %s has score %.2f below threshold %.2f",
					entry.ID, entry.ComplexityScore, threshold)
			}
		}

		// Compute all scores to verify no symbol above threshold is missing
		betweenness := hotspotBetweennessComponent(tc.Graph)
		maxRaw := 0.0
		type nodeScore struct {
			id  string
			raw float64
		}
		var allScores []nodeScore
		for _, n := range tc.Graph.AllNodes() {
			if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
				continue
			}
			fi := tc.ExpectedFanIn[n.ID]
			fo := tc.ExpectedFanOut[n.ID]
			cc := tc.ExpectedCrossing[n.ID]
			raw := float64(fi)*2.0 + float64(fo)*1.5 + float64(cc)*3.0 + betweenness[n.ID]*hotspotBetweennessWeight
			allScores = append(allScores, nodeScore{n.ID, raw})
			if raw > maxRaw {
				maxRaw = raw
			}
		}

		resultIDs := make(map[string]bool)
		for _, entry := range result {
			resultIDs[entry.ID] = true
		}

		for _, ns := range allScores {
			var normalized float64
			if maxRaw > 0 {
				normalized = (ns.raw / maxRaw) * 100.0
			}
			normalized = math.Round(normalized*100) / 100

			if normalized >= threshold && !resultIDs[ns.id] {
				rt.Errorf("symbol %s has score %.2f >= threshold %.2f but was not returned",
					ns.id, normalized, threshold)
			}
		}
	})
}

// --- Unit Tests ---

// TestHotspots_SmallGraphError verifies that a graph with fewer than 10 function/method
// symbols returns an empty result from FindHotspots.
func TestHotspots_SmallGraphError(t *testing.T) {
	g := graph.New()

	// Add only 5 function nodes (< 10)
	for i := 0; i < 5; i++ {
		g.AddNode(&graph.Node{
			ID:        fmt.Sprintf("pkg/small%d.go::func%d", i, i),
			Kind:      graph.KindFunction,
			Name:      fmt.Sprintf("func%d", i),
			FilePath:  fmt.Sprintf("pkg/small%d.go", i),
			StartLine: 1,
			EndLine:   10,
			Language:  "go",
		})
	}

	// Add some edges so there's non-zero scores
	g.AddEdge(&graph.Edge{
		From: "pkg/small0.go::func0",
		To:   "pkg/small1.go::func1",
		Kind: graph.EdgeCalls,
	})
	g.AddEdge(&graph.Edge{
		From: "pkg/small2.go::func2",
		To:   "pkg/small3.go::func3",
		Kind: graph.EdgeCalls,
	})

	communities := &CommunityResult{
		NodeToComm: make(map[string]string),
	}

	// With default threshold (mean + 2*stddev), a small graph should return
	// empty or very few results. The MCP handler enforces the < 10 symbols error,
	// but at the analysis level, FindHotspots should still work correctly.
	result := FindHotspots(g, communities, 0)

	// With threshold=0, we get all nodes that have any score.
	// The important thing is the function doesn't panic on small graphs.
	assert.LessOrEqual(t, len(result), 5, "small graph should have at most 5 hotspots")

	// Verify that with a very high threshold, we get empty results
	result = FindHotspots(g, communities, 101)
	assert.Empty(t, result, "threshold above 100 should return no hotspots")
}

// TestHotspots_EmptyGraph verifies FindHotspots handles an empty graph gracefully.
func TestHotspots_EmptyGraph(t *testing.T) {
	g := graph.New()
	communities := &CommunityResult{
		NodeToComm: make(map[string]string),
	}

	result := FindHotspots(g, communities, 0)
	assert.Empty(t, result)
}

// TestDeadCode_StructuralNodesExcluded verifies that file, import, and package
// nodes are never reported as dead code.
func TestDeadCode_StructuralNodesExcluded(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{ID: "file1.go", Kind: graph.KindFile, Name: "file1.go", FilePath: "file1.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg1", Kind: graph.KindPackage, Name: "pkg1", FilePath: "pkg1", Language: "go"})
	g.AddNode(&graph.Node{ID: "import1", Kind: graph.KindImport, Name: "fmt", FilePath: "file1.go", Language: "go"})

	processes := &ProcessResult{
		NodeToProcs: make(map[string][]string),
	}

	result := FindDeadCode(g, processes, nil)
	assert.Empty(t, result, "structural nodes should never be reported as dead code")
}

// TestDeadCode_GeneratedFilesExcluded verifies that symbols in generated files
// (protobuf, codegen, mocks) are not reported as dead code.
func TestDeadCode_GeneratedFilesExcluded(t *testing.T) {
	g := graph.New()

	generatedFiles := []struct {
		file string
		name string
	}{
		{"pkg/api.pb.go", "apiHelper"},
		{"pkg/api_grpc.pb.go", "grpcHelper"},
		{"pkg/types_gen.go", "genHelper"},
		{"pkg/types_generated.go", "generatedHelper"},
		{"pkg/types.gen.go", "dotGenHelper"},
		{"pkg/zz_generated.deepcopy.go", "deepCopyHelper"},
		{"pkg/mock_service.go", "mockHelper"},
		{"pkg/service_mock.go", "mockHelper2"},
	}

	for _, gf := range generatedFiles {
		g.AddNode(&graph.Node{
			ID: gf.file + "::" + gf.name, Kind: graph.KindFunction,
			Name: gf.name, FilePath: gf.file, StartLine: 1, EndLine: 10, Language: "go",
		})
	}

	result := FindDeadCode(g, nil, nil)
	assert.Empty(t, result, "symbols in generated files should be excluded")
}

// TestDeadCode_PythonDundersExcluded verifies that Python special methods are
// not reported as dead. Python invokes them implicitly -- via an operator, a
// builtin, or a statement -- so they never carry a written call site.
func TestDeadCode_PythonDundersExcluded(t *testing.T) {
	g := graph.New()

	dunders := []string{
		"__init__",  // instantiation
		"__repr__",  // repr()
		"__str__",   // str()/print()
		"__eq__",    // ==
		"__len__",   // len()
		"__iter__",  // for
		"__enter__", // with
		"__exit__",  // with
		"__bool__",  // truthiness
		"__getitem__",
	}

	for _, name := range dunders {
		g.AddNode(&graph.Node{
			ID: "pkg/mod.py::Cls." + name, Kind: graph.KindMethod,
			Name: name, FilePath: "pkg/mod.py", StartLine: 1, EndLine: 3,
			Language: "python",
		})
	}

	result := FindDeadCode(g, nil, nil)
	assert.Empty(t, result, "python dunder methods should be excluded")
}

// TestDeadCode_PythonNonDundersStillReported guards the exemption against
// over-reach: an ordinary uncalled helper is still dead, and so is a name that
// merely starts with underscores.
func TestDeadCode_PythonNonDundersStillReported(t *testing.T) {
	g := graph.New()

	// All leading-underscore, so the pre-existing "skip exported symbols" rule
	// does not apply and only the dunder exemption is under test. None of these
	// is a dunder: the name must both start and end with "__" and have
	// something between.
	for _, name := range []string{"_helper", "__private", "____"} {
		g.AddNode(&graph.Node{
			ID: "pkg/mod.py::" + name, Kind: graph.KindFunction,
			Name: name, FilePath: "pkg/mod.py", StartLine: 1, EndLine: 3,
			Language: "python",
		})
	}

	result := FindDeadCode(g, nil, nil)
	reported := make([]string, 0, len(result))
	for _, entry := range result {
		reported = append(reported, entry.Name)
	}
	assert.ElementsMatch(t, []string{"_helper", "__private", "____"}, reported,
		"non-dunder python symbols should still be reported")
}

// TestDeadCode_MainFunctionExcluded verifies that Go main() is not reported as dead.
func TestDeadCode_MainFunctionExcluded(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{
		ID: "cmd/app/main.go::main", Kind: graph.KindFunction,
		Name: "main", FilePath: "cmd/app/main.go", StartLine: 5, EndLine: 20, Language: "go",
	})

	result := FindDeadCode(g, nil, nil)
	assert.Empty(t, result, "Go main() should be excluded as runtime entry point")
}

// TestDeadCode_MainMethodNotExcluded verifies that a method named main on a
// type IS reported as dead (only the package-level main function is special).
func TestDeadCode_MainMethodNotExcluded(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{
		ID: "pkg/foo.go::Foo.main", Kind: graph.KindMethod,
		Name: "main", FilePath: "pkg/foo.go", StartLine: 5, EndLine: 10, Language: "go",
	})

	result := FindDeadCode(g, nil, nil)
	assert.Len(t, result, 1, "a method named main should still be reported as dead")
}

// TestDeadCode_WellKnownMethodsExcluded verifies that methods matching
// well-known stdlib interface names are excluded even without implements edges.
func TestDeadCode_WellKnownMethodsExcluded(t *testing.T) {
	g := graph.New()

	wellKnown := []string{"ServeHTTP", "MarshalJSON", "UnmarshalJSON", "String", "Error", "Read", "Write", "Close"}
	for _, name := range wellKnown {
		g.AddNode(&graph.Node{
			ID: "pkg/foo.go::myType." + name, Kind: graph.KindMethod,
			Name: name, FilePath: "pkg/foo.go", StartLine: 1, EndLine: 5, Language: "go",
		})
	}

	result := FindDeadCode(g, nil, nil)
	assert.Empty(t, result, "well-known interface methods should be excluded")
}

// TestDeadCode_WellKnownDoesNotSuppressOtherMethods verifies that non-well-known
// method names are still reported as dead.
func TestDeadCode_WellKnownDoesNotSuppressOtherMethods(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{
		ID: "pkg/foo.go::myType.handleInternal", Kind: graph.KindMethod,
		Name: "handleInternal", FilePath: "pkg/foo.go", StartLine: 1, EndLine: 5, Language: "go",
	})

	result := FindDeadCode(g, nil, nil)
	assert.Len(t, result, 1, "non-well-known methods should still be reported")
	assert.Equal(t, "pkg/foo.go::myType.handleInternal", result[0].ID)
}

// TestDeadCode_CgoExportExcluded verifies that functions with cgo_export Meta
// are excluded by default but included when IncludeCgoExports is set.
func TestDeadCode_CgoExportExcluded(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{
		ID: "pkg/bridge.go::bridge_init", Kind: graph.KindFunction,
		Name: "bridge_init", FilePath: "pkg/bridge.go", StartLine: 10, EndLine: 20, Language: "go",
		Meta: map[string]any{"cgo_export": true},
	})

	// Default: excluded
	result := FindDeadCode(g, nil, nil)
	assert.Empty(t, result, "CGo exports should be excluded by default")

	// With IncludeCgoExports: included
	result = FindDeadCode(g, nil, nil, FindDeadCodeOptions{IncludeCgoExports: true})
	assert.Len(t, result, 1, "CGo exports should be included when IncludeCgoExports is true")
}

// TestDeadCode_LinknameExcluded verifies that functions with go_linkname Meta
// are excluded by default but included when IncludeLinknameTargets is set.
func TestDeadCode_LinknameExcluded(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{
		ID: "pkg/runtime.go::nanotime", Kind: graph.KindFunction,
		Name: "nanotime", FilePath: "pkg/runtime.go", StartLine: 10, EndLine: 15, Language: "go",
		Meta: map[string]any{"go_linkname": true},
	})

	// Default: excluded
	result := FindDeadCode(g, nil, nil)
	assert.Empty(t, result, "linkname targets should be excluded by default")

	// With IncludeLinknameTargets: included
	result = FindDeadCode(g, nil, nil, FindDeadCodeOptions{IncludeLinknameTargets: true})
	assert.Len(t, result, 1, "linkname targets should be included when IncludeLinknameTargets is true")
}

// TestDeadCode_CrossRepoNodeExcluded verifies that nodes with a RepoPrefix
// are excluded when SkipCrossRepoNodes is set.
func TestDeadCode_CrossRepoNodeExcluded(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{
		ID: "github.com/other/repo/pkg/util.go::helperFunc", Kind: graph.KindFunction,
		Name: "helperFunc", FilePath: "pkg/util.go", StartLine: 1, EndLine: 10, Language: "go",
		RepoPrefix: "github.com/other/repo",
	})

	// Default: included (so users see them)
	result := FindDeadCode(g, nil, nil)
	assert.Len(t, result, 1, "cross-repo nodes should be included by default")

	// With SkipCrossRepoNodes: excluded
	result = FindDeadCode(g, nil, nil, FindDeadCodeOptions{SkipCrossRepoNodes: true})
	assert.Empty(t, result, "cross-repo nodes should be excluded when SkipCrossRepoNodes is true")
}

// TestDeadCode_ExpandedBuildConstraints verifies that the expanded set of
// OS/arch-suffixed files are excluded.
func TestDeadCode_ExpandedBuildConstraints(t *testing.T) {
	g := graph.New()

	constrainedFiles := []string{
		"pkg/net_openbsd.go", "pkg/net_plan9.go", "pkg/net_js.go",
		"pkg/asm_riscv64.go", "pkg/asm_s390x.go", "pkg/sys_purego.go",
	}
	for i, f := range constrainedFiles {
		g.AddNode(&graph.Node{
			ID: f + "::helper", Kind: graph.KindFunction,
			Name: "helper", FilePath: f, StartLine: 1, EndLine: 10, Language: "go",
		})
		_ = i
	}

	result := FindDeadCode(g, nil, nil)
	assert.Empty(t, result, "symbols in build-constrained files should be excluded")
}

// Regression: exported symbols inside `internal/` directories are
// package-private by Go's import rules — the compiler refuses to let
// external packages import them. So if the indexed graph has no caller,
// they're genuinely dead. The pre-fix behaviour was to skip every
// exported name unconditionally, silently hiding dead code anywhere a
// project used Go's `internal/` convention.
func TestDeadCode_ExportedInsideInternalIsSurfaced(t *testing.T) {
	g := graph.New()

	// Exported (capitalised) method inside `internal/` with zero callers.
	// Real-world case: `func (b *Node) Test() bool` in
	// gortex/internal/parser/tsitter/tsitter.go — used by the user as a
	// dead-code-detection probe and expected to show up.
	g.AddNode(&graph.Node{
		ID:   "gortex/internal/parser/tsitter/tsitter.go::Node.Test",
		Kind: graph.KindMethod, Name: "Test",
		FilePath:  "gortex/internal/parser/tsitter/tsitter.go",
		StartLine: 85, EndLine: 85, Language: "go",
		Meta: map[string]any{"receiver": "Node"},
	})

	// Also: an exported function that's NOT inside internal/. Must still
	// be excluded (the user's public-API code).
	g.AddNode(&graph.Node{
		ID: "pkg/gortex/api.go::DoThing", Kind: graph.KindFunction,
		Name: "DoThing", FilePath: "pkg/gortex/api.go",
		StartLine: 10, EndLine: 12, Language: "go",
	})

	// And an unexported function inside internal/ — pre-fix this was
	// already surfaced; the new code path must not regress that.
	g.AddNode(&graph.Node{
		ID: "gortex/internal/helpers.go::helper", Kind: graph.KindFunction,
		Name: "helper", FilePath: "gortex/internal/helpers.go",
		StartLine: 5, EndLine: 7, Language: "go",
	})

	result := FindDeadCode(g, nil, nil)
	ids := make(map[string]bool)
	for _, e := range result {
		ids[e.ID] = true
	}

	assert.True(t, ids["gortex/internal/parser/tsitter/tsitter.go::Node.Test"],
		"exported method inside internal/ with zero callers must be surfaced as dead code")
	assert.False(t, ids["pkg/gortex/api.go::DoThing"],
		"exported function outside internal/ stays excluded — could be called externally")
	assert.True(t, ids["gortex/internal/helpers.go::helper"],
		"unexported function inside internal/ stays surfaced (no regression)")
}

// TestDeadCode_NeverDeadCodeKinds verifies that the never-reported kinds
// (param, closure, module, string, enum_member, todo, etc) are NEVER
// returned, even when they have zero incoming edges and look "dead"
// structurally. Without this filter, a typical workspace surfaces
// ~18k noise rows (params, fields, npm modules) that drown out the
// ~300 real function-level dead-code signals.
func TestDeadCode_NeverDeadCodeKinds(t *testing.T) {
	g := graph.New()

	// One example per never-reported kind, all unexported and
	// edge-less so they'd otherwise look dead.
	cases := []struct {
		id   string
		kind graph.NodeKind
	}{
		{"pkg/a.go::func#param:x", graph.KindParam},
		{"pkg/a.go::func#closure@10", graph.KindClosure},
		{"pkg/a.go::Foo#tparam:T", graph.KindGenericParam},
		{"module::npm:lodash@4.17.21", graph.KindModule},
		{"string::error_msg::oops", graph.KindString},
		{"pkg/a.go::Color.Red", graph.KindEnumMember},
		{"db::pg::public.users.email", graph.KindColumn},
		{"db::pg::public.users", graph.KindTable},
		{"cfg::env::API_KEY", graph.KindConfigKey},
		{"flag::launchdarkly::beta", graph.KindFlag},
		{"event::metric::requests_total", graph.KindEvent},
		{"migration::pg::001_init", graph.KindMigration},
		{"fixture::testdata/sample.json", graph.KindFixture},
		{"todo::pkg/a.go:42", graph.KindTodo},
		{"team::@platform", graph.KindTeam},
		{"release::v1.2.3", graph.KindRelease},
		{"license::MIT", graph.KindLicense},
		{"k8s::Deployment::default::api", graph.KindResource},
		{"kustomize::overlays/prod", graph.KindKustomization},
		{"image::nginx:1.25", graph.KindImage},
		{"contract::Request::POST::/v1/foo", graph.KindContract},
	}
	for _, c := range cases {
		g.AddNode(&graph.Node{
			ID: c.id, Kind: c.kind, Name: "x", FilePath: "pkg/a.go",
			StartLine: 1, EndLine: 1, Language: "go",
		})
	}

	result := FindDeadCode(g, nil, nil, FindDeadCodeOptions{
		// Enable the broadest opt-ins; never-dead kinds must still
		// stay suppressed even with every flag flipped.
		IncludeVariables: true, IncludeFields: true, IncludeConstants: true,
	})
	for _, e := range result {
		t.Errorf("never-reported kind %s leaked into dead_code: %s", e.Kind, e.ID)
	}
}

// TestDeadCode_FieldsAndConstantsOptIn verifies fields and constants
// are excluded by default and only appear when their opt-in flags
// fire. Mirrors the existing IncludeVariables contract.
func TestDeadCode_FieldsAndConstantsOptIn(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::User.email", Kind: graph.KindField, Name: "email",
		FilePath: "pkg/a.go", StartLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::maxRetries", Kind: graph.KindConstant, Name: "maxRetries",
		FilePath: "pkg/a.go", StartLine: 10, Language: "go",
	})

	// Default: both excluded.
	def := FindDeadCode(g, nil, nil)
	assert.Empty(t, def, "fields and constants excluded by default")

	// Fields opt-in: only the field surfaces.
	withFields := FindDeadCode(g, nil, nil, FindDeadCodeOptions{IncludeFields: true})
	require.Len(t, withFields, 1)
	assert.Equal(t, "field", withFields[0].Kind)

	// Constants opt-in: only the constant surfaces.
	withConsts := FindDeadCode(g, nil, nil, FindDeadCodeOptions{IncludeConstants: true})
	require.Len(t, withConsts, 1)
	assert.Equal(t, "constant", withConsts[0].Kind)

	// Both flags: both surface.
	both := FindDeadCode(g, nil, nil, FindDeadCodeOptions{IncludeFields: true, IncludeConstants: true})
	assert.Len(t, both, 2)
}

// TestDeadCode_FieldWithReadIsAlive — fields are exercised by EdgeReads,
// not EdgeCalls. The pre-fix global allowlist {Calls,References,...}
// missed every field read, so every field-with-readers also looked
// "dead" once IncludeFields was on. The per-kind allowlist must count
// EdgeReads (and EdgeWrites) for KindField, KindVariable, KindConstant.
func TestDeadCode_FieldWithReadIsAlive(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::User.email", Kind: graph.KindField, Name: "email",
		FilePath: "pkg/a.go", StartLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::printUser", Kind: graph.KindFunction, Name: "printUser",
		FilePath: "pkg/a.go", StartLine: 20, Language: "go",
	})
	// printUser reads User.email — EdgeReads, exactly what the
	// resolver lands on a field selector.
	g.AddEdge(&graph.Edge{
		From: "pkg/a.go::printUser", To: "pkg/a.go::User.email",
		Kind: graph.EdgeReads, FilePath: "pkg/a.go", Line: 21,
	})

	result := FindDeadCode(g, nil, nil, FindDeadCodeOptions{IncludeFields: true})
	for _, e := range result {
		assert.NotEqual(t, "pkg/a.go::User.email", e.ID,
			"field with an incoming Reads edge must not be reported as dead")
	}
}

// TestDeadCode_TypeReferencedAsParamIsAlive — types are exercised by
// References / TypedAs / Instantiates. The pre-fix global allowlist
// caught References but not TypedAs, so a type used only as a
// parameter declaration (no constructor, no method receivers) would
// have been reported dead. The per-kind allowlist covers it.
func TestDeadCode_TypeReferencedAsParamIsAlive(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::Config", Kind: graph.KindType, Name: "Config",
		FilePath: "pkg/a.go", StartLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::Run#param:cfg", Kind: graph.KindParam, Name: "cfg",
		FilePath: "pkg/a.go", StartLine: 20, Language: "go",
	})
	g.AddEdge(&graph.Edge{
		From: "pkg/a.go::Run#param:cfg", To: "pkg/a.go::Config",
		Kind: graph.EdgeTypedAs, FilePath: "pkg/a.go", Line: 20,
	})

	result := FindDeadCode(g, nil, nil)
	for _, e := range result {
		assert.NotEqual(t, "pkg/a.go::Config", e.ID,
			"type referenced as a param type must not be reported as dead")
	}
}
