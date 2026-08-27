package mcp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// TestResponseBudgetContractMatrix sweeps the schema's byte/token
// budget across EVERY find_usages rendering on BOTH backends: the
// budget is a contract on the response, not on one renderer, so no
// format — new, or newly brought to life — can step outside it
// silently. Text renderers trim to a hard ceiling; structured
// renderers trim list tails, so their floor is the scalar skeleton —
// the matrix budget sits above it.
func TestResponseBudgetContractMatrix(t *testing.T) {
	renderings := []struct {
		name string
		args map[string]any
	}{
		{"json", map[string]any{}},
		{"compact", map[string]any{"compact": true}},
		{"toon", map[string]any{"format": "toon"}},
		{"gcx", map[string]any{"format": "gcx"}},
		{"mermaid", map[string]any{"format": "mermaid"}},
		{"dot", map[string]any{"format": "dot"}},
		{"group_by_file", map[string]any{"group_by": "file"}},
	}
	budgets := []struct {
		name string
		args map[string]any
		cap  int
	}{
		{"max_bytes", map[string]any{"max_bytes": 900}, 900},
		{"max_tokens", map[string]any{"max_tokens": 300}, tokensToBytes(300)},
	}
	build := func(g graph.Store) string {
		hot := &graph.Node{ID: "pkg/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "pkg/hot.go", StartLine: 1}
		g.AddNode(hot)
		for i := 0; i < 60; i++ {
			file := fmt.Sprintf("pkg/use%02d.go", i)
			caller := &graph.Node{ID: fmt.Sprintf("%s::Use%02d", file, i), Kind: graph.KindFunction, Name: fmt.Sprintf("Use%02d", i), FilePath: file, StartLine: 3}
			g.AddNode(caller)
			g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5, Origin: graph.OriginASTResolved, Confidence: 0.9})
		}
		return hot.ID
	}

	run := func(t *testing.T, g graph.Store, hotID string) {
		eng := query.NewEngine(g)
		eng.SetSearch(search.NewNull())
		srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
		for _, r := range renderings {
			for _, b := range budgets {
				t.Run(r.name+"/"+b.name, func(t *testing.T) {
					args := map[string]any{"id": hotID, "limit": 0}
					for k, v := range r.args {
						args[k] = v
					}
					for k, v := range b.args {
						args[k] = v
					}
					out := findUsagesText(t, srv, args)
					require.LessOrEqual(t, len(out), b.cap,
						"rendering %q must honor %s (cap %d), got %d bytes", r.name, b.name, b.cap, len(out))
					require.NotEmpty(t, out, "a budgeted response is trimmed, never blank")
				})
			}
		}
	}

	t.Run("memory", func(t *testing.T) {
		g := graph.New()
		run(t, g, build(g))
	})
	t.Run("sqlite", func(t *testing.T) {
		g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "matrix.sqlite"))
		require.NoError(t, err)
		defer g.Close()
		run(t, g, build(g))
	})
}

// TestResponseLimitContractMatrix sweeps the schema's `limit` across
// the row-bearing renderings on both backends: every shape that lists
// usages must page by the same cap.
func TestResponseLimitContractMatrix(t *testing.T) {
	build := func(g graph.Store) string {
		hot := &graph.Node{ID: "pkg/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "pkg/hot.go", StartLine: 1}
		g.AddNode(hot)
		for i := 0; i < 8; i++ {
			file := fmt.Sprintf("pkg/use%d.go", i)
			caller := &graph.Node{ID: fmt.Sprintf("%s::Use%d", file, i), Kind: graph.KindFunction, Name: fmt.Sprintf("Use%d", i), FilePath: file, StartLine: 3}
			g.AddNode(caller)
			g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5, Origin: graph.OriginASTResolved, Confidence: 0.9})
		}
		return hot.ID
	}
	countRows := map[string]func(t *testing.T, out string) int{
		"json": func(t *testing.T, out string) int {
			var resp usagesLimitResponse
			require.NoError(t, json.Unmarshal([]byte(out), &resp))
			return len(resp.Edges)
		},
		"group_by_file": func(t *testing.T, out string) int {
			var resp struct {
				Groups []struct {
					Uses []json.RawMessage `json:"uses"`
				} `json:"groups"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &resp))
			rows := 0
			for _, g := range resp.Groups {
				rows += len(g.Uses)
			}
			return rows
		},
	}
	argsFor := map[string]map[string]any{
		"json":          {},
		"group_by_file": {"group_by": "file"},
	}

	run := func(t *testing.T, g graph.Store, hotID string) {
		eng := query.NewEngine(g)
		eng.SetSearch(search.NewNull())
		srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
		for name, counter := range countRows {
			t.Run(name, func(t *testing.T) {
				args := map[string]any{"id": hotID, "limit": 3}
				for k, v := range argsFor[name] {
					args[k] = v
				}
				out := findUsagesText(t, srv, args)
				require.Equal(t, 3, counter(t, out), "rendering %q must page by the caller's limit", name)
			})
		}
	}

	t.Run("memory", func(t *testing.T) {
		g := graph.New()
		run(t, g, build(g))
	})
	t.Run("sqlite", func(t *testing.T) {
		g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "matrix-limit.sqlite"))
		require.NoError(t, err)
		defer g.Close()
		run(t, g, build(g))
	})
}
