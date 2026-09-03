package tstypes

// Shape pins for stub adoption beyond the core four in
// csharp_accessor_attribution_test.go: the initializer lanes, chained
// heads, pre-resolved stubs, snapshot-hygiene guards, and the tie
// semantics (the fact's own author breaks a same-line same-name tie;
// line containment is the fallback and breaks it only WITHIN the tied
// owner set — a callable that merely shares the line never collects a
// call it did not author).

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

// Two authored calls of the SAME trailing name on one line from the SAME
// owner — two stubs, one owner. stubOwnersAt must not read the repeat as
// a tie: without the owner dedupe the duplicate looks like a multi-owner
// tie, and a property owner cannot win the containment tie-break (it is
// not in idx.funcs), so the call would be dropped outright.
//
// Two same-name MEMBER calls cannot produce the shape — they stub to the
// same target (unresolved::*.Run) and collapse into one edge under the
// graph's (From, To, Kind, FilePath, Line) edge key. The receiverless
// form stubs to a DIFFERENT id (unresolved::Run) with the same trailing
// name, so member + receiverless is the minimal two-stub fixture; the
// precondition below keeps it honest.
func TestCSharpAdopt_SameOwnerTwoSameNameCallsOnOneLine(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;

        public int Tick {
            get { worker.Run(); Run(); return 1; }
        }
    }
}
`,
	})
	tick := nodeByNameKind(t, g, "Tick", graph.KindField)
	if stubs := callEdgesNamed(g, tick.ID, "Run"); len(stubs) != 2 {
		t.Fatalf("fixture precondition: want 2 extracted Run stubs on the property, got %d: %v", len(stubs), stubs)
	}
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
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
// shared with a method that calls it NOT AT ALL. Each property's fact
// names its own author, so both sites resolve onto their own stubs; the
// method never enters — falling back to line containment would name it,
// find no claimable stub, and MINT an ast_resolved edge for a call it
// never authored.
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
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	for _, name := range []string{"A1", "A2"} {
		prop := nodeByNameKind(t, g, name, graph.KindField)
		if e := callEdgeTo(g, prop.ID, run.ID); e == nil {
			t.Errorf("%s: own call not resolved on a same-name tie; edges: %v", name, g.GetOutEdges(prop.ID))
		} else {
			assertASTProvenance(t, e, "csharp-types")
		}
		if edges := callEdgesNamed(g, prop.ID, "Run"); len(edges) != 1 {
			t.Errorf("%s: %d Run edge(s), want exactly its own: %v", name, len(edges), edges)
		}
	}
}

// A same-name tie where a property and a method call the SAME target on
// one shared line: a tie is broken per authored fact, not once per line,
// so each owner resolves its own site onto its own stub — the method's
// claimed in place, the property's independently — never handed to a
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
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	busy := nodeByNameKind(t, g, "Busy", graph.KindMethod)
	quick := nodeByNameKind(t, g, "Quick", graph.KindField)
	for _, owner := range []*graph.Node{busy, quick} {
		if e := callEdgeTo(g, owner.ID, run.ID); e == nil {
			t.Errorf("%s: own call lost on a same-name tie; edges: %v", owner.Name, g.GetOutEdges(owner.ID))
		} else {
			assertASTProvenance(t, e, "csharp-types")
		}
		if edges := callEdgesNamed(g, owner.ID, "Run"); len(edges) != 1 {
			t.Errorf("%s: %d Run edge(s), want exactly its own: %v", owner.Name, len(edges), edges)
		}
	}
}

// The issue-730 shape: two tied owners call the same NAME on different
// TYPES. Each fact must land on its own author's stub. Before the authored
// owner rode on the fact, the first fact applied (the property's)
// retargeted the METHOD's stub to the property's target — the method then
// carried a confident edge to a type it never calls, and its real call
// was unreachable because the edge was no longer claimable.
func TestCSharp_SameNameTieDifferentTargets(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"Core/Engine.cs": `namespace Core {
    public class Engine {
        public void Fire() {}
        public void Halt() {}
    }
}
`,
		"Core/Other.cs": `namespace Core {
    public class Other {
        public void Fire() {}
    }
}
`,
		"Use/Rig.cs": `namespace Use {
    public class Rig {
        private Engine motor; private Other pal;
        public int Level { get { motor.Fire(); return 7; } } public void Busy() { pal.Fire(); }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	const engineFire, otherFire = "Core/Engine.cs::Engine.Fire", "Core/Other.cs::Other.Fire"
	busy := nodeByNameKind(t, g, "Busy", graph.KindMethod)
	level := nodeByNameKind(t, g, "Level", graph.KindField)
	if e := callEdgeTo(g, busy.ID, otherFire); e == nil {
		t.Errorf("method's own call (Other.Fire) lost; edges: %v", g.GetOutEdges(busy.ID))
	} else {
		assertASTProvenance(t, e, "csharp-types")
	}
	if e := callEdgeTo(g, busy.ID, engineFire); e != nil {
		t.Errorf("method carries the property's resolution (Engine.Fire): %v", e)
	}
	if e := callEdgeTo(g, level.ID, engineFire); e == nil {
		t.Errorf("property's own call (Engine.Fire) not resolved; edges: %v", g.GetOutEdges(level.ID))
	} else {
		assertASTProvenance(t, e, "csharp-types")
	}
	for _, owner := range []*graph.Node{busy, level} {
		if edges := callEdgesNamed(g, owner.ID, "Fire"); len(edges) != 1 {
			t.Errorf("%s: %d Fire edge(s), want exactly its own: %v", owner.Name, len(edges), edges)
		}
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
