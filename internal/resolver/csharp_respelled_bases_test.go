package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// Verbatim spellings in BASE-LIST position (an @-spelled alias, the
// interface name itself spelled `@IBox`) are legal respellings whose
// raw use bypassed alias and duplicate protection: the respelled path
// vanished, the surviving stamp read as the unique closure, and the
// whole dual-interface type was filtered out of the family fan-out
// (round-5 finding 7 end-to-end; the escape-decode quadrants are pinned
// at the extractor level).
// Round-5 canonicalized the BASE-LIST side only, so a type whose name
// must be spelled verbatim - `@event` is the only legal spelling of a
// keyword-named interface - became unreachable from any base list: the
// declaration minted `R.cs::@event` while every base entry asked for
// `event`, the hierarchy edge died unresolved, and the interface test
// missed so it was misclassified extends (round-6 finding B2). The
// declaration side now canonicalizes into the same domain at all three
// reads: the node ID, the local-interface prescan, and the member
// owner - members must hang off the canonical ID or the family fan-out
// stays empty even once the edge resolves.
func TestResolveCSharp_VerbatimDeclaredTypeKeepsItsHierarchy(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ev.cs": `namespace App {
    public interface @event { void Put(int x); }
    public class EvA : @event { public void Put(int x) { } }
    public class EvB : @event { public void Put(int x) { } }
    public class EvFlow {
        private readonly @event _e;
        public EvFlow(@event e) { _e = e; }
        public void Go(int x) { _e.Put(x); }
    }
}`,
	})
	New(g).ResolveAll()

	implements := map[string]string{}
	for _, e := range g.AllEdges() {
		if e == nil || (e.Kind != graph.EdgeImplements && e.Kind != graph.EdgeExtends) {
			continue
		}
		if e.From == "Ev.cs::EvA" || e.From == "Ev.cs::EvB" {
			implements[e.From] = string(e.Kind) + " -> " + e.To
		}
	}
	assert.Equal(t, "implements -> Ev.cs::event", implements["Ev.cs::EvA"],
		"the verbatim spelling and the canonical node are the same identifier - the edge resolves and classifies as implements")
	assert.Equal(t, "implements -> Ev.cs::event", implements["Ev.cs::EvB"])

	const callerID = "Ev.cs::EvFlow.Go"
	bindMemberCallAtLine(t, g, callerID, "Put", "Ev.cs::event.Put")
	ResolveCSharpInterfaceDispatch(g)
	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Ev.cs::EvA.Put",
		"members hang off the canonical owner, so the family fan-out reaches the implementors, got %v", targets)
	assert.Contains(t, targets, "Ev.cs::EvB.Put")
}

func TestResolveCSharpInterfaceDispatch_RespelledBasesKeepTheFamily(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Rack.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { void Put(T item); }
    public class PlainCrateBox : IBox<Crate> { public void Put(Crate item) { } }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> box) { _box = box; }
        public void Pull(Crate item) { _box.Put(item); }
    }
}`,
		"Dual.cs": `using @BX = App.IBox<App.Crate>;
namespace App {
    public class DualA : @BX, IBox<Widget> {
        public void Put(Crate item) { }
        public void Put(Widget item) { }
    }
    public class DualB : @IBox<Crate>, IBox<Widget> {
        public void Put(Crate item) { }
        public void Put(Widget item) { }
    }
}`,
	})
	New(g).ResolveAll()

	const callerID = "Rack.cs::Flow.Pull"
	bindMemberCallAtLine(t, g, callerID, "Put", "Rack.cs::IBox.Put")
	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	hasA, hasB := false, false
	for _, to := range targets {
		switch to {
		case "Dual.cs::DualA.Put":
			hasA = true
		case "Dual.cs::DualB.Put":
			hasB = true
		}
	}
	assert.True(t, hasA, "the alias-based dual implementor must stay in the fan-out, got %v", targets)
	assert.True(t, hasB, "the verbatim-direct dual implementor must stay in the fan-out, got %v", targets)
}
