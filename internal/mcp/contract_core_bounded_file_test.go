package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

type contractCoreFileSpy struct {
	graph.Reader
	calls, fullReads, limit int
	path                    string
	scope                   graph.LocalizationNodeScope
	ctx                     context.Context
	err                     error
	cancel                  context.CancelFunc
}

func (s *contractCoreFileSpy) FindFileNodesBounded(ctx context.Context, path string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
	s.calls++
	s.ctx, s.path, s.scope, s.limit = ctx, path, scope, limit
	if s.cancel != nil {
		s.cancel()
	}
	if s.err != nil || ctx.Err() != nil {
		// A failed backend may return a partial page; it must not become
		// enclosing-symbol evidence through the outer wrapper.
		return graph.BoundedNodeProjection{Nodes: []*graph.Node{{ID: "partial"}}, Total: 1}, s.err
	}
	return s.Reader.(graph.BoundedFileNodeReader).FindFileNodesBounded(ctx, path, scope, limit)
}

func (s *contractCoreFileSpy) AllNodes() []*graph.Node {
	s.fullReads++
	return s.Reader.AllNodes()
}

func (s *contractCoreFileSpy) GetFileNodes(path string) []*graph.Node {
	s.fullReads++
	return s.Reader.GetFileNodes(path)
}

type contractCoreFilteredFileSpy struct{ *contractCoreFileSpy }

func (s *contractCoreFilteredFileSpy) FindNodesByNameContainingFilteredContext(ctx context.Context, substr string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, error) {
	return graph.FindNodesByNameContainingFilteredContext(ctx, s.Reader, substr, limit, filter)
}

func TestContractCoreBoundedFileCapabilitiesRemainConditional(t *testing.T) {
	base := graph.New()
	for _, test := range []struct {
		name              string
		reader            graph.Reader
		filtered, bounded bool
	}{
		{"neither", struct{ graph.Reader }{base}, false, false},
		{"names", &contractCoreLookupSpy{Reader: base}, true, false},
		{"files", &contractCoreFileSpy{Reader: base}, false, true},
		{"both", &contractCoreFilteredFileSpy{&contractCoreFileSpy{Reader: base}}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := newContractCoreEdges(test.reader, t.Context(), nil)
			_, filtered := reader.(graph.FilteredContainingNameReader)
			_, bounded := reader.(graph.BoundedFileNodeReader)
			require.Equal(t, test.filtered, filtered)
			require.Equal(t, test.bounded, bounded)
			if !bounded {
				indexes := (&Server{}).buildFileSymbolIndexForOrderedPathsScopedReaderContext(t.Context(), reader, []string{"repo/a.go"}, query.QueryOptions{})
				require.True(t, indexes["repo/a.go"].saturated, "legacy absence must not become an unbounded file fallback")
			}
		})
	}
}

func TestContractCoreBoundedFileRetainsSelectedEnclosingSymbol(t *testing.T) {
	const path = "repo/handler.go"
	const deleted = "repo/deleted.go"
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: path + "::old", Name: "old", Kind: graph.KindFunction, FilePath: path, RepoPrefix: "repo", StartLine: 1, EndLine: 20},
		{ID: deleted + "::gone", Name: "gone", Kind: graph.KindFunction, FilePath: deleted, RepoPrefix: "repo", StartLine: 1, EndLine: 20},
	}, nil)
	layer := graph.NewOverlayLayer()
	layer.MarkFile(path, false)
	layer.MarkFile(deleted, true)
	current := &graph.Node{ID: path + "::current", Name: "current", Kind: graph.KindFunction, FilePath: path, RepoPrefix: "repo", StartLine: 2, EndLine: 18}
	layer.AddNode(path, current)
	selected := graph.NewOverlaidView(base, layer)
	spy := &contractCoreFileSpy{Reader: selected}
	reader := newContractCoreEdges(&contractCoreFilteredFileSpy{spy}, t.Context(), nil)
	indexes := (&Server{}).buildFileSymbolIndexForOrderedPathsScopedReaderContext(t.Context(), reader, []string{path, deleted}, query.QueryOptions{RepoAllow: map[string]bool{"repo": true}})
	require.NotNil(t, indexes[path])
	require.False(t, indexes[path].saturated)
	id, name := indexes[path].find(10)
	require.Equal(t, current.ID, id)
	require.Equal(t, current.Name, name)
	require.Nil(t, indexes[deleted], "selected tombstone must suppress stale ownership")
	require.Equal(t, 2, spy.calls)
	require.Zero(t, spy.fullReads)
	rows, err := graph.FindNodesByNameContainingFilteredContext(t.Context(), reader, "current", 1, graph.NameSearchFilter{})
	require.NoError(t, err)
	require.Equal(t, []*graph.Node{current}, rows, "the combined wrapper must retain filtered-name lookup too")
}

func TestContractCoreBoundedFileForwardsScopeAndCapBeforeLimit(t *testing.T) {
	const path = "repo/shared.go"
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "a-foreign", Name: "foreign", Kind: graph.KindFunction, FilePath: path, RepoPrefix: "other", StartLine: 1, EndLine: 5},
		{ID: "b-owned", Name: "owned", Kind: graph.KindFunction, FilePath: path, RepoPrefix: "repo", StartLine: 1, EndLine: 5},
		{ID: "c-owned", Name: "owned2", Kind: graph.KindFunction, FilePath: path, RepoPrefix: "repo", StartLine: 6, EndLine: 10},
	}, nil)
	spy := &contractCoreFileSpy{Reader: base}
	reader := newContractCoreEdges(spy, t.Context(), nil).(graph.BoundedFileNodeReader)
	scope := graph.LocalizationNodeScope{RepoAllow: map[string]bool{"repo": true}, Kinds: map[graph.NodeKind]bool{graph.KindFunction: true}}
	page, err := reader.FindFileNodesBounded(t.Context(), path, scope, 1)
	require.NoError(t, err)
	require.Len(t, page.Nodes, 1)
	require.Equal(t, "b-owned", page.Nodes[0].ID)
	require.Equal(t, 2, page.Total)
	require.True(t, page.Truncated)
	require.Equal(t, t.Context(), spy.ctx)
	require.Equal(t, path, spy.path)
	require.Equal(t, scope, spy.scope)
	require.Equal(t, 1, spy.limit)
	require.Zero(t, spy.fullReads)
}

func TestContractCoreBoundedFileRejectsErrorsAndCancellation(t *testing.T) {
	backendErr := errors.New("bounded file read failed")
	for _, name := range []string{"error", "canceled-before", "canceled-during"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			spy := &contractCoreFileSpy{Reader: graph.New()}
			wantErr := error(context.Canceled)
			switch name {
			case "error":
				spy.err, wantErr = backendErr, backendErr
			case "canceled-before":
				cancel()
			case "canceled-during":
				spy.cancel = cancel
			}
			reader := newContractCoreEdges(spy, ctx, nil)
			page, err := reader.(graph.BoundedFileNodeReader).FindFileNodesBounded(ctx, "repo/a.go", graph.LocalizationNodeScope{}, 1)
			require.ErrorIs(t, err, wantErr)
			require.Equal(t, graph.BoundedNodeProjection{}, page)
			if name == "canceled-before" {
				require.Zero(t, spy.calls)
			}
			indexes := (&Server{}).buildFileSymbolIndexForOrderedPathsScopedReaderContext(ctx, reader, []string{"repo/a.go"}, query.QueryOptions{})
			require.True(t, indexes["repo/a.go"].saturated)
			require.Zero(t, spy.fullReads)
		})
	}
}
