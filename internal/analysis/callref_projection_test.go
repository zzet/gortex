package analysis

import (
	"fmt"
	"iter"
	"math"
	"runtime"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// orderedStore streams the in-memory graph in a fixed order, as the SQLite
// store does, so a direct read and a replay can be compared operation for
// operation. It counts its scans and can slow every row down.
type orderedStore struct {
	graph.Store
	nodeScans, edgeScans atomic.Int64
	rows                 atomic.Int64
	perRow               time.Duration
}

func (o *orderedStore) spin() {
	o.rows.Add(1)
	if o.perRow <= 0 {
		return
	}
	for start := time.Now(); time.Since(start) < o.perRow; {
	}
}

func (o *orderedStore) NodesLightSeq() iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		o.nodeScans.Add(1)
		var nodes []*graph.Node
		for n := range graph.NodesLightSeq(o.Store) {
			nodes = append(nodes, n)
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
		for _, n := range nodes {
			o.spin()
			if !yield(n) {
				return
			}
		}
	}
}

func (o *orderedStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		o.edgeScans.Add(1)
		var edges []*graph.Edge
		for e := range graph.EdgesLightSeq(o.Store, kinds...) {
			edges = append(edges, e)
		}
		sort.SliceStable(edges, func(i, j int) bool {
			a, b := edges[i], edges[j]
			if a.From != b.From {
				return a.From < b.From
			}
			if a.To != b.To {
				return a.To < b.To
			}
			if a.Kind != b.Kind {
				return a.Kind < b.Kind
			}
			return a.Line < b.Line
		})
		for _, e := range edges {
			o.spin()
			if !yield(e) {
				return
			}
		}
	}
}

// projectionFixture is a call/reference graph with every shape the consumers
// treat specially: every provenance tier (and an unset origin backfilled from
// the confidence), duplicate edges, self-loops, a proxy node and edges to it,
// dangling targets, and edge kinds the consumers ignore.
func projectionFixture(nodes int) *orderedStore {
	g := graph.New()
	for i := 0; i < nodes; i++ {
		g.AddNode(&graph.Node{ID: fmt.Sprintf("pkg%d/f.go::F%d", i%7, i), Kind: graph.KindFunction, Name: fmt.Sprintf("F%d", i), FilePath: fmt.Sprintf("pkg%d/f.go", i%7)})
	}
	g.AddNode(&graph.Node{ID: "remote:peer~x/y.go::R", Kind: graph.KindFunction, Name: "R", Stub: true, Origin: "peer"})
	origins := []string{graph.OriginLSPResolved, graph.OriginLSPDispatch, graph.OriginASTResolved, graph.OriginASTInferred, graph.OriginTextMatched, ""}
	for i := 0; i < nodes; i++ {
		from := fmt.Sprintf("pkg%d/f.go::F%d", i%7, i)
		for k, step := range []int{1, 3, 7, 11} {
			j := (i + step) % nodes
			kind := graph.EdgeCalls
			if k%2 == 1 {
				kind = graph.EdgeReferences
			}
			g.AddEdge(&graph.Edge{From: from, To: fmt.Sprintf("pkg%d/f.go::F%d", j%7, j), Kind: kind, Origin: origins[(i+k)%len(origins)], Confidence: float64((i+k)%10) / 10, Line: k + 1})
		}
		// A hub with hundreds of in-links of mixed weights: its score is a
		// long floating-point sum, so any change in edge order shows in the
		// last bits.
		if i%3 == 0 {
			g.AddEdge(&graph.Edge{From: from, To: "pkg0/f.go::F0", Kind: graph.EdgeCalls, Origin: origins[i%len(origins)], Confidence: 0.3, Line: 50})
		}
		if i%5 == 0 {
			g.AddEdge(&graph.Edge{From: from, To: from, Kind: graph.EdgeCalls, Line: 99})
			g.AddEdge(&graph.Edge{From: from, To: "remote:peer~x/y.go::R", Kind: graph.EdgeCalls, Line: 98})
			g.AddEdge(&graph.Edge{From: from, To: "unresolved::Missing", Kind: graph.EdgeReferences, Line: 97})
			g.AddEdge(&graph.Edge{From: from, To: fmt.Sprintf("pkg%d/f.go::F%d", (i+1)%7, (i+1)%nodes), Kind: graph.EdgeImports, Line: 96})
		}
	}
	return &orderedStore{Store: g}
}

// Over a projection, PageRank, the adjacency snapshot and HITS equal their
// direct reads of the store exactly (every score bit, every CSR slot), and the
// store is read once instead of three times.
func TestCallRefProjectionReplaysTheStoreExactly(t *testing.T) {
	direct := projectionFixture(400)
	wantPR := ComputePageRank(direct)
	wantAdj := BuildAdjacencySnapshot(direct)
	wantHITS := ComputeHITS(direct)
	if n, e := direct.nodeScans.Load(), direct.edgeScans.Load(); n != 3 || e != 3 {
		t.Fatalf("fixture precondition: the direct reads scanned %d/%d times, want 3/3", n, e)
	}

	shared := projectionFixture(400)
	projection := NewCallRefProjection(shared, nil)
	gotPR := ComputePageRankPaced(projection, nil)
	gotAdj := BuildAdjacencySnapshotPaced(projection, nil)
	gotHITS := ComputeHITSPaced(projection, nil)

	if fmt.Sprint(gotPR) != fmt.Sprint(wantPR) {
		t.Fatalf("PageRank over the projection differs from the direct read")
	}
	for id, v := range wantPR.Scores {
		if gotPR.Scores[id] != v {
			t.Fatalf("PageRank of %s: projection %v, direct %v", id, gotPR.Scores[id], v)
		}
	}
	if !adjacencyEqual(gotAdj, wantAdj) {
		t.Fatalf("the adjacency snapshot over the projection differs from the direct read")
	}
	// HITS normalizes by summing over a Go map, so two direct runs over the
	// same store already differ in the last bits (map iteration order). The
	// projection must stay inside that envelope: the bound is the direct
	// run's own run-to-run spread, measured here, plus one ulp-scale margin.
	again := ComputeHITS(projectionFixture(400))
	spread := hitsSpread(wantHITS, again)
	if d := hitsSpread(wantHITS, gotHITS); d > spread+1e-15 {
		t.Fatalf("HITS over the projection differs by %g from the direct read; direct runs differ by %g", d, spread)
	}
	if gotPR.Max != wantPR.Max {
		t.Fatalf("PageRank maxima differ")
	}
	if n, e := projection.StoreScans(); n != 1 || e != 1 {
		t.Fatalf("the projection read the store %d/%d times, want 1/1", n, e)
	}
	if n, e := shared.nodeScans.Load(), shared.edgeScans.Load(); n != 1 || e != 1 {
		t.Fatalf("the store was scanned %d/%d times, want 1/1", n, e)
	}
}

func adjacencyEqual(a, b *AdjacencySnapshot) bool {
	if len(a.ids) != len(b.ids) || len(a.neighbors) != len(b.neighbors) {
		return false
	}
	for i := range a.ids {
		if a.ids[i] != b.ids[i] || a.outWeight[i] != b.outWeight[i] {
			return false
		}
	}
	for i := range a.offsets {
		if a.offsets[i] != b.offsets[i] {
			return false
		}
	}
	for i := range a.neighbors {
		if a.neighbors[i] != b.neighbors[i] || a.weights[i] != b.weights[i] {
			return false
		}
	}
	return fmt.Sprint(a.pkgRoots) == fmt.Sprint(b.pkgRoots)
}

// A consumer that stops early leaves no record: the next read goes to the
// store again rather than replaying a partial corpus.
func TestCallRefProjectionKeepsNoPartialRecord(t *testing.T) {
	store := projectionFixture(50)
	projection := NewCallRefProjection(store, nil)
	for range projection.EdgesLightSeq(graph.EdgeCalls, graph.EdgeReferences) {
		break
	}
	count := 0
	for range projection.EdgesLightSeq(graph.EdgeCalls, graph.EdgeReferences) {
		count++
	}
	if _, e := projection.StoreScans(); e != 2 {
		t.Fatalf("an interrupted read was replayed: %d store scans, want 2", e)
	}
	full := 0
	for range graph.EdgesLightSeq(store, graph.EdgeCalls, graph.EdgeReferences) {
		full++
	}
	if count != full {
		t.Fatalf("the second read saw %d edges, the store has %d", count, full)
	}
}

// parkLatency runs analyze over a slowed store at GOMAXPROCS=1, raises the
// yield predicate once the scan is under way, and returns how long the pass
// took to park, and how many rows it read while it should have been parked.
func parkLatency(t *testing.T, analyze func(graph.Store, *Pace)) (time.Duration, int64) {
	t.Helper()
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	store := projectionFixture(3000)
	store.perRow = 20 * time.Microsecond
	var editing atomic.Bool
	pace := NewPace(editing.Load)
	parked := make(chan time.Time, 1)
	pace.onPark = func() {
		select {
		case parked <- time.Now():
		default:
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		analyze(store, pace)
	}()
	for store.rows.Load() < 3600 { // past the node scan: mid edge scan
		time.Sleep(time.Millisecond)
	}
	raised := time.Now()
	editing.Store(true)
	var at time.Time
	select {
	case at = <-parked:
	case <-time.After(5 * time.Second):
		editing.Store(false)
		<-done
		t.Fatal("the pass never parked")
	}
	// While parked the pass reads nothing.
	rowsAtPark := store.rows.Load()
	time.Sleep(200 * time.Millisecond)
	moved := store.rows.Load() - rowsAtPark
	editing.Store(false)
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the pass never finished after the edit")
	}
	return at.Sub(raised), moved
}

// A background pass gives the core back within 100 ms of an edit cycle (or a
// tool call) starting, in the middle of a PageRank, HITS, adjacency or Leiden
// scan, and reads nothing while it is parked.
func TestPacedAnalysisParksMidScanWithinTheBound(t *testing.T) {
	cases := map[string]func(graph.Store, *Pace){
		"pagerank":  func(g graph.Store, p *Pace) { ComputePageRankPaced(g, p) },
		"hits":      func(g graph.Store, p *Pace) { ComputeHITSPaced(g, p) },
		"adjacency": func(g graph.Store, p *Pace) { BuildAdjacencySnapshotPaced(g, p) },
		"leiden":    func(g graph.Store, p *Pace) { DetectCommunitiesLeidenIncrementalPaced(g, nil, p) },
		"projection": func(g graph.Store, p *Pace) {
			projection := NewCallRefProjection(g, p)
			ComputePageRankPaced(projection, p)
			ComputeHITSPaced(projection, p)
		},
	}
	for name, analyze := range cases {
		t.Run(name, func(t *testing.T) {
			latency, moved := parkLatency(t, analyze)
			if latency > 100*time.Millisecond {
				t.Fatalf("the pass parked %v after the edit began, want at most 100ms", latency)
			}
			if moved != 0 {
				t.Fatalf("the pass read %d rows while it should have been parked", moved)
			}
		})
	}
}

// A nil Pace never parks: every existing caller keeps its behaviour.
func TestNilPaceNeverParks(t *testing.T) {
	var p *Pace
	for i := 0; i < 1000; i++ {
		p.Tick()
	}
	if parks, _ := p.Stats(); parks != 0 {
		t.Fatal("a nil pace parked")
	}
	if NewPace(nil) != nil {
		t.Fatal("a nil predicate made a pace")
	}
}

// hitsSpread is the largest relative difference between two HITS results.
func hitsSpread(a, b *HITSResult) float64 {
	worst := 0.0
	rel := func(x, y float64) float64 {
		d := math.Abs(x - y)
		if m := math.Max(math.Abs(x), math.Abs(y)); m > 0 {
			d /= m
		}
		return d
	}
	for id, v := range a.Authorities {
		worst = math.Max(worst, rel(v, b.Authorities[id]))
		worst = math.Max(worst, rel(a.Hubs[id], b.Hubs[id]))
	}
	if len(a.Authorities) != len(b.Authorities) {
		return math.Inf(1)
	}
	return worst
}
