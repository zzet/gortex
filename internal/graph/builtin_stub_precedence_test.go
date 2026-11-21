package graph

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// The resolver stamps boundary identity and metadata on explicit builtin
// nodes. Later derived edges must not replace those nodes with bare fallbacks.
func TestGraphBuiltinFallbackPreservesExplicitAndStoredNodes(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(fmt.Sprintf("previous=%v", previous), func(t *testing.T) {
			g := New()
			rich := &Node{
				ID: "repo::builtin::go::type::int", Kind: KindBuiltin, Name: "int", Language: "go",
				RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project",
				Meta: map[string]any{"builtin": true, "builtin_kind": "type", "other": "retained"},
			}
			caller := &Node{ID: "repo/a.go::f", Kind: KindFunction, Name: "f", FilePath: "repo/a.go", RepoPrefix: "repo"}
			edge := &Edge{From: caller.ID, To: rich.ID, Kind: EdgeTypedAs, FilePath: caller.FilePath}
			if previous {
				g.AddBatch([]*Node{caller, rich}, nil)
				g.AddBatch(nil, []*Edge{edge})
			} else {
				g.AddBatch([]*Node{caller, rich}, []*Edge{edge})
			}
			require.Equal(t, rich, g.GetNode(rich.ID))
			require.Equal(t, 2, g.NodeCount())

			updated := *rich
			updated.ProjectID = "updated-project"
			g.AddBatch([]*Node{&updated}, []*Edge{edge})
			require.Equal(t, &updated, g.GetNode(rich.ID), "explicit writes remain upserts")
		})
	}
}

func TestGraphBuiltinFallbackCannotOverwriteAConcurrentExplicitNode(t *testing.T) {
	g := New()
	for i := range 32 {
		rich := &Node{ID: fmt.Sprintf("repo::builtin::go::type::probe%d", i), Kind: KindBuiltin, Name: fmt.Sprintf("probe%d", i), RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project", Language: "go"}
		edge := &Edge{From: "repo/a.go::f", To: rich.ID, Kind: EdgeTypedAs, FilePath: "repo/a.go"}
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(2)
		go func() { defer done.Done(); <-start; g.AddNode(rich) }()
		go func() { defer done.Done(); <-start; g.AddBatch(nil, []*Edge{edge}) }()
		close(start)
		done.Wait()
		require.Equal(t, rich, g.GetNode(rich.ID))
	}
}

func TestGraphBuiltinFallbackReplacesProxy(t *testing.T) {
	g := New()
	proxy := &Node{ID: "repo::builtin::go::make", Stub: true, Origin: "remote"}
	g.AddBatch([]*Node{proxy}, []*Edge{
		{From: "repo/a.go::f", To: proxy.ID, Kind: EdgeCalls, FilePath: "repo/a.go"},
	})
	node := g.GetNode(proxy.ID)
	require.NotNil(t, node)
	require.Equal(t, KindBuiltin, node.Kind)
	require.False(t, IsProxyNode(node))
}

func TestGraphBuiltinFallbackPreservesColdExplicitUpdate(t *testing.T) {
	g := New()
	rich := &Node{ID: "repo::builtin::go::type::int", Kind: KindBuiltin, Name: "int", Language: "go", RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "old-project"}
	g.AddNode(rich)
	updated := *rich
	updated.ProjectID = "new-project"
	// No previous edge warmed builtinSeen. Both the explicit update and a
	// generated fallback now have this ID; only the latter is insert-only.
	g.AddBatch([]*Node{&updated}, []*Edge{
		{From: "repo/a.go::f", To: rich.ID, Kind: EdgeTypedAs, FilePath: "repo/a.go"},
	})
	require.Equal(t, &updated, g.GetNode(rich.ID))
}
