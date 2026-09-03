package analysis

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// EvaluateArchitecture checks the declarative architecture DSL — named
// layers with directional allow/deny constraints — against a set of
// changed symbols.
//
// For each changed symbol it resolves the symbol's layer, walks its
// outgoing call / reference edges, resolves each target's layer, and
// reports a violation when a cross-layer dependency breaks the source
// layer's allow/deny rules. Symbols in no declared layer, and edges
// to such symbols, are unconstrained.
func EvaluateArchitecture(g graph.Reader, arch config.ArchitectureConfig, changedSymbolIDs []string) []GuardViolation {
	if g == nil || arch.IsEmpty() {
		return nil
	}
	names := sortedLayerNames(arch.Layers)

	var violations []GuardViolation
	seen := make(map[string]bool)
	for _, id := range changedSymbolIDs {
		n := g.GetNode(id)
		if n == nil {
			continue
		}
		fromLayer := layerOf(effectivePath(n), arch.Layers, names)
		if fromLayer == "" {
			continue
		}
		for _, e := range g.GetOutEdges(id) {
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
				continue
			}
			target := g.GetNode(e.To)
			if target == nil {
				continue
			}
			toLayer := layerOf(effectivePath(target), arch.Layers, names)
			if toLayer == "" || toLayer == fromLayer {
				continue
			}
			ok, reason := layerAllows(arch.Layers[fromLayer], fromLayer, toLayer)
			if ok {
				continue
			}
			key := id + "\x00" + e.To
			if seen[key] {
				continue
			}
			seen[key] = true
			violations = append(violations, GuardViolation{
				RuleName: "layer:" + fromLayer,
				Kind:     "layer",
				Description: fmt.Sprintf("%s (layer %s) %s %s (layer %s): %s",
					n.ID, fromLayer, e.Kind, target.ID, toLayer, reason),
				Violator:  n.ID,
				LayerFrom: fromLayer,
				LayerTo:   toLayer,
				EdgeType:  string(e.Kind),
				Severity:  ruleSeverity(arch.Severity),
			})
		}
	}
	violations = append(violations, evaluateArchRules(g, arch, changedSymbolIDs, names)...)
	return violations
}

// evaluateArchRules checks the per-layer / per-pattern dependency-cone
// rules — fan-out caps and caller-boundary restrictions — for a set
// of changed symbols.
func evaluateArchRules(g graph.Reader, arch config.ArchitectureConfig, changedSymbolIDs, layerNames []string) []GuardViolation {
	if len(arch.Rules) == 0 {
		return nil
	}
	var violations []GuardViolation
	for _, id := range changedSymbolIDs {
		n := g.GetNode(id)
		if n == nil {
			continue
		}
		ep := effectivePath(n)
		nodeLayer := layerOf(ep, arch.Layers, layerNames)
		for _, rule := range arch.Rules {
			if !ruleApplies(rule, ep, nodeLayer) {
				continue
			}
			if matchesAnyGlob(ep, rule.Except) {
				continue
			}
			label := archRuleLabel(rule)
			if rule.MaxFanOut > 0 {
				if fan := distinctCallTargets(g, id); fan > rule.MaxFanOut {
					violations = append(violations, GuardViolation{
						RuleName: label,
						Kind:     "fan_out",
						Description: ruleMessage(rule, fmt.Sprintf(
							"%s has dependency fan-out %d, exceeding the limit of %d",
							n.ID, fan, rule.MaxFanOut)),
						Violator: n.ID,
						Severity: ruleSeverity(rule.Severity),
					})
				}
			}
			if len(rule.DenyCallersOutside) > 0 {
				seen := make(map[string]bool)
				for _, e := range g.GetInEdges(id) {
					if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
						continue
					}
					caller := g.GetNode(e.From)
					if caller == nil || seen[caller.ID] {
						continue
					}
					seen[caller.ID] = true
					cp := effectivePath(caller)
					if callerWithinBoundary(cp, rule, layerOf(cp, arch.Layers, layerNames)) {
						continue
					}
					violations = append(violations, GuardViolation{
						RuleName: label,
						Kind:     "caller_boundary",
						Description: ruleMessage(rule, fmt.Sprintf(
							"%s calls into %s from outside the permitted set", caller.ID, n.ID)),
						Violator: caller.ID,
						EdgeType: string(e.Kind),
						Severity: ruleSeverity(rule.Severity),
					})
				}
			}
		}
	}
	return violations
}

// ruleApplies reports whether an architecture rule is scoped to a
// symbol. A rule with neither a Layer nor a Pattern selector matches
// nothing; when both are set the symbol must satisfy both.
func ruleApplies(rule config.ArchRule, effPath, nodeLayer string) bool {
	if rule.Layer == "" && rule.Pattern == "" {
		return false
	}
	if rule.Layer != "" && nodeLayer != rule.Layer {
		return false
	}
	if rule.Pattern != "" && !globMatch(rule.Pattern, effPath) {
		return false
	}
	return true
}

// callerWithinBoundary reports whether a caller is permitted to depend
// on a symbol guarded by a deny_callers_outside rule. The guarded set
// may always call within itself; every other caller must match one of
// the allowlist globs.
func callerWithinBoundary(callerPath string, rule config.ArchRule, callerLayer string) bool {
	if ruleApplies(rule, callerPath, callerLayer) {
		return true
	}
	for _, allow := range rule.DenyCallersOutside {
		if globMatch(allow, callerPath) {
			return true
		}
	}
	return false
}

// distinctCallTargets counts the distinct symbols a node calls or
// references — the dependency-cone size.
func distinctCallTargets(g graph.Reader, id string) int {
	seen := make(map[string]bool)
	for _, e := range g.GetOutEdges(id) {
		if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
			continue
		}
		seen[e.To] = true
	}
	return len(seen)
}

// archRuleLabel derives a stable rule name for a violation.
func archRuleLabel(rule config.ArchRule) string {
	switch {
	case rule.Name != "":
		return rule.Name
	case rule.Layer != "":
		return "arch:layer:" + rule.Layer
	case rule.Pattern != "":
		return "arch:pattern:" + rule.Pattern
	default:
		return "arch:rule"
	}
}

// ruleMessage prefixes a rule's configured Message onto the derived
// description when one is set.
func ruleMessage(rule config.ArchRule, detail string) string {
	if rule.Message != "" {
		return rule.Message + ": " + detail
	}
	return detail
}

// layerAllows reports whether a dependency from one layer to another
// is permitted, and the human-readable reason when it is not.
//
// Precedence: an explicit deny of the target layer always wins; then
// a non-empty Allow whitelist requires the target to be listed; with
// no whitelist a wildcard deny ("*") blocks every cross-layer edge.
func layerAllows(rule config.LayerRule, from, to string) (bool, string) {
	for _, d := range rule.Deny {
		if d == to {
			return false, fmt.Sprintf("layer %q denies dependencies on %q", from, to)
		}
	}
	if len(rule.Allow) > 0 {
		for _, a := range rule.Allow {
			if a == "*" || a == to {
				return true, ""
			}
		}
		return false, fmt.Sprintf("layer %q may depend only on %s, not %q",
			from, strings.Join(rule.Allow, ", "), to)
	}
	for _, d := range rule.Deny {
		if d == "*" {
			return false, fmt.Sprintf("layer %q denies all cross-layer dependencies", from)
		}
	}
	return true, ""
}

// layerOf returns the name of the layer a file belongs to, or "" when
// no layer claims it. names must be sorted so an ambiguous file
// (matched by two layers) resolves deterministically to the first.
func layerOf(filePath string, layers map[string]config.LayerRule, names []string) string {
	for _, name := range names {
		rule := layers[name]
		if len(rule.Paths) > 0 {
			for _, p := range rule.Paths {
				if globMatch(p, filePath) {
					return name
				}
			}
			continue
		}
		// A layer with no explicit paths claims files that carry the
		// layer name as a path segment — supports the terse config form.
		if pathHasSegment(filePath, name) {
			return name
		}
	}
	return ""
}

// sortedLayerNames returns the layer names in a stable order.
func sortedLayerNames(layers map[string]config.LayerRule) []string {
	names := make([]string, 0, len(layers))
	for name := range layers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// effectivePath strips the multi-repo prefix from a node's file path
// so per-repo architecture globs match in both single- and multi-repo
// graphs.
func effectivePath(n *graph.Node) string {
	if n.RepoPrefix != "" {
		return strings.TrimPrefix(n.FilePath, n.RepoPrefix+"/")
	}
	return n.FilePath
}

// pathHasSegment reports whether seg appears as a full path segment of
// filePath (e.g. seg "domain" matches "internal/domain/user.go").
func pathHasSegment(filePath, seg string) bool {
	for _, part := range strings.Split(graphpath.Norm(filePath), "/") {
		if part == seg {
			return true
		}
	}
	return false
}

// globMatch reports whether path matches a glob pattern. "**" matches
// any number of path segments (including zero); "*" and "?" match
// within a single segment via the stdlib path.Match rules.
func globMatch(pattern, p string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(graphpath.Norm(p), "/"))
}

func matchSegments(pat, seg []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(seg); i++ {
				if matchSegments(pat[1:], seg[i:]) {
					return true
				}
			}
			return false
		}
		if len(seg) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], seg[0]); err != nil || !ok {
			return false
		}
		pat, seg = pat[1:], seg[1:]
	}
	return len(seg) == 0
}
