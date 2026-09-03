package analysis

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// SignatureChange describes a proposed change to a symbol's signature.
type SignatureChange struct {
	SymbolID     string `json:"symbol_id"`
	NewSignature string `json:"new_signature"` // e.g. "func(ctx context.Context, id string) (Result, error)"
}

// ContractViolation describes a single contract violation found during verification.
type ContractViolation struct {
	SymbolID    string `json:"symbol_id"`
	Name        string `json:"name"`
	FilePath    string `json:"file_path"`
	Line        int    `json:"line"`
	Kind        string `json:"kind"` // "caller_mismatch", "interface_violation", "removed_param"
	Description string `json:"description"`
	RepoPrefix  string `json:"repo_prefix,omitempty"`
}

// VerifyResult is the output of contract violation verification.
type VerifyResult struct {
	Violations          []ContractViolation  `json:"violations"`
	CheckedCallers      int                  `json:"checked_callers"`
	CheckedImpls        int                  `json:"checked_impls"`
	Clean               bool                 `json:"clean"`
	Errors              []string             `json:"errors,omitempty"`
	CrossRepoViolations bool                 `json:"cross_repo_violations,omitempty"`
	ReturnUsage         []ReturnUsageSummary `json:"return_usage,omitempty"`
}

// ReturnUsageSummary aggregates how the call sites of one changed
// function or method consume its return value — the "who actually uses
// the return?" answer an agent needs before changing a return
// signature. Counts come from the extractor-stamped return-usage label
// on each incoming call edge; call sites the classifier could not
// place are reported as unclassified rather than guessed.
type ReturnUsageSummary struct {
	SymbolID     string         `json:"symbol_id"`
	CallSites    int            `json:"call_sites"`
	Counts       map[string]int `json:"counts,omitempty"`
	Unclassified int            `json:"unclassified,omitempty"`
}

// summarizeReturnUsage builds the return-usage distribution for one
// function/method's incoming call edges. Returns nil when the symbol
// has no call sites at all (nothing to report).
func summarizeReturnUsage(g graph.Reader, node *graph.Node) *ReturnUsageSummary {
	if node == nil || (node.Kind != graph.KindFunction && node.Kind != graph.KindMethod) {
		return nil
	}
	summary := &ReturnUsageSummary{SymbolID: node.ID}
	for _, e := range g.GetInEdges(node.ID) {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		// Skip speculative dispatch edges: the read surfaces (find_usages
		// and the rest) hide them by default, so counting them here would
		// make this distribution disagree with the call sites a user
		// actually sees.
		if e.IsSpeculative() {
			continue
		}
		summary.CallSites++
		if usage := graph.ReturnUsageOf(e); usage != "" {
			if summary.Counts == nil {
				summary.Counts = map[string]int{}
			}
			summary.Counts[usage]++
		} else {
			summary.Unclassified++
		}
	}
	if summary.CallSites == 0 {
		return nil
	}
	return summary
}

// parsedSignature holds the extracted parameter and return type info from a signature string.
type parsedSignature struct {
	Params  []string
	Returns []string
}

// VerifyChanges checks proposed signature changes against all callers and interface
// implementors, returning any contract violations found.
func VerifyChanges(g graph.Reader, engine *query.Engine, changes []SignatureChange) *VerifyResult {
	result := &VerifyResult{}

	for _, change := range changes {
		node := g.GetNode(change.SymbolID)
		if node == nil {
			// Report error for missing symbol, continue checking others
			result.Errors = append(result.Errors, fmt.Sprintf("symbol not found: %s", change.SymbolID))
			continue
		}

		newSig := parseSignature(change.NewSignature)

		// Get old signature from node metadata
		var oldSig parsedSignature
		if meta := node.Meta; meta != nil {
			if sig, ok := meta["signature"].(string); ok {
				oldSig = parseSignature(sig)
			}
		}

		// For a function/method, summarise how its call sites consume
		// the return value — the sites that bind / return / branch on the
		// result are the ones a return-type change touches. The discarded
		// count is not a blanket all-clear: a discarded label also folds
		// every-sink-blank multi-assignment (Go `_, _ = f()`), which still
		// breaks on a return-arity change because the blank list must
		// match the result count. Read it as "the value is unused here",
		// not "this site is safe to change".
		if summary := summarizeReturnUsage(g, node); summary != nil {
			result.ReturnUsage = append(result.ReturnUsage, *summary)
		}

		// Check callers for parameter mismatches
		callerSG := engine.GetCallers(change.SymbolID, query.QueryOptions{Depth: 2, Limit: 500})
		for _, callerNode := range callerSG.Nodes {
			if callerNode.ID == change.SymbolID {
				continue
			}
			result.CheckedCallers++

			if len(oldSig.Params) != len(newSig.Params) {
				result.Violations = append(result.Violations, ContractViolation{
					SymbolID:    callerNode.ID,
					Name:        callerNode.Name,
					FilePath:    callerNode.FilePath,
					Line:        callerNode.StartLine,
					Kind:        "caller_mismatch",
					Description: fmt.Sprintf("parameter count changed from %d to %d in %s", len(oldSig.Params), len(newSig.Params), change.SymbolID),
					RepoPrefix:  callerNode.RepoPrefix,
				})
			} else {
				// Check for type changes in parameters
				for i := range oldSig.Params {
					if oldSig.Params[i] != newSig.Params[i] {
						result.Violations = append(result.Violations, ContractViolation{
							SymbolID:    callerNode.ID,
							Name:        callerNode.Name,
							FilePath:    callerNode.FilePath,
							Line:        callerNode.StartLine,
							Kind:        "caller_mismatch",
							Description: fmt.Sprintf("parameter %d type changed from %s to %s in %s", i+1, oldSig.Params[i], newSig.Params[i], change.SymbolID),
							RepoPrefix:  callerNode.RepoPrefix,
						})
						break // one violation per caller is enough
					}
				}
			}
		}

		// Check for removed parameters specifically
		if len(newSig.Params) < len(oldSig.Params) {
			for i := len(newSig.Params); i < len(oldSig.Params); i++ {
				// Find callers that pass the removed parameter
				for _, callerNode := range callerSG.Nodes {
					if callerNode.ID == change.SymbolID {
						continue
					}
					result.Violations = append(result.Violations, ContractViolation{
						SymbolID:    callerNode.ID,
						Name:        callerNode.Name,
						FilePath:    callerNode.FilePath,
						Line:        callerNode.StartLine,
						Kind:        "removed_param",
						Description: fmt.Sprintf("parameter %d (%s) removed from %s", i+1, oldSig.Params[i], change.SymbolID),
						RepoPrefix:  callerNode.RepoPrefix,
					})
				}
			}
		}

		// Check interface implementors
		checkInterfaceViolations(g, engine, node, &newSig, result)
	}

	// Deduplicate violations by (SymbolID, Kind, Description)
	result.Violations = deduplicateViolations(result.Violations)

	// Detect cross-repo violations: check if any violation comes from
	// a different repo than the changed symbol.
	changedRepos := make(map[string]bool)
	for _, change := range changes {
		if n := g.GetNode(change.SymbolID); n != nil && n.RepoPrefix != "" {
			changedRepos[n.RepoPrefix] = true
		}
	}
	for _, v := range result.Violations {
		if v.RepoPrefix != "" && !changedRepos[v.RepoPrefix] {
			result.CrossRepoViolations = true
			break
		}
	}

	result.Clean = len(result.Violations) == 0
	return result
}

// checkInterfaceViolations checks if the changed symbol is a method that belongs to
// an interface, and if so, verifies all other implementors still conform.
// Traversal: EdgeMemberOf → parent type → EdgeImplements → interface → all implementors
func checkInterfaceViolations(g graph.Reader, engine *query.Engine, node *graph.Node, newSig *parsedSignature, result *VerifyResult) {
	if node.Kind != graph.KindMethod {
		return
	}

	// Find parent type via EdgeMemberOf
	outEdges := g.GetOutEdges(node.ID)
	for _, edge := range outEdges {
		if edge.Kind != graph.EdgeMemberOf {
			continue
		}
		parentNode := g.GetNode(edge.To)
		if parentNode == nil {
			continue
		}

		// Find interfaces this parent type implements via EdgeImplements
		parentOutEdges := g.GetOutEdges(parentNode.ID)
		for _, implEdge := range parentOutEdges {
			if implEdge.Kind != graph.EdgeImplements {
				continue
			}
			interfaceNode := g.GetNode(implEdge.To)
			if interfaceNode == nil {
				continue
			}

			// Find all other types that implement this interface
			implNodes := engine.FindImplementations(interfaceNode.ID)
			for _, implNode := range implNodes {
				if implNode.ID == parentNode.ID {
					continue // skip the type we're changing
				}
				result.CheckedImpls++

				// Find the corresponding method on this implementor
				implMethods := findMemberMethods(g, implNode.ID)
				for _, method := range implMethods {
					if method.Name != node.Name {
						continue
					}

					// Compare signatures
					var implSig parsedSignature
					if meta := method.Meta; meta != nil {
						if sig, ok := meta["signature"].(string); ok {
							implSig = parseSignature(sig)
						}
					}

					if len(implSig.Params) != len(newSig.Params) {
						result.Violations = append(result.Violations, ContractViolation{
							SymbolID:    method.ID,
							Name:        method.Name,
							FilePath:    method.FilePath,
							Line:        method.StartLine,
							Kind:        "interface_violation",
							Description: fmt.Sprintf("implementor %s has %d params but interface method now requires %d", implNode.Name, len(implSig.Params), len(newSig.Params)),
						})
					} else {
						for i := range implSig.Params {
							if implSig.Params[i] != newSig.Params[i] {
								result.Violations = append(result.Violations, ContractViolation{
									SymbolID:    method.ID,
									Name:        method.Name,
									FilePath:    method.FilePath,
									Line:        method.StartLine,
									Kind:        "interface_violation",
									Description: fmt.Sprintf("implementor %s param %d is %s but interface method now requires %s", implNode.Name, i+1, implSig.Params[i], newSig.Params[i]),
								})
								break
							}
						}
					}
				}
			}
		}
	}
}

// findMemberMethods returns all method nodes that are members of the given type.
func findMemberMethods(g graph.Reader, typeID string) []*graph.Node {
	inEdges := g.GetInEdges(typeID)
	var methods []*graph.Node
	for _, edge := range inEdges {
		if edge.Kind != graph.EdgeMemberOf {
			continue
		}
		n := g.GetNode(edge.From)
		if n != nil && n.Kind == graph.KindMethod {
			methods = append(methods, n)
		}
	}
	return methods
}

// parseSignature extracts parameter types and return types from a Go-style
// function signature string like "func(ctx context.Context, id string) (Result, error)".
func parseSignature(sig string) parsedSignature {
	result := parsedSignature{}
	sig = strings.TrimSpace(sig)

	if sig == "" {
		return result
	}

	// Strip leading "func" keyword if present
	if strings.HasPrefix(sig, "func") {
		sig = strings.TrimPrefix(sig, "func")
		sig = strings.TrimSpace(sig)
	}

	// Find the parameter list: first balanced parentheses
	params, rest := extractBalancedParens(sig)
	if params != "" {
		result.Params = parseParamList(params)
	}

	// Find return types: remaining balanced parentheses or single type
	rest = strings.TrimSpace(rest)
	if rest != "" {
		if strings.HasPrefix(rest, "(") {
			returns, _ := extractBalancedParens(rest)
			if returns != "" {
				result.Returns = parseParamList(returns)
			}
		} else {
			// Single return type
			result.Returns = []string{strings.TrimSpace(rest)}
		}
	}

	return result
}

// extractBalancedParens extracts the content of the first balanced parentheses
// and returns the content (without parens) and the remaining string.
func extractBalancedParens(s string) (content, rest string) {
	start := strings.IndexByte(s, '(')
	if start < 0 {
		return "", s
	}

	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[start+1 : i], s[i+1:]
			}
		}
	}
	// Unbalanced — return what we have
	return s[start+1:], ""
}

// parseParamList splits a comma-separated parameter list and extracts the types.
// Handles named params like "ctx context.Context, id string" and unnamed like "int, string".
func parseParamList(params string) []string {
	params = strings.TrimSpace(params)
	if params == "" {
		return nil
	}

	// Split by comma, respecting nested generics/parens
	parts := splitParams(params)
	var types []string

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		typ := extractType(part)
		types = append(types, typ)
	}

	return types
}

// splitParams splits a parameter string by commas, respecting nested brackets.
func splitParams(s string) []string {
	var parts []string
	depth := 0
	start := 0

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// extractType extracts the type from a parameter declaration.
// "ctx context.Context" → "context.Context"
// "id string" → "string"
// "string" → "string" (unnamed)
// "...string" → "...string" (variadic)
func extractType(param string) string {
	param = strings.TrimSpace(param)

	// Split by whitespace
	fields := strings.Fields(param)
	if len(fields) == 0 {
		return ""
	}

	// If only one field, it's the type itself (unnamed parameter)
	if len(fields) == 1 {
		return fields[0]
	}

	// Last field is the type (handles "ctx context.Context" and "x, y int" patterns)
	return fields[len(fields)-1]
}

// deduplicateViolations removes duplicate violations based on (SymbolID, Kind, Description).
func deduplicateViolations(violations []ContractViolation) []ContractViolation {
	type key struct {
		symbolID    string
		kind        string
		description string
	}
	seen := make(map[key]bool)
	var result []ContractViolation

	for _, v := range violations {
		k := key{symbolID: v.SymbolID, kind: v.Kind, description: v.Description}
		if !seen[k] {
			seen[k] = true
			result = append(result, v)
		}
	}
	return result
}
