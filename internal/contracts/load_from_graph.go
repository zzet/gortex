package contracts

import (
	"iter"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

// LoadRegistryFromGraph rebuilds a Registry by scanning every
// KindContract node under repoPrefix and reconstructing the Contract
// struct from Node.Meta. The reverse of the AddNode stamping the
// indexer's commitContracts (and contracts/wrapper.go's
// commitInlinedContractToGraph) do — both write the full record onto
// Meta so a daemon restart can rehydrate without replaying the gob
// snapshot.
//
// Empty repoPrefix loads every contract — useful for ad-hoc probes,
// not a path the daemon normally takes (the warmup rehydrates the
// per-repo registries one prefix at a time so a stale repo's
// contracts don't bleed into a fresh sibling). Returns nil when no
// contracts are recorded for the prefix.
func LoadRegistryFromGraph(g graph.Store, repoPrefix string) *Registry {
	if g == nil {
		return nil
	}
	var nodes iter.Seq[*graph.Node]
	if repoPrefix == "" {
		// Preserve the store's global empty-prefix behavior; scoped readers
		// require an explicit repository or file frontier.
		nodes = slices.Values(g.GetRepoNodes(""))
	} else {
		// Filter before hydration, retaining full metadata for sparse and
		// wrapper-generated contracts. SQLite streams bounded pages here.
		nodes = graph.NodesInScopeSeq(g, []string{repoPrefix}, nil, graph.KindContract)
	}
	reg := NewRegistry()
	for n := range nodes {
		if n == nil || n.Kind != graph.KindContract {
			continue
		}
		c := contractFromNode(n)
		if c.ID == "" {
			continue
		}
		reg.Add(c)
	}
	if len(reg.All()) == 0 {
		return nil
	}
	return reg
}

// contractFromNode decodes a Contract from a KindContract graph node's
// Meta payload. Inverse of the AddNode stamping the indexer does.
// Missing fields are left at their zero value — preserves forward
// compatibility if the indexer adds new Meta keys before this loader
// learns about them.
func contractFromNode(n *graph.Node) Contract {
	c := Contract{
		ID:         n.ID,
		FilePath:   n.FilePath,
		RepoPrefix: n.RepoPrefix,
	}
	if n.Meta == nil {
		return c
	}
	if v, ok := n.Meta["type"].(string); ok {
		c.Type = ContractType(v)
	}
	if v, ok := n.Meta["role"].(string); ok {
		c.Role = Role(v)
	}
	if v, ok := n.Meta["symbol_id"].(string); ok {
		c.SymbolID = v
	}
	if v, ok := n.Meta["line"].(int); ok {
		c.Line = v
	} else if v, ok := n.Meta["line"].(int64); ok {
		c.Line = int(v)
	}
	if v, ok := n.Meta["confidence"].(float64); ok {
		c.Confidence = v
	}
	c.WorkspaceID = n.WorkspaceID
	c.ProjectID = n.ProjectID
	if v, ok := n.Meta["contract_meta"].(map[string]any); ok && len(v) > 0 {
		c.Meta = v
	}
	return c
}
