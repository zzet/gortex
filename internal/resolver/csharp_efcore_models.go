package resolver

import (
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	csharpEFActionMapping            = "mapping"
	csharpEFActionApplyConfiguration = "apply_configuration"
	csharpEFActionApplyAssembly      = "apply_assembly"
)

type csharpEFDecisionState uint8

const (
	csharpEFUnclaimed csharpEFDecisionState = iota
	csharpEFWinner
	csharpEFRejected
)

// ResolveCSharpEFCoreModels is the full/cold entry point. Incremental callers
// use ResolveCSharpEFCoreModelsScoped so a configuration-only edit reconciles
// the complete projection for every entity in the changed repository.
func ResolveCSharpEFCoreModels(g graph.Store) int {
	return ResolveCSharpEFCoreModelsScoped(g, nil)
}

// ResolveCSharpEFCoreModelsScoped joins ordered EF Core OnModelCreating actions
// with configuration definitions, entity attributes, and DbSet conventions.
// A non-nil scope is repository-bounded but still reads every current
// models_table sibling for each affected entity, enabling deletion, reversion,
// and duplicate coalescing.
func ResolveCSharpEFCoreModelsScoped(g graph.Store, scope map[string]bool) int {
	if g == nil {
		return 0
	}

	nodes := csharpEFNodesForScope(g, scope)
	classesByKey := map[string][]*graph.Node{}
	configsByKey := map[string][]csharpEFConfigFact{}
	configsByBoundary := map[string][]csharpEFConfigFact{}
	entitiesByID := map[string]*graph.Node{}
	var actions []csharpEFAction
	var dbsets []csharpDbSetFact

	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind == graph.KindFile {
			actions = append(actions, csharpEFActionsFromFile(n)...)
			continue
		}
		if !strings.EqualFold(n.Language, "csharp") {
			continue
		}
		switch n.Kind {
		case graph.KindType:
			entitiesByID[n.ID] = n
			nameKey := csharpEFNameKey(n.RepoPrefix, n.WorkspaceID, n.Name)
			classesByKey[nameKey] = append(classesByKey[nameKey], n)
			if cfg, ok := csharpEFConfigFactFromNode(n); ok {
				key := csharpEFNameKey(cfg.repoPrefix, cfg.workspaceID, cfg.name)
				configsByKey[key] = append(configsByKey[key], cfg)
				boundary := csharpEFBoundaryKey(cfg.repoPrefix, cfg.workspaceID)
				configsByBoundary[boundary] = append(configsByBoundary[boundary], cfg)
			}
		case graph.KindField:
			if fact, ok := csharpDbSetFactFromNode(n); ok {
				dbsets = append(dbsets, fact)
			}
		}
	}

	for key := range configsByKey {
		sort.Slice(configsByKey[key], func(i, j int) bool {
			return configsByKey[key][i].siteID < configsByKey[key][j].siteID
		})
	}
	for key := range configsByBoundary {
		sort.Slice(configsByBoundary[key], func(i, j int) bool {
			return configsByBoundary[key][i].siteID < configsByBoundary[key][j].siteID
		})
	}

	decisions := csharpEFReduceActions(actions, classesByKey, configsByKey, configsByBoundary)
	dbsetsByEntity := csharpEFResolveDbSets(dbsets, classesByKey)

	entityIDs := make([]string, 0, len(entitiesByID))
	for id := range entitiesByID {
		entityIDs = append(entityIDs, id)
	}
	sort.Strings(entityIDs)
	outgoing := g.GetOutEdgesByNodeIDs(entityIDs)
	currentByEntity := map[string][]*graph.Edge{}
	affected := map[string]bool{}
	for _, id := range entityIDs {
		entity := entitiesByID[id]
		if _, ok := csharpEFAttributeMapping(entity); ok {
			affected[id] = true
		}
		for _, edge := range outgoing[id] {
			if csharpEFOwnsModelsTableEdge(edge) {
				currentByEntity[id] = append(currentByEntity[id], edge)
				affected[id] = true
			}
		}
	}
	for id := range decisions {
		affected[id] = true
	}
	for id := range dbsetsByEntity {
		affected[id] = true
	}

	var removeEdges []*graph.Edge
	addEdges := map[graph.EdgeIdentity]*graph.Edge{}
	addNodes := map[string]*graph.Node{}
	changed := 0
	orderedAffected := make([]string, 0, len(affected))
	for id := range affected {
		orderedAffected = append(orderedAffected, id)
	}
	sort.Strings(orderedAffected)

	for _, entityID := range orderedAffected {
		entity := entitiesByID[entityID]
		if entity == nil {
			continue
		}
		current := currentByEntity[entityID]
		sort.Slice(current, func(i, j int) bool {
			return csharpEFEdgeOrderKey(current[i]) < csharpEFEdgeOrderKey(current[j])
		})
		desired := csharpEFDesiredProjection(entity, decisions[entityID], dbsetsByEntity[entityID], current)
		if len(current) == 1 && desired != nil && csharpEFEdgesEqual(current[0], desired) {
			continue
		}
		if len(current) == 0 && desired == nil {
			continue
		}
		removeEdges = append(removeEdges, current...)
		if desired != nil {
			addEdges[graph.EdgeIdentityFor(desired)] = desired
			if g.GetNode(desired.To) == nil {
				if table := csharpEFTableNodeForEdge(entity, desired); table != nil {
					addNodes[table.ID] = table
				}
			}
		}
		changed++
	}

	if changed == 0 {
		return 0
	}
	nodeBatch := make([]*graph.Node, 0, len(addNodes))
	for _, node := range addNodes {
		nodeBatch = append(nodeBatch, node)
	}
	sort.Slice(nodeBatch, func(i, j int) bool { return nodeBatch[i].ID < nodeBatch[j].ID })
	edgeBatch := make([]*graph.Edge, 0, len(addEdges))
	for _, edge := range addEdges {
		edgeBatch = append(edgeBatch, edge)
	}
	sort.Slice(edgeBatch, func(i, j int) bool {
		return csharpEFEdgeOrderKey(edgeBatch[i]) < csharpEFEdgeOrderKey(edgeBatch[j])
	})

	// Full framework runs wrap legacy functions in frameworkEdgeBatchStore.
	// This resolver performs one replacement and has no staged AddEdge writes;
	// unwrap the facade so Graph/SQLite retain exact, atomic replacement.
	replacementStore := g
	if batch, ok := g.(*frameworkEdgeBatchStore); ok {
		batch.flush()
		if batch.readCache != nil && len(nodeBatch) > 0 {
			batch.readCache.invalidateNodes()
		}
		replacementStore = batch.Store
	}
	_, err := graph.ReplaceDerivedContracts(replacementStore, graph.DerivedContractReplacement{
		RemoveEdges: removeEdges,
		Nodes:       nodeBatch,
		Edges:       edgeBatch,
	})
	if err != nil {
		return 0
	}
	return changed
}

func csharpEFNodesForScope(g graph.Store, scope map[string]bool) []*graph.Node {
	if scope == nil {
		return nodesByKindsOrAll(g, graph.KindType, graph.KindField, graph.KindFile)
	}
	prefixes := frameworkScopePrefixes(scope)
	var out []*graph.Node
	for node := range graph.NodesInScopeSeq(g, prefixes, nil, graph.KindType, graph.KindField, graph.KindFile) {
		if node != nil {
			out = append(out, node)
		}
	}
	return out
}

type csharpEFMapping struct {
	entity      string
	table       string
	schema      string
	relation    string
	context     string
	siteID      string
	filePath    string
	line        int
	repoPrefix  string
	workspaceID string
	sources     []string
	contexts    []string
}

type csharpEFAction struct {
	kind        string
	context     string
	config      string
	ordinal     int
	line        int
	siteID      string
	filePath    string
	repoPrefix  string
	workspaceID string
	mapping     csharpEFMapping
}

type csharpEFConfigFact struct {
	name        string
	siteID      string
	repoPrefix  string
	workspaceID string
	mapping     csharpEFMapping
}

type csharpEFDecision struct {
	state   csharpEFDecisionState
	mapping csharpEFMapping
}

func csharpEFReduceActions(
	actions []csharpEFAction,
	classesByKey map[string][]*graph.Node,
	configsByKey map[string][]csharpEFConfigFact,
	configsByBoundary map[string][]csharpEFConfigFact,
) map[string]csharpEFDecision {
	byContext := map[string][]csharpEFAction{}
	for _, action := range actions {
		key := csharpEFBoundaryKey(action.repoPrefix, action.workspaceID) + "\x00" + action.context
		byContext[key] = append(byContext[key], action)
	}
	contextKeys := make([]string, 0, len(byContext))
	for key := range byContext {
		contextKeys = append(contextKeys, key)
	}
	sort.Strings(contextKeys)

	perEntity := map[string][]csharpEFDecision{}
	for _, contextKey := range contextKeys {
		stream := byContext[contextKey]
		sort.SliceStable(stream, func(i, j int) bool {
			if stream[i].ordinal != stream[j].ordinal {
				return stream[i].ordinal < stream[j].ordinal
			}
			if stream[i].line != stream[j].line {
				return stream[i].line < stream[j].line
			}
			return stream[i].kind < stream[j].kind
		})
		current := map[string]csharpEFDecision{}
		for _, action := range stream {
			switch action.kind {
			case csharpEFActionMapping:
				if id, mapping, ok := csharpEFResolveMapping(action.mapping, classesByKey); ok {
					current[id] = csharpEFDecision{state: csharpEFWinner, mapping: mapping}
				}
			case csharpEFActionApplyConfiguration:
				key := csharpEFNameKey(action.repoPrefix, action.workspaceID, action.config)
				configs := configsByKey[key]
				if len(configs) != 1 {
					continue
				}
				mapping := csharpEFActivateConfig(configs[0], action)
				if id, resolved, ok := csharpEFResolveMapping(mapping, classesByKey); ok {
					current[id] = csharpEFDecision{state: csharpEFWinner, mapping: resolved}
				}
			case csharpEFActionApplyAssembly:
				boundary := csharpEFBoundaryKey(action.repoPrefix, action.workspaceID)
				atEvent := map[string][]csharpEFMapping{}
				for _, config := range configsByBoundary[boundary] {
					mapping := csharpEFActivateConfig(config, action)
					if id, resolved, ok := csharpEFResolveMapping(mapping, classesByKey); ok {
						atEvent[id] = append(atEvent[id], resolved)
					}
				}
				for entityID, mappings := range atEvent {
					current[entityID] = csharpEFMergeMappings(mappings)
				}
			}
		}
		for entityID, decision := range current {
			perEntity[entityID] = append(perEntity[entityID], decision)
		}
	}

	out := map[string]csharpEFDecision{}
	for entityID, decisions := range perEntity {
		if len(decisions) == 0 {
			continue
		}
		merged := decisions[0]
		for _, decision := range decisions[1:] {
			if merged.state == csharpEFRejected || decision.state == csharpEFRejected ||
				!csharpEFMappingEqual(merged.mapping, decision.mapping) {
				merged = csharpEFDecision{state: csharpEFRejected}
				continue
			}
			merged.mapping = csharpEFCombineMappingEvidence(merged.mapping, decision.mapping)
		}
		out[entityID] = merged
	}
	return out
}

func csharpEFMergeMappings(mappings []csharpEFMapping) csharpEFDecision {
	if len(mappings) == 0 {
		return csharpEFDecision{state: csharpEFUnclaimed}
	}
	sort.Slice(mappings, func(i, j int) bool {
		return csharpEFMappingEvidenceKey(mappings[i]) < csharpEFMappingEvidenceKey(mappings[j])
	})
	winner := mappings[0]
	for _, mapping := range mappings[1:] {
		if !csharpEFMappingEqual(winner, mapping) {
			return csharpEFDecision{state: csharpEFRejected}
		}
		winner = csharpEFCombineMappingEvidence(winner, mapping)
	}
	return csharpEFDecision{state: csharpEFWinner, mapping: winner}
}

func csharpEFResolveMapping(
	mapping csharpEFMapping,
	classesByKey map[string][]*graph.Node,
) (string, csharpEFMapping, bool) {
	key := csharpEFNameKey(mapping.repoPrefix, mapping.workspaceID, mapping.entity)
	candidates := classesByKey[key]
	if len(candidates) != 1 {
		return "", csharpEFMapping{}, false
	}
	entity := candidates[0]
	mapping.entity = entity.Name
	return entity.ID, mapping, true
}

func csharpEFActivateConfig(config csharpEFConfigFact, action csharpEFAction) csharpEFMapping {
	mapping := config.mapping
	mapping.context = action.context
	mapping.contexts = []string{action.context}
	mapping.siteID = action.siteID
	mapping.filePath = action.filePath
	mapping.line = action.line
	mapping.repoPrefix = action.repoPrefix
	mapping.workspaceID = action.workspaceID
	mapping.sources = csharpEFUniqueStrings([]string{config.siteID, csharpEFActionSource(action)})
	return mapping
}

func csharpEFResolveDbSets(
	facts []csharpDbSetFact,
	classesByKey map[string][]*graph.Node,
) map[string][]csharpDbSetFact {
	out := map[string][]csharpDbSetFact{}
	sort.Slice(facts, func(i, j int) bool { return facts[i].siteID < facts[j].siteID })
	for _, fact := range facts {
		key := csharpEFNameKey(fact.repoPrefix, fact.workspaceID, fact.entity)
		candidates := classesByKey[key]
		if len(candidates) != 1 {
			continue
		}
		out[candidates[0].ID] = append(out[candidates[0].ID], fact)
	}
	return out
}

func csharpEFDesiredProjection(
	entity *graph.Node,
	decision csharpEFDecision,
	dbsets []csharpDbSetFact,
	current []*graph.Edge,
) *graph.Edge {
	attribute, hasAttribute := csharpEFAttributeMapping(entity)
	switch decision.state {
	case csharpEFWinner:
		return csharpEFProjectionEdge(entity, decision.mapping, "fluent", "override")
	case csharpEFRejected:
		// An activated same-boundary conflict claims the entity but has no
		// defensible target. Drop the projection entirely; do not expose the
		// lower-precedence attribute or DbSet as if it were the runtime winner.
		return nil
	default:
		if hasAttribute {
			return csharpEFAttributeEdge(entity, attribute, current)
		}
		mapping, ok := csharpEFDbSetMapping(dbsets)
		if !ok {
			return nil
		}
		return csharpEFProjectionEdge(entity, mapping, "dbset", "convention")
	}
}

func csharpEFDbSetMapping(facts []csharpDbSetFact) (csharpEFMapping, bool) {
	if len(facts) == 0 {
		return csharpEFMapping{}, false
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].siteID < facts[j].siteID })
	first := facts[0]
	for _, fact := range facts[1:] {
		if fact.table != first.table {
			return csharpEFMapping{}, false
		}
	}
	sources := make([]string, 0, len(facts))
	for _, fact := range facts {
		sources = append(sources, fact.siteID)
	}
	return csharpEFMapping{
		entity: first.entity, table: first.table,
		siteID: first.siteID, filePath: first.filePath, line: first.line,
		repoPrefix: first.repoPrefix, workspaceID: first.workspaceID,
		sources: csharpEFUniqueStrings(sources),
	}, true
}

func csharpEFProjectionEdge(entity *graph.Node, mapping csharpEFMapping, binding, derivation string) *graph.Edge {
	tableID := csharpEFTableNodeID(entity.RepoPrefix, mapping.table, mapping.schema)
	meta := map[string]any{
		"orm":        "efcore",
		"binding":    binding,
		"table_name": mapping.table,
		"derivation": derivation,
	}
	if mapping.schema != "" {
		meta["schema"] = mapping.schema
	}
	if mapping.relation != "" {
		meta["relation"] = mapping.relation
	}
	if sources := csharpEFUniqueStrings(mapping.sources); len(sources) > 0 {
		meta["ef_sources"] = strings.Join(sources, "\x1f")
	}
	if contexts := csharpEFUniqueStrings(mapping.contexts); len(contexts) > 0 {
		meta["ef_contexts"] = strings.Join(contexts, "\x1f")
	}
	filePath := mapping.filePath
	line := mapping.line
	if filePath == "" {
		filePath = entity.FilePath
	}
	if line < 1 {
		line = entity.StartLine
	}
	edge := &graph.Edge{
		From: entity.ID, To: tableID, Kind: graph.EdgeModelsTable,
		FilePath:        filePath,
		Line:            line,
		Origin:          graph.OriginASTInferred,
		Confidence:      ConfidenceTyped,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeModelsTable, ConfidenceTyped),
		Meta:            meta,
	}
	StampSynthesizedTyped(edge, SynthCSharpEFCoreModels)
	return edge
}

func csharpEFAttributeMapping(entity *graph.Node) (csharpEFMapping, bool) {
	if entity == nil || entity.Meta == nil {
		return csharpEFMapping{}, false
	}
	table, _ := entity.Meta["ef_attribute_table"].(string)
	if table == "" {
		return csharpEFMapping{}, false
	}
	schema, _ := entity.Meta["ef_attribute_schema"].(string)
	return csharpEFMapping{
		entity: entity.Name, table: table, schema: schema,
		siteID: entity.ID, filePath: entity.FilePath, line: entity.StartLine,
		repoPrefix: entity.RepoPrefix, workspaceID: entity.WorkspaceID,
	}, true
}

func csharpEFAttributeEdge(entity *graph.Node, mapping csharpEFMapping, current []*graph.Edge) *graph.Edge {
	tableID := csharpEFTableNodeID(entity.RepoPrefix, mapping.table, mapping.schema)
	for _, edge := range current {
		if edge == nil || edge.To != tableID || edge.Meta == nil {
			continue
		}
		binding, _ := edge.Meta["binding"].(string)
		if binding == "attribute" {
			return cloneFrameworkEdge(edge)
		}
	}
	meta := map[string]any{
		"orm":        "efcore",
		"binding":    "attribute",
		"table_name": mapping.table,
		"derivation": "override",
	}
	if mapping.schema != "" {
		meta["schema"] = mapping.schema
	}
	return &graph.Edge{
		From: entity.ID, To: tableID, Kind: graph.EdgeModelsTable,
		FilePath:        entity.FilePath,
		Line:            entity.StartLine,
		Origin:          graph.OriginASTInferred,
		Confidence:      ConfidenceTyped,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeModelsTable, ConfidenceTyped),
		Meta:            meta,
	}
}

func csharpEFTableNodeForEdge(entity *graph.Node, edge *graph.Edge) *graph.Node {
	if entity == nil || edge == nil || edge.Meta == nil {
		return nil
	}
	table, _ := edge.Meta["table_name"].(string)
	if table == "" {
		return nil
	}
	schema, _ := edge.Meta["schema"].(string)
	return &graph.Node{
		ID:          edge.To,
		Kind:        graph.KindTable,
		Name:        table,
		FilePath:    edge.FilePath,
		Language:    "csharp",
		RepoPrefix:  entity.RepoPrefix,
		WorkspaceID: entity.WorkspaceID,
		Meta: map[string]any{
			"dialect": "orm",
			"schema":  schema,
			"source":  "csharp-orm",
		},
	}
}

func csharpEFOwnsModelsTableEdge(edge *graph.Edge) bool {
	if edge == nil || edge.Kind != graph.EdgeModelsTable || edge.Meta == nil {
		return false
	}
	orm, _ := edge.Meta["orm"].(string)
	synth, _ := edge.Meta[MetaSynthesizedBy].(string)
	return orm == "efcore" || synth == SynthCSharpEFCoreModels
}

func csharpEFEdgesEqual(a, b *graph.Edge) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.From == b.From && a.To == b.To && a.Kind == b.Kind &&
		a.FilePath == b.FilePath && a.Line == b.Line && a.Origin == b.Origin &&
		a.Confidence == b.Confidence && a.ConfidenceLabel == b.ConfidenceLabel &&
		reflect.DeepEqual(a.Meta, b.Meta)
}

func csharpEFMappingEqual(a, b csharpEFMapping) bool {
	return a.table == b.table && a.schema == b.schema && a.relation == b.relation
}

func csharpEFCombineMappingEvidence(a, b csharpEFMapping) csharpEFMapping {
	if csharpEFMappingEvidenceKey(b) < csharpEFMappingEvidenceKey(a) {
		a, b = b, a
	}
	a.sources = csharpEFUniqueStrings(append(append([]string(nil), a.sources...), b.sources...))
	a.contexts = csharpEFUniqueStrings(append(append([]string(nil), a.contexts...), b.contexts...))
	return a
}

func csharpEFMappingEvidenceKey(mapping csharpEFMapping) string {
	return mapping.filePath + "\x00" + strconv.Itoa(mapping.line) + "\x00" + mapping.siteID
}

func csharpEFEdgeOrderKey(edge *graph.Edge) string {
	if edge == nil {
		return ""
	}
	return edge.From + "\x00" + edge.To + "\x00" + string(edge.Kind) + "\x00" + edge.FilePath + "\x00" + strconv.Itoa(edge.Line)
}

func csharpEFBoundaryKey(repoPrefix, workspaceID string) string {
	return repoPrefix + "\x00" + workspaceID
}

func csharpEFNameKey(repoPrefix, workspaceID, name string) string {
	return csharpEFBoundaryKey(repoPrefix, workspaceID) + "\x00" + csharpEFBareName(name)
}

func csharpEFBareName(name string) string {
	name = strings.TrimSpace(strings.TrimPrefix(name, "global::"))
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimPrefix(name, "@")
}

func csharpEFUniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func csharpEFActionSource(action csharpEFAction) string {
	return action.siteID + "#" + action.context + "#" + strconv.Itoa(action.ordinal)
}

func csharpEFConfigFactFromNode(n *graph.Node) (csharpEFConfigFact, bool) {
	if n == nil || n.Meta == nil {
		return csharpEFConfigFact{}, false
	}
	entity, _ := n.Meta["ef_config_entity"].(string)
	table, _ := n.Meta["ef_config_table"].(string)
	if entity == "" || table == "" {
		return csharpEFConfigFact{}, false
	}
	schema, _ := n.Meta["ef_config_schema"].(string)
	relation, _ := n.Meta["ef_config_relation"].(string)
	mapping := csharpEFMapping{
		entity: csharpEFBareName(entity), table: table, schema: schema, relation: relation,
		siteID: n.ID, filePath: n.FilePath, line: n.StartLine,
		repoPrefix: n.RepoPrefix, workspaceID: n.WorkspaceID,
		sources: []string{n.ID},
	}
	return csharpEFConfigFact{
		name: csharpEFBareName(n.Name), siteID: n.ID,
		repoPrefix: n.RepoPrefix, workspaceID: n.WorkspaceID,
		mapping: mapping,
	}, true
}

func csharpEFActionsFromFile(n *graph.Node) []csharpEFAction {
	if n == nil || n.Meta == nil {
		return nil
	}
	value := n.Meta["ef_fluent"]
	var out []csharpEFAction
	appendMap := func(index int, raw map[string]any) {
		kind, _ := raw["kind"].(string)
		kind = strings.ToLower(strings.TrimSpace(kind))
		context, _ := raw["context"].(string)
		if context == "" {
			context = n.ID
		}
		line, ok := csharpEFMetaInt(raw["line"])
		if !ok || line < 1 {
			line = 1
		}
		ordinal, ok := csharpEFMetaInt(raw["ordinal"])
		if !ok {
			ordinal = index
		}
		action := csharpEFAction{
			kind: kind, context: context, ordinal: ordinal, line: line,
			siteID: n.ID, filePath: n.FilePath,
			repoPrefix: n.RepoPrefix, workspaceID: n.WorkspaceID,
		}
		switch kind {
		case csharpEFActionMapping:
			action.mapping = csharpEFMapping{
				entity:   csharpEFBareName(csharpEFString(raw["entity"])),
				table:    csharpEFString(raw["table"]),
				schema:   csharpEFString(raw["schema"]),
				relation: csharpEFString(raw["relation"]),
				context:  context, contexts: []string{context},
				siteID: n.ID, filePath: n.FilePath, line: line,
				repoPrefix: n.RepoPrefix, workspaceID: n.WorkspaceID,
				sources: []string{csharpEFActionSource(action)},
			}
			if action.mapping.entity == "" || action.mapping.table == "" {
				return
			}
		case csharpEFActionApplyConfiguration:
			action.config = csharpEFBareName(csharpEFString(raw["config"]))
			if action.config == "" {
				return
			}
		case csharpEFActionApplyAssembly:
		default:
			return
		}
		out = append(out, action)
	}

	switch entries := value.(type) {
	case []map[string]any:
		for i, entry := range entries {
			appendMap(i, entry)
		}
	case []any:
		for i, entry := range entries {
			if raw, ok := entry.(map[string]any); ok {
				appendMap(i, raw)
				continue
			}
			if legacy, ok := entry.(string); ok {
				if action, ok := csharpEFLegacyAction(n, legacy, i); ok {
					out = append(out, action)
				}
			}
		}
	case []string:
		for i, entry := range entries {
			if action, ok := csharpEFLegacyAction(n, entry, i); ok {
				out = append(out, action)
			}
		}
	}
	return out
}

func csharpEFLegacyAction(n *graph.Node, entry string, ordinal int) (csharpEFAction, bool) {
	parts := strings.SplitN(entry, "|", 5)
	if len(parts) != 5 || parts[0] == "" || parts[1] == "" {
		return csharpEFAction{}, false
	}
	line, err := strconv.Atoi(parts[4])
	if err != nil || line < 1 {
		line = 1
	}
	action := csharpEFAction{
		kind: csharpEFActionMapping, context: n.ID, ordinal: ordinal, line: line,
		siteID: n.ID, filePath: n.FilePath,
		repoPrefix: n.RepoPrefix, workspaceID: n.WorkspaceID,
	}
	action.mapping = csharpEFMapping{
		entity: csharpEFBareName(parts[0]), table: parts[1], schema: parts[2], relation: parts[3],
		context: action.context, contexts: []string{action.context},
		siteID: n.ID, filePath: n.FilePath, line: line,
		repoPrefix: n.RepoPrefix, workspaceID: n.WorkspaceID,
		sources: []string{csharpEFActionSource(action)},
	}
	return action, true
}

func csharpEFMetaInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float32:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	default:
		return 0, false
	}
}

func csharpEFString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func csharpEFTableNodeID(repoPrefix, table, schema string) string {
	id := "db::orm::" + table
	if schema != "" {
		id = "db::orm::" + schema + "." + table
	}
	if repoPrefix != "" {
		id = repoPrefix + "/" + id
	}
	return id
}

// csharpEFEnsureTableNode remains for sibling tests and callers that construct
// one table eagerly; the reconciler itself batches table nodes atomically.
func csharpEFEnsureTableNode(g graph.Store, tableID, table, schema, filePath, repoPrefix string) {
	if g.GetNode(tableID) != nil {
		return
	}
	g.AddNode(&graph.Node{
		ID: tableID, Kind: graph.KindTable, Name: table,
		FilePath: filePath, Language: "csharp", RepoPrefix: repoPrefix,
		Meta: map[string]any{"dialect": "orm", "schema": schema, "source": "csharp-orm"},
	})
}

type csharpDbSetFact struct {
	entity      string
	table       string
	siteID      string
	filePath    string
	line        int
	repoPrefix  string
	workspaceID string
}

var csharpDbSetType = regexp.MustCompile(`^(?:[A-Za-z_][\w.]*\.)?DbSet<(.+)>\??$`)

func csharpDbSetFactFromNode(n *graph.Node) (csharpDbSetFact, bool) {
	if n == nil || n.Meta == nil {
		return csharpDbSetFact{}, false
	}
	if kind, _ := n.Meta["kind"].(string); kind != "property" {
		return csharpDbSetFact{}, false
	}
	fieldType, _ := n.Meta["field_type"].(string)
	match := csharpDbSetType.FindStringSubmatch(strings.TrimSpace(fieldType))
	if len(match) < 2 {
		return csharpDbSetFact{}, false
	}
	entity := strings.TrimSpace(match[1])
	if strings.ContainsAny(entity, "<>,") {
		return csharpDbSetFact{}, false
	}
	entity = csharpEFBareName(entity)
	if entity == "" || n.Name == "" {
		return csharpDbSetFact{}, false
	}
	return csharpDbSetFact{
		entity: entity, table: n.Name,
		siteID: n.ID, filePath: n.FilePath, line: n.StartLine,
		repoPrefix: n.RepoPrefix, workspaceID: n.WorkspaceID,
	}, true
}
