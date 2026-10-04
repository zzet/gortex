package contracts

import (
	"encoding/json"
	"reflect"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type persistedContractRecordKey struct {
	id, file, symbol, repo, workspace, project string
	role                                       Role
}

func persistedContractKey(c Contract) persistedContractRecordKey {
	return persistedContractRecordKey{c.ID, c.FilePath, c.SymbolID, c.RepoPrefix, c.WorkspaceID, c.ProjectID, c.Role}
}

// LoadRegistryFromGraph reconstructs records from repo-owned provides/consumes
// rows plus recoverable legacy scalar nodes. Canonical IDs are shared semantic
// identities: a canonical node's last-writer scope does not own every record.
// Empty prefix remains the exact single-repository namespace, not global.
func LoadRegistryFromGraph(g graph.Store, repoPrefix string) *Registry {
	return loadRegistryFromGraph(g, repoPrefix, "", "", false, nil)
}

// LoadRegistryFromGraphWithScope restores an indexer's legacy scope while
// preserving explicitly persisted owner scope, including empty strings. The
// ordinary loader continues to derive its fallback from the canonical node.
func LoadRegistryFromGraphWithScope(g graph.Store, repoPrefix, workspaceID, projectID string) *Registry {
	return loadRegistryFromGraph(g, repoPrefix, workspaceID, projectID, true, nil)
}

// RegistryLoadStats describes one restoration, without shared counters. Read
// phases include their projection/filtering work; construction covers decoding,
// deduplication and registry indexing, excluding the legacy liveness read phase.
type RegistryLoadStats struct {
	OwnerEdgesMS          float64 `json:"owner_edges_ms"`
	ScopedNodesMS         float64 `json:"scoped_nodes_ms"`
	MissingTargetsMS      float64 `json:"missing_targets_ms"`
	LegacyLivenessMS      float64 `json:"legacy_liveness_ms"`
	ConstructionMS        float64 `json:"construction_ms"`
	OwnerEdgeRows         int     `json:"owner_edge_rows"`
	ScopedNodeRows        int     `json:"scoped_node_rows"`
	MissingTargetIDs      int     `json:"missing_target_ids"`
	MissingTargetBatches  int     `json:"missing_target_batches"`
	MissingTargetRows     int     `json:"missing_target_rows"`
	LegacyCandidateIDs    int     `json:"legacy_candidate_ids"`
	LegacyLivenessBatches int     `json:"legacy_liveness_batches"`
	RecoveredRecords      int     `json:"recovered_records"`
}

// LoadRegistryFromGraphWithScopeAndStats is the observed sibling of the scoped
// loader. Existing entrypoints avoid clock reads and return identical records.
func LoadRegistryFromGraphWithScopeAndStats(g graph.Store, repoPrefix, workspaceID, projectID string) (*Registry, RegistryLoadStats) {
	var stats RegistryLoadStats
	registry := loadRegistryFromGraph(g, repoPrefix, workspaceID, projectID, true, &stats)
	return registry, stats
}

func registryLoadStart(stats *RegistryLoadStats) time.Time {
	if stats == nil {
		return time.Time{}
	}
	return time.Now()
}

func registryLoadMillis(start time.Time) float64 {
	return float64(time.Since(start).Nanoseconds()) / 1e6
}

func loadRegistryFromGraph(g graph.Store, repoPrefix, workspaceID, projectID string, useScope bool, stats *RegistryLoadStats) *Registry {
	if g == nil {
		return nil
	}
	started := registryLoadStart(stats)
	owners := graph.ReadRepoEdgesByKinds(g, []string{repoPrefix}, []graph.EdgeKind{graph.EdgeProvides, graph.EdgeConsumes})
	if stats != nil {
		stats.OwnerEdgesMS = registryLoadMillis(started)
		stats.OwnerEdgeRows = len(owners)
	}
	started = registryLoadStart(stats)
	nodes := make(map[string]*graph.Node)
	rememberNode := func(node *graph.Node) {
		if node != nil && node.ID != "" && node.Kind == graph.KindContract {
			nodes[node.ID] = node
		}
	}
	if repoPrefix == "" {
		// Retain the original branch's backend-defined exact empty scope.
		for _, node := range g.GetRepoNodes("") {
			rememberNode(node)
			if stats != nil {
				stats.ScopedNodeRows++
			}
		}
	} else {
		for node := range graph.NodesInScopeSeq(g, []string{repoPrefix}, nil, graph.KindContract) {
			rememberNode(node)
			if stats != nil {
				stats.ScopedNodeRows++
			}
		}
	}
	if stats != nil {
		stats.ScopedNodesMS = registryLoadMillis(started)
	}
	started = registryLoadStart(stats)
	missing := make(map[string]struct{})
	for _, row := range owners {
		if row.Edge != nil && row.Edge.To != "" && nodes[row.Edge.To] == nil {
			missing[row.Edge.To] = struct{}{}
		}
	}
	ids := make([]string, 0, len(missing))
	for id := range missing {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if stats != nil {
		stats.MissingTargetIDs = len(ids)
	}
	for start := 0; start < len(ids); start += 128 {
		if stats != nil {
			stats.MissingTargetBatches++
		}
		for _, node := range g.GetNodesByIDs(ids[start:min(start+128, len(ids))]) {
			rememberNode(node)
			if stats != nil {
				stats.MissingTargetRows++
			}
		}
	}
	if stats != nil {
		stats.MissingTargetsMS = registryLoadMillis(started)
	}
	started = registryLoadStart(stats)
	registry := NewRegistry()
	seenRecords := make(map[string][]Contract)
	ownerIdentities := make(map[persistedContractRecordKey]struct{})
	add := func(c Contract) {
		if c.ID == "" || c.RepoPrefix != repoPrefix {
			return
		}
		// Deduplicate exact recovered records only. Line and metadata can
		// distinguish records even when their semantic contract ID is shared.
		// If an in-memory adapter exposes a non-serializable metadata value,
		// preserve its record rather than silently discarding it.
		if encoded, err := json.Marshal(c); err == nil {
			key := string(encoded)
			// JSON is only a bucket key: concrete metadata types such as
			// int and float64 can encode identically but remain distinct.
			for _, existing := range seenRecords[key] {
				if reflect.DeepEqual(existing, c) {
					return
				}
			}
			seenRecords[key] = append(seenRecords[key], c)
		}
		registry.Add(c)
		if stats != nil {
			stats.RecoveredRecords++
		}
	}
	// A scalar payload for an existing owner identity is stale fallback, not
	// another owner merely because its confidence/type/metadata differs.
	// Recovered owner rows remain complete even where Registry.All deliberately
	// coalesces line/payload variants; ByID and other scoped indices retain them.
	for _, row := range owners {
		edge := row.Edge
		if edge == nil {
			continue
		}
		var c Contract
		if useScope {
			c, _ = ContractFromOwnerEdge(nodes[edge.To], edge, repoPrefix, workspaceID, projectID)
		} else {
			c = contractFromOwnerNode(nodes[edge.To], edge, repoPrefix)
		}
		if c.ID != "" && c.RepoPrefix == repoPrefix {
			ownerIdentities[persistedContractKey(c)] = struct{}{}
		}
		add(c)
	}
	if stats != nil {
		stats.ConstructionMS += registryLoadMillis(started)
	}
	started = registryLoadStart(stats)
	scalarLiveness := false
	if guarantee, ok := g.(graph.ContractOwnerScalarLiveness); ok {
		scalarLiveness = guarantee.ContractOwnerScalarLivenessGuaranteed()
	}
	conservativeOwners := make(map[string]bool)
	if !scalarLiveness {
		// Opaque adapters keep the old non-atomic replacement behavior. They
		// cannot safely invalidate a legacy scalar through read -> AddNode.
		// A surviving owner anywhere in this selected store/view makes scalar
		// fallback ambiguous, so omit it conservatively instead of reviving a
		// deleted record. Scalar-only legacy contracts still remain readable.
		candidates := make([]string, 0, len(nodes))
		for id, node := range nodes {
			ownerBacked, _ := node.Meta["contract_owner_record"].(bool)
			removed, _ := node.Meta["contract_owner_removed"].(bool)
			if node.RepoPrefix == repoPrefix && !ownerBacked && !removed {
				candidates = append(candidates, id)
			}
		}
		sort.Strings(candidates)
		if stats != nil {
			stats.LegacyCandidateIDs = len(candidates)
		}
		for start := 0; start < len(candidates); start += 128 {
			if stats != nil {
				stats.LegacyLivenessBatches++
			}
			for id, incoming := range g.GetInEdgesByNodeIDs(candidates[start:min(start+128, len(candidates))]) {
				for _, edge := range incoming {
					if edge != nil && (edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes || edge.Kind == graph.EdgeHandlesRoute) {
						conservativeOwners[id] = true
						break
					}
				}
			}
		}
	}
	if stats != nil {
		stats.LegacyLivenessMS = registryLoadMillis(started)
	}
	started = registryLoadStart(stats)
	var sparseOwnerIdentities map[persistedContractRecordKey]struct{}
	ids = ids[:0]
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		node := nodes[id]
		if node.RepoPrefix != repoPrefix {
			continue
		}
		ownerBacked, _ := node.Meta["contract_owner_record"].(bool)
		removed, _ := node.Meta["contract_owner_removed"].(bool)
		if ownerBacked || removed || conservativeOwners[id] {
			continue
		}
		c := contractFromNode(node)
		if useScope {
			// Legacy scalar reconstruction historically uses the owning
			// indexer's scope even when stale scalar scope is populated.
			c.RepoPrefix, c.WorkspaceID, c.ProjectID = repoPrefix, workspaceID, projectID
		}
		key := persistedContractKey(c)
		_, ownerExists := ownerIdentities[key]
		if _, symbolPresent := node.Meta["symbol_id"]; !symbolPresent && !ownerExists && len(ownerIdentities) > 0 {
			// Missing symbol identity is sparse legacy data, unlike an
			// explicitly empty string. Modern owner-backed loads never
			// allocate this fallback index; build it only once if needed.
			if sparseOwnerIdentities == nil {
				sparseOwnerIdentities = make(map[persistedContractRecordKey]struct{}, len(ownerIdentities))
				for ownerKey := range ownerIdentities {
					ownerKey.symbol = ""
					sparseOwnerIdentities[ownerKey] = struct{}{}
				}
			}
			key.symbol = ""
			_, ownerExists = sparseOwnerIdentities[key]
		}
		if !ownerExists {
			add(c)
		}
	}
	empty := len(registry.All()) == 0
	if stats != nil {
		stats.ConstructionMS += registryLoadMillis(started)
	}
	if empty {
		return nil
	}
	return registry
}

func contractFromNode(node *graph.Node) Contract {
	if node == nil || node.Kind != graph.KindContract || node.ID == "" {
		return Contract{}
	}
	c := Contract{ID: node.ID, FilePath: node.FilePath, RepoPrefix: node.RepoPrefix, WorkspaceID: node.WorkspaceID, ProjectID: node.ProjectID}
	if node.Meta == nil {
		return c
	}
	if value, ok := node.Meta["type"].(string); ok {
		c.Type = ContractType(value)
	}
	if value, ok := node.Meta["role"].(string); ok {
		c.Role = Role(value)
	}
	if value, ok := node.Meta["symbol_id"].(string); ok {
		c.SymbolID = value
	}
	c.Line = persistedContractLine(node.Meta["line"])
	c.Confidence = persistedContractConfidence(node.Meta["confidence"])
	if value, ok := node.Meta["contract_meta"].(map[string]any); ok {
		c.Meta = value
	}
	return c
}

// ContractFromGraphNode decodes the scalar payload, not its ownership liveness.
// LoadRegistryFromGraph applies persisted-owner/removal fallback policy.
func ContractFromGraphNode(node *graph.Node) (Contract, bool) {
	c := contractFromNode(node)
	return c, c.ID != ""
}

func contractFromOwnerNode(node *graph.Node, edge *graph.Edge, repo string) Contract {
	workspace, project := "", ""
	if node != nil && node.RepoPrefix == repo {
		workspace, project = node.WorkspaceID, node.ProjectID
	}
	c, _ := ContractFromOwnerEdge(node, edge, repo, workspace, project)
	return c
}

// ContractFromOwnerEdge is shared with incremental reconstruction. Scope
// arguments are fallbacks only; explicit durable owner payload is authoritative.
func ContractFromOwnerEdge(node *graph.Node, edge *graph.Edge, repo, workspace, project string) (Contract, bool) {
	c := contractFromNode(node)
	if c.ID == "" || edge == nil || edge.From == "" || (edge.Kind != graph.EdgeProvides && edge.Kind != graph.EdgeConsumes) {
		return Contract{}, false
	}
	c.WorkspaceID, c.ProjectID = workspace, project
	c.RepoPrefix, c.FilePath, c.SymbolID, c.Line = repo, edge.FilePath, edge.From, edge.Line
	if symbol, explicit := edge.Meta["contract_owner_symbol_id"].(string); explicit {
		c.SymbolID = symbol
		// New complete owner payloads must not inherit another record's
		// canonical metadata when this owner's metadata is genuinely nil.
		c.Meta = nil
	}
	c.Role = RoleProvider
	if edge.Kind == graph.EdgeConsumes {
		c.Role = RoleConsumer
	}
	if value, ok := edge.Meta["contract_owner_repo_prefix"].(string); ok {
		c.RepoPrefix = value
	}
	if value, ok := edge.Meta["contract_owner_workspace"].(string); ok {
		c.WorkspaceID = value
	}
	if value, ok := edge.Meta["contract_owner_project"].(string); ok {
		c.ProjectID = value
	}
	if value, ok := edge.Meta["contract_owner_type"].(string); ok {
		c.Type = ContractType(value)
	}
	if value, exists := edge.Meta["contract_owner_confidence"]; exists {
		c.Confidence = persistedContractConfidence(value)
	}
	if value, exists := edge.Meta["contract_owner_meta"]; exists {
		if value == nil {
			c.Meta = nil
		} else if meta, ok := value.(map[string]any); ok {
			c.Meta = meta
		}
	}
	if value, ok := edge.Meta["contract_owner_symbol_id"].(string); ok {
		c.SymbolID = value
	}
	return c, true
}

func persistedContractLine(value any) int {
	switch value := value.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func persistedContractConfidence(value any) float64 {
	switch value := value.(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		parsed, _ := value.Float64()
		return parsed
	default:
		return 0
	}
}
