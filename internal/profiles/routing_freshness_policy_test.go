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

// bodyByteBudgets is the absolute budget each profile body is held to.
//
// A ceiling with no head-room is not a budget, it is a tripwire: an earlier
// round paid for one qualified sentence out of this slack alone and left the
// core body 3 BYTES under its ceiling, so the next truthfulness fix to any
// shared section — the routing policy renders into every profile — would have
// had to choose between shipping a falsehood and re-basing the ceiling upward.
// Both are the wrong answer, so the head-room is asserted instead, and the
// only way to satisfy it is to trim elaboration.
//
// These are LITERALS, not a fraction of bodyByteCeilings. A fraction moves with
// the ceiling: raising a ceiling raised the allowance by 95 % of the raise, so
// the one edit this test exists to make expensive — re-basing a ceiling upward
// — also made this test easier to pass. Exactly backwards for a head-room
// gate. A literal cannot be relaxed by editing bodyByteCeilings at all; the
// only way to move it is to edit it here, which is a reviewable line in the
// diff rather than a side effect of a number in another file.
//
// Each value is the tighter of the two budgets that were ever in force: the
// 5 %-of-ceiling budget this replaced, and ceiling less
// bodyCeilingHeadroomBytes. No profile's gate is looser than it was.
var bodyByteBudgets = map[string]int{
	"core":         4377,
	"full":         7782,
	"localization": 3610,
}

// bodyCeilingHeadroomBytes is the slack a body must keep under its ceiling
// whatever the literal above says. It only ever TIGHTENS the budget, so
// LOWERING a ceiling still pulls the gate down with it while raising one does
// nothing. Its size is the 5 % of the core ceiling the old fraction bought,
// frozen: enough room for one qualified contract sentence in a shared section.
const bodyCeilingHeadroomBytes = 230

// profileBodyBudget is the gate one profile body is held to, given the ceiling
// in force. Split out so a test can ask what the budget WOULD be under a
// different ceiling.
func profileBodyBudget(name string, ceiling int) int {
	budget, ok := bodyByteBudgets[name]
	if !ok {
		return 0
	}
	if clamped := ceiling - bodyCeilingHeadroomBytes; clamped < budget {
		return clamped
	}
	return budget
}

func TestProfileBodiesKeepCeilingHeadroom(t *testing.T) {
	for _, p := range Table() {
		ceiling, ok := bodyByteCeilings[p.Name]
		if !ok {
			t.Errorf("profile %q has no body byte ceiling", p.Name)
			continue
		}
		if _, ok := bodyByteBudgets[p.Name]; !ok {
			t.Errorf("profile %q has no body byte budget — add one to bodyByteBudgets", p.Name)
			continue
		}
		got := len(p.Body())
		budget := profileBodyBudget(p.Name, ceiling)
		t.Logf("profile %-13s body bytes=%d budget=%d ceiling=%d head-room=%d bytes",
			p.Name, got, budget, ceiling, budget-got)
		if got > budget {
			t.Errorf("profile %q body is %d bytes, over the %d-byte budget (ceiling %d); "+
				"trim elaboration — raising the ceiling does not move this budget",
				p.Name, got, budget, ceiling)
		}
	}
}

// The property the fractional budget did not have: a raised ceiling must not
// buy a bigger body. Under `int(ceiling * 0.95)` doubling a ceiling nearly
// doubled the allowance, so "do not raise the ceiling" was advice in a comment
// rather than a thing the test enforced.
func TestRaisingACeilingCannotRelaxTheBodyBudget(t *testing.T) {
	for _, p := range Table() {
		ceiling, ok := bodyByteCeilings[p.Name]
		if !ok {
			continue
		}
		budget := profileBodyBudget(p.Name, ceiling)
		for _, raised := range []int{ceiling + 1024, 2 * ceiling, 10 * ceiling} {
			if got := profileBodyBudget(p.Name, raised); got != budget {
				t.Errorf("profile %q: raising the ceiling %d -> %d moved the body budget %d -> %d; "+
					"the budget must not be derivable from a raisable ceiling",
					p.Name, ceiling, raised, budget, got)
			}
		}
		// The converse still holds: a LOWERED ceiling tightens the gate, so the
		// clamp is not dead code.
		if lowered := profileBodyBudget(p.Name, budget); lowered >= budget {
			t.Errorf("profile %q: lowering the ceiling to %d left the budget at %d; "+
				"the head-room clamp no longer tightens", p.Name, budget, lowered)
		}
	}
}
