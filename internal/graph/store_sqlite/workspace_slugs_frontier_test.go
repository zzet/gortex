package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestWorkspaceSlugFrontierPreservesNeutralFills(t *testing.T) {
	for _, tc := range []struct {
		name      string
		node      graph.Node
		slug      graph.WorkspaceSlug
		workspace string
		project   string
	}{
		{
			name:      "builtin only",
			node:      graph.Node{ID: "repo::builtin", Kind: graph.KindBuiltin, RepoPrefix: "repo"},
			slug:      graph.WorkspaceSlug{RepoPrefix: "repo", Workspace: "shared", Project: "project"},
			workspace: "shared", project: "project",
		},
		{
			name:      "project only configuration",
			node:      graph.Node{ID: "repo::fn", Kind: graph.KindFunction, RepoPrefix: "repo"},
			slug:      graph.WorkspaceSlug{RepoPrefix: "repo", Project: "project"},
			workspace: "", project: "project",
		},
		{
			name:      "existing workspace",
			node:      graph.Node{ID: "repo::fn", Kind: graph.KindFunction, RepoPrefix: "repo", WorkspaceID: "existing"},
			slug:      graph.WorkspaceSlug{RepoPrefix: "repo", Workspace: "different", Project: "project"},
			workspace: "existing", project: "project",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			s.AddBatch([]*graph.Node{&tc.node}, nil)
			before := s.AnalysisMutationRevision()
			require.Equal(t, graph.WorkspaceSlugBackfillResult{Changed: 1}, s.BackfillWorkspaceSlugsWithImpact([]graph.WorkspaceSlug{tc.slug}))
			require.Equal(t, before+1, s.AnalysisMutationRevision())
			node := s.GetNode(tc.node.ID)
			require.NotNil(t, node)
			require.Equal(t, tc.workspace, node.WorkspaceID)
			require.Equal(t, tc.project, node.ProjectID)
			require.Equal(t, graph.WorkspaceSlugBackfillResult{}, s.BackfillWorkspaceSlugsWithImpact([]graph.WorkspaceSlug{tc.slug}))
			require.Equal(t, before+1, s.AnalysisMutationRevision())
		})
	}
}

func TestWorkspaceSlugFrontierTracksConfigurationAndMutations(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.AddBatch([]*graph.Node{{ID: "repo::first", Kind: graph.KindFunction, RepoPrefix: "repo"}}, nil)
	empty := []graph.WorkspaceSlug{{RepoPrefix: "repo"}}
	before := s.AnalysisMutationRevision()
	require.Equal(t, graph.WorkspaceSlugBackfillResult{}, s.BackfillWorkspaceSlugsWithImpact(empty))
	require.Equal(t, before, s.AnalysisMutationRevision())
	slugs := []graph.WorkspaceSlug{{RepoPrefix: "repo", Workspace: "shared", Project: "project"}}
	require.Equal(t, graph.WorkspaceSlugBackfillResult{Changed: 1, ResolutionAffected: 1}, s.BackfillWorkspaceSlugsWithImpact(slugs))
	require.False(t, workspaceFrontierHasCandidates(t, s, 0, slugs))
	s.AddBatch([]*graph.Node{{ID: "repo::later", Kind: graph.KindBuiltin, RepoPrefix: "repo"}}, nil)
	require.True(t, workspaceFrontierHasCandidates(t, s, 0, slugs))
	require.Equal(t, graph.WorkspaceSlugBackfillResult{Changed: 1}, s.BackfillWorkspaceSlugsWithImpact(slugs))
	// Any writer that reintroduces a missing column must reopen eligibility;
	// a separately maintained Boolean would miss this mutation.
	tx, err := s.beginWrite()
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE nodes SET project_id = '' WHERE view_gen = 0 AND id = ?`, "repo::first")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.True(t, workspaceFrontierHasCandidates(t, s, 0, slugs))
	require.Equal(t, graph.WorkspaceSlugBackfillResult{Changed: 1}, s.BackfillWorkspaceSlugsWithImpact(slugs))
	require.False(t, workspaceFrontierHasCandidates(t, s, 0, slugs))
}

func TestWorkspaceSlugFrontierIsGenerationScopedAndIndexed(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.AddBatch([]*graph.Node{
		{ID: "repo::complete", Kind: graph.KindFunction, RepoPrefix: "repo", WorkspaceID: "shared", ProjectID: "project"},
		{ID: "repo::other-generation", Kind: graph.KindFunction, RepoPrefix: "repo"},
		{ID: "sibling::missing", Kind: graph.KindFunction, RepoPrefix: "sibling"},
	}, nil)
	tx, err := s.beginWrite()
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE nodes SET view_gen = 7 WHERE view_gen = 0 AND id = ?`, "repo::other-generation")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	slugs := []graph.WorkspaceSlug{{RepoPrefix: "repo", Workspace: "shared", Project: "project"}}
	require.False(t, workspaceFrontierHasCandidates(t, s, 0, slugs))
	require.True(t, workspaceFrontierHasCandidates(t, s, 7, slugs))
	require.Equal(t, graph.WorkspaceSlugBackfillResult{}, s.BackfillWorkspaceSlugsWithImpact(slugs))
	require.Equal(t, "", s.GetNode("sibling::missing").WorkspaceID)
	query, args := workspaceSlugCandidates(0, slugs)
	tx, err = s.beginWrite()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	joined := strings.Join(plan, "\n")
	require.Contains(t, joined, "SEARCH n USING INDEX nodes_missing_workspace_slugs")
	require.NotContains(t, joined, "SCAN n ")
}

func workspaceFrontierHasCandidates(t *testing.T, s *Store, generation int64, slugs []graph.WorkspaceSlug) bool {
	t.Helper()
	tx, err := s.beginWrite()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	query, args := workspaceSlugCandidates(generation, slugs)
	var found bool
	require.NoError(t, tx.QueryRow(query, args...).Scan(&found))
	return found
}

func TestWorkspaceSlugFrontierSurvivesBulkAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		// Open may return nil on a failed reopen after the first store closed.
		if s != nil {
			require.NoError(t, s.Close())
		}
	})
	slugs := []graph.WorkspaceSlug{{RepoPrefix: "repo", Workspace: "shared", Project: "project"}}
	require.True(t, s.BeginCoordinatedBulkLoad())
	s.AddBatch([]*graph.Node{{ID: "repo::fn", Kind: graph.KindFunction, RepoPrefix: "repo"}}, nil)
	require.True(t, workspaceFrontierHasCandidates(t, s, 0, slugs))
	require.Equal(t, graph.WorkspaceSlugBackfillResult{Changed: 1, ResolutionAffected: 1}, s.BackfillWorkspaceSlugsWithImpact(slugs))
	require.False(t, workspaceFrontierHasCandidates(t, s, 0, slugs))
	require.NoError(t, s.EndCoordinatedBulkLoad())
	require.NoError(t, s.Close())
	s, err = Open(path)
	require.NoError(t, err)
	require.False(t, workspaceFrontierHasCandidates(t, s, 0, slugs))
	s.AddBatch([]*graph.Node{{ID: "repo::later", Kind: graph.KindFunction, RepoPrefix: "repo"}}, nil)
	require.True(t, workspaceFrontierHasCandidates(t, s, 0, slugs))
}

func BenchmarkWorkspaceSlugNoopBackfill(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "graph.sqlite"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	var nodes []*graph.Node
	var slugs []graph.WorkspaceSlug
	for repo := 0; repo < 10; repo++ {
		prefix := fmt.Sprintf("repo%d", repo)
		slugs = append(slugs, graph.WorkspaceSlug{RepoPrefix: prefix, Workspace: "shared", Project: "project"})
		for n := 0; n < 1000; n++ {
			nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s::%d", prefix, n), Kind: graph.KindFunction, RepoPrefix: prefix, WorkspaceID: "shared", ProjectID: "project"})
		}
	}
	s.AddBatch(nodes, nil)
	b.Run("frontier", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got := s.BackfillWorkspaceSlugsWithImpact(slugs); got != (graph.WorkspaceSlugBackfillResult{}) {
				b.Fatalf("unexpected mutation: %+v", got)
			}
		}
	})
	// Retain both historical queries on the same indexed store, so this
	// isolates the gate's savings rather than attributing index gains twice.
	b.Run("legacy_count_and_update", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			legacy := func() error {
				s.writeMu.Lock()
				defer s.writeMu.Unlock()
				tx, err := s.beginWrite()
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				impactQuery, impactArgs := workspaceSlugResolutionImpact(0, slugs)
				updateQuery, updateArgs := workspaceSlugUpdate(0, slugs)
				var affected int
				if err := tx.QueryRow(impactQuery, impactArgs...).Scan(&affected); err != nil {
					return err
				}
				result, err := tx.Exec(updateQuery, updateArgs...)
				if err != nil {
					return err
				}
				changed, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if affected != 0 || changed != 0 {
					return fmt.Errorf("unexpected mutation: affected=%d changed=%d", affected, changed)
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				s.finishAnalysisMutationLocked(false)
				return nil
			}
			if err := legacy(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
