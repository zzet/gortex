package mcp

import (
	"fmt"
	"strings"
)

// The refinement page is byte-budgeted (see the tight-budget builder test), so
// this stays the happy-path instruction only. The release that unsticks a wrong
// ranking is named on the refusals, which is where a blocked caller reads.
const localizationRefinementRequiredActionFormat = `Call Gortex MCP read(operation:"source", target:{symbol:%q}); the named symbol is recommended; any returned candidate is permitted only when its ID appears in completion.allowed_symbols; do not call a host file-read tool.`

// A directive naming one symbol is obeyed whether or not that symbol is the
// answer, and the ranked alternate that was the answer is never opened. Naming
// the ranked set — and saying in the same sentence what to do when the first
// candidate does not match — keeps obedience productive when the ranking is
// wrong, which is the case this contract exists for.
const localizationRefinementRequiredActionSetFormat = `Call Gortex MCP read(operation:"source", target:{symbol:%q}); if it does not match the task's anchor terms, try %s, or run one bounded search before answering;`

const (
	localizationRefinementAllowanceClause = ` any returned candidate is permitted only when its ID appears in completion.allowed_symbols;`
	localizationRefinementHostToolClause  = ` do not call a host file-read tool.`
	// localizationRefinementNamedCandidateCap names the preferred symbol plus
	// two ranked alternates. A longer list costs page bytes the evidence rows
	// need and stops reading as a decision.
	localizationRefinementNamedCandidateCap = 3
	// localizationRefinementRequiredActionMaxBytes bounds the directive so a
	// widened candidate set cannot push the refinement envelope over budget.
	localizationRefinementRequiredActionMaxBytes = 512
)

// localizationRefinementRequiredAction renders the directive for the ranked
// candidate set. The trailing clauses are dropped before a named candidate is:
// both restate machine fields the completion already carries, while a dropped
// candidate is a symbol the caller can no longer reach.
func localizationRefinementRequiredAction(preferred string, allowed []string) string {
	preferred = strings.TrimSpace(preferred)
	alternates := localizationRefinementAlternates(preferred, allowed)
	for count := len(alternates); count > 0; count-- {
		lead := fmt.Sprintf(localizationRefinementRequiredActionSetFormat, preferred,
			localizationRefinementAlternateList(alternates[:count]))
		for _, tail := range []string{
			localizationRefinementAllowanceClause + localizationRefinementHostToolClause,
			localizationRefinementHostToolClause,
			"",
		} {
			if action := lead + tail; len(action) <= localizationRefinementRequiredActionMaxBytes {
				return action
			}
		}
	}
	return fmt.Sprintf(localizationRefinementRequiredActionFormat, preferred)
}

func localizationRefinementAlternates(preferred string, allowed []string) []string {
	alternates := make([]string, 0, localizationRefinementNamedCandidateCap-1)
	seen := map[string]struct{}{preferred: {}}
	for _, symbol := range allowed {
		symbol = strings.TrimSpace(symbol)
		if symbol == "" {
			continue
		}
		if _, duplicate := seen[symbol]; duplicate {
			continue
		}
		seen[symbol] = struct{}{}
		alternates = append(alternates, symbol)
		if len(alternates) == localizationRefinementNamedCandidateCap-1 {
			break
		}
	}
	return alternates
}

func localizationRefinementAlternateList(alternates []string) string {
	quoted := make([]string, 0, len(alternates))
	for _, symbol := range alternates {
		quoted = append(quoted, fmt.Sprintf("%q", symbol))
	}
	if len(quoted) < 2 {
		return strings.Join(quoted, "")
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}
