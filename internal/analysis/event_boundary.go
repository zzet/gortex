package analysis

import (
	"fmt"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// EventBoundaryFamily (PYG-1) evaluates declarative event-boundary rules over
// the pub/sub graph. A changed symbol's produce edges (EdgeEmits /
// EdgeProducesTopic) and consume edges (EdgeListensOn / EdgeConsumesTopic) are
// matched against config rules: which paths may produce or consume a topic,
// whether a produced topic must have a consumer, and which paths are forbidden
// from it. It is a RuleFamily, so change_contract runs it like any other.
type EventBoundaryFamily struct {
	Rules []config.EventRule
}

func (f EventBoundaryFamily) Name() string { return "events" }

var (
	produceEdges = []graph.EdgeKind{graph.EdgeEmits, graph.EdgeProducesTopic}
	consumeEdges = []graph.EdgeKind{graph.EdgeListensOn, graph.EdgeConsumesTopic}
)

func edgeKindIn(k graph.EdgeKind, set []graph.EdgeKind) bool {
	for _, e := range set {
		if e == k {
			return true
		}
	}
	return false
}

// topicTargets returns the topic/event nodes a symbol links to via the given
// edge kinds.
func topicTargets(g graph.Reader, id string, kinds []graph.EdgeKind) []*graph.Node {
	var out []*graph.Node
	for _, e := range g.GetOutEdges(id) {
		if !edgeKindIn(e.Kind, kinds) {
			continue
		}
		if t := g.GetNode(e.To); t != nil {
			out = append(out, t)
		}
	}
	return out
}

// topicHasConsumer reports whether any symbol consumes the topic node.
func topicHasConsumer(g graph.Reader, topicID string) bool {
	for _, e := range g.GetInEdges(topicID) {
		if edgeKindIn(e.Kind, consumeEdges) {
			return true
		}
	}
	return false
}

func (f EventBoundaryFamily) Evaluate(g graph.Reader, changedSet []string) []GuardViolation {
	if g == nil || len(f.Rules) == 0 {
		return nil
	}
	var violations []GuardViolation
	seen := make(map[string]bool)
	add := func(v GuardViolation) {
		key := v.RuleName + "\x00" + v.Violator + "\x00" + v.Description
		if seen[key] {
			return
		}
		seen[key] = true
		violations = append(violations, v)
	}

	for _, id := range changedSet {
		n := g.GetNode(id)
		if n == nil {
			continue
		}
		produced := topicTargets(g, id, produceEdges)
		consumed := topicTargets(g, id, consumeEdges)
		if len(produced) == 0 && len(consumed) == 0 {
			continue
		}
		for _, rule := range f.Rules {
			sev := ruleSeverity(rule.Severity)
			for _, topic := range produced {
				if rule.Topic != "" && !globMatch(rule.Topic, topic.Name) {
					continue
				}
				if rule.Producer != "" && !globMatch(rule.Producer, n.FilePath) {
					add(GuardViolation{
						RuleName:    eventRuleLabel(rule, topic.Name),
						Kind:        "event_boundary",
						Description: eventMessage(rule, fmt.Sprintf("%s produces topic %q from %s, outside the permitted producer path %q", n.Name, topic.Name, n.FilePath, rule.Producer)),
						Violator:    n.ID,
						Severity:    sev,
					})
				}
				if matchesAnyGlob(n.FilePath, rule.Forbid) {
					add(GuardViolation{
						RuleName:    eventRuleLabel(rule, topic.Name),
						Kind:        "event_boundary",
						Description: eventMessage(rule, fmt.Sprintf("%s in %s is forbidden from producing topic %q", n.Name, n.FilePath, topic.Name)),
						Violator:    n.ID,
						Severity:    sev,
					})
				}
				if rule.RequireConsumer && !topicHasConsumer(g, topic.ID) {
					add(GuardViolation{
						RuleName:    eventRuleLabel(rule, topic.Name),
						Kind:        "event_boundary",
						Description: eventMessage(rule, fmt.Sprintf("topic %q is produced by %s but has no consumer", topic.Name, n.Name)),
						Violator:    n.ID,
						Severity:    sev,
					})
				}
			}
			for _, topic := range consumed {
				if rule.Topic != "" && !globMatch(rule.Topic, topic.Name) {
					continue
				}
				if rule.Consumer != "" && !globMatch(rule.Consumer, n.FilePath) {
					add(GuardViolation{
						RuleName:    eventRuleLabel(rule, topic.Name),
						Kind:        "event_boundary",
						Description: eventMessage(rule, fmt.Sprintf("%s consumes topic %q from %s, outside the permitted consumer path %q", n.Name, topic.Name, n.FilePath, rule.Consumer)),
						Violator:    n.ID,
						Severity:    sev,
					})
				}
				if matchesAnyGlob(n.FilePath, rule.Forbid) {
					add(GuardViolation{
						RuleName:    eventRuleLabel(rule, topic.Name),
						Kind:        "event_boundary",
						Description: eventMessage(rule, fmt.Sprintf("%s in %s is forbidden from consuming topic %q", n.Name, n.FilePath, topic.Name)),
						Violator:    n.ID,
						Severity:    sev,
					})
				}
			}
		}
	}
	return violations
}

func eventRuleLabel(rule config.EventRule, topic string) string {
	if rule.Name != "" {
		return rule.Name
	}
	return "event:" + topic
}

func eventMessage(rule config.EventRule, fallback string) string {
	if rule.Message != "" {
		return rule.Message
	}
	return fallback
}
