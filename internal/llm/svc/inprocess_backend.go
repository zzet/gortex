package svc

import (
	"context"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/query"
)

// InProcessBackend implements Backend by calling gortex's in-process
// graph engine + contract registry directly — no MCP subprocess, no
// JSON round-trip. Used when the LLM service runs inside the same
// process as the gortex daemon (the common deployment).
//
// All methods are read-only and concurrent-safe (they delegate to
// the engine and registry, both of which already handle their own
// locking).
type InProcessBackend struct {
	engine      *query.Engine
	contractsFn func() *contracts.Registry
}

// NewInProcessBackend constructs an in-process backend. engine must
// not be nil. contractsFn is called per ListContracts request so the
// backend always sees the latest registry — gortex swaps the
// registry pointer when indexes re-extract contracts.
func NewInProcessBackend(engine *query.Engine, contractsFn func() *contracts.Registry) *InProcessBackend {
	return &InProcessBackend{engine: engine, contractsFn: contractsFn}
}

var _ llm.Backend = (*InProcessBackend)(nil)

// SearchSymbols delegates to engine.SearchSymbolsScoped and adapts
// the result. RepoAllow + ProjectID on QueryOptions preserve repository
// and project scoping independently of the repository's workspace. Ref
// is not yet plumbed through the engine query layer.
func (b *InProcessBackend) SearchSymbols(_ context.Context, q string, scope llm.Scope, limit int) ([]llm.Match, error) {
	if limit <= 0 {
		limit = 20
	}
	opts := scopeToOptions(scope)
	opts.Limit = limit
	nodes := b.engine.SearchSymbolsScoped(q, limit, opts)
	out := make([]llm.Match, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		out = append(out, nodeToMatch(n))
	}
	return out, nil
}

// GetCallers returns every node in the get_callers subgraph that's
// not the queried symbol — i.e. all callers (direct and transitive
// up to QueryOptions.Depth).
func (b *InProcessBackend) GetCallers(_ context.Context, id string, scope llm.Scope, limit int) ([]llm.Caller, error) {
	opts := scopeToOptions(scope)
	if limit > 0 {
		opts.Limit = limit
	}
	sg := b.engine.GetCallers(id, opts)
	if sg == nil {
		return nil, nil
	}
	out := make([]llm.Caller, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n == nil || n.ID == id {
			continue
		}
		out = append(out, nodeToCaller(n))
	}
	return out, nil
}

// GetDependencies returns every node the queried symbol depends on
// — direct and transitive up to depth. The Kind field on each Dep
// is filled from the edge kind when an edge from id→dep exists in
// the subgraph; otherwise left empty.
func (b *InProcessBackend) GetDependencies(_ context.Context, id string, scope llm.Scope, depth, limit int) ([]llm.Dep, error) {
	opts := scopeToOptions(scope)
	if depth > 0 {
		opts.Depth = depth
	}
	if limit > 0 {
		opts.Limit = limit
	}
	sg := b.engine.GetDependencies(id, opts)
	if sg == nil {
		return nil, nil
	}
	// Map id → edge.Kind for one-hop direct deps so we can attribute
	// the dep kind ("calls", "imports", "references") when the agent
	// inspects the result.
	directKind := make(map[string]string, len(sg.Edges))
	for _, e := range sg.Edges {
		if e == nil || e.From != id {
			continue
		}
		if _, ok := directKind[e.To]; !ok {
			directKind[e.To] = string(e.Kind)
		}
	}
	out := make([]llm.Dep, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n == nil || n.ID == id {
			continue
		}
		out = append(out, llm.Dep{
			ID:   n.ID,
			Kind: directKind[n.ID], // empty for transitive deps
			File: n.FilePath,
			Repo: n.RepoPrefix,
		})
	}
	return out, nil
}

// ListContracts iterates the contract registry and applies the
// filter client-side. Scope is honoured by matching on
// Contract.EffectiveWorkspace / EffectiveProject.
func (b *InProcessBackend) ListContracts(_ context.Context, f llm.ContractFilter, scope llm.Scope) ([]llm.Contract, error) {
	if b.contractsFn == nil {
		return nil, nil
	}
	reg := b.contractsFn()
	if reg == nil {
		return nil, nil
	}
	all := reg.All()
	out := make([]llm.Contract, 0, len(all))
	for _, c := range all {
		if f.Type != "" && string(c.Type) != f.Type {
			continue
		}
		if f.Role != "" && string(c.Role) != f.Role {
			continue
		}
		method, path := contractMethodPath(c)
		if f.Method != "" && method != f.Method {
			continue
		}
		if f.Path != "" && path != f.Path {
			continue
		}
		if scope.Repo != "" && c.RepoPrefix != scope.Repo {
			continue
		}
		if scope.Project != "" && c.EffectiveProject() != scope.Project {
			continue
		}
		out = append(out, llm.Contract{
			ID:       c.ID,
			Type:     string(c.Type),
			Role:     string(c.Role),
			Repo:     c.RepoPrefix,
			Method:   method,
			Path:     path,
			File:     c.FilePath,
			Line:     c.Line,
			SymbolID: c.SymbolID,
		})
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

// ListRepos isn't exposed by the engine directly. Returns nil for
// now — the agent doesn't currently rely on this, and chain-mode
// tracing finds cross-repo edges via Dep.Repo on dependency results.
// A real implementation would query the daemon's MultiIndexer; that
// can be added if a future feature needs it.
func (b *InProcessBackend) ListRepos(_ context.Context) ([]llm.Repo, error) {
	return nil, nil
}

// nodeToMatch converts an indexed graph node into the agent-facing
// Match shape. Field mapping follows the daemon's canonical JSON:
// kind / path / line / repo come straight from the node's metadata.
func nodeToMatch(n *graph.Node) llm.Match {
	return llm.Match{
		ID:   n.ID,
		Name: n.Name,
		Kind: string(n.Kind),
		Path: n.FilePath,
		Line: n.StartLine,
		Repo: n.RepoPrefix,
	}
}

func nodeToCaller(n *graph.Node) llm.Caller {
	return llm.Caller{ID: n.ID, File: n.FilePath, Repo: n.RepoPrefix}
}

// scopeToOptions keeps repository identity separate from workspace identity.
// Multiple repositories may share a workspace, and the repository prefix need
// not equal that workspace. An explicit repo also disambiguates ProjectID.
// Project-only scope retains the query engine's existing unqualified behavior.
func scopeToOptions(scope llm.Scope) query.QueryOptions {
	opts := query.DefaultOptions()
	if scope.Repo != "" {
		opts.RepoAllow = map[string]bool{scope.Repo: true}
	}
	if scope.Project != "" {
		opts.ProjectID = scope.Project
	}
	return opts
}

// contractMethodPath pulls HTTP method/path out of a Contract. They
// usually live in Meta for non-DI types; HTTP contracts also encode
// them in the ID as `http::METHOD::/path`. Try meta first, then ID.
func contractMethodPath(c contracts.Contract) (method, path string) {
	if m := c.Meta; m != nil {
		if v, ok := m["method"].(string); ok {
			method = v
		}
		if v, ok := m["path"].(string); ok {
			path = v
		}
	}
	if (method == "" || path == "") && strings.HasPrefix(c.ID, "http::") {
		parts := strings.SplitN(c.ID, "::", 3)
		if len(parts) == 3 {
			if method == "" {
				method = parts[1]
			}
			if path == "" {
				path = parts[2]
			}
		}
	}
	return method, path
}
