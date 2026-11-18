package store_sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A store file that does not exist yet reads back as an empty map, not an
// error — "nothing indexed", so callers fall back to other sources.
func TestReadRepoIndexStates_MissingFile(t *testing.T) {
	got, err := store_sqlite.ReadRepoIndexStates(filepath.Join(t.TempDir(), "absent.sqlite"))
	require.NoError(t, err)
	require.Empty(t, got)
}

// The read-only reader returns exactly the rows SetRepoIndexState wrote,
// keyed by repo prefix, including the empty (lone-repo) prefix.
func TestReadRepoIndexStates_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "is.sqlite")

	s, err := store_sqlite.Open(path)
	require.NoError(t, err)
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "alpha", IndexedSHA: "aaa111", Dirty: false, IndexedAt: 1700000000,
		WorkspaceFP: "fp-a", NodeCount: 10, EdgeCount: 20, ExtractorVersions: `{"go":1}`,
	}))
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "", IndexedSHA: "bbb222", Dirty: true, IndexedAt: 1700000001,
	}))
	require.NoError(t, s.Close())

	got, err := store_sqlite.ReadRepoIndexStates(path)
	require.NoError(t, err)
	require.Len(t, got, 2)

	alpha, ok := got["alpha"]
	require.True(t, ok)
	require.Equal(t, "aaa111", alpha.IndexedSHA)
	require.False(t, alpha.Dirty)
	require.EqualValues(t, 1700000000, alpha.IndexedAt)
	require.Equal(t, 10, alpha.NodeCount)

	lone, ok := got[""]
	require.True(t, ok)
	require.Equal(t, "bbb222", lone.IndexedSHA)
	require.True(t, lone.Dirty)
}

// Reading is non-destructive and repeatable while the underlying store is
// reopened for writing — the read path must never lock the writer out.
func TestReadRepoIndexStates_ConcurrentWithOpenStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "is.sqlite")

	s, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "live", IndexedSHA: "c0ffee", IndexedAt: 1700000002,
	}))

	// Read while the writer store is still open (WAL allows concurrent readers).
	got, err := store_sqlite.ReadRepoIndexStates(path)
	require.NoError(t, err)
	require.Equal(t, "c0ffee", got["live"].IndexedSHA)

	// A subsequent write is still possible — the reader did not wedge the writer.
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "live", IndexedSHA: "feedface", IndexedAt: 1700000003,
	}))
	got, err = store_sqlite.ReadRepoIndexStates(path)
	require.NoError(t, err)
	require.Equal(t, "feedface", got["live"].IndexedSHA)
}

// TestReadRepoIndexStates_IsBaseOnly pins the declared scope of this door: it
// reads the base generation and nothing else.
//
// The predicate was a literal `view_gen = 0` in the SQL; it is now bound from
// RepoIndexStateBaseViewGen, which is the constant `gortex repos` declares its
// output scope with. A reader that started returning derived rows would make
// that declaration a lie, and this is the test that notices.
func TestReadRepoIndexStates_IsBaseOnly(t *testing.T) {
	require.Equal(t, 0, store_sqlite.RepoIndexStateBaseViewGen,
		"the base generation is zero everywhere else in the store")

	path := filepath.Join(t.TempDir(), "scoped.sqlite")
	st, err := store_sqlite.Open(path)
	require.NoError(t, err)
	require.NoError(t, st.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "base-repo",
		IndexedSHA: "aaaa",
		IndexedAt:  100,
	}))
	require.NoError(t, st.Close())

	// A derived generation's row, written the way a routed worktree's build
	// stamps one: same table, same shape, a non-base view generation.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`
INSERT INTO repo_index_state
  (view_gen, repo_prefix, indexed_sha, dirty, indexed_at, workspace_fp, node_count, edge_count, extractor_versions)
VALUES (7, 'derived-repo', 'bbbb', 0, 200, '', 0, 0, '')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	got, err := store_sqlite.ReadRepoIndexStates(path)
	require.NoError(t, err)
	require.Contains(t, got, "base-repo", "the base row is what this door serves")
	require.NotContains(t, got, "derived-repo",
		"a derived generation's freshness leaked through a door that declares itself base-only")
}
