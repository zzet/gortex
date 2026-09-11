package profiles

import (
	"strings"
	"testing"
)

// The routing policy is the agent-facing contract for the three request-level
// freshness knobs, and it is the ONLY place they are advertised on the
// instruction surface — no tool declares them in its input schema, because
// three more properties on every registered tool would grow every tools/list.
// That makes this sentence load-bearing twice over: it is the discovery path,
// and it is the thing an agent will believe.
//
// It used to promise more than the server does. `require_fresh:true to await
// filesystem state` is unqualified, but the wait only advances a routed
// automatic checkout: a shared corpus, a dedicated or primary checkout, and a
// labelled base / ref / commit view are all answered `fresh:false` with a
// reason instead of being waited on (internal/mcp/checkout_binding.go,
// freshReasonCommittedBaseAdvance). An agent that believed the unqualified
// sentence would read a stale committed base and think it had waited for the
// working copy.
func TestRoutingPolicyQualifiesTheFreshnessWait(t *testing.T) {
	policy := WorktreeBranchRoutingPolicy

	for _, required := range []string{
		"require_exact:true",
		"require_fresh:true",
		"wait_deadline",
		"RFC3339",
	} {
		if !strings.Contains(policy, required) {
			t.Errorf("the routing policy no longer advertises %q — with no tool schema declaring it, "+
				"nothing else tells an agent the knob exists", required)
		}
	}

	// The qualification itself: what require_fresh waits for, and what the
	// rest of the view space gets instead.
	for _, required := range []string{
		"routed automatic checkouts only",
		"fresh:false",
	} {
		if !strings.Contains(policy, required) {
			t.Errorf("the routing policy lost %q: it promises a wait the server performs only for a "+
				"routed automatic checkout", required)
		}
	}

	// And the promise is not restated unqualified somewhere else in the same
	// bullet, which is how the over-promise got shipped the first time.
	if strings.Contains(policy, "to await filesystem state, and absolute RFC3339") {
		t.Error("the routing policy still carries the unqualified freshness sentence")
	}
}

// bodyCeilingHeadroomFraction is the slack every profile body must keep under
// its byte ceiling.
//
// A ceiling with no head-room is not a budget, it is a tripwire: the previous
// round paid for one qualified sentence out of this constant alone and left
// the core body 3 BYTES under its ceiling, so the next truthfulness fix to any
// shared section — the routing policy renders into every profile — would have
// had to choose between shipping a falsehood and re-basing the ceiling upward.
// Both are the wrong answer, so the head-room is asserted instead, and the
// only way to satisfy it is to trim elaboration.
//
// The ceilings themselves are NEVER raised to satisfy this: bodyByteCeilings
// is the shared budget the tool-list and instruction surfaces are sized
// against, and this test reads it rather than restating it, so raising one
// raises the bar here too.
const bodyCeilingHeadroomFraction = 0.05

func TestProfileBodiesKeepCeilingHeadroom(t *testing.T) {
	for _, p := range Table() {
		ceiling, ok := bodyByteCeilings[p.Name]
		if !ok {
			t.Errorf("profile %q has no body byte ceiling", p.Name)
			continue
		}
		got := len(p.Body())
		budget := int(float64(ceiling) * (1 - bodyCeilingHeadroomFraction))
		t.Logf("profile %-13s body bytes=%d budget=%d ceiling=%d head-room=%.2f%%",
			p.Name, got, budget, ceiling, 100*float64(ceiling-got)/float64(ceiling))
		if got > budget {
			t.Errorf("profile %q body is %d bytes, over the %d-byte budget (%d ceiling less %.0f%% head-room); "+
				"trim elaboration — do not raise the ceiling",
				p.Name, got, budget, ceiling, 100*bodyCeilingHeadroomFraction)
		}
	}
}
