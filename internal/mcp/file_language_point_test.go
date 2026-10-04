package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

type fileLanguagePointSpy struct {
	graph.Store
	ids              []string
	err              error
	scans, adjacency int
}

func (r *fileLanguagePointSpy) GetNodeContext(ctx context.Context, id string) (*graph.Node, error) {
	r.ids = append(r.ids, id)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.Store.GetNode(id), r.err
}
func (r *fileLanguagePointSpy) GetFileNodes(string) []*graph.Node { r.scans++; return nil }
func (r *fileLanguagePointSpy) AllNodes() []*graph.Node           { r.scans++; return nil }
func (r *fileLanguagePointSpy) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	r.adjacency++
	return nil
}
func (r *fileLanguagePointSpy) GetInEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	r.adjacency++
	return nil
}

func newFileLanguageServer(reader graph.Store) *Server {
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := indexer.New(graph.New(), reg, config.Default().Index, zap.NewNop())
	return &Server{graph: reader, indexer: idx}
}

func TestDetectFileLanguageUsesOnlySelectedCanonicalPoint(t *testing.T) {
	g := graph.New()
	path := "repo/file.go"
	g.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo", Language: "indexed-go"})
	spy := &fileLanguagePointSpy{Store: g}
	s := newFileLanguageServer(spy)
	require.Equal(t, "indexed-go", s.detectLanguageForPath(t.Context(), filepath.Join(t.TempDir(), "file.go"), path))
	require.Equal(t, []string{path}, spy.ids)
	require.Zero(t, spy.scans)
	require.Zero(t, spy.adjacency)
}

func TestDetectFileLanguageSelectedReplacementAndTombstone(t *testing.T) {
	const path = "repo/file.go"
	base := graph.New()
	base.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo", Language: "old-language"})
	replacement := graph.NewOverlayLayer()
	replacement.AddNode(path, &graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo", Language: "selected-language"})
	selected := graph.NewOverlaidViewWithLayer(base, replacement)
	s := newFileLanguageServer(base)
	ctx := context.WithValue(t.Context(), requestViewCtxKey{}, &requestView{reader: selected})
	abs := filepath.Join(t.TempDir(), "file.go")
	require.NoError(t, os.WriteFile(abs, []byte("package sample\n"), 0o600))
	require.Equal(t, "selected-language", s.detectLanguageForPath(ctx, abs, path))
	deleted := graph.NewOverlayLayer()
	deleted.MarkFile(path, true)
	ctx = context.WithValue(t.Context(), requestViewCtxKey{}, &requestView{reader: graph.NewOverlaidViewWithLayer(selected, deleted)})
	require.Equal(t, "go", s.detectLanguageForPath(ctx, abs, path), "a tombstone must not inherit the old indexed language")
}

func TestDetectFileLanguageRegistryFallbackAndSourceBypass(t *testing.T) {
	const path = "repo/file.go"
	for _, mode := range []string{"legacy_id", "wrong_kind", "wrong_path", "read_error", "source_only", "wrong_repo"} {
		t.Run(mode, func(t *testing.T) {
			g := graph.New()
			n := &graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo", Language: "stale-language"}
			switch mode {
			case "legacy_id":
				n.ID = "custom-file-id"
			case "wrong_kind":
				n.Kind = graph.KindFunction
			case "wrong_path":
				n.FilePath = "repo/other.go"
			case "wrong_repo":
				n.RepoPrefix = "other"
			}
			g.AddNode(n)
			spy := &fileLanguagePointSpy{Store: g}
			if mode == "read_error" {
				spy.err = errors.New("checked lookup failed")
			}
			s := newFileLanguageServer(spy)
			ctx := t.Context()
			if mode == "source_only" {
				ctx = context.WithValue(ctx, requestViewCtxKey{}, &requestView{sourceScope: "file"})
			}
			if mode == "wrong_repo" {
				ctx = context.WithValue(ctx, requestViewCtxKey{}, &requestView{reader: &baseGraphReader{base: spy, repoPrefix: "repo"}})
			}
			abs := filepath.Join(t.TempDir(), "file.go")
			require.NoError(t, os.WriteFile(abs, []byte("package sample\n"), 0o600))
			require.Equal(t, "go", s.detectLanguageForPath(ctx, abs, path))
			require.Zero(t, spy.scans)
			require.Zero(t, spy.adjacency)
			if mode == "source_only" {
				require.Empty(t, spy.ids)
			}
			require.NoError(t, contractCoreReadError(ctx), "bookkeeping failures must not overwrite a successful tool response")
		})
	}
}
