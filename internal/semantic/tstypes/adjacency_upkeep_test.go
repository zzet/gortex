package tstypes

import (
	"fmt"
	"sort"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// Shapes that reach the applier's own adjacency upkeep for split calls, kept
// as literal fixtures so the package-wide parity corpus harvests them too.

// repeatedStublessCallFixture makes one caller apply the same desugared
// operator call twice on different lines. Desugared operator calls carry no
// extracted stub, so each source site must mint its own calls edge
// (addASTEdge). Reapplying the facts must reuse those site-specific edges.
func repeatedStublessCallFixture() map[string]string {
	return map[string]string{
		"A.kt": `class A {
    operator fun plus(o: A): A {
        return this
    }
}
`,
		"App.kt": `class App {
    fun run(a: A, b: A) {
        val c = a + b
        val d = a + b
    }
}
`,
	}
}

// topLevelCallFixture holds module-level calls, which the Python and
// TypeScript extractors park on the file node, beside the same calls inside
// a function.
func topLevelCallFixture() map[string]string {
	return map[string]string{
		"app/a.py": `class A:
    def run(self):
        pass

A().run()
x = A()
x.run()

def go():
    y = A()
    y.run()
`,
		"src/a.ts": `export class A {
  run(): void {}
}
const x = new A();
x.run();
function go(): void {
  const y = new A();
  y.run();
}
`,
	}
}

// Each upkeep fixture runs legacy versus filtered on both backends, through
// the streamed pass and per file, and on the compatibility store paths.
func TestAdjacencyUpkeepParity(t *testing.T) {
	for name, fixture := range map[string]map[string]string{
		"repeated_stubless_call": repeatedStublessCallFixture(),
		"top_level_calls":        topLevelCallFixture(),
	} {
		providers := fixtureProviders(t, fixture)
		if len(providers) == 0 {
			t.Fatalf("%s: no provider serves the fixture", name)
		}
		for _, sqlite := range []bool{false, true} {
			for _, compat := range []bool{false, true} {
				for _, perFile := range []bool{false, true} {
					label := fmt.Sprintf("%s sqlite=%v compat=%v perFile=%v", name, sqlite, compat, perFile)
					run := parityRun{sqlite: sqlite, fan: 8, perFile: perFile, compat: compat}
					filtered := enrichSnapshot(t, label, fixture, providers, run)
					run.full = true
					legacy := enrichSnapshot(t, label, fixture, providers, run)
					diffParity(t, label, legacy, filtered)
				}
			}
		}
	}
}

// Distinct source sites retain distinct edges. A second enrichment must reuse
// each site's minted edge rather than duplicate it or collapse the two sites.
func TestRepeatedStublessCallReusesMintedEdge(t *testing.T) {
	g, dir := buildFixture(t, repeatedStublessCallFixture())
	caller := nodeByNameKind(t, g, "run", graph.KindMethod)
	plus := nodeByNameKind(t, g, "plus", graph.KindMethod)
	for _, e := range g.GetOutEdges(caller.ID) {
		if e.Kind == graph.EdgeCalls {
			t.Fatalf("fixture no longer stubless: extractor emitted %s -> %s", e.From, e.To)
		}
	}
	res, err := NewProvider(KotlinSpec(), zap.NewNop()).EnrichRepo(g, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	checkSites := func() {
		t.Helper()
		lines := map[int]int{}
		for _, e := range g.GetOutEdges(caller.ID) {
			if e.Kind == graph.EdgeCalls && e.To == plus.ID {
				lines[e.Line]++
			}
		}
		if len(lines) != 2 || lines[3] != 1 || lines[4] != 1 {
			t.Fatalf("call sites = %v, want one edge each on lines 3 and 4", lines)
		}
	}
	checkSites()
	if res.EdgesAdded != 2 {
		t.Fatalf("added=%d, want one edge per source site", res.EdgesAdded)
	}
	res, err = NewProvider(KotlinSpec(), zap.NewNop()).EnrichRepo(g, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	checkSites()
	if res.EdgesAdded != 0 || res.EdgesConfirmed != 0 {
		t.Fatalf("repeated enrichment added=%d confirmed=%d, want to reuse both resolved sites",
			res.EdgesAdded, res.EdgesConfirmed)
	}
}

// callsPageReads is the caller-calls reads one page (or one EnrichFile)
// issued: store queries and the node IDs they requested.
type callsPageReads struct {
	queries, ids int
}

func (s *adjacencyLoadStore) callReads() callsPageReads {
	r := callsPageReads{queries: s.callQueries}
	for _, n := range s.callIDs {
		r.ids += n
	}
	return r
}

func (r callsPageReads) minus(o callsPageReads) callsPageReads {
	return callsPageReads{queries: r.queries - o.queries, ids: r.ids - o.ids}
}

// A calls page reads its callers' outgoing calls in one batch
// (loadCallerCalls) before the first buildIndex. The one-node fallback in
// outCalls keeps a skipped batch correct, so only the query count shows it:
// one query per caller instead of one per page.
func TestCallsPhaseLoadsCallerCallsInOneBatch(t *testing.T) {
	s, root := hubPreloadFixture(t, 4, 3)
	loads := &adjacencyLoadStore{Store: s, kinds: map[graph.EdgeKind]int{}}
	p := NewProvider(CSharpSpec(), zap.NewNop())
	var pages []callsPageReads
	var seen callsPageReads
	p.observePage = func(stats factPageStats) {
		now := loads.callReads()
		if stats.Class == classCalls {
			pages = append(pages, now.minus(seen))
		} else if d := now.minus(seen); d.queries != 0 {
			t.Errorf("%v page issued %d caller-calls queries, want 0", stats.Class, d.queries)
		}
		seen = now
	}
	if _, err := p.EnrichRepo(loads, "", root); err != nil {
		t.Fatal(err)
	}
	callers := 0
	for i, page := range pages {
		callers += page.ids
		if page.queries > 1 {
			t.Errorf("calls page %d issued %d caller-calls queries for %d callers, want one batch", i, page.queries, page.ids)
		}
	}
	if callers < 2 {
		t.Fatalf("calls pages %v read %d callers: the fixture no longer exercises the batch", pages, callers)
	}

	// EnrichFile runs every phase on one applier: still one batch per file.
	s2, root2 := hubPreloadFixture(t, 4, 3)
	files := make([]string, 0, len(goldenFixtureFiles()))
	for rel := range goldenFixtureFiles() {
		files = append(files, rel)
	}
	sort.Strings(files)
	batched := 0
	for _, rel := range files {
		perFile := &adjacencyLoadStore{Store: s2, kinds: map[graph.EdgeKind]int{}}
		if _, err := NewProvider(CSharpSpec(), zap.NewNop()).EnrichFile(perFile, root2, rel); err != nil {
			t.Fatal(err)
		}
		r := perFile.callReads()
		if r.queries > 1 {
			t.Errorf("EnrichFile %s issued %d caller-calls queries for %d callers, want one batch", rel, r.queries, r.ids)
		}
		if r.ids > 1 {
			batched++
		}
	}
	if batched == 0 {
		t.Fatal("no fixture file has two callers: EnrichFile does not exercise the batch")
	}
}

// compatOnlyStore hides every optional capability of the store it wraps, so
// the applier takes its compatibility paths: loadPageFileNodes projects the
// page files' nodes from GetRepoNodes, file nodes included, and the frontier
// preload then loads the file nodes' adjacency too.
type compatOnlyStore struct{ graph.Store }

// Python and TypeScript park module-level calls on the file node. The file
// node is never a caller (buildIndex and applyCall consider callables only),
// so the caller batch must not load its calls — even on the compatibility
// path, where the file node's structural adjacency IS loaded — and the
// top-level calls stay unresolved stubs (in both preload modes:
// TestAdjacencyUpkeepParity).
func TestCallerCallsSkipFileNodes(t *testing.T) {
	fixture := topLevelCallFixture()
	for _, compat := range []bool{false, true} {
		for _, perFile := range []bool{false, true} {
			label := fmt.Sprintf("compat=%v perFile=%v", compat, perFile)
			g, dir := buildFixture(t, fixture)
			fileCalls := map[string]int{}
			for rel := range fixture {
				for _, n := range g.GetFileNodes(rel) {
					if n.Kind != graph.KindFile {
						continue
					}
					for _, e := range g.GetOutEdges(n.ID) {
						if e.Kind == graph.EdgeCalls {
							fileCalls[n.ID]++
						}
					}
				}
			}
			if len(fileCalls) != len(fixture) {
				t.Fatalf("file nodes with calls %v: want every fixture file's top-level calls parked on its file node", fileCalls)
			}
			var s graph.Store = g
			if compat {
				s = compatOnlyStore{g}
			}
			var fileLoaded, callersLoaded int
			var failures []string
			observe := func(ap *applier) {
				for id := range ap.callsLoaded {
					if fileCalls[id] > 0 {
						failures = append(failures, id)
					}
				}
				if len(ap.callsLoaded) == 0 {
					return
				}
				callersLoaded += len(ap.callsLoaded)
				for id := range fileCalls {
					if ap.outLoaded[id] {
						fileLoaded++
					}
				}
			}
			for _, base := range fixtureProviders(t, fixture) {
				p := NewProvider(base.spec, zap.NewNop())
				p.observePreload = observe
				if perFile {
					for rel := range fixture {
						if _, err := p.EnrichFile(s, dir, rel); err != nil {
							t.Fatal(err)
						}
					}
					continue
				}
				if _, err := p.EnrichRepo(s, "", dir); err != nil {
					t.Fatal(err)
				}
			}
			if len(failures) > 0 {
				t.Fatalf("%s: caller batch loaded the calls of file nodes %v", label, failures)
			}
			if callersLoaded == 0 || (compat && fileLoaded == 0) {
				t.Fatalf("%s: callers loaded %d, file nodes with loaded adjacency %d: the fixture no longer exercises the skip",
					label, callersLoaded, fileLoaded)
			}
			for id := range fileCalls {
				for _, e := range g.GetOutEdges(id) {
					if e.Kind == graph.EdgeCalls && e.Origin == graph.OriginASTResolved {
						t.Errorf("%s: top-level call %s -> %s resolved through the file node", label, e.From, e.To)
					}
				}
			}
		}
	}
}

// splitCalls is derived from the outgoing kinds, never set on its own: calls
// are split out exactly when the outgoing preload does not load them.
func TestSplitCallsFollowsOutgoingKinds(t *testing.T) {
	structural := adjacencyOutKinds(CSharpSpec())
	withCalls := append(append([]graph.EdgeKind{}, structural...), graph.EdgeCalls)
	inWithCalls := append(append([]graph.EdgeKind{}, adjacencyInKinds...), graph.EdgeCalls)
	for _, tc := range []struct {
		name    string
		out, in []graph.EdgeKind
		split   bool
	}{
		{"legacy all kinds", nil, nil, false},
		{"structural", structural, adjacencyInKinds, true},
		{"outgoing calls with the frontier", withCalls, adjacencyInKinds, false},
		{"incoming calls", structural, inWithCalls, true},
	} {
		ap := newApplier(graph.New(), CSharpSpec(), "csharp-types")
		ap.setAdjacencyKinds(tc.out, tc.in)
		if ap.splitCalls != tc.split {
			t.Errorf("%s: splitCalls=%v, want %v", tc.name, ap.splitCalls, tc.split)
		}
	}
	p := NewProvider(CSharpSpec(), zap.NewNop())
	p.fullAdjacency = true
	if ap := p.newApplier(graph.New()); ap.splitCalls || ap.outKinds != nil || ap.inKinds != nil {
		t.Fatalf("fullAdjacency applier out=%v in=%v splitCalls=%v, want the legacy preload", ap.outKinds, ap.inKinds, ap.splitCalls)
	}
}

// The split calls branches of the mutation upkeep maintain callsByID and must
// still keep incoming lists — and hot in entries — coherent when the incoming
// preload loads calls: nothing exempts calls from the in-list upkeep.
func TestSplitCallsUpkeepKeepsIncomingCalls(t *testing.T) {
	g := graph.New()
	caller := &graph.Node{ID: "repo/a.cs::A.M", Kind: graph.KindMethod, Name: "M", FilePath: "repo/a.cs", Language: "csharp", StartLine: 1, EndLine: 3}
	first := &graph.Node{ID: "repo/b.cs::B.Run", Kind: graph.KindMethod, Name: "Run", FilePath: "repo/b.cs", Language: "csharp", StartLine: 1, EndLine: 1}
	second := &graph.Node{ID: "repo/c.cs::C.Run", Kind: graph.KindMethod, Name: "Run", FilePath: "repo/c.cs", Language: "csharp", StartLine: 1, EndLine: 1}
	g.AddBatch([]*graph.Node{caller, first, second}, nil)
	inKinds := append(append([]graph.EdgeKind{}, adjacencyInKinds...), graph.EdgeCalls)
	hot := newApplyHotCache(defaultApplyHotCacheBytes)
	page := func() *applier {
		ap := newApplier(g, CSharpSpec(), "csharp-types").withHotCache(hot)
		ap.setAdjacencyKinds(adjacencyOutKinds(CSharpSpec()), inKinds)
		if !ap.splitCalls {
			t.Fatal("calls not split")
		}
		ap.loadAdjacency([]string{caller.ID, first.ID, second.ID})
		return ap
	}
	coherent := func(step string, ap *applier) {
		t.Helper()
		for _, ap := range []*applier{ap, page()} {
			fresh := g.GetInEdgesByNodeIDs([]string{first.ID, second.ID})
			for _, id := range []string{first.ID, second.ID} {
				if got, want := byKindIdentities(ap.inByID[id], nil), byKindIdentities(fresh[id], inKinds); got != want {
					t.Fatalf("after %s: in-list of %s\n  served %s\n  store  %s", step, id, got, want)
				}
			}
		}
	}

	ap := page()
	e := ap.addASTEdge(caller.ID, first.ID, graph.EdgeCalls, "repo/a.cs", 2, strategyDirect, astConfidence)
	coherent("add", ap)

	ap = page()
	for _, c := range ap.outCalls(caller.ID) {
		if c.To == first.ID {
			e = c
		}
	}
	e.To = second.ID
	ap.reindexEdge(e, first.ID)
	coherent("retarget", ap)

	ap = page()
	ap.removeEdge(ap.outCalls(caller.ID)[0])
	coherent("remove", ap)
}
