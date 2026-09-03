package tstypes

// Pins for issue #731: the extractor owns a C# property's calls by the
// span it RECORDED (the body-bearing fragment of a partial property, the
// body-bearing arm of a conditional-compilation pair), which can lie
// outside the property NODE's lines (the first fragment's). buildIndex's
// extent guard must test the stub against the recorded ownership span,
// not the declaration span, or adoption never fires for exactly the
// owner kind it exists to serve.

import (
	"encoding/json"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// The stamped span ADDS to the node's lines; it does not replace the
// guard. A synthesized edge parked on a stamped property at a line
// outside BOTH intervals must still be refused - here at the line of a
// second property that authors the same call, where admitting it would
// tie the two, and a tie between two properties has no containing
// callable, so the real owner would lose its site.
func TestCSharp_StampedPropertyStillRefusesEdgeOutsideBothSpans(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvcInt,
		"B/App.cs": `namespace B {
    public partial class App {
        private Svc worker;
        public partial int Tick { get; set; }
    }

    public partial class App {
        public partial int Tick {
            get { worker.Run(); return 1; }
        }
        public int Peek { get { worker.Run(); return 2; } }
    }
}
`,
	})
	tick := nodeByNameKind(t, g, "Tick", graph.KindField)
	peek := nodeByNameKind(t, g, "Peek", graph.KindField)
	lo, hi, ok := recordedOwnershipSpan(tick)
	if !ok {
		t.Fatalf("Tick carries no ownership stamp; meta=%v", tick.Meta)
	}
	// The fixture's whole point: Peek's line lies outside BOTH of Tick's
	// intervals (its node line and its stamped span).
	if (peek.StartLine >= lo && peek.StartLine <= hi) || (peek.StartLine >= tick.StartLine && peek.StartLine <= tick.EndLine) {
		t.Fatalf("fixture drift: Peek line %d inside Tick's node %d..%d or stamped span %d..%d", peek.StartLine, tick.StartLine, tick.EndLine, lo, hi)
	}
	g.AddEdge(&graph.Edge{
		From:     tick.ID,
		To:       "unresolved::*.Run",
		Kind:     graph.EdgeCalls,
		FilePath: "B/App.cs",
		Line:     peek.StartLine, // outside Tick's node lines AND its stamped span
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	if e := callEdgeTo(g, peek.ID, run.ID); e == nil {
		t.Errorf("synthesized edge outside both spans was admitted: Peek lost its call; edges: %v", g.GetOutEdges(peek.ID))
	}
	if got := callEdgesNamed(g, tick.ID, "Run"); len(got) != 2 {
		// Tick's own resolved body call plus the untouched synthetic stub.
		t.Errorf("Tick Run edges = %d, want 2 (own resolved call + refused synthetic stub): %v", len(got), got)
	}
	if e := callEdgeTo(g, tick.ID, run.ID); e == nil {
		t.Errorf("Tick's own body call not resolved; edges: %v", g.GetOutEdges(tick.ID))
	}
}

// The stamp reader: exact on the shapes the store hands back, lenient on
// the ones another writer might, and ok=false on anything unusable.
func TestRecordedOwnershipSpan_ReaderShapes(t *testing.T) {
	cases := []struct {
		name   string
		lo, hi any
		wantLo int
		wantHi int
		ok     bool
	}{
		{"ints", 10, 12, 10, 12, true},
		{"int64", int64(10), int64(12), 10, 12, true},
		{"float64", 10.0, 12.0, 10, 12, true},
		{"json.Number", json.Number("10"), json.Number("12"), 10, 12, true},
		{"strings", "10", "12", 10, 12, true},
		{"single line", 7, 7, 7, 7, true},
		{"missing end", 10, nil, 0, 0, false},
		{"missing start", nil, 12, 0, 0, false},
		{"inverted", 12, 10, 0, 0, false},
		{"zero start", 0, 12, 0, 0, false},
		{"garbage", "x", 12, 0, 0, false},
		{"bool", true, 12, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &graph.Node{Meta: map[string]any{}}
			if tc.lo != nil {
				n.Meta[graph.MetaOwnershipStartLine] = tc.lo
			}
			if tc.hi != nil {
				n.Meta[graph.MetaOwnershipEndLine] = tc.hi
			}
			lo, hi, ok := recordedOwnershipSpan(n)
			if ok != tc.ok || lo != tc.wantLo || hi != tc.wantHi {
				t.Fatalf("recordedOwnershipSpan = (%d, %d, %v), want (%d, %d, %v)", lo, hi, ok, tc.wantLo, tc.wantHi, tc.ok)
			}
		})
	}
	if _, _, ok := recordedOwnershipSpan(&graph.Node{}); ok {
		t.Fatal("nil meta must read as unstamped")
	}
}

// C# 13 partial property, declaring fragment first: the node spans the
// declaring line, the extractor's stub sits in the implementing fragment.
func TestCSharp_PartialPropertyDeclaringFirstResolvesBodyCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvcInt,
		"B/App.cs": `namespace B {
    public partial class App {
        private Svc worker;
        public partial int Tick { get; set; }
    }

    public partial class App {
        public partial int Tick {
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
		t.Fatalf("partial property's body call not resolved (EdgesConfirmed=%d); edges: %v", res.EdgesConfirmed, g.GetOutEdges(tick.ID))
	}
	assertASTProvenance(t, e, "csharp-types")
	if got := callEdgesNamed(g, tick.ID, "Run"); len(got) != 1 {
		t.Errorf("want exactly one Run edge on the property, got %d: %v", len(got), got)
	}
}

// Conditional compilation: the same property declared in both arms of an
// #if / #else (tree-sitter parses both); the body-bearing arm is second.
func TestCSharp_ConditionalPropertySecondArmResolvesBodyCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvcInt,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;
#if LEGACY
        public int Tick { get; set; }
#else
        public int Tick { get { worker.Run(); return 1; } }
#endif
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
		t.Fatalf("conditional property's body call not resolved (EdgesConfirmed=%d); edges: %v", res.EdgesConfirmed, g.GetOutEdges(tick.ID))
	}
	assertASTProvenance(t, e, "csharp-types")
	if got := callEdgesNamed(g, tick.ID, "Run"); len(got) != 1 {
		t.Errorf("want exactly one Run edge on the property, got %d: %v", len(got), got)
	}
}

// Control: implementing fragment FIRST. The node already spans the body,
// no ownership stamp is needed, and the call resolves as before.
func TestCSharp_PartialPropertyImplementingFirstStillResolves(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvcInt,
		"B/App.cs": `namespace B {
    public partial class App {
        private Svc worker;
        public partial int Tick {
            get { worker.Run(); return 1; }
        }
    }

    public partial class App {
        public partial int Tick { get; set; }
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
	if e := callEdgeTo(g, tick.ID, run.ID); e == nil {
		t.Fatalf("implementing-first partial property's body call not resolved; edges: %v", g.GetOutEdges(tick.ID))
	} else {
		assertASTProvenance(t, e, "csharp-types")
	}
}
