package mcp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

type projectionBundleStore struct {
	graph.Store
	hydrated []string
}

func (s *projectionBundleStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.hydrated = append(s.hydrated, ids...)
	return s.Store.GetNodesByIDs(ids)
}

func TestProjectedSearchLayersOwnOnlyCoveredBundleHits(t *testing.T) {
	for _, contract := range []bool{false, true} {
		for _, editor := range []bool{false, true} {
			name := "base"
			if contract {
				name = "contract"
			}
			if editor {
				name += "_editor"
			}
			t.Run(name, func(t *testing.T) {
				srv, dir := setupTestServer(t)
				old := srv.graph.GetNode("main.go::helper")
				require.NotNil(t, old)
				untouched := &graph.Node{ID: "unrelated.go::helperOther", Name: "helperOther", Kind: graph.KindFunction, FilePath: "unrelated.go", StartLine: 3}
				srv.graph.AddNode(untouched)
				backend := &searchSymbolsContextBackend{bundles: []search.SymbolBundle{{Node: old, Score: 10}, {Node: untouched, Score: 9}}}
				srv.engine.SetSearch(backend)
				store := &projectionBundleStore{Store: srv.graph}
				srv.graph = store
				if contract {
					srv.contractAnalysisRuntime = &ContractAnalysisRuntime{}
				}
				path := filepath.Join(dir, "main.go")
				original, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, append([]byte("\n\n\n"), original...), 0o644))
				ctx := t.Context()
				var buffer *graph.Node
				if editor {
					layer := graph.NewOverlayLayer()
					// An unrelated editor keeps its own top layer while this
					// request still needs a disk projection for main.go.
					buffer = &graph.Node{ID: "buffer.go::BufferOnly", Name: "BufferOnly", Kind: graph.KindFunction, FilePath: "buffer.go", StartLine: 999}
					layer.MarkFile(buffer.FilePath, false)
					layer.AddNode(buffer.FilePath, buffer)
					ctx = WithOverlayView(ctx, graph.NewOverlaidView(store, layer))
				}
				ctx, err = srv.ensureProjected(ctx, []string{"main.go"})
				require.NoError(t, err)
				projections := requestProjectionLayers(ctx)
				require.Len(t, projections, 1)
				get := func(ctx context.Context) []*rerank.Candidate {
					return srv.engineFor(ctx).GatherSymbolCandidates("helper", 5, query.QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, nil)
				}
				got := get(ctx)
				byID := map[string]*graph.Node{}
				for _, candidate := range got {
					byID[candidate.Node.ID] = candidate.Node
				}
				require.NotNil(t, byID[old.ID])
				require.Equal(t, old.StartLine+3, byID[old.ID].StartLine, "stale base bundle must hydrate from current disk projection")
				if editor {
					require.Same(t, buffer, srv.readerFor(ctx).GetNode(buffer.ID), "unrelated pinned editor remains above projection")
				}
				require.Same(t, untouched, byID[untouched.ID], "unrelated bundle payload remains reusable")
				require.False(t, slices.Contains(store.hydrated, untouched.ID), "one pending file cannot invalidate unrelated bundle hydration")
				layers := srv.requestLayers(ctx)
				require.True(t, slices.Contains(layers, projections[0]), "request ownership omitted its immutable disk projection")
				if editor {
					require.Same(t, OverlayViewFromContext(ctx).LayerReader(), layers[len(layers)-1], "buffer must compose above projection")
				}
				require.Equal(t, old.StartLine, store.Store.GetNode(old.ID).StartLine, "readthrough must not publish into base")
			})
		}
	}
}

func TestProjectedSearchOwnershipRetainsImmutableSnapshotVersions(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := filepath.Join(dir, "main.go")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	old := srv.graph.GetNode("main.go::helper")
	require.NotNil(t, old)
	backend := &searchSymbolsContextBackend{bundles: []search.SymbolBundle{{Node: old, Score: 10}}}
	srv.engine.SetSearch(backend)
	require.NoError(t, os.WriteFile(path, append([]byte("\n\n"), original...), 0o644))
	first, err := srv.ensureProjected(t.Context(), []string{"main.go"})
	require.NoError(t, err)
	sourceEntry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, sourceEntry)
	before := parser.ParseCount()
	repeated, err := srv.ensureProjected(t.Context(), []string{"main.go"})
	require.NoError(t, err)
	firstLayers := srv.requestLayers(first)
	repeatedLayers := srv.requestLayers(repeated)
	require.Len(t, firstLayers, 1, "projection ownership must be attached even without an editor")
	require.Len(t, repeatedLayers, 1)
	require.Equal(t, before, parser.ParseCount(), "an unchanged immutable source snapshot is not parsed again")
	require.Same(t, sourceEntry, liveEntry(pendingFileProjections, path), "reuse the immutable registry entry, even when request-local carried-edge wrappers differ")
	require.Same(t, srv.readerFor(first).GetNode(old.ID), srv.readerFor(repeated).GetNode(old.ID), "both visibility wrappers borrow exactly the same immutable nodes")
	require.NoError(t, os.WriteFile(path, append([]byte("\n\n\n\n"), original...), 0o644))
	newer, err := srv.ensureProjected(t.Context(), []string{"main.go"})
	require.NoError(t, err)
	newerLayers := srv.requestLayers(newer)
	require.Len(t, newerLayers, 1)
	require.NotSame(t, firstLayers[0], newerLayers[0], "new bytes require a new immutable snapshot version")
	require.NotSame(t, sourceEntry, liveEntry(pendingFileProjections, path), "new bytes must have a distinct source owner, not just a new visibility wrapper")
	for _, item := range []struct {
		ctx   context.Context
		shift int
	}{{first, 2}, {newer, 4}, {first, 2}} {
		got := srv.engineFor(item.ctx).GatherSymbolCandidates("helper", 5, query.QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, nil)
		require.Len(t, got, 1)
		require.Equal(t, old.StartLine+item.shift, got[0].Node.StartLine, "new publication must not rewrite an already pinned request")
	}
}
