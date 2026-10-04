package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type contractCoreLookupSpy struct {
	graph.Reader
	calls, legacyCalls int
	limit              int
	seenContext        context.Context
	lookupError        error
	cancel             context.CancelFunc
}

func (s *contractCoreLookupSpy) begin(ctx context.Context) error {
	s.calls++
	s.seenContext = ctx
	if s.cancel != nil {
		s.cancel()
	}
	return s.lookupError
}
func (s *contractCoreLookupSpy) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	s.legacyCalls++
	return s.Reader.FindNodesByNameContaining(substr, limit)
}
func (s *contractCoreLookupSpy) FindNodesByNameContainingFilteredContext(ctx context.Context, substr string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	s.limit = limit
	return graph.FindNodesByNameContainingFilteredContext(ctx, s.Reader, substr, limit, filter)
}
func (s *contractCoreLookupSpy) FindNodesByNameContext(ctx context.Context, name string) ([]*graph.Node, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	return s.FindNodesByName(name), nil
}
func (s *contractCoreLookupSpy) FindNodesByNameContainingContext(ctx context.Context, name string, limit int) ([]*graph.Node, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	s.limit = limit
	return s.Reader.FindNodesByNameContaining(name, limit), nil
}
func (s *contractCoreLookupSpy) GetNodeContext(ctx context.Context, id string) (*graph.Node, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	return s.GetNode(id), nil
}
func (s *contractCoreLookupSpy) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	return s.GetNodesByIDs(ids), nil
}

func TestContractCoreLookupPreservesBoundedSelectedNameCapability(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "repo/edit.go::Old", Name: "Handler", Kind: graph.KindFunction, FilePath: "repo/edit.go", RepoPrefix: "repo"},
		{ID: "foreign", Name: "Handler", Kind: graph.KindFunction, FilePath: "other/a.go", RepoPrefix: "other"},
		{ID: "contract", Name: "Handler", Kind: graph.KindContract, RepoPrefix: "repo"},
	}, nil)
	layer := graph.NewOverlayLayer()
	current := &graph.Node{ID: "repo/edit.go::Current", Name: "Handler", Kind: graph.KindFunction, FilePath: "repo/edit.go", RepoPrefix: "repo"}
	layer.AddNode(current.FilePath, current)
	selected := graph.NewOverlaidViewWithLayer(base, layer)
	spy := &contractCoreLookupSpy{Reader: selected}
	reader := newContractCoreEdges(spy, t.Context(), nil)
	ctx := t.Context()
	filter := graph.NameSearchFilter{RepoAllow: map[string]bool{"repo": true}, Accept: func(n *graph.Node) bool { return n.Kind == graph.KindFunction }}
	rows, err := graph.FindNodesByNameContainingFilteredContext(ctx, reader, "Handler", 1, filter)
	require.NoError(t, err)
	require.Equal(t, []*graph.Node{current}, rows)
	require.Equal(t, 1, spy.calls)
	require.Equal(t, 1, spy.limit, "filter and candidate budget reach the selected compact reader")
	require.Equal(t, 0, spy.legacyCalls, "wrapper must not trigger the unlimited contextless fallback")
	require.Equal(t, ctx, spy.seenContext)
	rows, err = graph.FindNodesByNameContext(ctx, reader, "Handler")
	require.NoError(t, err)
	for _, row := range rows {
		require.NotEqual(t, "repo/edit.go::Old", row.ID, "selected file replacement is authoritative")
	}
	checked := reader.(interface {
		GetNodeContext(context.Context, string) (*graph.Node, error)
		GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error)
	})
	node, err := checked.GetNodeContext(ctx, "repo/edit.go::Current")
	require.NoError(t, err)
	require.Same(t, current, node)
	node, err = checked.GetNodeContext(ctx, "repo/edit.go::Old")
	require.NoError(t, err)
	require.Nil(t, node)
	nodes, err := checked.GetNodesByIDsContext(ctx, []string{"repo/edit.go::Old", "repo/edit.go::Current"})
	require.NoError(t, err)
	require.Equal(t, map[string]*graph.Node{"repo/edit.go::Current": current}, nodes)
	_, fastBFS := any(reader).(graph.BFSCapable)
	_, fastFrontier := any(reader).(graph.FrontierExpander)
	require.False(t, fastBFS)
	require.False(t, fastFrontier)
}

func TestContractCoreLookupCancellationAndCheckedErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		read func(context.Context, graph.Reader) error
	}{
		{"exact", func(ctx context.Context, r graph.Reader) error {
			_, err := graph.FindNodesByNameContext(ctx, r, "Handler")
			return err
		}},
		{"containing", func(ctx context.Context, r graph.Reader) error {
			_, err := graph.FindNodesByNameContainingContext(ctx, r, "Handle", 1)
			return err
		}},
		{"filtered", func(ctx context.Context, r graph.Reader) error {
			_, err := graph.FindNodesByNameContainingFilteredContext(ctx, r, "Handle", 1, graph.NameSearchFilter{})
			return err
		}},
		{"node", func(ctx context.Context, r graph.Reader) error {
			_, err := r.(interface {
				GetNodeContext(context.Context, string) (*graph.Node, error)
			}).GetNodeContext(ctx, "handler")
			return err
		}},
		{"nodes", func(ctx context.Context, r graph.Reader) error {
			_, err := r.(interface {
				GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error)
			}).GetNodesByIDsContext(ctx, []string{"handler"})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			spy := &contractCoreLookupSpy{Reader: graph.New()}
			reader := newContractCoreEdges(spy, t.Context(), nil)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			require.ErrorIs(t, test.read(ctx, reader), context.Canceled)
			require.Zero(t, spy.calls)
			sentinel := errors.New("selected read failed")
			spy.lookupError = sentinel
			require.ErrorIs(t, test.read(t.Context(), reader), sentinel)
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			spy.lookupError, spy.cancel = nil, cancel
			require.ErrorIs(t, test.read(ctx, reader), context.Canceled)
		})
	}
}

type contractCoreLegacyNameSpy struct {
	graph.Reader
	limits []int
}

func (s *contractCoreLegacyNameSpy) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	s.limits = append(s.limits, limit)
	return s.Reader.FindNodesByNameContaining(substr, limit)
}

func TestContractCoreLookupLegacyOverlayKeepsBoundedFetch(t *testing.T) {
	base := graph.New()
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		base.AddNode(&graph.Node{ID: id, Name: "Handler" + id, Kind: graph.KindFunction, RepoPrefix: "repo"})
	}
	spy := &contractCoreLegacyNameSpy{Reader: base}
	reader := newContractCoreEdges(spy, t.Context(), nil)
	_, compact := reader.(graph.FilteredContainingNameReader)
	require.False(t, compact, "a legacy base must retain its outer overlay fetch policy")
	selected := graph.NewOverlaidViewWithLayer(reader, graph.NewOverlayLayer())
	rows, err := graph.FindNodesByNameContainingFilteredContext(t.Context(), selected, "Handler", 3, graph.NameSearchFilter{
		Accept: func(n *graph.Node) bool { return n.Kind == graph.KindFunction },
	})
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.Equal(t, []int{6}, spy.limits, "legacy candidates stay bounded rather than fetching every match")
}
