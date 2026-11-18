package indexer

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// The value is the FINAL output of the original provider kernel for an exact
// preimage, not a BodyFacts substitute. Only a plain path is factored out:
// neither regex nor AST provider kernels consume it except for path_params,
// which are ineligible here. Current handler lookup and ordered full file-node
// reads remain in the normal ledger. No AST, reader or evidence is retained.
type contractProviderSchemaAnswer struct {
	input         contracts.Contract
	source, nodes string
	output        map[string]any
}

func providerSchemaQuestion(input contracts.Contract) (contracts.Contract, string, bool) {
	path, ok := input.Meta["path"].(string)
	if input.Type != contracts.ContractHTTP || input.Role != contracts.RoleProvider || detectLangFromPath(input.FilePath) != "go" || !ok || path == "" || strings.ContainsAny(path, "{}") {
		return contracts.Contract{}, "", false
	}
	if _, present := input.Meta["path_params"]; present {
		return contracts.Contract{}, "", false
	}
	if contractRetainedSize(reflect.ValueOf(input)) > 1<<20 || !contractDeltaMetaValue(reflect.ValueOf(input.Meta)) {
		return contracts.Contract{}, "", false
	}
	if _, err := json.Marshal(input.Meta); err != nil {
		return contracts.Contract{}, "", false
	}
	input = cloneContractRecords([]contracts.Contract{input})[0]
	input.ID = "" // The kernel consumes no contract ID; route changes mint IDs.
	input.Meta["path"] = ""
	if _, present := input.Meta["original_path"]; present {
		if raw, ok := input.Meta["original_path"].(string); !ok || strings.ContainsAny(raw, "{}") {
			return contracts.Contract{}, "", false
		}
		input.Meta["original_path"] = ""
	}
	key, valid := contractAnswerDigest(input)
	return input, key, valid
}

func providerSchemaHandler(input contracts.Contract, nodes []*graph.Node) *graph.Node {
	var found *graph.Node
	for _, node := range nodes {
		if node != nil && node.ID == input.SymbolID {
			if found != nil {
				return nil
			}
			found = node
		}
	}
	// The fallback uses registration Line and brace windows. It is deliberately
	// left on the original path rather than certified by a body-span cache.
	if found == nil || found.FilePath != input.FilePath || found.StartLine <= 0 || found.EndLine <= found.StartLine {
		return nil
	}
	return found
}

func (c *contractConvergence) providerSchemaUnit(path string) *contractUnit {
	unit := c.units[path]
	stamp, present := c.clock.last[path]
	if !contracts.NativeGoBodyFactsFactory() || c.ctx.Err() != nil || c.enrichReads == nil || unit == nil || !unit.bodyBindingEligible || unit.ledger == nil || unit.ledger.opaque || unit.excluded || !c.live[path] || c.suspect[path] || len(unit.torn) != 0 || !present || !stamp.quiescent() || unit.stamp != stamp {
		return nil
	}
	return unit
}

func (c *contractConvergence) ownedProviderSchema(input contracts.Contract, nodes []*graph.Node) (map[string]any, bool) {
	unit := c.providerSchemaUnit(input.FilePath)
	if unit == nil || providerSchemaHandler(input, nodes) == nil {
		return nil, false
	}
	_, key, ok := providerSchemaQuestion(input)
	if !ok {
		return nil, false
	}
	answer, found := unit.providerSchemas[key]
	digest, valid := contractOrderedProviderNodesDigest(nodes)
	if !found || !valid || answer.nodes != digest || answer.source == "" || answer.source != unit.stamp.SourceFingerprint {
		return nil, false
	}
	c.enrichReads.source(input.FilePath, answer.source)
	c.enrichReads.owner(input.FilePath)
	meta := cloneContractRecords([]contracts.Contract{{Meta: answer.output}})[0].Meta
	meta["path"] = input.Meta["path"]
	if value, present := input.Meta["original_path"]; present {
		meta["original_path"] = value
	}
	c.report.ProviderSchemaHits++
	return meta, true
}

func (c *contractConvergence) rememberProviderSchema(input contracts.Contract, nodes []*graph.Node, output map[string]any) {
	unit := c.providerSchemaUnit(input.FilePath)
	if unit == nil || providerSchemaHandler(input, nodes) == nil {
		return
	}
	question, key, ok := providerSchemaQuestion(input)
	if !ok {
		return
	}
	digest, ok := contractOrderedProviderNodesDigest(nodes)
	source := c.enrichReads.sources[input.FilePath]
	if !ok || source == "" {
		return
	}
	storeContractProviderSchema(unit, key, contractProviderSchemaAnswer{input: question, source: source, nodes: digest, output: output})
}

func (c *contractConvergence) snapshotProviderSchemas(unit *contractUnit, nodes []*graph.Node, tree *parser.ParseTree, src []byte) error {
	if !contracts.NativeGoBodyFactsFactory() || c.req.delta == nil || tree == nil || tree.Lang() != "go" {
		return nil
	}
	for _, input := range c.req.delta.providerQuestions[unit.path] {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		handler := providerSchemaHandler(input, nodes)
		if handler == nil {
			continue
		}
		if _, native := contracts.OwnedGoVarBinding(contracts.MakeBodyFacts(tree, handler), ""); !native {
			continue
		}
		key, ok := contractAnswerDigest(input)
		if !ok {
			continue
		}
		result := cloneContractRecords([]contracts.Contract{input})[0]
		contracts.EnrichHTTPContractWithTree(&result, splitLines(src), nodes, "go", tree)
		digest, ok := contractOrderedProviderNodesDigest(nodes)
		if ok && storeContractProviderSchema(unit, key, contractProviderSchemaAnswer{input: input, source: contractInputHash(src), nodes: digest, output: result.Meta}) {
			c.report.ProviderSchemaSnapshots++
		}
	}
	return c.ctx.Err()
}

func cloneContractProviderSchema(answer contractProviderSchemaAnswer) contractProviderSchemaAnswer {
	answer.input = cloneContractRecords([]contracts.Contract{answer.input})[0]
	answer.source, answer.nodes = strings.Clone(answer.source), strings.Clone(answer.nodes)
	answer.output = cloneContractRecords([]contracts.Contract{{Meta: answer.output}})[0].Meta
	return answer
}
func cloneContractProviderSchemas(in map[string]contractProviderSchemaAnswer) map[string]contractProviderSchemaAnswer {
	if in == nil {
		return nil
	}
	out := make(map[string]contractProviderSchemaAnswer, len(in))
	for key, value := range in {
		out[strings.Clone(key)] = cloneContractProviderSchema(value)
	}
	return out
}
func storeContractProviderSchema(unit *contractUnit, key string, answer contractProviderSchemaAnswer) bool {
	if len(unit.providerSchemas) >= graph.ContractProjectionRowLimit || !contractDeltaMetaValue(reflect.ValueOf(answer.output)) {
		return false
	}
	if _, err := json.Marshal(answer.output); err != nil {
		return false
	}
	charge := int64(1024+2*(len(key)+len(answer.source)+len(answer.nodes))) + contractRetainedSize(reflect.ValueOf(answer.input)) + contractRetainedSize(reflect.ValueOf(answer.output))
	used := unit.providerSchemaBytes
	if old, present := unit.providerSchemas[key]; present {
		used -= contractProviderSchemaCharge(key, old)
	}
	// A small per-file ceiling is within, not additional to, the actor's existing
	// total checkpoint admission. Unknown/large outputs retain the original path.
	if charge > 1<<20-used {
		return false
	}
	if unit.providerSchemas == nil {
		unit.providerSchemas = map[string]contractProviderSchemaAnswer{}
	}
	answer = cloneContractProviderSchema(answer)
	answer.output["path"] = ""
	if _, present := answer.output["original_path"]; present {
		answer.output["original_path"] = ""
	}
	unit.providerSchemas[strings.Clone(key)] = answer
	unit.providerSchemaBytes = used + charge
	return true
}
func contractProviderSchemaCharge(key string, a contractProviderSchemaAnswer) int64 {
	return int64(1024+2*(len(key)+len(a.source)+len(a.nodes))) + contractRetainedSize(reflect.ValueOf(a.input)) + contractRetainedSize(reflect.ValueOf(a.output))
}

func contractOrderedProviderNodesDigest(nodes []*graph.Node) (string, bool) {
	encoded, err := json.Marshal(nodes)
	if err != nil {
		return "", false
	}
	return contractInputHash(encoded), true
}
