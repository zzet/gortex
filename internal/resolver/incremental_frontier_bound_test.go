package resolver

import (
	"errors"
	"fmt"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
)

// observedResolverLogger captures the Warn/Info records the bounded legs emit.
// The refusal log is the only channel the entrypoints whose stats the indexer
// discards actually have, so it is asserted as behaviour, not decoration.
func observedResolverLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// incomingFanOutGraph builds the adversarial shape the incremental incoming
// leg is exposed to: one changed file defining a common name, and fanIn other
// files holding references parked on that name's unresolved stub. The changed
// file also holds one outgoing unresolved edge that DOES bind, so the batch is
// never mistaken for a no-op pass.
func incomingFanOutGraph(t *testing.T, fanIn int) (*graph.Graph, string) {
	t.Helper()
	return incomingFanOutGraphMode(t, fanIn, true)
}

// incomingFanOutGraphMode with withOutgoing=false is the shape the per-save
// entrypoint sees most often for a widely referenced definition file: the save
// left no outgoing unresolved edge of its own, so the WHOLE pending frontier is
// the incoming leg and a refusal empties it.
func incomingFanOutGraphMode(t *testing.T, fanIn int, withOutgoing bool) (*graph.Graph, string) {
	t.Helper()
	const changed = "pkg/target.go"
	g := graph.New()
	nodes := []*graph.Node{
		{ID: changed, Kind: graph.KindFile, Name: changed, FilePath: changed},
		{ID: changed + "::Close", Kind: graph.KindFunction, Name: "Close", FilePath: changed},
		{ID: "pkg/helper.go", Kind: graph.KindFile, Name: "pkg/helper.go", FilePath: "pkg/helper.go"},
		{ID: "pkg/helper.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "pkg/helper.go"},
	}
	var edges []*graph.Edge
	if withOutgoing {
		edges = append(edges, &graph.Edge{
			From: changed + "::Close", To: graph.UnresolvedMarker + "Helper",
			Kind: graph.EdgeCalls, FilePath: changed, Line: 2,
		})
	}
	for i := range fanIn {
		path := fmt.Sprintf("caller/c%06d.go", i)
		callerID := path + "::Caller"
		nodes = append(nodes,
			&graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path},
			&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "Caller", FilePath: path},
		)
		edges = append(edges,
			// The import makes the reference genuinely bindable, so "the edges
			// were not rebound" is evidence about the admission and not about
			// an unrelated resolution gate.
			&graph.Edge{From: path, To: changed, Kind: graph.EdgeImports, FilePath: path, Line: 1},
			&graph.Edge{
				From: callerID, To: graph.UnresolvedMarker + "Close",
				Kind: graph.EdgeCalls, FilePath: path, Line: 3,
			})
	}
	g.AddBatch(nodes, edges)
	return g, changed
}

func unresolvedInEdgeCount(g *graph.Graph, stub string) int {
	n := 0
	for _, edge := range g.GetInEdges(stub) {
		if edge != nil && graph.IsUnresolvedTarget(edge.To) {
			n++
		}
	}
	return n
}

// A frontier inside the shared ceiling must admit exactly the edges the
// unbounded read returned, in the same order, as the same pointers.
func TestIncrementalFrontierAdmitsNormalIncomingUnchanged(t *testing.T) {
	g, changed := incomingFanOutGraph(t, 64)

	frontier := collectIncrementalFileFrontier(g, []string{changed}, nil)
	if frontier.incomingRefusal != nil {
		t.Fatalf("a 64-edge fan-out was refused: %v (%+v)", frontier.incomingRefusal, frontier.incomingAdmission)
	}
	if frontier.incomingAdmission.Refused || frontier.incomingAdmission.Inspected != 64 {
		t.Fatalf("completeness fact = %+v, want 64 inspected rows and no refusal", frontier.incomingAdmission)
	}

	var want []*graph.Edge
	inByStub := g.GetInEdgesByNodeIDs(frontier.stubKeys)
	for _, key := range frontier.stubKeys {
		for _, edge := range inByStub[key] {
			if edge != nil && graph.IsUnresolvedTarget(edge.To) {
				want = append(want, edge)
			}
		}
	}
	got := frontier.pending[frontier.outgoingPending:]
	if len(got) != len(want) {
		t.Fatalf("bounded admission changed the frontier: %d incoming edges, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("incoming edge %d differs from the unbounded read: %+v vs %+v", i, got[i], want[i])
		}
	}

	// The whole batch still admits every parked reference: 64 rows through the
	// preparation leg and the same 64 through the resolution leg, none refused.
	stats := New(g).ResolveFilesAndIncoming([]string{changed})
	if stats.IncomingAdmissionRefused {
		t.Fatalf("normal batch reported a refused admission: %+v", stats)
	}
	if stats.IncomingAdmissionInspected != 128 {
		t.Fatalf("inspected rows = %d, want 64 preparation + 64 resolution", stats.IncomingAdmissionInspected)
	}
	// And the admitted edges are actually rebound: the same 64 references the
	// refusal case below leaves parked.
	if left := unresolvedInEdgeCount(g, graph.UnresolvedMarker+"Close"); left != 0 {
		t.Fatalf("%d incoming references stayed parked on an admitted batch", left)
	}
}

// The adversarial fan-out must produce the typed limit error and a
// completeness fact, and must admit NOTHING: no partial projection, no
// partial write-back.
func TestIncrementalFrontierRefusesAdversarialIncomingFanOut(t *testing.T) {
	fanIn := graph.MaxIncomingSourceCandidateRows + 1
	g, changed := incomingFanOutGraph(t, fanIn)
	stub := graph.UnresolvedMarker + "Close"

	frontier := collectIncrementalFileFrontier(g, []string{changed}, nil)
	var limit *graph.BoundedLocalizationLimitError
	if !errors.As(frontier.incomingRefusal, &limit) {
		t.Fatalf("frontier refusal = %v, want *graph.BoundedLocalizationLimitError", frontier.incomingRefusal)
	}
	if limit.Limit != graph.MaxIncomingSourceCandidateRows {
		t.Fatalf("refusal limit = %d, want the shared incoming-source ceiling %d", limit.Limit, graph.MaxIncomingSourceCandidateRows)
	}
	if !frontier.incomingAdmission.Refused {
		t.Fatalf("completeness fact = %+v, want Refused", frontier.incomingAdmission)
	}
	if len(frontier.pending) != frontier.outgoingPending {
		t.Fatalf("refused frontier admitted %d incoming edges; a refusal must admit none",
			len(frontier.pending)-frontier.outgoingPending)
	}
	if frontier.outgoingPending != 1 {
		t.Fatalf("the outgoing leg was collateral: outgoing pending = %d, want 1", frontier.outgoingPending)
	}

	// Production entrypoint: the batched incremental pass reaches the bound,
	// carries the fact on its stats, and writes back nothing from the leg.
	stats := New(g).ResolveFilesAndIncoming([]string{changed})
	if !stats.IncomingAdmissionRefused {
		t.Fatalf("ResolveFilesAndIncoming did not surface the refusal: %+v", stats)
	}
	if stats.IncomingAdmissionLimit != graph.MaxIncomingSourceCandidateRows {
		t.Fatalf("stats ceiling = %d, want %d", stats.IncomingAdmissionLimit, graph.MaxIncomingSourceCandidateRows)
	}
	if left := unresolvedInEdgeCount(g, stub); left != fanIn {
		t.Fatalf("%d of %d parked references were rebound by a refused admission", fanIn-left, fanIn)
	}
	// The forward leg is unaffected: the batch still did its own file's work.
	if stats.Resolved != 1 {
		t.Fatalf("outgoing resolution = %d, want the changed file's own edge bound", stats.Resolved)
	}

	// The receipt-exact names entrypoint carries the same bound.
	nameStats := New(g).ResolveIncomingForNames([]string{"Close"}, nil)
	if !nameStats.IncomingAdmissionRefused || nameStats.Resolved != 0 {
		t.Fatalf("ResolveIncomingForNames bypassed the bound: %+v", nameStats)
	}
	if left := unresolvedInEdgeCount(g, stub); left != fanIn {
		t.Fatalf("the names entrypoint rebound %d parked references", fanIn-left)
	}
}

// F1 — ResolveFileAndIncoming is the per-save hot path and the most exposed
// entrypoint to this bound. When the changed file holds no outgoing unresolved
// edge, a refused incoming leg empties the whole pending frontier and the pass
// takes its "nothing to do" early return. Without the fact recorded before that
// return the save is indistinguishable from a genuine no-op: no stats, no log,
// no outcome — the exact silent truncation the bound exists to prevent.
func TestResolveFileAndIncomingReportsRefusalWithNoOutgoingWork(t *testing.T) {
	fanIn := graph.MaxIncomingSourceCandidateRows + 1
	g, changed := incomingFanOutGraphMode(t, fanIn, false)
	stub := graph.UnresolvedMarker + "Close"

	logs, observed := observedResolverLogger()
	r := New(g)
	r.SetLogger(logs)
	stats := r.ResolveFileAndIncoming(changed)

	if stats == nil {
		t.Fatal("per-save pass returned no stats at all")
	}
	if !stats.IncomingAdmissionRefused {
		t.Fatalf("refused reverse pass reported as a clean no-op: %+v", stats)
	}
	if stats.IncomingAdmissionLimit != graph.MaxIncomingSourceCandidateRows {
		t.Fatalf("stats ceiling = %d, want %d", stats.IncomingAdmissionLimit, graph.MaxIncomingSourceCandidateRows)
	}
	if stats.IncomingAdmissionDropped != fanIn {
		t.Fatalf("dropped rows = %d, want every parked reference %d", stats.IncomingAdmissionDropped, fanIn)
	}
	if stats.IncomingAdmissionInspected != fanIn {
		t.Fatalf("inspected rows = %d, want %d", stats.IncomingAdmissionInspected, fanIn)
	}
	if left := unresolvedInEdgeCount(g, stub); left != fanIn {
		t.Fatalf("%d of %d parked references were rebound by a refused admission", fanIn-left, fanIn)
	}
	// Both production callers of this entrypoint discard the stats, so the Warn
	// is the only channel that reaches them.
	entries := observed.FilterMessage("resolver: incoming stub admission refused").All()
	if len(entries) != 1 {
		t.Fatalf("refusal log records = %d, want exactly 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["leg"] != "preparation" {
		t.Fatalf("refusal logged for leg %v, want preparation", fields["leg"])
	}
	if fields["dropped"] != int64(fanIn) {
		t.Fatalf("logged dropped = %v, want %d", fields["dropped"], fanIn)
	}
}

// The complement: an in-ceiling reverse pass on a file with no outgoing work
// must stay a complete pass — no refusal fact, no Warn, and the parked
// references actually rebound. Without it the test above could be satisfied by
// flagging every pass as refused.
func TestResolveFileAndIncomingKeepsAdmittedPassClean(t *testing.T) {
	g, changed := incomingFanOutGraphMode(t, 64, false)

	logs, observed := observedResolverLogger()
	r := New(g)
	r.SetLogger(logs)
	stats := r.ResolveFileAndIncoming(changed)

	if stats.IncomingAdmissionRefused || stats.IncomingAdmissionDropped != 0 {
		t.Fatalf("a 64-edge fan-out was reported as bounded: %+v", stats)
	}
	if left := unresolvedInEdgeCount(g, graph.UnresolvedMarker+"Close"); left != 0 {
		t.Fatalf("%d incoming references stayed parked on an admitted batch", left)
	}
	if n := observed.FilterMessage("resolver: incoming stub admission refused").Len(); n != 0 {
		t.Fatalf("clean pass logged %d refusals", n)
	}
}

// F6 — the phase logging keys on `outcome`. A pass whose incoming leg was
// refused must never be reported as complete, nor as having had no pending work
// at all.
func TestResolveFilesAndIncomingNeverLabelsARefusedPassComplete(t *testing.T) {
	for _, tc := range []struct {
		name         string
		withOutgoing bool
		wantRefused  bool
		wantOutcome  string
	}{
		{name: "refused_with_outgoing_work", withOutgoing: true, wantRefused: true, wantOutcome: "incoming_refused"},
		{name: "refused_without_outgoing_work", withOutgoing: false, wantRefused: true, wantOutcome: "incoming_refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, changed := incomingFanOutGraphMode(t, graph.MaxIncomingSourceCandidateRows+1, tc.withOutgoing)
			logs, observed := observedResolverLogger()
			r := New(g)
			r.SetLogger(logs)
			stats := r.ResolveFilesAndIncoming([]string{changed})
			if stats.IncomingAdmissionRefused != tc.wantRefused {
				t.Fatalf("refused = %v, want %v (%+v)", stats.IncomingAdmissionRefused, tc.wantRefused, stats)
			}
			entries := observed.FilterMessage("resolver: incremental files phases").All()
			if len(entries) != 1 {
				t.Fatalf("phase log records = %d, want 1", len(entries))
			}
			fields := entries[0].ContextMap()
			if fields["outcome"] != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", fields["outcome"], tc.wantOutcome)
			}
			if fields["incoming_admission_refused"] != tc.wantRefused {
				t.Fatalf("phase log refusal flag = %v, want %v", fields["incoming_admission_refused"], tc.wantRefused)
			}
		})
	}

	// A complete pass keeps its ordinary label.
	g, changed := incomingFanOutGraphMode(t, 64, true)
	logs, observed := observedResolverLogger()
	r := New(g)
	r.SetLogger(logs)
	if stats := r.ResolveFilesAndIncoming([]string{changed}); stats.IncomingAdmissionRefused {
		t.Fatalf("in-ceiling batch reported refused: %+v", stats)
	}
	fields := observed.FilterMessage("resolver: incremental files phases").All()[0].ContextMap()
	if fields["outcome"] != "complete" {
		t.Fatalf("complete pass outcome = %v, want complete", fields["outcome"])
	}
}

// F2 — the frontier's documented "one incoming-stub read" is the invariant the
// batch hot-path guard (batch_hotpaths_test.go) protects, and that guard runs
// under the 256-key scoped cap where any chunking is invisible. Pin the real
// call count past the cap.
func TestIncrementalFrontierKeepsOneIncomingReadPastTheScopedKeyCap(t *testing.T) {
	g := graph.New()
	// Four candidate stub forms per referenceable symbol, so 200 definitions in
	// 200 changed files carry well past MaxBoundedAdjacencyKeys (256) keys.
	const files = 200
	paths := make([]string, 0, files)
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := range files {
		path := fmt.Sprintf("pkg/def%03d.go", i)
		defID := fmt.Sprintf("%s::Def%03d", path, i)
		callerPath := fmt.Sprintf("caller/c%03d.go", i)
		callerID := callerPath + "::Caller"
		paths = append(paths, path)
		nodes = append(nodes,
			&graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path},
			&graph.Node{ID: defID, Kind: graph.KindFunction, Name: fmt.Sprintf("Def%03d", i), FilePath: path},
			&graph.Node{ID: callerPath, Kind: graph.KindFile, Name: callerPath, FilePath: callerPath},
			&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "Caller", FilePath: callerPath},
		)
		edges = append(edges, &graph.Edge{
			From: callerID, To: graph.UnresolvedMarker + fmt.Sprintf("Def%03d", i),
			Kind: graph.EdgeCalls, FilePath: callerPath, Line: 3,
		})
	}
	g.AddBatch(nodes, edges)

	counting := &resolverBatchCountingStore{Store: g}
	frontier := New(counting).collectIncrementalFileFrontier(paths)
	if len(frontier.stubKeys) <= graph.MaxBoundedAdjacencyKeys {
		t.Fatalf("fixture built %d stub keys, need more than the scoped key cap %d",
			len(frontier.stubKeys), graph.MaxBoundedAdjacencyKeys)
	}
	if counting.getInEdgesByNodeIDsCalls != 1 {
		t.Fatalf("incoming-stub reads = %d for %d keys, want the documented single batched read",
			counting.getInEdgesByNodeIDsCalls, len(frontier.stubKeys))
	}
	if got := len(frontier.pending); got != files {
		t.Fatalf("pending frontier = %d, want %d admitted incoming edges", got, files)
	}
	if frontier.incomingRefusal != nil || frontier.incomingAdmission.Refused {
		t.Fatalf("an in-ceiling %d-row admission was refused: %+v", files, frontier.incomingAdmission)
	}
}
