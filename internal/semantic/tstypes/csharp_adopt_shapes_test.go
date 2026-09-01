package tstypes

// Shape pins for stub adoption beyond the core four in
// csharp_accessor_attribution_test.go: the initializer lanes, chained
// heads, pre-resolved stubs, snapshot-hygiene guards, and the tie
// semantics (line containment breaks a tie only WITHIN the tied owner
// set — a callable that merely shares the line never collects a call it
// did not author).

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

const csSvcInt = `namespace A {
    public class Svc {
        public int Run() { return 1; }
        public int Stop() { return 2; }
    }
}
`

// A property INITIALIZER call (not an accessor body) sharing a line with
// a method: the stub rides the property, the line-keyed lookup answers
// the method.
func TestCSharpAdopt_PropertyInitializerSharedLine(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvcInt,
		"B/App.cs": `namespace B {
    public class App {
        private static Svc worker = new Svc();
        public int Seed { get; } = worker.Run(); public void Idle() { }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	seed := nodeByNameKind(t, g, "Seed", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	idle := nodeByNameKind(t, g, "Idle", graph.KindMethod)
	e := callEdgeTo(g, seed.ID, run.ID)
	if e == nil {
		t.Fatalf("property-initializer call not resolved from the property; edges: %v", g.GetOutEdges(seed.ID))
	}
	assertASTProvenance(t, e, "csharp-types")
	if dupes := callEdgesNamed(g, idle.ID, "Run"); len(dupes) != 0 {
		t.Errorf("line-sharing method minted %d edge(s) for the property's call: %v", len(dupes), dupes)
	}
}

// A FIELD initializer declarator sharing a line with a method — the
// field is the same node kind the property case rides on, but reached
// through a different extractor path.
func TestCSharpAdopt_FieldInitializerSharedLine(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvcInt,
		"B/App.cs": `namespace B {
    public class App {
        private static Svc worker = new Svc();
        private int seed = worker.Run(); public void Idle() { }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	seed := nodeByNameKind(t, g, "seed", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	idle := nodeByNameKind(t, g, "Idle", graph.KindMethod)
	e := callEdgeTo(g, seed.ID, run.ID)
	if e == nil {
		t.Fatalf("field-initializer call not resolved from the field; edges: %v", g.GetOutEdges(seed.ID))
	}
	assertASTProvenance(t, e, "csharp-types")
	if dupes := callEdgesNamed(g, idle.ID, "Run"); len(dupes) != 0 {
		t.Errorf("line-sharing method minted %d edge(s) for the field's call: %v", len(dupes), dupes)
	}
}

// The head of a chained call inside an accessor body. (The chain TAIL is
// a pre-existing engine gap — it is dropped from an ordinary method too —
// so only the head is pinned here.)
func TestCSharpAdopt_ChainHeadInAccessor(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": `namespace A {
    public class Inner {
        public void Deep() {}
    }
    public class Svc {
        public Inner Get() { return null; }
    }
}
`,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;

        public int Tick {
            get { worker.Get().Deep(); return 1; }
        }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	tick := nodeByNameKind(t, g, "Tick", graph.KindField)
	get := nodeByNameKind(t, g, "Get", graph.KindMethod)
	e := callEdgeTo(g, tick.ID, get.ID)
	if e == nil {
		t.Fatalf("chain head Get() not resolved from the property; edges: %v", g.GetOutEdges(tick.ID))
	}
	assertASTProvenance(t, e, "csharp-types")
}

// The accessor's stub is ALREADY resolver-resolved before Enrich.
// Adoption must land on the same owner and CONFIRM the edge in place —
// never mint a second one beside it.
func TestCSharpAdopt_AlreadyResolvedAccessorStubIsConfirmed(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;

        public int Tick {
            get { worker.Run(); return 1; }
        }
    }
}
`,
	})
	tick := nodeByNameKind(t, g, "Tick", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	var stub *graph.Edge
	for _, e := range g.GetOutEdges(tick.ID) {
		if e.Kind == graph.EdgeCalls {
			stub = e
		}
	}
	if stub == nil {
		t.Fatal("no extracted calls stub on the property")
	}
	oldTo := stub.To
	stub.To = run.ID
	g.ReindexEdge(stub, oldTo)

	p := NewProvider(CSharpSpec(), zap.NewNop())
	res, err := p.Enrich(g, dir)
	if err != nil {
		t.Fatal(err)
	}
	if edges := callEdgesNamed(g, tick.ID, "Run"); len(edges) != 1 {
		t.Errorf("pre-resolved stub produced %d Run edges, want 1 (no re-mint): %v", len(edges), edges)
	}
	if res.EdgesAdded != 0 {
		t.Errorf("EdgesAdded = %d, want 0 (confirm, not mint): %+v", res.EdgesAdded, res)
	}
	if res.EdgesConfirmed == 0 {
		t.Errorf("EdgesConfirmed = 0, want >= 1: %+v", res)
	}
}

// Two authored calls of the SAME name on one line from the SAME owner —
// two stubs, one owner. stubOwnerAt must not read the repeat as a tie.
func TestCSharpAdopt_SameOwnerTwoSameNameCallsOnOneLine(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;

        public int Tick {
            get { worker.Run(); worker.Run(); return 1; }
        }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	tick := nodeByNameKind(t, g, "Tick", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	e := callEdgeTo(g, tick.ID, run.ID)
	if e == nil {
		t.Fatalf("same-owner repeated-name line not resolved; edges: %v", g.GetOutEdges(tick.ID))
	}
	assertASTProvenance(t, e, "csharp-types")
}

// The FilePath guard in buildIndex: a stale calls-edge carrying ANOTHER
// file's path, parked on a line-sharing node at the same line and name,
// must not be admitted to the snapshot. Without the guard it makes the
// (line, name) pair ambiguous and the property loses its call.
func TestCSharpAdopt_ForeignFilePathStubNotAdopted(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;
        public int Quick { get { worker.Run(); return 1; } } public void Onlooker() { }
    }
}
`,
	})
	onlooker := nodeByNameKind(t, g, "Onlooker", graph.KindMethod)
	g.AddEdge(&graph.Edge{
		From:     onlooker.ID,
		To:       "unresolved::*.Run",
		Kind:     graph.EdgeCalls,
		FilePath: "C/Other.cs",
		Line:     4,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	quick := nodeByNameKind(t, g, "Quick", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	if e := callEdgeTo(g, quick.ID, run.ID); e == nil {
		t.Errorf("foreign-file stub broke adoption: property's call not resolved; edges: %v", g.GetOutEdges(quick.ID))
	}
	if e := callEdgeTo(g, onlooker.ID, run.ID); e != nil {
		t.Errorf("foreign-file stub was adopted as this file's owner: %v", e)
	}
}

// The extent guard: a synthesized calls-edge whose Line lies OUTSIDE its
// owning node's span (the framework-dispatch shape — Rails callbacks and
// Laravel middleware park an action's edge on a class-body line) must not
// enter the snapshot as an owner at that line, or it would tie against
// the real owner and cost the property its call.
func TestCSharpAdopt_OutOfExtentSyntheticEdgeNotAdopted(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;
        public void Macro() { }
        public int Late { get { worker.Run(); return 1; } }
    }
}
`,
	})
	macro := nodeByNameKind(t, g, "Macro", graph.KindMethod)
	late := nodeByNameKind(t, g, "Late", graph.KindField)
	g.AddEdge(&graph.Edge{
		From:     macro.ID,
		To:       "unresolved::*.Run",
		Kind:     graph.EdgeCalls,
		FilePath: "B/App.cs",
		Line:     late.StartLine, // outside Macro's own span
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	if e := callEdgeTo(g, late.ID, run.ID); e == nil {
		t.Errorf("out-of-extent synthetic edge broke adoption: property's call not resolved; edges: %v", g.GetOutEdges(late.ID))
	}
	if e := callEdgeTo(g, macro.ID, run.ID); e != nil {
		t.Errorf("out-of-extent synthetic edge's owner collected the call: %v", e)
	}
}

// The dangerous tie variant: TWO properties call the same name on a line
// shared with a method that calls it NOT AT ALL. The tie must refuse the
// site outright — falling back to line containment would name the
// method, find no claimable stub, and MINT an ast_resolved edge for a
// call the method never authored.
func TestCSharp_TieWithNonCandidateCallableRefusesMint(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;
        public int A1 { get { worker.Run(); return 1; } } public int A2 { get { worker.Run(); return 2; } } public void Idle() { }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	idle := nodeByNameKind(t, g, "Idle", graph.KindMethod)
	if got := callEdgesNamed(g, idle.ID, "Run"); len(got) != 0 {
		t.Errorf("tie fallback fabricated %d Run edge(s) on a method that authored none: %v", len(got), got)
	}
	a1 := nodeByNameKind(t, g, "A1", graph.KindField)
	a2 := nodeByNameKind(t, g, "A2", graph.KindField)
	assertUntouched(t, g, a1.ID, "Run", "csharp-types")
	assertUntouched(t, g, a2.ID, "Run", "csharp-types")
}

// A same-name tie where the line-containment answer IS one of the tied
// owners: the method keeps its own call (claimed in place) and the
// property's indistinguishable site stays untouched — never handed to a
// third party, never doubled.
func TestCSharp_SameNameTieMethodKeepsOwnCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;
        public int Quick { get { worker.Run(); return 1; } } public void Busy() { worker.Run(); }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	busy := nodeByNameKind(t, g, "Busy", graph.KindMethod)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	if e := callEdgeTo(g, busy.ID, run.ID); e == nil {
		t.Errorf("method's own call lost on a same-name tie; edges: %v", g.GetOutEdges(busy.ID))
	}
	quick := nodeByNameKind(t, g, "Quick", graph.KindField)
	if edges := callEdgesNamed(g, busy.ID, "Run"); len(edges) > 1 {
		t.Errorf("same-name tie doubled the method's edges: %v", edges)
	}
	if e := callEdgeTo(g, quick.ID, run.ID); e != nil {
		t.Logf("note: property side also resolved (%v) — acceptable, but not required by the tie contract", e)
	}
}

// Enrich twice: the second pass sees its own confirmed stub in the
// snapshot (confirmation stamps semantic_source on the EXTRACTION edge,
// so any filter keyed on that stamp would evict the real owner and
// re-open the shared-line theft). Idempotency is the pin.
func TestCSharp_SecondEnrichIsIdempotentOnSharedLine(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;
        public int Quick { get { worker.Run(); return 1; } } public void Onlooker() { }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	quick := nodeByNameKind(t, g, "Quick", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	if edges := callEdgesNamed(g, quick.ID, "Run"); len(edges) != 1 {
		t.Errorf("second enrich changed the property's Run edges: %v", edges)
	}
	if callEdgeTo(g, quick.ID, run.ID) == nil {
		t.Errorf("property's resolved call lost on second enrich")
	}
	onlooker := nodeByNameKind(t, g, "Onlooker", graph.KindMethod)
	if dupes := callEdgesNamed(g, onlooker.ID, "Run"); len(dupes) != 0 {
		t.Errorf("second enrich minted %d edge(s) on the line-sharing method: %v", len(dupes), dupes)
	}
}
