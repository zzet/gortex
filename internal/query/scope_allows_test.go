package query

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// TestQueryOptions_ScopeAllows is the per-node enforcement matrix for
// the workspace boundary. It covers the singleton fallback (a node
// with no declared workspace is keyed on its repo prefix) which keeps
// the session boundary consistent with how the indexer stamps nodes.
func TestQueryOptions_ScopeAllows(t *testing.T) {
	node := func(ws, proj, prefix string) *graph.Node {
		return &graph.Node{WorkspaceID: ws, ProjectID: proj, RepoPrefix: prefix}
	}

	tests := []struct {
		name string
		opts QueryOptions
		node *graph.Node
		want bool
	}{
		{
			name: "empty scope allows everything",
			opts: QueryOptions{},
			node: node("vio", "", "rate_checkers_detector"),
			want: true,
		},
		{
			// ScopeAllows treats a nil node as "allow" — every caller
			// nil-checks before calling (the engine traversal does, and
			// the MCP layer's nodeInSessionScope rejects nil itself).
			name: "nil node passes (callers nil-check upstream)",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: nil,
			want: true,
		},
		{
			name: "same workspace passes",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: node("gortex", "", "gortex"),
			want: true,
		},
		{
			name: "different workspace is rejected",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: node("vio", "", "rate_checkers_detector"),
			want: false,
		},
		{
			name: "node with empty workspace falls back to repo prefix — match",
			opts: QueryOptions{WorkspaceID: "rate_checkers_detector"},
			node: node("", "", "rate_checkers_detector"),
			want: true,
		},
		{
			name: "node with empty workspace falls back to repo prefix — mismatch",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: node("", "", "rate_checkers_detector"),
			want: false,
		},
		{
			name: "project narrows within the workspace — match",
			opts: QueryOptions{WorkspaceID: "gortex", ProjectID: "web"},
			node: node("gortex", "web", "web"),
			want: true,
		},
		{
			name: "project narrows within the workspace — mismatch",
			opts: QueryOptions{WorkspaceID: "gortex", ProjectID: "web"},
			node: node("gortex", "core", "core"),
			want: false,
		},
		{
			name: "empty project on opts allows any project in the workspace",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: node("gortex", "web", "web"),
			want: true,
		},
		{
			name: "project match cannot rescue a workspace mismatch",
			opts: QueryOptions{WorkspaceID: "gortex", ProjectID: "web"},
			node: node("vio", "web", "web"),
			want: false,
		},
		{
			name: "repo allow nil does not narrow",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: node("gortex", "core", "core"),
			want: true,
		},
		{
			name: "repo allow match passes",
			opts: QueryOptions{RepoAllow: map[string]bool{"core": true}},
			node: node("", "", "core"),
			want: true,
		},
		{
			name: "repo allow mismatch is rejected",
			opts: QueryOptions{RepoAllow: map[string]bool{"web": true}},
			node: node("", "", "core"),
			want: false,
		},
		{
			name: "workspace project and repo allow compose",
			opts: QueryOptions{
				WorkspaceID: "gortex",
				ProjectID:   "backend",
				RepoAllow:   map[string]bool{"payments": true},
			},
			node: node("gortex", "backend", "payments"),
			want: true,
		},
		{
			name: "repo allow cannot rescue a project mismatch",
			opts: QueryOptions{
				WorkspaceID: "gortex",
				ProjectID:   "backend",
				RepoAllow:   map[string]bool{"payments": true},
			},
			node: node("gortex", "frontend", "payments"),
			want: false,
		},
		{
			// Single-repo (unprefixed) mode: nodes carry no RepoPrefix
			// while the registry — and therefore every RepoAllow set —
			// keys the repo by name. Repo narrowing must not reject the
			// lone repo's own nodes (the fresh-install search_symbols
			// zero-rows regression).
			name: "repo allow admits an unprefixed single-repo node",
			opts: QueryOptions{RepoAllow: map[string]bool{"gin": true}},
			node: node("gin", "gin", ""),
			want: true,
		},
		{
			name: "unprefixed node passes repo allow under a matching workspace",
			opts: QueryOptions{
				WorkspaceID: "gin",
				RepoAllow:   map[string]bool{"gin": true},
			},
			node: node("gin", "gin", ""),
			want: true,
		},
		{
			// The carve-out must not weaken the workspace boundary: an
			// unprefixed node outside the session workspace stays
			// rejected even when RepoAllow would vacuously admit it.
			name: "workspace mismatch still rejects an unprefixed node",
			opts: QueryOptions{
				WorkspaceID: "other",
				RepoAllow:   map[string]bool{"other": true},
			},
			node: node("gin", "gin", ""),
			want: false,
		},
		{
			// A global external (no repository, no workspace) is visible
			// from every scope: find_usages on ext::go:fmt::Errorf must not
			// read it as outside the session's workspace.
			name: "a global external passes a workspace scope",
			opts: QueryOptions{
				WorkspaceID: "gin",
				ProjectID:   "gin",
				RepoAllow:   map[string]bool{"gin": true},
			},
			node: &graph.Node{ID: "ext::go:fmt::Errorf", FilePath: "external::go:fmt"},
			want: true,
		},
		{
			// Only a synthetic external is global: an unowned node at a
			// source path is not admitted by the carve-out.
			name: "an unowned node at a source path is rejected by a workspace scope",
			opts: QueryOptions{WorkspaceID: "gin"},
			node: &graph.Node{ID: "svc/unowned.go", FilePath: "svc/unowned.go"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.opts.ScopeAllows(tt.node))
		})
	}
}
