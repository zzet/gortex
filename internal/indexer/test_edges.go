package indexer

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Keep graph reads bounded without falling back to one backend query per file
// or annotation. SQLite applies its own lower SQL-variable chunks inside each
// call; this cap bounds the Go-side adjacency retained by this whole-graph pass.
const (
	testMetadataLookupBatchSize = 512
	// Projection rows are compact identifiers/source locations. 4096 amortizes
	// cursor setup while bounding callback working sets; call-source batches stay
	// lower because a single source may have high fan-out.
	testProjectionPageSize  = 4096
	testCallSourceBatchSize = 512
)

// markTestSymbolsAndEmitEdges runs after the resolver and before
// community detection. It performs two passes over the graph:
//
//  1. Walk every function/method node that lives in a test file (per
//     IsTestFile) and stamp Meta["test_role"] — "benchmark", "fuzz",
//     or "example" when the name matches a per-language convention
//     (per TestRole), otherwise "test" for plain test support code.
//     Meta["is_test"] = true is stamped alongside for back-compat with
//     consumers that only need the boolean. Symbols whose runner
//     discovers tests by attribute rather than by file location (Rust
//     #[test], JVM @Test — see AnnotationTestRole) are additionally
//     stamped from their EdgeAnnotated edges, so an inline #[test] fn
//     in a production-path file classifies too.
//
//  2. Walk every EdgeCalls. For each call whose source is a test
//     function and whose target is non-test, emit a parallel
//     EdgeTests pointing to the same target.
//
// The split lets agents distinguish prod callers from test callers
// (find_usages with exclude_tests) and lets get_test_targets answer
// "which tests cover X?" with a single reverse-edge walk instead of
// the runtime call-graph traversal it does today.
//
// Returns counts for telemetry: number of nodes marked as test,
// number of EdgeTests emitted.
func markTestSymbolsAndEmitEdges(g graph.Store) (markedTests int, edgesEmitted int) {
	return markTestSymbolsAndEmitEdgesScoped(g, nil)
}

// markTestSymbolsAndEmitEdgesScoped is markTestSymbolsAndEmitEdges with an armed
// changed-repo scope for the end-of-batch pass. A nil scope emits over the whole
// graph, so the fresh-index / single-repo path is byte-identical.
//
// Pass 1 (test-symbol classification) always runs whole-graph: the testNodes
// membership set it builds must be COMPLETE, because Pass 2 skips test→test
// calls via testNodes[e.To] and a callee can be a test in an unchanged repo
// (a cross-repo test→test call). Only Pass 2's driving EdgeCalls scan is scoped
// — an EdgeTests edge is FROM a test function, so a changed repo owns exactly
// the test edges its reindex dropped; an unchanged repo's persist on disk.
func markTestSymbolsAndEmitEdgesScoped(g graph.Store, changedPrefixes map[string]bool, changedFiles ...string) (markedTests int, edgesEmitted int) {
	if g == nil {
		return 0, 0
	}
	// A normal partial index carries an exact file frontier. Keep its symbol
	// classification, annotation lookups, and call walk proportional to those
	// files; the repo-scoped/global path remains the cold-index oracle.
	if len(changedFiles) > 0 {
		g.ResolveMutex().Lock()
		defer g.ResolveMutex().Unlock()
		return markTestSymbolsAndEmitEdgesForFilesLocked(g, changedFiles)
	}
	// Serialise Node.Meta mutation against other graph-wide passes
	// (detectClonesAndEmitEdges, ResolveTemporalCalls, reach.BuildIndex).
	// See clones.go for the rationale — without this lock the writes
	// below race the readers and the runtime aborts with "concurrent
	// map read and map write".
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()

	testNodes, markedTests, changedNodes := markTestSymbolsLocked(g)
	if len(testNodes) == 0 {
		// Test-file metadata still needs persistence even when the file has no
		// function or method symbols. There are no test edges to batch with in
		// this case, so persist just the changed nodes while ResolveMutex is held.
		g.AddBatch(changedNodes, nil)
		return markedTests, 0
	}
	edgesEmitted = emitTestEdgesAndPersistLocked(g, testNodes, changedNodes, changedPrefixes)
	return markedTests, edgesEmitted
}

// markTestSymbolsAndEmitEdgesForFilesLocked is the exact-file partial path.
// Besides the changed symbols it includes only their incoming test callers: a
// callee switching between production and test changes whether those callers'
// parallel EdgeTests edges are valid. The caller holds ResolveMutex.
func markTestSymbolsAndEmitEdgesForFilesLocked(g graph.Store, changedFiles []string) (int, int) {
	files := make([]string, 0, len(changedFiles))
	seenFiles := make(map[string]struct{}, len(changedFiles))
	for _, file := range changedFiles {
		if file == "" {
			continue
		}
		if _, seen := seenFiles[file]; seen {
			continue
		}
		seenFiles[file] = struct{}{}
		files = append(files, file)
	}
	if len(files) == 0 {
		return 0, 0
	}

	wantedKinds := map[graph.NodeKind]bool{
		graph.KindFile: true, graph.KindFunction: true, graph.KindMethod: true,
	}
	var localNodes []*graph.Node
	if finder, ok := g.(graph.NodesInFilesByKindFinder); ok {
		localNodes = finder.NodesInFilesByKind(files, []graph.NodeKind{
			graph.KindFile, graph.KindFunction, graph.KindMethod,
		})
	} else {
		for _, nodes := range g.GetFileNodesByPaths(files) {
			for _, node := range nodes {
				if node != nil && wantedKinds[node.Kind] {
					localNodes = append(localNodes, node)
				}
			}
		}
	}
	if len(localNodes) == 0 {
		return 0, 0
	}

	setMeta := func(n *graph.Node, key string, value any) bool {
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		switch want := value.(type) {
		case bool:
			if got, ok := n.Meta[key].(bool); ok && got == want {
				return false
			}
		case string:
			if got, ok := n.Meta[key].(string); ok && got == want {
				return false
			}
		}
		n.Meta[key] = value
		return true
	}
	isStampedTest := func(n *graph.Node) bool {
		if n == nil || n.Meta == nil {
			return false
		}
		value, _ := n.Meta["is_test"].(bool)
		return value
	}

	localIDs := make([]string, 0, len(localNodes))
	changedSymbolIDs := make([]string, 0, len(localNodes))
	for _, node := range localNodes {
		localIDs = append(localIDs, node.ID)
		if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
			changedSymbolIDs = append(changedSymbolIDs, node.ID)
		}
	}
	localAdjacency := g.GetOutEdgesByNodeIDs(localIDs)
	testFiles := make(map[string]bool)
	fileRunners := make(map[string]string)
	var changedNodes []*graph.Node
	for _, node := range localNodes {
		if node.Kind != graph.KindFile || !IsTestFile(node.FilePath) {
			continue
		}
		testFiles[node.ID] = true
		testFiles[node.FilePath] = true
		changed := setMeta(node, "is_test_file", true)
		if runner := detectTestRunnerForFileEdges(node, localAdjacency[node.ID]); runner != "" {
			changed = setMeta(node, "test_runner", runner) || changed
			fileRunners[node.ID] = runner
			fileRunners[node.FilePath] = runner
		}
		if changed {
			changedNodes = append(changedNodes, node)
		}
	}

	annotationTargets := make([]string, 0)
	annotationRefs := make(map[string][]string)
	seenAnnotationTargets := make(map[string]struct{})
	for _, node := range localNodes {
		if node.Kind != graph.KindFunction && node.Kind != graph.KindMethod {
			continue
		}
		for _, edge := range localAdjacency[node.ID] {
			if edge == nil || edge.Kind != graph.EdgeAnnotated {
				continue
			}
			annotationRefs[node.ID] = append(annotationRefs[node.ID], edge.To)
			if _, seen := seenAnnotationTargets[edge.To]; !seen {
				seenAnnotationTargets[edge.To] = struct{}{}
				annotationTargets = append(annotationTargets, edge.To)
			}
		}
	}
	annotationNodes := g.GetNodesByIDs(annotationTargets)
	annotationRoles := make(map[string]string, len(annotationRefs))
	for source, targets := range annotationRefs {
		for _, target := range targets {
			annotation := annotationNodes[target]
			if annotation == nil {
				continue
			}
			role := AnnotationTestRole(annotation.Language, annotation.Name)
			if role == "" {
				continue
			}
			if current := annotationRoles[source]; current == "" || (current == "benchmark" && role == "test") {
				annotationRoles[source] = role
			}
		}
	}

	localTests := make(map[string]bool)
	markedTests := 0
	for _, node := range localNodes {
		if node.Kind != graph.KindFunction && node.Kind != graph.KindMethod {
			continue
		}
		var role, runner string
		switch {
		case testFiles[node.FilePath]:
			role = TestRole(node.Name, node.Language)
			if role == "" {
				role = "test"
			}
			runner = fileRunners[node.FilePath]
		case annotationRoles[node.ID] != "":
			role = annotationRoles[node.ID]
			runner = AnnotationTestRunner(node.Language)
		default:
			continue
		}
		changed := setMeta(node, "is_test", true)
		changed = setMeta(node, "test_role", role) || changed
		if runner != "" {
			changed = setMeta(node, "test_runner", runner) || changed
		}
		if changed {
			changedNodes = append(changedNodes, node)
		}
		localTests[node.ID] = true
		markedTests++
	}

	// A changed callee's test classification can invalidate edges owned by an
	// unchanged test caller. Pull that one-hop incoming neighborhood in one
	// batch, then reconcile each affected caller's complete EdgeTests set.
	callerIDs := make([]string, 0)
	seenCallerIDs := make(map[string]struct{})
	for _, edges := range g.GetInEdgesByNodeIDs(changedSymbolIDs) {
		for _, edge := range edges {
			if edge == nil || edge.Kind != graph.EdgeCalls {
				continue
			}
			if _, seen := seenCallerIDs[edge.From]; seen {
				continue
			}
			seenCallerIDs[edge.From] = struct{}{}
			callerIDs = append(callerIDs, edge.From)
		}
	}
	callerNodes := g.GetNodesByIDs(callerIDs)
	testSources := make(map[string]bool, len(localTests)+len(callerNodes))
	for id := range localTests {
		testSources[id] = true
	}
	for id, node := range callerNodes {
		if localTests[id] || isStampedTest(node) {
			testSources[id] = true
		}
	}
	if len(testSources) == 0 {
		g.AddBatch(changedNodes, nil)
		return markedTests, 0
	}

	sourceIDs := make([]string, 0, len(testSources))
	for id := range testSources {
		sourceIDs = append(sourceIDs, id)
	}
	sourceAdjacency := g.GetOutEdgesByNodeIDs(sourceIDs)
	targetIDs := make([]string, 0)
	seenTargets := make(map[string]struct{})
	for _, source := range sourceIDs {
		for _, edge := range sourceAdjacency[source] {
			if edge == nil || edge.Kind != graph.EdgeCalls {
				continue
			}
			if _, seen := seenTargets[edge.To]; !seen {
				seenTargets[edge.To] = struct{}{}
				targetIDs = append(targetIDs, edge.To)
			}
		}
	}
	targetNodes := g.GetNodesByIDs(targetIDs)
	isTestTarget := func(id string) bool {
		return localTests[id] || isStampedTest(targetNodes[id])
	}

	seenEdges := make(map[string]bool)
	edges := make([]*graph.Edge, 0)
	for _, source := range sourceIDs {
		for _, edge := range sourceAdjacency[source] {
			if edge == nil || edge.Kind != graph.EdgeCalls || isTestTarget(edge.To) {
				continue
			}
			// Unresolved calls never clone — see emitTestEdgesAndPersistLocked.
			if graph.IsUnresolvedTarget(edge.To) {
				continue
			}
			key := edge.From + "\x00" + edge.To
			if seenEdges[key] {
				continue
			}
			seenEdges[key] = true
			edges = append(edges, &graph.Edge{
				From: edge.From, To: edge.To, Kind: graph.EdgeTests,
				FilePath: edge.FilePath, Line: edge.Line, Origin: graph.OriginASTInferred,
			})
		}
	}
	_, supported, err := graph.EvictEdgesFromSourcesByKindsBackground(
		g, sourceIDs, []graph.EdgeKind{graph.EdgeTests},
	)
	if err != nil || !supported {
		g.AddBatch(changedNodes, nil)
		return markedTests, 0
	}
	g.AddBatch(changedNodes, edges)
	return markedTests, len(edges)
}

func setTestMetadata(n *graph.Node, key string, value any) bool {
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	switch want := value.(type) {
	case bool:
		if got, ok := n.Meta[key].(bool); ok && got == want {
			return false
		}
	case string:
		if got, ok := n.Meta[key].(string); ok && got == want {
			return false
		}
	}
	n.Meta[key] = value
	return true
}

// markTestSymbolsProjectedLocked is the SQLite/global fast path. Its scanner
// yields metadata-free pages with every cursor already closed, so the callback
// may safely fetch the small set of test files, annotation targets, and symbols
// that actually need full metadata. Production symbols never cross the driver
// as full graph.Node values.
func markTestSymbolsProjectedLocked(g graph.Store, scanner graph.TestProjectionScanner) (testNodes map[string]bool, markedTests int, changedNodes []*graph.Node) {
	testFiles := make(map[string]bool)
	fileRunners := make(map[string]string)
	scanner.ScanTestNodeProjections([]graph.NodeKind{graph.KindFile}, testProjectionPageSize, func(rows []graph.TestNodeProjection) bool {
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			if !IsTestFile(row.FilePath) {
				continue
			}
			testFiles[row.ID] = true
			testFiles[row.FilePath] = true
			ids = append(ids, row.ID)
		}
		if len(ids) == 0 {
			return true
		}
		fullNodes := g.GetNodesByIDs(ids)
		outEdges := g.GetOutEdgesByNodeIDs(ids)
		for _, id := range ids {
			node := fullNodes[id]
			if node == nil {
				continue
			}
			changed := setTestMetadata(node, "is_test_file", true)
			if runner := detectTestRunnerForFileEdges(node, outEdges[id]); runner != "" {
				changed = setTestMetadata(node, "test_runner", runner) || changed
				fileRunners[node.ID] = runner
				fileRunners[node.FilePath] = runner
			}
			if changed {
				changedNodes = append(changedNodes, node)
			}
		}
		return true
	})

	// Resolve annotation targets in bounded batches. Only endpoint IDs cross
	// the whole-edge scan; annotation node metadata is fetched on demand.
	annoTestRole := make(map[string]string)
	annoNodeRole := make(map[string]string)
	type annotationRef struct{ from, to string }
	annotationBatch := make([]annotationRef, 0, testMetadataLookupBatchSize)
	flushAnnotations := func() {
		if len(annotationBatch) == 0 {
			return
		}
		missingSet := make(map[string]struct{})
		for _, ref := range annotationBatch {
			if _, cached := annoNodeRole[ref.to]; !cached {
				missingSet[ref.to] = struct{}{}
			}
		}
		if len(missingSet) > 0 {
			missing := make([]string, 0, len(missingSet))
			for id := range missingSet {
				missing = append(missing, id)
			}
			nodes := g.GetNodesByIDs(missing)
			for id := range missingSet {
				role := ""
				if annotation := nodes[id]; annotation != nil {
					role = AnnotationTestRole(annotation.Language, annotation.Name)
				}
				annoNodeRole[id] = role
			}
		}
		for _, ref := range annotationBatch {
			role := annoNodeRole[ref.to]
			if role == "" {
				continue
			}
			if current := annoTestRole[ref.from]; current == "" || (current == "benchmark" && role == "test") {
				annoTestRole[ref.from] = role
			}
		}
		annotationBatch = annotationBatch[:0]
	}
	scanner.ScanTestEdgeProjections([]graph.EdgeKind{graph.EdgeAnnotated}, testProjectionPageSize, func(rows []graph.TestEdgeProjection) bool {
		for _, row := range rows {
			annotationBatch = append(annotationBatch, annotationRef{from: row.From, to: row.To})
			if len(annotationBatch) == cap(annotationBatch) {
				flushAnnotations()
			}
		}
		return true
	})
	flushAnnotations()

	testNodes = make(map[string]bool)
	scanner.ScanTestNodeProjections([]graph.NodeKind{graph.KindFunction, graph.KindMethod}, testProjectionPageSize, func(rows []graph.TestNodeProjection) bool {
		type plannedStamp struct {
			id, role, runner string
		}
		plans := make([]plannedStamp, 0, len(rows))
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			var role, runner string
			switch {
			case testFiles[row.FilePath]:
				role = TestRole(row.Name, row.Language)
				if role == "" {
					role = "test"
				}
				runner = fileRunners[row.FilePath]
			case annoTestRole[row.ID] != "":
				role = annoTestRole[row.ID]
				runner = AnnotationTestRunner(row.Language)
			default:
				continue
			}
			plans = append(plans, plannedStamp{id: row.ID, role: role, runner: runner})
			ids = append(ids, row.ID)
		}
		if len(ids) == 0 {
			return true
		}
		fullNodes := g.GetNodesByIDs(ids)
		for _, plan := range plans {
			node := fullNodes[plan.id]
			if node == nil {
				continue
			}
			changed := setTestMetadata(node, "is_test", true)
			changed = setTestMetadata(node, "test_role", plan.role) || changed
			if plan.runner != "" {
				changed = setTestMetadata(node, "test_runner", plan.runner) || changed
			}
			if changed {
				changedNodes = append(changedNodes, node)
			}
			testNodes[node.ID] = true
			markedTests++
		}
		return true
	})
	return testNodes, markedTests, changedNodes
}

// markTestSymbolsLocked runs Pass 1: it stamps test Meta on every test symbol
// and returns the complete test-node membership set, marked count, and every
// node whose test metadata changed. The caller persists changedNodes through
// the same AddBatch as the derived test edges. The caller must hold
// g.ResolveMutex(). Always whole-graph — see the scoped entry point for why the
// set must be complete.
func markTestSymbolsLocked(g graph.Store) (testNodes map[string]bool, markedTests int, changedNodes []*graph.Node) {
	if scanner, ok := g.(graph.TestProjectionScanner); ok {
		return markTestSymbolsProjectedLocked(g, scanner)
	}
	setMeta := func(n *graph.Node, key string, value any) bool {
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		switch want := value.(type) {
		case bool:
			if got, ok := n.Meta[key].(bool); ok && got == want {
				return false
			}
		case string:
			if got, ok := n.Meta[key].(string); ok && got == want {
				return false
			}
		}
		n.Meta[key] = value
		return true
	}

	// Pass 1: classify file nodes, then function/method nodes. Build
	// a local testNodes set keyed by node id so Pass 2 can probe it
	// without re-walking the Meta. (Node.Meta mutations on returned
	// nodes don't persist back to disk backends, so a later GetNode
	// in Pass 2 wouldn't see the is_test flag we set here.)
	testFiles := map[string]bool{}     // file node ID → is test file
	fileRunners := map[string]string{} // file FilePath → test runner
	var testFileNodes []*graph.Node
	for n := range g.NodesByKind(graph.KindFile) {
		if n != nil && IsTestFile(n.FilePath) {
			testFileNodes = append(testFileNodes, n)
		}
	}
	for start := 0; start < len(testFileNodes); start += testMetadataLookupBatchSize {
		end := min(start+testMetadataLookupBatchSize, len(testFileNodes))
		batch := testFileNodes[start:end]
		ids := make([]string, 0, len(batch))
		for _, n := range batch {
			ids = append(ids, n.ID)
		}
		outEdges := g.GetOutEdgesByNodeIDs(ids)
		for _, n := range batch {
			testFiles[n.ID] = true
			changed := setMeta(n, "is_test_file", true)
			if runner := detectTestRunnerForFileEdges(n, outEdges[n.ID]); runner != "" {
				changed = setMeta(n, "test_runner", runner) || changed
				fileRunners[n.FilePath] = runner
			}
			if changed {
				changedNodes = append(changedNodes, n)
			}
		}
	}

	// Annotation-driven test detection. Rust (#[test], #[tokio::test],
	// #[bench]) and JVM JUnit/TestNG (@Test, @ParameterizedTest, …)
	// runners discover tests by attribute, not by file location. The
	// language extractors already emit EdgeAnnotated edges to synthetic
	// annotation nodes (see EmitAnnotationEdge); consult them so an
	// inline #[test] fn in a production-path src/foo.rs — or a @Test
	// method in a class whose file name carries no test suffix — gets
	// the same is_test / test_role / EdgeTests treatment as a function
	// in a *_test.go file. Without this pass those tests are invisible
	// to get_test_targets / analyze kind=tests_as_edges / coverage_gaps.
	annoTestRole := map[string]string{} // symbol node ID → test role
	annoNodeRole := map[string]string{} // annotation node ID → role (cached resolution)
	type annotationRef struct{ from, to string }
	annotationBatch := make([]annotationRef, 0, testMetadataLookupBatchSize)
	flushAnnotations := func() {
		if len(annotationBatch) == 0 {
			return
		}
		missingSet := make(map[string]struct{})
		for _, ref := range annotationBatch {
			if _, cached := annoNodeRole[ref.to]; !cached {
				missingSet[ref.to] = struct{}{}
			}
		}
		if len(missingSet) > 0 {
			missing := make([]string, 0, len(missingSet))
			for id := range missingSet {
				missing = append(missing, id)
			}
			nodes := g.GetNodesByIDs(missing)
			for id := range missingSet {
				role := ""
				if anno := nodes[id]; anno != nil {
					role = AnnotationTestRole(anno.Language, anno.Name)
				}
				// Cache misses too, so a widely-used non-test annotation is never
				// fetched again in a later bounded chunk.
				annoNodeRole[id] = role
			}
		}
		for _, ref := range annotationBatch {
			role := annoNodeRole[ref.to]
			if role == "" {
				continue
			}
			// Prefer the more specific "test" over "benchmark" when a
			// single symbol carries both (rare).
			if existing := annoTestRole[ref.from]; existing == "" || (existing == "benchmark" && role == "test") {
				annoTestRole[ref.from] = role
			}
		}
		annotationBatch = annotationBatch[:0]
	}
	for e := range g.EdgesByKind(graph.EdgeAnnotated) {
		if e == nil {
			continue
		}
		annotationBatch = append(annotationBatch, annotationRef{from: e.From, to: e.To})
		if len(annotationBatch) == cap(annotationBatch) {
			flushAnnotations()
		}
	}
	flushAnnotations()

	testNodes = map[string]bool{}
	stampTestSymbol := func(n *graph.Node) {
		inTestFile := testFiles[n.FilePath]
		var role, runner string
		switch {
		case inTestFile:
			role = TestRole(n.Name, n.Language)
			if role == "" {
				role = "test"
			}
			runner = fileRunners[n.FilePath]
		case annoTestRole[n.ID] != "":
			role = annoTestRole[n.ID]
			runner = AnnotationTestRunner(n.Language)
		default:
			return
		}
		changed := setMeta(n, "is_test", true)
		changed = setMeta(n, "test_role", role) || changed
		if runner != "" {
			changed = setMeta(n, "test_runner", runner) || changed
		}
		if changed {
			changedNodes = append(changedNodes, n)
		}
		testNodes[n.ID] = true
		markedTests++
	}
	for n := range g.NodesByKind(graph.KindFunction) {
		if n != nil {
			// Test-file membership is the authoritative signal. No
			// standard runner (go test, pytest, ...) picks up a test
			// by name outside a test file, so a production function
			// that merely starts with "Test"/"Benchmark" (e.g.
			// TestRole) must not be flagged. The name convention only
			// refines the *role* — benchmark / fuzz / example — for
			// symbols already inside a test file; anything else there
			// is test support code: role "test".
			stampTestSymbol(n)
		}
	}
	for n := range g.NodesByKind(graph.KindMethod) {
		if n != nil {
			stampTestSymbol(n)
		}
	}
	return testNodes, markedTests, changedNodes
}

// emitTestEdgesLocked runs Pass 2: for each (test, non-test) call it emits a
// parallel EdgeTests, deduped per (From, To) because a single test can call the
// same subject repeatedly. The testNodes set from Pass 1 is authoritative — no
// inline GetNode is needed because "From must be a test symbol" already enforces
// the kind filter (only function/method ids land in testNodes). The caller must
// hold g.ResolveMutex().
//
// With a nil scope it walks every EdgeCalls edge; with a scope it walks only the
// changed repos' out-edges (GetRepoEdges — one backend query per repo). The
// testNodes[e.To] test→test skip stays correct across repos because testNodes is
// complete (Pass 1 is whole-graph).
func emitTestEdgesLocked(g graph.Store, testNodes map[string]bool, changedPrefixes map[string]bool) int {
	return emitTestEdgesAndPersistLocked(g, testNodes, nil, changedPrefixes)
}

// emitTestEdgesAndPersistLocked additionally persists changedNodes in the same
// AddBatch as the derived edges so disk-backed stores retain the classification.
func emitTestEdgesAndPersistLocked(g graph.Store, testNodes map[string]bool, changedNodes []*graph.Node, changedPrefixes map[string]bool) int {
	seen := make(map[graph.EdgeEndpoint]struct{})
	type pending struct {
		from, to, file string
		line           int
	}
	var out []pending
	processFields := func(from, to, file string, line int) {
		if !testNodes[from] {
			return
		}
		if testNodes[to] {
			return // test → test calls are infrastructure, not subject coverage
		}
		// An unresolved call clones into a tests edge that can never say
		// what is tested — and, stripped of the call's receiver evidence,
		// it is a naked stub a later resolve pass would bind without the
		// guards that protect the calls edge. Only resolved subjects link.
		if graph.IsUnresolvedTarget(to) {
			return
		}
		key := graph.EdgeEndpoint{From: from, To: to}
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		out = append(out, pending{from: from, to: to, file: file, line: line})
	}
	process := func(e *graph.Edge) {
		if e != nil && e.Kind == graph.EdgeCalls {
			processFields(e.From, e.To, e.FilePath, e.Line)
		}
	}
	if changedPrefixes == nil {
		if scanner, ok := g.(graph.TestCallProjectionScanner); ok {
			sourceIDs := make([]string, 0, len(testNodes))
			for sourceID := range testNodes {
				sourceIDs = append(sourceIDs, sourceID)
			}
			scanner.ScanTestCallProjections(sourceIDs, testCallSourceBatchSize, testProjectionPageSize, func(rows []graph.TestEdgeProjection) bool {
				for _, row := range rows {
					processFields(row.From, row.To, row.FilePath, row.Line)
				}
				return true
			})
		} else if scanner, ok := g.(graph.TestProjectionScanner); ok {
			scanner.ScanTestEdgeProjections([]graph.EdgeKind{graph.EdgeCalls}, testProjectionPageSize, func(rows []graph.TestEdgeProjection) bool {
				for _, row := range rows {
					if row.Kind == graph.EdgeCalls {
						processFields(row.From, row.To, row.FilePath, row.Line)
					}
				}
				return true
			})
		} else {
			for e := range g.EdgesByKind(graph.EdgeCalls) {
				process(e)
			}
		}
	} else {
		prefixes := make([]string, 0, len(changedPrefixes))
		for prefix := range changedPrefixes {
			if prefix == "" {
				continue
			}
			prefixes = append(prefixes, prefix)
		}
		for _, row := range graph.ReadRepoEdgesByKinds(g, prefixes, []graph.EdgeKind{graph.EdgeCalls}) {
			process(row.Edge)
		}
	}
	edges := make([]*graph.Edge, 0, len(out))
	for _, p := range out {
		edges = append(edges, &graph.Edge{
			From:     p.from,
			To:       p.to,
			Kind:     graph.EdgeTests,
			FilePath: p.file,
			Line:     p.line,
			Origin:   graph.OriginASTInferred,
		})
	}
	g.AddBatch(changedNodes, edges)
	return len(edges)
}

// detectTestRunnerForFileEdges is the adjacency-prefetched form used by the
// indexing pass. Keeping runner precedence here makes the batched and focused
// entry points byte-for-byte equivalent.
func detectTestRunnerForFileEdges(fileNode *graph.Node, outEdges []*graph.Edge) string {
	if fileNode == nil {
		return ""
	}
	// 1) Parser-stamped runner (JS / TS).
	if fileNode.Meta != nil {
		if v, ok := fileNode.Meta["test_runner"].(string); ok && v != "" {
			return v
		}
	}
	// 2) Import-edge signal.
	if runner := detectRunnerFromImportEdgeSlice(fileNode, outEdges); runner != "" {
		return runner
	}
	// 3) Language-level defaults.
	switch fileNode.Language {
	case "go":
		return "gotest"
	case "python":
		return "pytest"
	case "ruby":
		base := strings.ToLower(filepath.Base(fileNode.FilePath))
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		switch {
		case strings.HasSuffix(stem, "_spec"):
			return "rspec"
		case strings.HasSuffix(stem, "_test"):
			return "minitest"
		}
	case "php":
		// PHPUnit is the default when nothing more specific was imported.
		// Pest reuses the same file layout, so it is only claimed when the
		// import signal above actually saw it.
		return "phpunit"
	}
	return ""
}

func detectRunnerFromImportEdgeSlice(fileNode *graph.Node, edges []*graph.Edge) string {
	if fileNode == nil {
		return ""
	}
	const prefix = "unresolved::import::"
	for _, e := range edges {
		if e == nil || e.Kind != graph.EdgeImports {
			continue
		}
		path := strings.TrimPrefix(e.To, prefix)
		path = strings.Trim(path, "\"'`")
		switch fileNode.Language {
		case "javascript", "typescript", "tsx", "jsx":
			switch {
			case path == "bun:test":
				return "bun-test"
			case path == "vitest" || strings.HasPrefix(path, "vitest/"):
				return "vitest"
			case path == "@playwright/test" || strings.HasPrefix(path, "@playwright/test/"):
				return "playwright"
			case path == "cypress" || strings.HasPrefix(path, "cypress/"):
				return "cypress"
			case path == "node:test" || strings.HasPrefix(path, "node:test/"):
				return "node-test"
			case path == "@jest/globals" || strings.HasPrefix(path, "@jest/globals/"),
				path == "jest" || strings.HasPrefix(path, "jest/"),
				path == "jest-mock", path == "ts-jest", path == "babel-jest",
				path == "@types/jest":
				return "jest"
			case path == "mocha" || strings.HasPrefix(path, "mocha/"),
				path == "@types/mocha", path == "mochawesome":
				return "mocha"
			}
		case "php":
			// Read the namespace off the edge rather than its target: a PHP
			// import that resolved to the class it names — or fell through to
			// an external:: stub — no longer carries the specifier in e.To.
			// Meta["fqn"] is stamped at extraction and survives both.
			ns := path
			if e.Meta != nil {
				if fqn, _ := e.Meta["fqn"].(string); fqn != "" {
					ns = fqn
				}
			}
			ns = strings.ReplaceAll(ns, `\`, "/")
			switch {
			case ns == "Pest" || strings.HasPrefix(ns, "Pest/"):
				return "pest"
			case strings.HasPrefix(ns, "PHPUnit/"):
				return "phpunit"
			case strings.HasPrefix(ns, "Codeception/"):
				return "codeception"
			case strings.HasPrefix(ns, "Behat/"):
				return "behat"
			}
		case "python":
			switch {
			case path == "pytest" || strings.HasPrefix(path, "pytest."),
				path == "pytest_asyncio" || path == "_pytest" || strings.HasPrefix(path, "_pytest."):
				return "pytest"
			case path == "unittest" || strings.HasPrefix(path, "unittest."):
				return "unittest"
			}
		case "ruby":
			switch {
			case path == "rspec" || strings.HasPrefix(path, "rspec/"),
				path == "rspec-core", path == "rspec/core":
				return "rspec"
			case path == "minitest" || strings.HasPrefix(path, "minitest/"),
				path == "minitest/autorun":
				return "minitest"
			case path == "test/unit":
				return "test-unit"
			}
		}
	}
	return ""
}
