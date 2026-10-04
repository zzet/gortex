package contracts

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// RegistryLoadOptions selects an exact repo namespace and optional canonical
// cohorts. FilePaths are graph paths, not filesystem paths. ExpandFileIDs keeps
// every in-repo side of canonical IDs owned by a selected file (check/validate);
// otherwise file selection retains only records owned by those exact files.
// RPC/tRPC file matching broadens to the complete exact repo (see loader).
// UseScope supplies legacy fallback scope, never replacing durable owner scope.
// Limit refuses the complete load rather than truncating it; zero uses the
// projection bound. Empty RepoPrefix denotes the exact standalone namespace.
type RegistryLoadOptions struct {
	RepoPrefix, WorkspaceID, ProjectID string
	UseScope                           bool
	FilePaths, ContractIDs             []string
	ExpandFileIDs                      bool
	Limit                              int
}

// LoadRegistryFromGraphChecked reconstructs a selected reader's persisted
// registry using only checked bounded projections. No usable registry is
// returned on read, cancellation, incomplete, stale or limit errors. The caller
// owns selected-view witness capture/validation around this multi-read pass.
// Unsupported readers are refused, never adapted through errorless Store APIs.
func LoadRegistryFromGraphChecked(ctx context.Context, r graph.Reader, opts RegistryLoadOptions) (*Registry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, graph.ErrContractProjectionUnsupported
	}
	var p graph.ContractFileProjection
	var err error
	switch {
	case len(opts.FilePaths) > 0:
		reader, ok := r.(graph.ContractFileProjectionReader)
		if !ok {
			return nil, graph.ErrContractProjectionUnsupported
		}
		p, err = reader.LoadContractFileProjectionContext(ctx, opts.RepoPrefix, opts.FilePaths)
	case len(opts.ContractIDs) > 0:
		reader, ok := r.(graph.ContractFileProjectionReader)
		if !ok {
			return nil, graph.ErrContractProjectionUnsupported
		}
		p, err = reader.LoadContractIDProjectionContext(ctx, opts.ContractIDs)
	default:
		reader, ok := r.(graph.ContractRepoProjectionReader)
		if !ok {
			return nil, graph.ErrContractProjectionUnsupported
		}
		p, err = reader.LoadContractRepoProjectionContext(ctx, opts.RepoPrefix)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files := make(map[string]bool, len(opts.FilePaths))
	for _, path := range opts.FilePaths {
		files[path] = true
	}
	// RPC and tRPC matching joins non-identical service/procedure IDs. Until
	// a checked canonical cohort index exists, selected file matching explicitly
	// falls back to the complete exact repo. Callers must use repo-wide readiness
	// and must not describe this fallback as file-local proof.
	if opts.ExpandFileIDs && len(files) > 0 {
		broad := false
		for _, row := range p.OwnerRows {
			if row.Edge == nil || !files[row.Edge.FilePath] {
				continue
			}
			c := contractFromOwnerNode(p.Targets[row.Edge.To], row.Edge, row.RepoPrefix)
			if isRPCFamily(c) || c.Type == ContractTRPC {
				broad = true
				break
			}
		}
		for _, n := range p.ScalarNodes {
			if n != nil && files[n.FilePath] {
				c := contractFromNode(n)
				if isRPCFamily(c) || c.Type == ContractTRPC {
					broad = true
					break
				}
			}
		}
		if broad {
			opts.FilePaths = nil
			opts.ContractIDs = nil
			opts.ExpandFileIDs = false
			return LoadRegistryFromGraphChecked(ctx, r, opts)
		}
	}
	ids := make(map[string]bool, len(opts.ContractIDs))
	for _, id := range opts.ContractIDs {
		ids[id] = true
	}
	if opts.ExpandFileIDs && len(files) > 0 {
		fileIDs := make(map[string]bool)
		for _, row := range append(append([]graph.RepoEdgeRow(nil), p.OwnerRows...), p.OffFileOwnerRows...) {
			if row.Edge != nil && files[row.Edge.FilePath] {
				fileIDs[row.Edge.To] = true
			}
		}
		for _, n := range p.ScalarNodes {
			if n != nil && files[n.FilePath] {
				fileIDs[n.ID] = true
			}
		}
		if len(ids) == 0 {
			ids = fileIDs
		} else {
			for id := range ids {
				if !fileIDs[id] {
					delete(ids, id)
				}
			}
		}
	}
	selected := func(c Contract) bool {
		if c.ID == "" || c.RepoPrefix != opts.RepoPrefix {
			return false
		}
		if len(opts.ContractIDs) > 0 || (opts.ExpandFileIDs && len(files) > 0) {
			if !ids[c.ID] {
				return false
			}
		}
		return len(files) == 0 || opts.ExpandFileIDs || files[c.FilePath]
	}
	limit := opts.Limit
	if limit <= 0 || limit > graph.ContractProjectionRowLimit {
		limit = graph.ContractProjectionRowLimit
	}
	registry := NewRegistry()
	seen := make(map[string][]Contract)
	count := 0
	add := func(c Contract) error {
		if !selected(c) {
			return nil
		}
		if encoded, err := json.Marshal(c); err == nil {
			key := string(encoded)
			for _, old := range seen[key] {
				if reflect.DeepEqual(old, c) {
					return nil
				}
			}
			seen[key] = append(seen[key], c)
		}
		count++
		if count > limit {
			return graph.ErrContractProjectionLimit
		}
		registry.Add(c)
		return nil
	}
	identities := make(map[persistedContractRecordKey]bool)
	ownedIDs := make(map[string]bool)
	for _, row := range p.OwnerRows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e := row.Edge
		if e == nil {
			continue
		}
		ownedIDs[e.To] = true // includes handles_route evidence for scalar liveness
		if e.Kind != graph.EdgeProvides && e.Kind != graph.EdgeConsumes {
			continue
		}
		target := p.Targets[e.To]
		if target == nil {
			return nil, graph.ErrContractProjectionIncomplete
		}
		if p.SourceNodes[e.From] == nil {
			return nil, graph.ErrContractProjectionIncomplete
		}
		var c Contract
		if opts.UseScope {
			c, _ = ContractFromOwnerEdge(target, e, row.RepoPrefix, opts.WorkspaceID, opts.ProjectID)
		} else {
			c = contractFromOwnerNode(target, e, row.RepoPrefix)
		}
		if c.ID == "" {
			return nil, graph.ErrContractProjectionIncomplete
		}
		identities[persistedContractKey(c)] = true
		if err := add(c); err != nil {
			return nil, err
		}
	}
	nodes := make(map[string]*graph.Node)
	for _, n := range p.ScalarNodes {
		if n != nil {
			nodes[n.ID] = n
		}
	}
	for id, n := range p.Targets {
		nodes[id] = n
	}
	scalarLiveness := false
	if guarantee, ok := r.(graph.ContractOwnerScalarLiveness); ok {
		scalarLiveness = guarantee.ContractOwnerScalarLivenessGuaranteed()
	}
	keys := make([]string, 0, len(nodes))
	for id := range nodes {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := nodes[id]
		if n == nil || n.RepoPrefix != opts.RepoPrefix {
			continue
		}
		ownerBacked, _ := n.Meta["contract_owner_record"].(bool)
		removed, _ := n.Meta["contract_owner_removed"].(bool)
		if ownerBacked || removed || (!scalarLiveness && ownedIDs[id]) {
			continue
		}
		c := contractFromNode(n)
		if opts.UseScope {
			c.RepoPrefix, c.WorkspaceID, c.ProjectID = opts.RepoPrefix, opts.WorkspaceID, opts.ProjectID
		}
		key := persistedContractKey(c)
		ownerExists := identities[key]
		if _, present := n.Meta["symbol_id"]; !present && !ownerExists {
			key.symbol = ""
			for ownerKey := range identities {
				ownerKey.symbol = ""
				if ownerKey == key {
					ownerExists = true
					break
				}
			}
		}
		if !ownerExists {
			if err := add(c); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return registry, nil
}
