package analysis

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// GuardViolation describes a single guard rule violation.
type GuardViolation struct {
	RuleName    string `json:"rule_name"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	// Architecture-DSL fields. Populated only on layer / architecture
	// rule violations; empty on the flat co-change / boundary kinds.
	Violator  string `json:"violator,omitempty"`
	LayerFrom string `json:"layer_from,omitempty"`
	LayerTo   string `json:"layer_to,omitempty"`
	EdgeType  string `json:"edge_type,omitempty"`
	// Severity tiers the violation for the change_contract verdict mapping:
	// "error" → refuse, "warn" → warn, "info" → annotate. Stamped from the
	// rule; empty means the consumer applies its own default.
	Severity string `json:"severity,omitempty"`
}

// ruleSeverity normalises a configured severity, defaulting to "warn" so a
// rule advises until it explicitly opts into blocking ("error").
func ruleSeverity(s string) string {
	if s == "" {
		return "warn"
	}
	return strings.ToLower(s)
}

// matchesAnyGlob reports whether path p matches any of the (possibly **-using)
// globs — the except-list check shared by the guard and architecture families.
func matchesAnyGlob(p string, globs []string) bool {
	for _, g := range globs {
		if g != "" && globMatch(g, p) {
			return true
		}
	}
	return false
}

// EvaluateGuards checks the given guard rules against a set of changed symbol IDs
// and the graph's edge structure, returning any violations found.
//
// For "co-change" rules: reports a violation when the change set contains symbols
// whose file paths match the rule's Source prefix but none matching the Target prefix.
//
// For "boundary" rules: reports a violation when any changed symbol whose file path
// matches the Source prefix has outgoing call or reference edges to symbols whose
// file paths match the Target prefix.
func EvaluateGuards(g graph.Reader, rules []config.GuardRule, changedSymbolIDs []string) []GuardViolation {
	var violations []GuardViolation

	// Pre-resolve changed symbols to nodes for efficient lookup.
	changedNodes := make([]*graph.Node, 0, len(changedSymbolIDs))
	for _, id := range changedSymbolIDs {
		if n := g.GetNode(id); n != nil {
			changedNodes = append(changedNodes, n)
		}
	}

	for _, rule := range rules {
		switch rule.Kind {
		case "co-change":
			violations = append(violations, evaluateCoChange(rule, changedNodes)...)
		case "boundary":
			violations = append(violations, evaluateBoundary(g, rule, changedNodes)...)
		}
	}

	return violations
}

// evaluateCoChange checks whether the change set includes symbols from the source
// prefix but is missing symbols from the target prefix.
func evaluateCoChange(rule config.GuardRule, changedNodes []*graph.Node) []GuardViolation {
	hasSource := false
	hasTarget := false

	for _, n := range changedNodes {
		if matchesAnyGlob(n.FilePath, rule.Except) {
			continue
		}
		if graphpath.HasPrefix(n.FilePath, rule.Source) {
			hasSource = true
		}
		if graphpath.HasPrefix(n.FilePath, rule.Target) {
			hasTarget = true
		}
		if hasSource && hasTarget {
			return nil // both present, no violation
		}
	}

	if hasSource && !hasTarget {
		msg := rule.Message
		if msg == "" {
			msg = fmt.Sprintf("changes to %s require corresponding changes to %s", rule.Source, rule.Target)
		}
		return []GuardViolation{{
			RuleName:    rule.Name,
			Kind:        "co-change",
			Description: msg,
			Severity:    ruleSeverity(rule.Severity),
		}}
	}

	return nil
}

// evaluateBoundary checks whether any changed symbol in the source prefix has
// outgoing call or reference edges targeting symbols in the target prefix.
func evaluateBoundary(g graph.Reader, rule config.GuardRule, changedNodes []*graph.Node) []GuardViolation {
	var violations []GuardViolation
	seen := make(map[string]bool)

	for _, n := range changedNodes {
		if !graphpath.HasPrefix(n.FilePath, rule.Source) {
			continue
		}
		if matchesAnyGlob(n.FilePath, rule.Except) {
			continue
		}

		outEdges := g.GetOutEdges(n.ID)
		for _, edge := range outEdges {
			if edge.Kind != graph.EdgeCalls && edge.Kind != graph.EdgeReferences {
				continue
			}

			target := g.GetNode(edge.To)
			if target == nil {
				continue
			}

			if !graphpath.HasPrefix(target.FilePath, rule.Target) {
				continue
			}

			// Deduplicate by source→target pair.
			key := n.ID + "->" + target.ID
			if seen[key] {
				continue
			}
			seen[key] = true

			msg := rule.Message
			if msg == "" {
				msg = fmt.Sprintf("%s must not directly reference %s", rule.Source, rule.Target)
			}

			violations = append(violations, GuardViolation{
				RuleName:    rule.Name,
				Kind:        "boundary",
				Description: fmt.Sprintf("%s: %s %s %s", msg, n.ID, edge.Kind, target.ID),
				Violator:    n.ID,
				Severity:    ruleSeverity(rule.Severity),
			})
		}
	}

	return violations
}
