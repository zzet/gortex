package store_sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestEnrichmentResumeRoundTripsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	require.NoError(t, err)

	_, found, err := s.GetEnrichmentResume("repo", "typescript-types")
	require.NoError(t, err)
	require.False(t, found, "no point recorded yet")

	point := graph.EnrichmentResumePoint{
		RepoPrefix: "repo", Provider: "typescript-types", IndexedSHA: "sha-1",
		Version: "engine-1", Phase: 3, After: "repo/src/m.ts", Covered: 4096, LedgerDigest: "ledger-1", UpdatedAt: 1_700_000_000,
	}
	require.NoError(t, s.SetEnrichmentResume(point))
	point.Phase, point.After = 1, "repo/src/a.ts" // upsert in place
	require.NoError(t, s.SetEnrichmentResume(point))
	require.NoError(t, s.SetEnrichmentResume(graph.EnrichmentResumePoint{
		RepoPrefix: "repo", Provider: "csharp-types", IndexedSHA: "sha-1", Phase: 2,
	}))
	require.NoError(t, s.Close())

	s, err = Open(path)
	require.NoError(t, err)
	defer s.Close()
	got, found, err := s.GetEnrichmentResume("repo", "typescript-types")
	require.NoError(t, err)
	require.True(t, found, "point must survive a reopen")
	require.Equal(t, point, got)

	require.NoError(t, s.ClearEnrichmentResume("repo", "typescript-types"))
	require.NoError(t, s.ClearEnrichmentResume("repo", "typescript-types"), "clearing a missing point is not an error")
	_, found, err = s.GetEnrichmentResume("repo", "typescript-types")
	require.NoError(t, err)
	require.False(t, found)
	other, found, err := s.GetEnrichmentResume("repo", "csharp-types")
	require.NoError(t, err)
	require.True(t, found, "another provider's point is its own row")
	require.Equal(t, 2, other.Phase)
}

func TestPurgeRepoDropsEnrichmentResume(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	defer s.Close()
	for _, repo := range []string{"gone", "kept"} {
		require.NoError(t, s.SetEnrichmentResume(graph.EnrichmentResumePoint{
			RepoPrefix: repo, Provider: "typescript-types", IndexedSHA: "sha", Phase: 1,
		}))
	}
	require.NoError(t, s.PurgeRepo("gone"))
	_, found, err := s.GetEnrichmentResume("gone", "typescript-types")
	require.NoError(t, err)
	require.False(t, found, "an untracked repo must not keep a resume point")
	_, found, err = s.GetEnrichmentResume("kept", "typescript-types")
	require.NoError(t, err)
	require.True(t, found)
}

// A store written before the table existed gains it on Open, in place and
// without a schema step, the way the other state tables were added.
func TestEnrichmentResumeTableAppearsOnUpgradedStore(t *testing.T) {
	for _, fixture := range loadUpgradeSchemaFixtures(t) {
		t.Run(fmt.Sprintf("v%d", fixture.version), func(t *testing.T) {
			path := buildUpgradeFixtureStore(t, fixture, upgradeFixtureSmallScale, t.TempDir())
			raw, err := sql.Open("sqlite", sqliteDSN(path, "mode=ro"))
			require.NoError(t, err)
			var tables int
			require.NoError(t, raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'enrichment_resume'`).Scan(&tables))
			require.NoError(t, raw.Close())
			require.Zero(t, tables, "fixture predates the table")

			s, err := Open(path)
			require.NoError(t, err)
			defer s.Close()
			require.False(t, s.NeedsRebuild())
			version, err := readUserVersion(s.writerDB)
			require.NoError(t, err)
			require.Equal(t, currentSchemaVersion, version)
			point := graph.EnrichmentResumePoint{RepoPrefix: "repo0", Provider: "typescript-types", IndexedSHA: "sha", Phase: 3, After: "repo0/pkg1/f1.go"}
			require.NoError(t, s.SetEnrichmentResume(point))
			got, found, err := s.GetEnrichmentResume("repo0", "typescript-types")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, point, got)
		})
	}
}

// A whole-repository eviction drops every enrichment edge and stamp, so it
// must take the repository's resume points with it, durably and on every
// eviction path a re-track uses. A file eviction (a re-parse) keeps them: the
// provider re-applies re-parsed files itself.
func TestRepoEvictionDropsEnrichmentResume(t *testing.T) {
	seed := func(t *testing.T, s *Store) {
		t.Helper()
		nodes := []*graph.Node{
			{ID: "gone/a.ts::A", Kind: graph.KindFunction, Name: "A", FilePath: "gone/a.ts", RepoPrefix: "gone", Language: "typescript"},
			{ID: "kept/b.ts::B", Kind: graph.KindFunction, Name: "B", FilePath: "kept/b.ts", RepoPrefix: "kept", Language: "typescript"},
		}
		s.AddBatch(nodes, nil)
		for _, repo := range []string{"gone", "kept"} {
			require.NoError(t, s.SetEnrichmentResume(graph.EnrichmentResumePoint{
				RepoPrefix: repo, Provider: "typescript-types", IndexedSHA: "sha", Phase: 1, After: repo + "/a.ts",
			}))
		}
	}
	has := func(t *testing.T, s *Store, repo string) bool {
		t.Helper()
		_, found, err := s.GetEnrichmentResume(repo, "typescript-types")
		require.NoError(t, err)
		return found
	}
	cases := []struct {
		name  string
		evict func(*testing.T, *Store)
	}{
		{"current generation", func(_ *testing.T, s *Store) { s.EvictRepo("gone") }},
		{"all generations", func(_ *testing.T, s *Store) { s.EvictRepoAllGenerations("gone") }},
		{"shadow replacement", func(t *testing.T, s *Store) {
			_, _, err := s.EvictRepoForShadowReplacement(t.Context(), "gone")
			require.NoError(t, err)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.sqlite")
			s, err := Open(path)
			require.NoError(t, err)
			seed(t, s)
			tc.evict(t, s)
			require.NoError(t, s.Close())
			s, err = Open(path)
			require.NoError(t, err)
			defer s.Close()
			require.False(t, has(t, s, "gone"), "an evicted repository must not keep a resume point")
			require.True(t, has(t, s, "kept"), "another repository's point is untouched")
		})
	}
	t.Run("file eviction keeps it", func(t *testing.T) {
		s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
		require.NoError(t, err)
		defer s.Close()
		seed(t, s)
		s.EvictFile("gone/a.ts")
		require.True(t, has(t, s, "gone"))
	})
}
