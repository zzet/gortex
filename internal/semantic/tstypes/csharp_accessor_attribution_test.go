package tstypes

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// A call inside a property accessor body reaches the apply phase as an
// ordinary callFact (the binder walks accessor bodies and grounds field
// receivers through the type scope), but enclosingCallable answers nil —
// properties are field-kind nodes, outside idx.funcs — so applyCall
// drops the site: never typed-resolved, never confirmed. The extractor
// attributes these calls to the property node byte-precisely; the engine
// must land its resolution on the same owner.
func TestCSharp_AccessorBodyCallResolvesFromProperty(t *testing.T) {
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
	p := NewProvider(CSharpSpec(), zap.NewNop())
	res, err := p.Enrich(g, dir)
	if err != nil {
		t.Fatal(err)
	}
	tick := nodeByNameKind(t, g, "Tick", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	e := callEdgeTo(g, tick.ID, run.ID)
	if e == nil {
		t.Fatalf("accessor-body call not resolved from property; edges: %v", g.GetOutEdges(tick.ID))
	}
	assertASTProvenance(t, e, "csharp-types")
	if res.EdgesConfirmed+res.EdgesAdded == 0 {
		t.Errorf("result reported no edge work: %+v", res)
	}
}

// A property sharing a physical line with a method must not have its
// accessor call re-attributed to the method. The extractor's stub rides
// the property (byte attribution); a line-keyed caller lookup answers
// the method, misses the stub on the property's edge list, and mints a
// second edge for the same authored site — a duplicate nothing dedupes,
// because From is part of every edge identity.
func TestCSharp_SharedLineAccessorCallStaysWithProperty(t *testing.T) {
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
	quick := nodeByNameKind(t, g, "Quick", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	e := callEdgeTo(g, quick.ID, run.ID)
	if e == nil {
		t.Fatalf("shared-line accessor call not resolved from property; edges: %v", g.GetOutEdges(quick.ID))
	}
	assertASTProvenance(t, e, "csharp-types")
	onlooker := nodeByNameKind(t, g, "Onlooker", graph.KindMethod)
	if dupes := callEdgesNamed(g, onlooker.ID, "Run"); len(dupes) != 0 {
		t.Errorf("line-sharing method minted %d duplicate edge(s) for the property's call: %v", len(dupes), dupes)
	}
}

// A property and a method on one line, each with its own call: the stub
// name disambiguates the shared line, so each owner resolves exactly its
// own call and neither steals the other's.
func TestCSharp_SharedLineCallsSplitByOwner(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;
        public int Quick { get { worker.Run(); return 1; } } public void Busy() { worker.Stop(); }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	quick := nodeByNameKind(t, g, "Quick", graph.KindField)
	busy := nodeByNameKind(t, g, "Busy", graph.KindMethod)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	stop := nodeByNameKind(t, g, "Stop", graph.KindMethod)
	if e := callEdgeTo(g, quick.ID, run.ID); e == nil {
		t.Errorf("property's own call not resolved; edges: %v", g.GetOutEdges(quick.ID))
	} else {
		assertASTProvenance(t, e, "csharp-types")
	}
	if e := callEdgeTo(g, busy.ID, stop.ID); e == nil {
		t.Errorf("method's own call not resolved; edges: %v", g.GetOutEdges(busy.ID))
	} else {
		assertASTProvenance(t, e, "csharp-types")
	}
	if e := callEdgeTo(g, quick.ID, stop.ID); e != nil {
		t.Errorf("property stole the method's call: %v", e)
	}
	if e := callEdgeTo(g, busy.ID, run.ID); e != nil {
		t.Errorf("method stole the property's call: %v", e)
	}
}

// Two properties on one line calling the SAME method: the stub lookup
// cannot tell the owners apart by (line, name), and the engine never
// guesses among candidates — both sites stay untouched rather than one
// owner collecting both.
func TestCSharp_SharedLineSameNameAmbiguityRefused(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;
        public int A1 { get { worker.Run(); return 1; } } public int A2 { get { worker.Run(); return 2; } }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	a1 := nodeByNameKind(t, g, "A1", graph.KindField)
	a2 := nodeByNameKind(t, g, "A2", graph.KindField)
	assertUntouched(t, g, a1.ID, "Run", "csharp-types")
	assertUntouched(t, g, a2.ID, "Run", "csharp-types")
}
