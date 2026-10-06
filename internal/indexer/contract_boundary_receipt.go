package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const contractBoundaryReceiptVersion = "contract-boundary-v1"

// Both receipt producers and consumers use the same accepted policy identity.
// Omitting the projection field preserves the ordinary full-parser identity.
func contractBoundaryPolicy(idx *Indexer, language string, projectionPolicy int) (string, error) {
	encoded, err := json.Marshal(struct {
		Config                                    any
		EventBus                                  any
		Parser, PostExtraction                    int
		ContractPolicy, RecordPolicy, MatchPolicy string
		GeneratedProjectionPolicy                 int `json:",omitempty"`
	}{contractExtractionSettings(idx.config), idx.eventBusBoundaries(), extractorVersionForLang(language), postExtractionPolicyVersion, contractExtractionPolicyVersion, contracts.RecordFingerprintVersion, contracts.MatchDependencyKeyVersion, projectionPolicy})
	if err != nil {
		return "", err
	}
	return contractInputHash(encoded), nil
}

// This receipt records local accepted syntax, not completed contract analysis.
// Lookup attempts remain inputs even when a local lookup has no result: a later
// declaration can turn an unresolved endpoint into a real contract.
// It deliberately excludes the ordinary graph's declarations/runtime digest.
type contractBoundaryReceipt struct {
	Version        string                       `json:"version"`
	FilePath       string                       `json:"file_path"`
	Language       string                       `json:"language"`
	Source         string                       `json:"source"`
	Policy         string                       `json:"policy"`
	Records        contracts.RecordFingerprints `json:"records"`
	Groups         []graph.ContractWorkGroup    `json:"groups,omitempty"`
	HandlerInputs  map[string]string            `json:"handler_inputs,omitempty"`
	ProducedInputs map[string]string            `json:"produced_inputs,omitempty"`
	LookupKeys     []string                     `json:"lookup_keys,omitempty"`
	MatcherInputs  map[string]string            `json:"matcher_inputs,omitempty"`
	MountInputs    string                       `json:"mount_inputs,omitempty"`
}

// No graph reader is reachable from this collector. The source and extraction
// result must belong to the same accepted parse; the caller owns its tree.
func (idx *Indexer) collectContractBoundaryReceipt(ctx context.Context, path, language string, src []byte, result *parser.ExtractionResult) (contractBoundaryReceipt, error) {
	if ctx == nil {
		return contractBoundaryReceipt{}, fmt.Errorf("contract boundary receipt: nil context")
	}
	if err := ctx.Err(); err != nil {
		return contractBoundaryReceipt{}, err
	}
	if result == nil {
		return contractBoundaryReceipt{}, fmt.Errorf("contract boundary receipt: incomplete accepted extraction")
	}
	_, byLanguage := idx.buildPerFileContractExtractors()
	projectionPolicy := 0
	if extractionDispositionFor(result).omitSecondarySourceScans() {
		var verified bool
		projectionPolicy, verified = idx.contractGeneratedProjectionPolicy(path, language, src, result, len(byLanguage[language]))
		if !verified {
			return contractBoundaryReceipt{}, fmt.Errorf("contract boundary receipt: incomplete accepted extraction")
		}
	}
	if result.Tree != nil && !bytes.Equal(result.Tree.Source(), src) {
		return contractBoundaryReceipt{}, fmt.Errorf("contract boundary receipt: source differs from accepted tree")
	}
	// Cold/staged parses may already carry the repo namespace; the direct
	// fallback still owns raw extraction rows. Normalize detached copies so
	// dependency identities are identical without mutating the core parse.
	if idx.repoPrefix != "" && len(result.Nodes) > 0 && result.Nodes[0].RepoPrefix != idx.repoPrefix {
		detached := *result
		detached.Nodes = make([]*graph.Node, len(result.Nodes))
		for i, node := range result.Nodes {
			if node != nil {
				copyNode := *node
				detached.Nodes[i] = &copyNode
			}
		}
		detached.Edges = make([]*graph.Edge, len(result.Edges))
		for i, edge := range result.Edges {
			if edge != nil {
				copyEdge := *edge
				detached.Edges[i] = &copyEdge
			}
		}
		idx.applyRepoPrefix(detached.Nodes, detached.Edges)
		result = &detached
	}
	local := &localContractBoundaryInputs{nodes: make(map[string][]*graph.Node), values: make(map[string]string), lookups: make(map[string]struct{}), scope: idx.repoPrefix}
	for _, node := range result.Nodes {
		if node != nil {
			local.nodes[node.Name] = append(local.nodes[node.Name], node)
		}
	}
	for _, value := range result.ConstValues {
		id := value.NodeID
		if idx.repoPrefix != "" && !strings.HasPrefix(id, idx.repoPrefix+"/") {
			id = idx.repoPrefix + "/" + id
		}
		local.values[id] = value.Value
	}
	records := idx.collectContractRecordsForFile(path, src, result.Nodes, result.Edges, byLanguage[language], result.Tree, local)
	fingerprints, err := contracts.FingerprintRecords(records)
	if err != nil {
		return contractBoundaryReceipt{}, err
	}
	policy, err := contractBoundaryPolicy(idx, language, projectionPolicy)
	if err != nil {
		return contractBoundaryReceipt{}, err
	}
	receipt := contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: path, Language: language, Source: contractInputHash(src), Policy: policy, Records: fingerprints, HandlerInputs: make(map[string]string), ProducedInputs: make(map[string]string), MatcherInputs: make(map[string]string)}
	var bodyFacts map[string]contracts.BodyFacts
	if language == "go" {
		bodyFacts, err = contracts.GoBodyFactsForFile(ctx, result.Tree, result.Nodes)
		if err != nil {
			return contractBoundaryReceipt{}, err
		}
	}

	groups := make(map[graph.ContractWorkGroup]struct{})
	handlers := make(map[string]struct{})
	for _, record := range records {
		groups[graph.ContractWorkGroup{WorkspaceID: record.EffectiveWorkspace(), ProjectID: record.EffectiveProject(), ContractID: record.ID}] = struct{}{}
		for _, key := range contracts.MatchDependencyKeys(record) {
			local.lookups[key] = struct{}{}
			receipt.MatcherInputs[key] = fingerprints.Full
		}
		if record.Role == contracts.RoleProvider {
			handlers[record.SymbolID] = struct{}{}
		}
		for _, key := range []string{"handler_ident", "handler_trail", "handler_class", "request_type", "response_type"} {
			if name, ok := record.Meta[key].(string); ok && name != "" {
				kind := "symbol"
				if key == "request_type" || key == "response_type" || key == "handler_class" {
					kind = "type"
				}
				local.noteLookup(kind, name)
				if key == "handler_ident" || key == "handler_trail" {
					for _, alias := range contractBoundaryNameAliases(name) {
						handlers[alias] = struct{}{}
					}
				}
			}
		}
		if envelope, ok := record.Meta["response_envelope"].([]any); ok {
			for _, entry := range envelope {
				if field, ok := entry.(map[string]any); ok {
					if name, ok := field["type"].(string); ok {
						local.noteLookup("type", name)
					}
				}
			}
		}
	}

	produced := make(map[string][]string)
	lines := strings.Split(string(src), "\n")
	for _, node := range result.Nodes {
		if err := ctx.Err(); err != nil {
			return contractBoundaryReceipt{}, err
		}
		if node == nil {
			continue
		}
		_, byID := handlers[node.ID]
		_, byName := handlers[node.Name]
		// Provider SymbolID is the extractor's source attribution, which can
		// name a route constant. Go body facts belong only to callables; constant
		// dependencies are captured separately in ProducedInputs below.
		callable := language != "go" || node.Kind == graph.KindFunction || node.Kind == graph.KindMethod
		if (byID || byName) && callable {
			if node.StartLine <= 0 || node.EndLine < node.StartLine || node.EndLine > len(lines) {
				return contractBoundaryReceipt{}, fmt.Errorf("contract boundary receipt: missing accepted handler span %s", node.ID)
			}
			digest, err := contractBoundaryHandlerInputs(language, bodyFacts[node.ID], node, lines)
			if err != nil {
				return contractBoundaryReceipt{}, err
			}
			receipt.HandlerInputs[node.ID] = digest
			if facts := bodyFacts[node.ID]; facts != nil {
				contractBoundaryRecordBodyLookups(local, facts)
			}
		}
		switch node.Kind {
		case graph.KindFunction, graph.KindMethod:
			if node.StartLine <= 0 || node.EndLine < node.StartLine || node.EndLine > len(lines) {
				return contractBoundaryReceipt{}, fmt.Errorf("contract boundary receipt: missing accepted function span %s", node.ID)
			}
			digest, err := contractBoundaryHandlerInputs(language, bodyFacts[node.ID], node, lines)
			if err != nil {
				return contractBoundaryReceipt{}, err
			}
			produced[contractBoundaryLookupKey(idx.repoPrefix, "symbol_name", node.Name)] = append(produced[contractBoundaryLookupKey(idx.repoPrefix, "symbol_name", node.Name)], node.ID+":"+digest)
			produced[contractBoundaryLookupKey(idx.repoPrefix, "symbol_id", node.ID)] = append(produced[contractBoundaryLookupKey(idx.repoPrefix, "symbol_id", node.ID)], digest)
		case graph.KindType, graph.KindInterface, "struct", "enum", "class", "trait", graph.KindConstant, graph.KindEnumMember:
			// Source spans retain wire tags, embedding and aliases that generic node
			// metadata omits. Consumers of a type name see all of its field changes.
			if node.StartLine <= 0 || node.EndLine < node.StartLine || node.EndLine > len(lines) {
				return contractBoundaryReceipt{}, fmt.Errorf("contract boundary receipt: missing accepted declaration span %s", node.ID)
			}
			encoded, err := json.Marshal(struct {
				Kind                   graph.NodeKind
				Name, QualName, Source string
				Meta                   map[string]any
			}{node.Kind, node.Name, node.QualName, strings.Join(lines[node.StartLine-1:node.EndLine], "\n"), node.Meta})
			if err != nil {
				return contractBoundaryReceipt{}, err
			}
			kind := "type"
			if node.Kind == graph.KindConstant || node.Kind == graph.KindEnumMember {
				kind = "constant"
			}
			produced[contractBoundaryLookupKey(idx.repoPrefix, kind+"_name", node.Name)] = append(produced[contractBoundaryLookupKey(idx.repoPrefix, kind+"_name", node.Name)], node.ID+":"+contractInputHash(encoded))
			produced[contractBoundaryLookupKey(idx.repoPrefix, kind+"_id", node.ID)] = append(produced[contractBoundaryLookupKey(idx.repoPrefix, kind+"_id", node.ID)], contractInputHash(encoded))
		}

	}
	for _, value := range result.ConstValues {
		produced[contractBoundaryLookupKey(idx.repoPrefix, "constant_id", value.NodeID)] = append(produced[contractBoundaryLookupKey(idx.repoPrefix, "constant_id", value.NodeID)], value.Value)
		for _, node := range result.Nodes {
			if node != nil && node.ID == value.NodeID {
				produced[contractBoundaryLookupKey(idx.repoPrefix, "constant_name", node.Name)] = append(produced[contractBoundaryLookupKey(idx.repoPrefix, "constant_name", node.Name)], node.ID+":"+value.Value)
			}
		}
	}
	// Handler/type binding permits a unique foreign-repository fallback. Preserve
	// its namespace so a newly added foreign candidate/ambiguity is observable.
	for key, rows := range produced {
		var parts []string
		if json.Unmarshal([]byte(key), &parts) == nil && len(parts) == 3 && (strings.HasPrefix(parts[1], "symbol_") || strings.HasPrefix(parts[1], "type_")) {
			produced[contractBoundaryLookupKey("*", parts[1], parts[2])] = append([]string(nil), rows...)
		}
	}

	for key, rows := range produced {
		sort.Strings(rows)
		encoded, err := json.Marshal(rows)
		if err != nil {
			return contractBoundaryReceipt{}, err
		}
		receipt.ProducedInputs[key] = contractInputHash(encoded)
	}

	// Supported cross-file mount syntax must remain observable on removal as
	// well as addition. Full accepted source is conservative only for mount files;
	// unrelated function/call edits do not enter this branch.
	if contractSourceNeedsFullRefresh(path, language, src) || ((language == "typescript" || language == "javascript") && strings.Contains(string(src), "@Module")) {
		receipt.MountInputs = receipt.Source
	}
	for group := range groups {
		receipt.Groups = append(receipt.Groups, group)
	}
	sort.Slice(receipt.Groups, func(i, j int) bool {
		a, b := receipt.Groups[i], receipt.Groups[j]
		if a.WorkspaceID != b.WorkspaceID {
			return a.WorkspaceID < b.WorkspaceID
		}
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		return a.ContractID < b.ContractID
	})
	for key := range local.lookups {
		receipt.LookupKeys = append(receipt.LookupKeys, key)
	}
	sort.Strings(receipt.LookupKeys)
	if err := ctx.Err(); err != nil {
		return contractBoundaryReceipt{}, err
	}
	return receipt, nil
}

// Encoding components separately prevents a name containing separators from
// aliasing another typed lookup bucket. Scope is part of negative dependencies.
func contractBoundaryLookupKey(scope, kind, name string) string {
	encoded, _ := json.Marshal([]string{scope, kind, name})
	return string(encoded)
}

type localContractBoundaryInputs struct {
	nodes   map[string][]*graph.Node
	values  map[string]string
	lookups map[string]struct{}
	scope   string
}

func (s *localContractBoundaryInputs) FindNodesByNames(names []string) map[string][]*graph.Node {
	out := make(map[string][]*graph.Node, len(names))
	for _, name := range names {
		s.lookups[contractBoundaryLookupKey(s.scope, "constant_name", name)] = struct{}{}
		out[name] = s.nodes[name]
	}
	return out
}
func (s *localContractBoundaryInputs) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		s.lookups[contractBoundaryLookupKey(s.scope, "constant_id", id)] = struct{}{}
		if value, ok := s.values[id]; ok {
			out[id] = value
		}
	}
	return out, nil
}

// The caller resolves changed produced keys through durable reverse lookup
// membership. This method never decides that every repo file needs work.
type contractBoundaryDelta struct {
	Scope               graph.ContractWorkScope
	ChangedProducedKeys []string
}

func diffContractBoundaryReceipts(old, current *contractBoundaryReceipt) contractBoundaryDelta {
	var out contractBoundaryDelta
	groups := make(map[graph.ContractWorkGroup]struct{})
	symbols := make(map[string]struct{})
	lookups := make(map[string]struct{})
	collect := func(r *contractBoundaryReceipt) {
		if r == nil {
			return
		}
		for _, g := range r.Groups {
			groups[g] = struct{}{}
		}
		for id := range r.HandlerInputs {
			symbols[id] = struct{}{}
		}
		for _, key := range r.LookupKeys {
			lookups[key] = struct{}{}
		}
	}
	collect(old)
	collect(current)
	valid := func(r *contractBoundaryReceipt) bool {
		return r != nil && r.Version == contractBoundaryReceiptVersion && r.Source != "" && r.Policy != ""
	}
	if old != nil && !valid(old) || current != nil && !valid(current) {
		out.Scope.Unknown = true
		out.Scope.Causes = append(out.Scope.Causes, "legacy_inputs_unknown")
	}
	if old == nil && current == nil {
		return out
	}
	if current == nil {
		out.Scope.Deleted = true
	}
	if old != nil && current != nil && (old.Policy != current.Policy || old.Language != current.Language) {
		out.Scope.Unknown = true
		out.Scope.Causes = append(out.Scope.Causes, "extraction_policy_changed")
	}
	oldRecords, newRecords := contracts.RecordFingerprints{}, contracts.RecordFingerprints{}
	oldHandlers, newHandlers := map[string]string{}, map[string]string{}
	oldProduced, newProduced := map[string]string{}, map[string]string{}
	oldMount, newMount := "", ""
	if old != nil {
		oldRecords = old.Records
		oldHandlers = old.HandlerInputs
		oldProduced = old.ProducedInputs
		oldMount = old.MountInputs
	}
	if current != nil {
		newRecords = current.Records
		newHandlers = current.HandlerInputs
		newProduced = current.ProducedInputs
		newMount = current.MountInputs
	}
	// Empty-file addition/removal has no boundary relationship work. Full record
	// equality separately observes line/provenance updates for real owners.
	if len(groups) > 0 && oldRecords != newRecords {
		out.Scope.Causes = append(out.Scope.Causes, "local_boundary_changed")
	}
	if !equalContractBoundaryMap(oldHandlers, newHandlers) {
		out.Scope.Causes = append(out.Scope.Causes, "handler_inputs_changed")
	}
	if oldMount != newMount {
		out.Scope.Unknown = true
		out.Scope.Causes = append(out.Scope.Causes, "mount_inputs_changed")
	}
	produced := make(map[string]struct{})
	for k := range oldProduced {
		produced[k] = struct{}{}
	}
	for k := range newProduced {
		produced[k] = struct{}{}
	}
	for k := range produced {
		if oldProduced[k] != newProduced[k] {
			out.ChangedProducedKeys = append(out.ChangedProducedKeys, k)
		}
	}
	// Lookup changes can expose a previous-empty consumer. Preserve both sides
	// even when no emitted contract ID exists yet.
	if !equalContractBoundaryStrings(old, current) {
		out.Scope.Causes = append(out.Scope.Causes, "boundary_lookup_changed")
	}
	if len(out.Scope.Causes) > 0 {
		for g := range groups {
			out.Scope.Groups = append(out.Scope.Groups, g)
		}
		for id := range symbols {
			out.Scope.SymbolIDs = append(out.Scope.SymbolIDs, id)
		}
		for key := range lookups {
			out.Scope.LookupKeys = append(out.Scope.LookupKeys, key)
		}
		sort.Slice(out.Scope.Groups, func(i, j int) bool {
			a, _ := json.Marshal(out.Scope.Groups[i])
			b, _ := json.Marshal(out.Scope.Groups[j])
			return string(a) < string(b)
		})
		sort.Strings(out.Scope.SymbolIDs)
		sort.Strings(out.Scope.LookupKeys)
	}
	matcher := make(map[string]struct{})
	if old != nil {
		for k := range old.MatcherInputs {
			matcher[k] = struct{}{}
		}
	}
	if current != nil {
		for k := range current.MatcherInputs {
			matcher[k] = struct{}{}
		}
	}
	for key := range matcher {
		a, b := "", ""
		if old != nil {
			a = old.MatcherInputs[key]
		}
		if current != nil {
			b = current.MatcherInputs[key]
		}
		if a != b {
			out.ChangedProducedKeys = append(out.ChangedProducedKeys, key)
		}
	}
	out.ChangedProducedKeys = appendUniqueSorted(nil, out.ChangedProducedKeys...)
	return out
}
func equalContractBoundaryMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func equalContractBoundaryStrings(a, b *contractBoundaryReceipt) bool {
	var x, y []string
	if a != nil {
		x = a.LookupKeys
	}
	if b != nil {
		y = b.LookupKeys
	}
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// Go's established body-facts extractor identifies the serialized/request facts
// without hashing unrelated computations or comments. For languages without a
// complete fact extractor, retain a narrow handler-only conservative input.
func contractBoundaryHandlerInputs(language string, facts contracts.BodyFacts, handler *graph.Node, lines []string) (string, error) {
	if language != "go" {
		return contractInputHash([]byte(strings.Join(lines[handler.StartLine-1:handler.EndLine], "\n"))), nil
	}
	if facts == nil {
		return "", fmt.Errorf("contract boundary receipt: missing accepted body facts %s", handler.ID)
	}
	type response struct {
		Helper, Status, Value string
		Code                  int
		Known                 bool
		Binding               contracts.Binding
		Entries               []string
	}
	type request struct {
		Helper, Type string
		Binding      contracts.Binding
	}
	var responses []response
	for _, call := range facts.ResponseCalls() {
		binding := facts.VarBinding(call.ValueExpr)
		binding.Line = 0
		entry := response{Helper: call.Helper, Status: call.StatusArg.Text(), Value: call.ValueExpr, Code: call.StatusCode, Known: call.StatusKnown, Binding: binding}
		for _, field := range facts.MapLiteralEntries(call.ValueArg) {
			b := facts.VarBinding(field.ValueExpr)
			b.Line = 0
			encoded, err := json.Marshal(struct {
				Key, Expression string
				Binding         contracts.Binding
			}{field.Key, field.ValueExpr, b})
			if err != nil {
				return "", err
			}
			entry.Entries = append(entry.Entries, string(encoded))
		}
		responses = append(responses, entry)
	}
	var requests []request
	for _, call := range facts.RequestBindings() {
		b := facts.VarBinding(call.VarName)
		b.Line = 0
		requests = append(requests, request{call.Helper, call.CompositeType, b})
	}
	encoded, err := json.Marshal(struct {
		Signature, ReturnType any
		Responses             []response
		Requests              []request
		Status                []int
		Query                 []string
	}{handler.Meta["signature"], handler.Meta["return_type"], responses, requests, facts.StatusWrites(), facts.QueryReads()})
	if err != nil {
		return "", err
	}
	return contractInputHash(encoded), nil
}

// Both output buckets and negative lookup receipts use the same typed keys.
// The production reverse index can therefore select consumers without reading
// their source or rebuilding a repository registry.
func contractBoundaryAffectedByProducedKeys(receipt contractBoundaryReceipt, changedKeys []string) bool {
	keys := make(map[string]struct{}, len(changedKeys))
	for _, key := range changedKeys {
		keys[key] = struct{}{}
	}
	for _, key := range receipt.LookupKeys {
		if _, ok := keys[key]; ok {
			return true
		}
	}
	return false
}

// Handler trails carry wrapper/candidate names, and qualified type references
// retain both the exact ID and bare lookup bucket. An extra alias is a safe
// conservative dependency; an omitted alias can miss a changed provider.
func contractBoundaryNameAliases(value string) []string {
	names := strings.FieldsFunc(value, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	seen := make(map[string]struct{})
	for _, name := range names {
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (s *localContractBoundaryInputs) noteLookup(kind, name string) {
	if name == "" {
		return
	}
	for _, alias := range contractBoundaryNameAliases(name) {
		s.lookups[contractBoundaryLookupKey(s.scope, kind+"_name", alias)] = struct{}{}
		if kind == "symbol" || kind == "type" {
			s.lookups[contractBoundaryLookupKey("*", kind+"_name", alias)] = struct{}{}
		}
	}
	if strings.Contains(name, "::") {
		s.lookups[contractBoundaryLookupKey(s.scope, kind+"_id", name)] = struct{}{}
		if kind == "symbol" || kind == "type" {
			s.lookups[contractBoundaryLookupKey("*", kind+"_id", name)] = struct{}{}
		}
	}
}

func contractBoundaryRecordBodyLookups(local *localContractBoundaryInputs, facts contracts.BodyFacts) {
	binding := func(b contracts.Binding) { local.noteLookup("type", b.TypeID); local.noteLookup("symbol", b.CallExpr) }
	for _, call := range facts.ResponseCalls() {
		binding(facts.VarBinding(call.ValueExpr))
		for _, field := range facts.MapLiteralEntries(call.ValueArg) {
			binding(facts.VarBinding(field.ValueExpr))
		}
	}
	for _, call := range facts.RequestBindings() {
		local.noteLookup("type", call.CompositeType)
		binding(facts.VarBinding(call.VarName))
	}
}
