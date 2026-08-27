package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestLocalizationRefinementRequiredActionNamesFacadeReadSelector(t *testing.T) {
	const symbol = "repo/pkg/file.go::Resolver.Run"
	want := fmt.Sprintf(localizationRefinementRequiredActionFormat, symbol)
	completion := newLocalizationRefinementCompletion(symbol)
	if got := completion.RequiredAction; got != want {
		t.Fatalf("refinement action = %q, want %q", got, want)
	}
	if completion.refinementSymbol != symbol {
		t.Fatalf("refinement symbol = %q, want %q", completion.refinementSymbol, symbol)
	}
	if len(completion.AllowedSymbols) != 1 || completion.AllowedSymbols[0] != symbol {
		t.Fatalf("allowed symbols = %v, want [%q]", completion.AllowedSymbols, symbol)
	}
	if completion.ExactSymbol != "" {
		t.Fatalf("uncertain refinement falsely advertised exact symbol %q", completion.ExactSymbol)
	}

	encoded, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal completion: %v", err)
	}
	if got := payload["required_action"]; got != want {
		t.Fatalf("serialized refinement action = %q, want %q", got, want)
	}
	if _, exists := payload["exact_symbol"]; exists {
		t.Fatalf("uncertain refinement serialized exact_symbol: %#v", payload)
	}
}

// A directive naming one symbol is obeyed whether or not that symbol is the
// answer. The ranked set is named instead, and the instruction says what to do
// when the first candidate misses, so obeying it stays productive.
func TestRefinementDirectiveNamesTheRankedCandidateSetAndItsFallback(t *testing.T) {
	symbols := []string{
		"repo/pkg/resolve.go::Resolver.Run",
		"repo/pkg/dispatch.go::Dispatcher.Run",
		"repo/pkg/worker.go::Worker.Run",
		"repo/pkg/legacy.go::Legacy.Run",
	}
	completion := newLocalizationRefinementCompletionForSymbols(symbols[0], symbols)

	authorized := make(map[string]struct{}, len(completion.AllowedSymbols))
	for _, symbol := range completion.AllowedSymbols {
		authorized[symbol] = struct{}{}
	}
	for _, named := range symbols[:localizationRefinementNamedCandidateCap] {
		if !strings.Contains(completion.RequiredAction, named) {
			t.Fatalf("required action omits ranked candidate %q: %q", named, completion.RequiredAction)
		}
		if _, permitted := authorized[named]; !permitted {
			t.Fatalf("required action named %q outside allowed_symbols %v", named, completion.AllowedSymbols)
		}
	}
	if strings.Contains(completion.RequiredAction, symbols[localizationRefinementNamedCandidateCap]) {
		t.Fatalf("required action named more than the ranked head: %q", completion.RequiredAction)
	}
	for _, required := range []string{
		"does not match the task's anchor terms",
		"one bounded search before answering",
		"completion.allowed_symbols",
	} {
		if !strings.Contains(completion.RequiredAction, required) {
			t.Fatalf("required action is missing %q: %q", required, completion.RequiredAction)
		}
	}
}

// The refinement page is byte-budgeted. Widening the directive to a set may
// cost clauses that restate machine fields, never a candidate the caller would
// otherwise be unable to reach.
func TestRefinementDirectiveHoldsItsByteBoundOnLongIdentities(t *testing.T) {
	nested := strings.Repeat("deeply/nested/", 8)
	symbols := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		symbols = append(symbols, fmt.Sprintf("repo/%sservice%d.go::Service.HandleRequest%d", nested, index, index))
	}
	action := localizationRefinementRequiredAction(symbols[0], symbols)
	if len(action) > localizationRefinementRequiredActionMaxBytes {
		t.Fatalf("refinement directive = %d bytes, want <= %d: %q",
			len(action), localizationRefinementRequiredActionMaxBytes, action)
	}
	if !strings.Contains(action, symbols[0]) || !strings.Contains(action, symbols[1]) {
		t.Fatalf("byte trimming dropped a reachable candidate: %q", action)
	}
}
