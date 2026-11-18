package indexer

import (
	"slices"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

type contractBodyBindingQuestion struct{ handler, name string }
type contractBodyBindingAnswer struct {
	source, handler string
	binding         contracts.Binding
}

// This is a typed answer, not a replacement BodyFacts implementation or old
// work proof. Its caller still reads the current full handler node and current
// semantic/call/type projections through the normal ledger facade.
func (c *contractConvergence) ownedBodyBinding(handler *graph.Node, name string) (contracts.Binding, bool) {
	if !contracts.NativeGoBodyFactsFactory() || c.ctx.Err() != nil || handler == nil || c.enrichReads == nil {
		return contracts.Binding{}, false
	}
	unit := c.units[handler.FilePath]
	stamp, present := c.clock.last[handler.FilePath]
	if unit == nil || !unit.bodyBindingEligible || unit.ledger == nil || unit.ledger.opaque || unit.excluded || !c.live[handler.FilePath] || c.suspect[handler.FilePath] || len(unit.torn) != 0 || !present || !stamp.quiescent() || unit.stamp != stamp {
		return contracts.Binding{}, false
	}
	answer, ok := unit.bodyBindings[contractBodyBindingQuestion{handler.ID, name}]
	if !ok || answer.source == "" || answer.source != stamp.SourceFingerprint {
		return contracts.Binding{}, false
	}
	digest, valid := contractAnswerDigest(handler)
	if !valid || answer.handler != digest {
		return contracts.Binding{}, false
	}
	// Preserve the original whole accepted-byte dependency for subsequent PP
	// rechecks. Reuse changes computation, not the source/final-fence oracle.
	c.enrichReads.source(handler.FilePath, answer.source)
	c.enrichReads.owner(handler.FilePath)
	c.report.BodyBindingHits++
	return answer.binding, true
}

func (c *contractConvergence) rememberBodyBinding(handler *graph.Node, name string, binding contracts.Binding) {
	if handler == nil || c.enrichReads == nil {
		return
	}
	unit := c.units[handler.FilePath]
	if unit == nil || !unit.bodyBindingEligible || unit.ledger == nil || unit.ledger.opaque {
		return
	}
	source := c.enrichReads.sources[handler.FilePath]
	if source == "" {
		return
	}
	digest, ok := contractAnswerDigest(handler)
	if !ok {
		return
	}
	if !storeContractBodyBinding(unit, contractBodyBindingQuestion{handler.ID, name}, contractBodyBindingAnswer{source: source, handler: digest, binding: binding}) {
		return
	}
	c.report.BodyBindingObserved++
}

// Re-answer only previously requested names while the mandatory accepted parse
// is alive. The returned values retain neither its AST nor its source backing.
func (c *contractConvergence) snapshotBodyBindings(unit *contractUnit, nodes []*graph.Node, tree *parser.ParseTree, src []byte) error {
	if c.req.delta == nil {
		return nil
	}
	questions := c.req.delta.bodyQuestions[unit.path]
	if len(questions) == 0 {
		return nil
	}
	requests := make(map[string][]string)
	for _, question := range questions {
		requests[question.handler] = append(requests[question.handler], question.name)
	}
	source := contractInputHash(src)
	for _, node := range nodes {
		if node == nil || len(requests[node.ID]) == 0 {
			continue
		}
		names := requests[node.ID]
		slices.Sort(names)
		names = slices.Compact(names)
		answers, native, err := contracts.SnapshotGoVarBindings(c.ctx, tree, node, names, int64(1<<20)-unit.bodyBindingBytes)
		if err != nil {
			return err
		}
		if !native {
			continue
		}
		digest, ok := contractAnswerDigest(node)
		if !ok {
			continue
		}
		for name, binding := range answers {
			if storeContractBodyBinding(unit, contractBodyBindingQuestion{node.ID, name}, contractBodyBindingAnswer{source: source, handler: digest, binding: binding}) {
				c.report.BodyBindingSnapshots++
			}
		}
	}
	return c.ctx.Err()
}

func cloneContractBodyBindingAnswer(answer contractBodyBindingAnswer, pool ...*contractProofStrings) contractBodyBindingAnswer {
	answer.source, answer.handler = contractProofString(answer.source, pool...), contractProofString(answer.handler, pool...)
	b := &answer.binding
	b.Kind = contracts.BindingKind(contractProofString(string(b.Kind), pool...))
	b.TypeID, b.CallExpr, b.RawExpr = contractProofString(b.TypeID, pool...), contractProofString(b.CallExpr, pool...), contractProofString(b.RawExpr, pool...)
	b.Origin = contracts.Origin(contractProofString(string(b.Origin), pool...))
	return answer
}

func cloneContractBodyBindings(in map[contractBodyBindingQuestion]contractBodyBindingAnswer, pool ...*contractProofStrings) map[contractBodyBindingQuestion]contractBodyBindingAnswer {
	if in == nil {
		return nil
	}
	out := make(map[contractBodyBindingQuestion]contractBodyBindingAnswer, len(in))
	for question, answer := range in {
		key := contractBodyBindingQuestion{contractProofString(question.handler, pool...), contractProofString(question.name, pool...)}
		out[key] = cloneContractBodyBindingAnswer(answer, pool...)
	}
	return out
}

// The per-file bound is conservative and applied before cloning another owned
// backing allocation. The actual actor-retained structure is charged again by
// the existing total64MiB checkpoint accounting; admission may still refuse.
func contractBodyBindingCharge(q contractBodyBindingQuestion, a contractBodyBindingAnswer) int64 {
	b := a.binding
	return 1024 + 2*int64(len(q.handler)+len(q.name)+len(a.source)+len(a.handler)+len(b.Kind)+len(b.TypeID)+len(b.CallExpr)+len(b.RawExpr)+len(b.Origin))
}
func storeContractBodyBinding(unit *contractUnit, q contractBodyBindingQuestion, a contractBodyBindingAnswer) bool {
	old, exists := unit.bodyBindings[q]
	if !exists && len(unit.bodyBindings) >= graph.ContractProjectionRowLimit {
		return false
	}
	used := unit.bodyBindingBytes
	if exists {
		used -= contractBodyBindingCharge(q, old)
	}
	charge := contractBodyBindingCharge(q, a)
	if charge > int64(1<<20)-used {
		return false
	}
	if unit.bodyBindings == nil {
		unit.bodyBindings = make(map[contractBodyBindingQuestion]contractBodyBindingAnswer)
	}
	key := contractBodyBindingQuestion{contractProofString(q.handler), contractProofString(q.name)}
	unit.bodyBindings[key] = cloneContractBodyBindingAnswer(a)
	unit.bodyBindingBytes = used + charge
	return true
}
