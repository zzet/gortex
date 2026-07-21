package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRefinementRouteConcreteReadCompletesInOneCall(t *testing.T) {
	state := &localizationTerminalState{}
	concrete := "repo/replace.go::replaceAll"
	state.armRefinementRoutesForTask(
		"find the replace implementation",
		concrete,
		[]string{concrete},
		map[string]localizationRefinementRoute{concrete: {enforceable: true}},
		nil,
	)

	requireRefinementSourceReservation(t, state, concrete)
	completion := state.finishReservedRead(true)
	requireRefinementCompletion(t, completion, localizationStateAnswerReady, "", 0)

	result, reserved := state.authorize("read", "source", refinementSourceArgs(concrete))
	if reserved {
		t.Fatal("read after concrete completion reserved a handler")
	}
	requireLocalizationTerminalReplay(t, result, "read", "source")
}

func TestRefinementRouteUsesActuallySelectedAlternateGenericCandidate(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/replace.go::replaceAll"
	generic := "repo/replacer.go::Replacer.Replace"
	implementation := "repo/replacer.go::Replacer.replaceAll"
	routes := map[string]localizationRefinementRoute{
		preferred: {},
		generic:   {implementationSymbol: implementation},
	}
	state.armRefinementRoutesForTask(
		"find the replace implementation",
		preferred,
		[]string{preferred, generic},
		routes,
		nil,
	)

	requireRefinementSourceReservation(t, state, generic)
	completion := state.finishReservedRead(true)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, implementation, 1)

	requireRefinementSourceReservation(t, state, implementation)
	completion = state.finishReservedRead(true)
	requireRefinementCompletion(t, completion, localizationStateAnswerReady, "", 0)

	result, reserved := state.authorize("read", "source", refinementSourceArgs(implementation))
	if reserved {
		t.Fatal("third read reserved a handler")
	}
	requireLocalizationTerminalReplay(t, result, "read", "source")
}

func TestRefinementRouteGenericReadFailureRestoresFirstAllowance(t *testing.T) {
	state := &localizationTerminalState{}
	generic := "repo/replacer.go::Replacer.Replace"
	implementation := "repo/replacer.go::Replacer.replaceAll"
	state.armRefinementRoutesForTask(
		"find the replace implementation",
		generic,
		[]string{generic},
		map[string]localizationRefinementRoute{
			generic: {implementationSymbol: implementation},
		},
		nil,
	)

	requireRefinementSourceReservation(t, state, generic)
	completion := state.finishReservedRead(false)
	requireRefinementCompletion(t, completion, localizationStateNeedsRefinement, "", 1)

	requireRefinementSourceReservation(t, state, generic)
	state.finishReservedRead(false)
}

func TestRefinementRouteExactHopFailureRestoresExactAllowance(t *testing.T) {
	state := &localizationTerminalState{}
	generic := "repo/replacer.go::Replacer.Replace"
	implementation := "repo/replacer.go::Replacer.replaceAll"
	state.armRefinementRoutesForTask(
		"find the replace implementation",
		generic,
		[]string{generic},
		map[string]localizationRefinementRoute{
			generic: {implementationSymbol: implementation},
		},
		nil,
	)

	requireRefinementSourceReservation(t, state, generic)
	completion := state.finishReservedRead(true)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, implementation, 1)

	requireRefinementSourceReservation(t, state, implementation)
	completion = state.finishReservedRead(false)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, implementation, 1)

	requireRefinementSourceReservation(t, state, implementation)
	state.finishReservedRead(true)
}

func TestRefinementAllowedSymbolsMirrorSessionAuthorization(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/replace.go::replaceAll"
	generic := "repo/replacer.go::Replacer.Replace"
	implementation := "repo/replacer.go::Replacer.replaceAll"
	state.armRefinementRoutesForTask(
		"find the replace implementation",
		preferred,
		[]string{preferred, generic},
		map[string]localizationRefinementRoute{
			preferred: {},
			generic:   {implementationSymbol: implementation},
		},
		nil,
	)

	wire, err := json.Marshal(state.completionLocked())
	if err != nil {
		t.Fatalf("marshal localization completion: %v", err)
	}
	var completion localizationCompletion
	if err := json.Unmarshal(wire, &completion); err != nil {
		t.Fatalf("unmarshal localization completion: %v", err)
	}
	if !reflect.DeepEqual(completion.AllowedSymbols, state.refinementSymbols) {
		t.Fatalf("wire allowed symbols = %v, state authorization = %v", completion.AllowedSymbols, state.refinementSymbols)
	}
	if !strings.Contains(completion.RequiredAction, "completion.allowed_symbols") {
		t.Fatalf("required action does not name the authorization field: %q", completion.RequiredAction)
	}
	if strings.Contains(string(wire), implementation) {
		t.Fatalf("serialized completion leaks session-only implementation hop %q: %s", implementation, wire)
	}
}

func TestWeakPreferredReadOffersOnePrecomputedCorrection(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/localize.go::FindLocaliser"
	alternate := "repo/format.go::RegisterDefaultFormatter"
	third := "repo/format.go::Register"
	state.armRefinementRoutesForTask(
		"find where the short culture formatter is registered",
		preferred,
		[]string{preferred, alternate, third},
		map[string]localizationRefinementRoute{
			preferred: {},
			alternate: {enforceable: true},
			third:     {},
		},
		nil,
	)

	blocked, token := state.authorizeWithToken("read", "source", refinementSourceArgs(preferred))
	if blocked != nil || token == 0 {
		t.Fatalf("preferred weak read = (%+v, %d), want reservation", blocked, token)
	}
	completion := state.finishReservedReadToken(token, true)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, alternate, 1)
	if completion.Enforceable {
		t.Fatal("weak correction completion became enforceable")
	}
	if completion.RequiredAction != "read_exact" ||
		!strings.Contains(completion.Instruction, alternate) ||
		!strings.Contains(completion.Instruction, `read(operation:"source"`) ||
		!strings.Contains(completion.Instruction, "only permitted corrective read") {
		t.Fatalf("correction contract is not machine-readable and directional: %#v", completion)
	}

	if result, reserved := state.authorize("read", "source", refinementSourceArgs(third)); reserved || result == nil {
		t.Fatalf("wrong correction read = (%+v, %v), want blocked", result, reserved)
	}
	blocked, token = state.authorizeWithToken("read", "source", refinementSourceArgs(alternate))
	if blocked != nil || token == 0 {
		t.Fatalf("exact correction read = (%+v, %d), want reservation", blocked, token)
	}
	completion = state.finishReservedReadToken(token, true)
	requireRefinementCompletion(t, completion, localizationStateAnswerReady, "", 0)
	if !completion.Enforceable {
		t.Fatal("prevalidated correction lost enforceability")
	}

	if result, reserved := state.authorize("read", "source", refinementSourceArgs(third)); reserved {
		t.Fatal("third read reserved a handler")
	} else {
		requireLocalizationTerminalReplay(t, result, "read", "source")
	}
}

func TestStrongPreferredReadRemainsOneReadTerminal(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/replacer.go::Replacer.replaceAll"
	alternate := "repo/replacer.go::Replacer.Replace"
	state.armRefinementRoutesForTask(
		"find the exact replacement implementation",
		preferred,
		[]string{preferred, alternate},
		map[string]localizationRefinementRoute{
			preferred: {enforceable: true},
			alternate: {},
		},
		nil,
	)

	blocked, token := state.authorizeWithToken("read", "source", refinementSourceArgs(preferred))
	if blocked != nil || token == 0 {
		t.Fatalf("strong preferred read = (%+v, %d), want reservation", blocked, token)
	}
	completion := state.finishReservedReadToken(token, true)
	requireRefinementCompletion(t, completion, localizationStateAnswerReady, "", 0)
	if !completion.Enforceable {
		t.Fatal("strong completion lost enforceability")
	}
	if result, reserved := state.authorize("read", "source", refinementSourceArgs(alternate)); reserved {
		t.Fatal("strong route opened a corrective read")
	} else {
		requireLocalizationTerminalReplay(t, result, "read", "source")
	}
}

func TestWeakPreferredReadExecutesGenericCorrectionRoute(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/search.rs::find_candidate"
	generic := "repo/replacer.rs::Replacer::replace"
	implementation := "repo/replacer.rs::Replacer::replace_all"
	route := localizationRefinementRoute{
		implementationSymbol: implementation,
		proofSymbol:          "repo/replacer.rs::Replacer",
		enforceable:          true,
	}
	state.armRefinementRoutesForTask(
		"find the replacement implementation",
		preferred,
		[]string{preferred, generic, implementation},
		map[string]localizationRefinementRoute{
			preferred:      {},
			generic:        route,
			implementation: {enforceable: true},
		},
		nil,
	)

	blocked, token := state.authorizeWithToken("read", "source", refinementSourceArgs(preferred))
	if blocked != nil || token == 0 {
		t.Fatalf("preferred weak read = (%+v, %d), want reservation", blocked, token)
	}
	completion := state.finishReservedReadToken(token, true)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, generic, 1)
	if state.exactReadRoute != route {
		t.Fatalf("stored correction route = %#v, want %#v", state.exactReadRoute, route)
	}

	blocked, token = state.authorizeWithToken("read", "source", refinementSourceArgs(generic))
	if blocked != nil || token == 0 {
		t.Fatalf("generic correction read = (%+v, %d), want reservation", blocked, token)
	}
	completion = state.finishReservedReadToken(token, true)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, implementation, 1)

	blocked, token = state.authorizeWithToken("read", "source", refinementSourceArgs(implementation))
	if blocked != nil || token == 0 {
		t.Fatalf("implementation correction read = (%+v, %d), want reservation", blocked, token)
	}
	completion = state.finishReservedReadToken(token, true)
	requireRefinementCompletion(t, completion, localizationStateAnswerReady, "", 0)
	if !completion.Enforceable {
		t.Fatal("proven generic correction route lost enforceability")
	}
	if result, reserved := state.authorize("read", "source", refinementSourceArgs(generic)); reserved {
		t.Fatal("fourth route read reserved a handler")
	} else {
		requireLocalizationTerminalReplay(t, result, "read", "source")
	}
}

func TestWeakPreferredReadSkipsUnprovenAlternate(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/search.go::findCandidate"
	unproven := "repo/search.go::validateCandidate"
	prevalidated := "repo/search.go::resolveCandidate"
	state.armRefinementRoutesForTask(
		"find candidate resolution",
		preferred,
		[]string{preferred, unproven, prevalidated},
		map[string]localizationRefinementRoute{
			preferred:    {},
			unproven:     {},
			prevalidated: {enforceable: true},
		},
		nil,
	)

	requireRefinementSourceReservation(t, state, preferred)
	completion := state.finishReservedRead(true)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, prevalidated, 1)
}

func TestGenericCorrectionSharesOneRetryAcrossRouteHops(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/search.rs::find_candidate"
	generic := "repo/replacer.rs::Replacer::replace"
	implementation := "repo/replacer.rs::Replacer::replace_all"
	state.armRefinementRoutesForTask(
		"find the replacement implementation",
		preferred,
		[]string{preferred, generic, implementation},
		map[string]localizationRefinementRoute{
			preferred: {},
			generic:   {implementationSymbol: implementation, enforceable: true},
			implementation: {
				enforceable: true,
			},
		},
		nil,
	)

	requireRefinementSourceReservation(t, state, preferred)
	completion := state.finishReservedRead(true)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, generic, 1)

	requireRefinementSourceReservation(t, state, generic)
	completion = state.finishReservedRead(false)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, generic, 1)

	requireRefinementSourceReservation(t, state, generic)
	completion = state.finishReservedRead(true)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, implementation, 1)

	requireRefinementSourceReservation(t, state, implementation)
	completion = state.finishReservedRead(false)
	requireRefinementCompletion(t, completion, localizationStateAnswerReady, "", 0)
	if result, reserved := state.authorize("read", "source", refinementSourceArgs(implementation)); reserved {
		t.Fatal("implementation hop received a second route-level retry")
	} else {
		requireLocalizationTerminalReplay(t, result, "read", "source")
	}
}

func TestWeakPreferredReadWithoutPrevalidatedAlternateOffersRecovery(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/search.go::findCandidate"
	unproven := "repo/search.go::validateCandidate"
	state.armRefinementForTask("find candidate resolution", preferred, []string{preferred, unproven}, nil)

	requireRefinementSourceReservation(t, state, preferred)
	completion := state.finishReservedRead(true)
	requireRefinementCompletion(t, completion, localizationStateNeedsRecovery, "", 1)
	if completion.RequiredAction != "recover_once" {
		t.Fatalf("weak preferred completion action = %q, want recover_once", completion.RequiredAction)
	}
}

func TestInitialRefinementFailureRestoresOnlyOnce(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/search.go::findCandidate"
	state.armRefinementForTask("find candidate resolution", preferred, []string{preferred}, nil)

	requireRefinementSourceReservation(t, state, preferred)
	completion := state.finishReservedRead(false)
	requireRefinementCompletion(t, completion, localizationStateNeedsRefinement, "", 1)

	requireRefinementSourceReservation(t, state, preferred)
	completion = state.finishReservedRead(false)
	requireRefinementCompletion(t, completion, localizationStateAnswerReady, "", 0)
	if result, reserved := state.authorize("read", "source", refinementSourceArgs(preferred)); reserved {
		t.Fatal("initial refinement failure restored more than once")
	} else {
		requireLocalizationTerminalReplay(t, result, "read", "source")
	}
}

func TestWeakCorrectionFailureRestoresOnlyOnce(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/localize.go::FindLocaliser"
	alternate := "repo/format.go::RegisterDefaultFormatter"
	state.armRefinementRoutesForTask(
		"find the formatter registration",
		preferred,
		[]string{preferred, alternate},
		map[string]localizationRefinementRoute{
			preferred: {},
			alternate: {enforceable: true},
		},
		nil,
	)

	blocked, token := state.authorizeWithToken("read", "source", refinementSourceArgs(preferred))
	if blocked != nil || token == 0 {
		t.Fatalf("preferred read = (%+v, %d), want reservation", blocked, token)
	}
	completion := state.finishReservedReadToken(token, false)
	requireRefinementCompletion(t, completion, localizationStateNeedsRefinement, "", 1)

	blocked, token = state.authorizeWithToken("read", "source", refinementSourceArgs(preferred))
	if blocked != nil || token == 0 {
		t.Fatalf("restored preferred read = (%+v, %d), want reservation", blocked, token)
	}
	completion = state.finishReservedReadToken(token, true)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, alternate, 1)

	blocked, token = state.authorizeWithToken("read", "source", refinementSourceArgs(alternate))
	if blocked != nil || token == 0 {
		t.Fatalf("correction read = (%+v, %d), want reservation", blocked, token)
	}
	completion = state.finishReservedReadToken(token, false)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, alternate, 1)

	blocked, token = state.authorizeWithToken("read", "source", refinementSourceArgs(alternate))
	if blocked != nil || token == 0 {
		t.Fatalf("restored correction read = (%+v, %d), want reservation", blocked, token)
	}
	completion = state.finishReservedReadToken(token, false)
	requireRefinementCompletion(t, completion, localizationStateAnswerReady, "", 0)
	if result, reserved := state.authorize("read", "source", refinementSourceArgs(alternate)); reserved {
		t.Fatal("correction was restored more than once")
	} else {
		requireLocalizationTerminalReplay(t, result, "read", "source")
	}
}

func TestStaleWeakReadCannotConsumeNewTaskCorrection(t *testing.T) {
	state := &localizationTerminalState{}
	oldPreferred := "repo/old.go::Find"
	oldAlternate := "repo/old.go::Register"
	state.armRefinementRoutesForTask(
		"old task",
		oldPreferred,
		[]string{oldPreferred, oldAlternate},
		map[string]localizationRefinementRoute{oldPreferred: {}, oldAlternate: {enforceable: true}},
		nil,
	)
	blocked, oldToken := state.authorizeWithToken("read", "source", refinementSourceArgs(oldPreferred))
	if blocked != nil || oldToken == 0 {
		t.Fatalf("old read = (%+v, %d), want reservation", blocked, oldToken)
	}

	state.reset()
	newPreferred := "repo/new.go::Find"
	newAlternate := "repo/new.go::Register"
	state.armRefinementRoutesForTask(
		"new task",
		newPreferred,
		[]string{newPreferred, newAlternate},
		map[string]localizationRefinementRoute{newPreferred: {}, newAlternate: {enforceable: true}},
		nil,
	)
	blocked, newToken := state.authorizeWithToken("read", "source", refinementSourceArgs(newPreferred))
	if blocked != nil || newToken == 0 || newToken == oldToken {
		t.Fatalf("new read = (%+v, %d), old token %d", blocked, newToken, oldToken)
	}

	if stale := state.finishReservedReadToken(oldToken, true); stale.State != localizationStateInactive {
		t.Fatalf("stale completion state = %q, want inactive", stale.State)
	}
	completion := state.finishReservedReadToken(newToken, true)
	requireRefinementCompletion(t, completion, localizationStateNeedsExactRead, newAlternate, 1)
}

func refinementSourceArgs(symbol string) map[string]any {
	return map[string]any{"target": map[string]any{"symbol": symbol}}
}

func requireRefinementSourceReservation(t *testing.T, state *localizationTerminalState, symbol string) {
	t.Helper()
	blocked, reserved := state.authorize("read", "source", refinementSourceArgs(symbol))
	if blocked != nil || !reserved {
		t.Fatalf("source read reservation for %q = (%+v, %v), want reservation", symbol, blocked, reserved)
	}
}

func requireRefinementCompletion(t *testing.T, completion localizationCompletion, state, exactSymbol string, allowed int) {
	t.Helper()
	if completion.State != state || completion.ExactSymbol != exactSymbol || completion.AllowedToolCalls != allowed {
		t.Fatalf(
			"completion = {state:%q exact:%q allowed:%d}, want {state:%q exact:%q allowed:%d}",
			completion.State,
			completion.ExactSymbol,
			completion.AllowedToolCalls,
			state,
			exactSymbol,
			allowed,
		)
	}
}
