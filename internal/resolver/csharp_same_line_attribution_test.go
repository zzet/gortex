package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// Byte extents are recorded for methods, constructors, properties, and
// initialized field declarators. Every extent-carrying member must own
// its call BYTE-precisely, not hand it to whoever shares the line (the
// pre-owner-widening behavior this test silently tolerated).
//
// A member kind WITHOUT extents is the hard case: its call matches no
// recorded extent, and the line fallback answers a same-line neighbour
// that provably does NOT contain the call - the neighbour's recorded
// bytes exclude the offset. Handing it the call invents a false edge
// carrying full receiver evidence and no ambiguity stamp (ambiguousAt
// counts ranges, and an extent-less member contributes none),
// indistinguishable downstream from a correct edge. Such calls are
// REFUSED instead, trading that member's recall for precision. Distinct
// from the round-6 B3 erasure: there the "" answer dropped calls whose
// line owner was plausible - here the line owner is provably wrong.
//
// Indexers and event accessors were the original refused kinds; they now
// carry extents of their own, so the trade is off for them and they own
// their calls outright. Operators, conversion operators and destructors
// are still unemitted, and hold the refusal arm under test.
func TestResolveCSharp_SameLineMemberCallAttribution(t *testing.T) {
	cases := map[string]struct {
		src     string
		call    string // authored call's target name; "" = "Take"
		owner   string // this member owns the call byte-precisely
		refused bool   // no member may be handed the call
	}{
		"property_shares_line": {src: `namespace App {
 public class BLBag { public int Take() { return 1; } }
 public class BLProp { private BLBag _b = new BLBag();
  public int Q => _b.Take(); public void M() { }
 } }`, owner: "X.cs::BLProp.Q"},
		"indexer_beside_method_owns_its_call": {src: `namespace App {
 public class BLBag { public int Take() { return 1; } }
 public class BLIdx { private BLBag _b = new BLBag();
  public int this[int i] => _b.Take(); public void M() { }
 } }`, owner: "X.cs::BLIdx.this[]"},
		// Operators and destructors record no extents, so the refusal arm
		// still governs them: the line answer is a member whose own bytes
		// exclude the call, and a false edge there is worse than the drop.
		"operator_beside_property_refused": {src: `namespace App {
 public class OBag { public int Take() { return 1; } }
 public class OpRig { private static OBag _b = new OBag();
  public static OpRig operator +(OpRig a, OpRig b) { _b.Take(); return a; } public int Q => 1;
 } }`, refused: true},
		"destructor_beside_property_refused": {src: `namespace App {
 public class DBag { public int Take() { return 1; } }
 public class DtorRig { private DBag _b = new DBag();
  ~DtorRig() { _b.Take(); } public int Q => 1;
 } }`, refused: true},
		"conversion_operator_beside_property_refused": {src: `namespace App {
 public class CBag { public int Take() { return 1; } }
 public class ConvRig { private static CBag _b = new CBag();
  public static implicit operator int(ConvRig r) { _b.Take(); return 1; } public int Q => 1;
 } }`, refused: true},
		"field_init_shares_line": {src: `namespace App {
 public class BLBag { public int Take() { return 1; } }
 public class BLInit { private BLBag _b = new BLBag();
  private int _n = new BLBag().Take(); public void M() { }
 } }`, owner: "X.cs::BLInit._n"},
		"ctor_and_property_same_line": {src: `namespace App {
 public class BLBag { public int Take() { return 1; } }
 public class BLCtor { private BLBag _b = new BLBag();
  public BLCtor() { } public int Q => _b.Take();
 } }`, owner: "X.cs::BLCtor.Q"},
		// The two shapes below were the false-edge populations opened by
		// the owner widening itself - the base emitted nothing for them,
		// while widened properties made a wrong owner available. Issue
		// #728 is exactly this pair reaching the semantic tier, where
		// caller adoption turned the stub into a confident resolved edge.
		"indexer_beside_property_owns_its_call": {src: `namespace App {
 public class QBag { public int Idx() { return 1; } public int Pro() { return 2; } }
 public class Q01 { private QBag _b = new QBag();
  public int this[int i] => _b.Idx(); public int Q => _b.Pro();
 } }`, call: "Idx", owner: "X.cs::Q01.this[]"},
		"indexer_beside_property_neighbour_keeps_own": {src: `namespace App {
 public class QBag { public int Idx() { return 1; } public int Pro() { return 2; } }
 public class Q01 { private QBag _b = new QBag();
  public int this[int i] => _b.Idx(); public int Q => _b.Pro();
 } }`, call: "Pro", owner: "X.cs::Q01.Q"},
		"event_accessor_beside_property_owns_its_call": {src: `namespace App {
 public class QBag { public int Ev() { return 1; } public int Pro() { return 2; } }
 public class Q03 { private QBag _b = new QBag();
  public event System.EventHandler E { add { _b.Ev(); } remove { } } public int Q => _b.Pro();
 } }`, call: "Ev", owner: "X.cs::Q03.E"},
		"event_accessor_beside_property_neighbour_keeps_own": {src: `namespace App {
 public class QBag { public int Ev() { return 1; } public int Pro() { return 2; } }
 public class Q03 { private QBag _b = new QBag();
  public event System.EventHandler E { add { _b.Ev(); } remove { } } public int Q => _b.Pro();
 } }`, call: "Pro", owner: "X.cs::Q03.Q"},
	}
	for name, tc := range cases {
		g := buildCSharpResolverGraph(t, map[string]string{"X.cs": tc.src})
		target := tc.call
		if target == "" {
			target = "Take"
		}
		owners := map[string]int{}
		for _, e := range g.AllEdges() {
			if e != nil && e.Kind == graph.EdgeCalls && strings.Contains(e.To, target) {
				owners[e.From]++
			}
		}
		if tc.refused {
			assert.Empty(t, owners,
				"[%s] an extent-less member's call must be refused, not handed to a same-line neighbour, got owners %v", name, owners)
			continue
		}
		assert.NotZero(t, owners[tc.owner],
			"[%s] the extent-carrying member owns its call byte-precisely, got owners %v", name, owners)
		delete(owners, tc.owner)
		assert.Empty(t, owners,
			"[%s] no line-sharing member may carry a duplicate of the call", name)
	}
}

// Two methods with UNEQUAL line spans can share a physical line - a
// zero-span `public int B(){return 0;}` followed on the same line by the
// opening of A, whose call is split across two lines. Line-keyed
// attribution picked the smaller span (B) and ambiguousAt saw no tie
// (it requires EQUAL spans), so B carried A's call, the shadow refusal
// consulted B's parameter set, and the field's closure filtered the
// parameter-correct implementor away (round-5 finding 4).
//
// Byte-interval attribution is the complete fix: the call's offset lies
// inside A's byte extent and outside B's.
func TestResolveCSharp_UnequalSpanMethodsSharingALine(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"UnequalSpanSameLine.cs": `namespace App {
    public interface SMBox<T> { T Get(int id); }
    public sealed class SMCrate { }
    public sealed class SMWidget { }
    public sealed class SMCrateBox : SMBox<SMCrate> { public SMCrate Get(int id) { return new SMCrate(); } }
    public sealed class SMWidgetBox : SMBox<SMWidget> { public SMWidget Get(int id) { return new SMWidget(); } }
    public sealed class SMFlow {
        private SMBox<SMCrate> _store = new SMCrateBox();
        public int B(){return 0;} public SMWidget A(SMBox<SMWidget> _store, int id) { return _store.Get(
            id); }
    }
}`,
	})
	New(g).ResolveAll()

	assert.Empty(t, callsFrom(g, "UnequalSpanSameLine.cs::SMFlow.B"),
		"B's body is `return 0;` - it owns no call, whatever shares its line")

	// The fan-out assertion this test once ended on went vacuous when
	// the dispatch gate was deferred - with no type-argument gate every
	// implementor survives whoever owns the call (issue #726). The
	// round-5 failure chain is pinned at its first two links instead:
	// the companion must hang off A, and the shadow refusal must
	// consult A's OWN parameter set - `_store` is A's parameter, so no
	// field read may be minted anywhere. Under line-keyed attribution B
	// carried the call, B has no `_store` parameter, and the field read
	// appeared with full receiver evidence.
	const callerID = "UnequalSpanSameLine.cs::SMFlow.A"
	assert.Contains(t, callsFrom(g, callerID), "unresolved::*.Get",
		"the split-line call's companion rides on A, whose byte extent contains it")
	for _, e := range g.AllEdges() {
		if e != nil && e.Kind == graph.EdgeReads && strings.Contains(e.To, "_store") {
			t.Fatalf("the receiver is A's parameter - no member may read the `_store` FIELD, got %s -> %s", e.From, e.To)
		}
	}
}
