package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

type legacyContentMatchStore struct {
	graph.Store
	legacyCalls int
}

func (s *legacyContentMatchStore) FileMetasByPaths(repo string, paths []string) (map[string]graph.FileMetaRow, error) {
	s.legacyCalls++
	return s.Store.(graph.FileMetaPathReader).FileMetasByPaths(repo, paths)
}

type contextualContentMatchStore struct {
	*legacyContentMatchStore
	observed context.Context
	after    func()
	err      error
}

func (s *contextualContentMatchStore) FileMetasByPathsContext(ctx context.Context, repo string, paths []string) (map[string]graph.FileMetaRow, error) {
	s.observed = ctx
	rows, err := s.Store.(graph.FileMetaPathReader).FileMetasByPaths(repo, paths)
	if s.after != nil {
		s.after()
	}
	if s.err != nil {
		return nil, s.err
	}
	return rows, err
}

func TestIndexedContentMatchContextPreservesIdentityAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	plain := []byte("package main\n\nfunc A() {}\n")
	bom := append([]byte{0xef, 0xbb, 0xbf}, []byte("package main\n\nfunc B() {}\n")...)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), plain, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.go"), bom, 0o600))
	idx := newExtractSourceTestIndexer(t, config.Default().Index)
	_, err := idx.Index(root)
	require.NoError(t, err)
	legacy := &legacyContentMatchStore{Store: idx.graph}
	idx.graph = legacy
	require.True(t, idx.IndexedContentMatches("a.go", plain), "legacy callers retain their original metadata behavior")
	legacy.legacyCalls = 0
	match, err := idx.IndexedContentMatchesContext(t.Context(), "a.go", plain)
	require.NoError(t, err)
	require.False(t, match, "unsupported cancellable metadata cannot certify canonical content")
	require.Zero(t, legacy.legacyCalls, "never escape through an unbounded legacy metadata fallback")
	backend := &contextualContentMatchStore{legacyContentMatchStore: legacy}
	idx.graph = backend
	for _, test := range []struct {
		path string
		raw  []byte
		want bool
	}{{"a.go", plain, true}, {"b.go", bom, true}, {"a.go", []byte("package main\n\n\nfunc A() {}\n"), false}, {"missing.go", plain, false}} {
		match, err := idx.IndexedContentMatchesContext(t.Context(), test.path, test.raw)
		require.NoError(t, err)
		require.Equal(t, test.want, match, test.path)
		require.Same(t, t.Context(), backend.observed, "pass the caller's actual context without a reset")
	}
	backend.err = errors.New("metadata unavailable")
	match, err = idx.IndexedContentMatchesContext(t.Context(), "a.go", plain)
	require.NoError(t, err)
	require.False(t, match, "ordinary metadata failure retains unknown-match fallback")
	backend.err = nil
	caller, cancel := context.WithCancel(t.Context())
	backend.after = cancel // Real rows returned concurrently with cancellation.
	match, err = idx.IndexedContentMatchesContext(caller, "a.go", plain)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, match, "post-call cancellation cannot certify matching rows")
}
