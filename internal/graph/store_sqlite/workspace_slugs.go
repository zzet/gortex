package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.WorkspaceSlugBackfiller = (*Store)(nil)
var _ graph.WorkspaceSlugImpactBackfiller = (*Store)(nil)

const workspaceSlugChunkSize = 300 // 900 slug values plus two literals, below SQLite's conservative 999 limit.

// BackfillWorkspaceSlugs fills legacy empty boundary columns with set-oriented
// VALUES updates. It neither reads node rows nor rewrites Meta blobs.
func (s *Store) BackfillWorkspaceSlugs(slugs []graph.WorkspaceSlug) int {
	return s.BackfillWorkspaceSlugsWithImpact(slugs).Changed
}

// BackfillWorkspaceSlugsWithImpact preserves changed-row telemetry while also
// identifying fills that change the WorkspaceID fallback used by resolution.
func (s *Store) BackfillWorkspaceSlugsWithImpact(slugs []graph.WorkspaceSlug) graph.WorkspaceSlugBackfillResult {
	var backfill graph.WorkspaceSlugBackfillResult
	updates := make([]graph.WorkspaceSlug, 0, len(slugs))
	positions := make(map[string]int, len(slugs))
	for _, slug := range slugs {
		if slug.RepoPrefix == "" || (slug.Workspace == "" && slug.Project == "") {
			continue
		}
		if pos, ok := positions[slug.RepoPrefix]; ok {
			updates[pos] = slug
			continue
		}
		positions[slug.RepoPrefix] = len(updates)
		updates = append(updates, slug)
	}
	if len(updates) == 0 {
		return backfill
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		panicOnFatal(err)
		return backfill
	}
	for start := 0; start < len(updates); start += workspaceSlugChunkSize {
		end := minInt(start+workspaceSlugChunkSize, len(updates))
		chunk := updates[start:end]
		impactQuery, impactArgs := workspaceSlugResolutionImpact(s.viewGen, chunk)
		var resolutionAffected int
		if scanErr := tx.QueryRow(impactQuery, impactArgs...).Scan(&resolutionAffected); scanErr != nil {
			_ = tx.Rollback()
			panicOnFatal(scanErr)
			return graph.WorkspaceSlugBackfillResult{}
		}
		backfill.ResolutionAffected += resolutionAffected
		query, args := workspaceSlugUpdate(s.viewGen, chunk)
		result, execErr := tx.Exec(query, args...)
		if execErr != nil {
			_ = tx.Rollback()
			panicOnFatal(execErr)
			return graph.WorkspaceSlugBackfillResult{}
		}
		changed, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			_ = tx.Rollback()
			panicOnFatal(rowsErr)
			return graph.WorkspaceSlugBackfillResult{}
		}
		backfill.Changed += int(changed)
	}

	invalidatedAnalysis := false
	if backfill.Changed > 0 && s.analysisGenerationPresent {
		if err := invalidateAnalysisGenerationTx(tx); err != nil {
			_ = tx.Rollback()
			panicOnFatal(err)
			return graph.WorkspaceSlugBackfillResult{}
		}
		invalidatedAnalysis = true
	}
	if err := tx.Commit(); err != nil {
		panicOnFatal(err)
		return graph.WorkspaceSlugBackfillResult{}
	}
	if invalidatedAnalysis {
		s.analysisGenerationPresent = false
	}
	s.finishAnalysisMutationLocked(backfill.Changed > 0)
	return backfill
}

func workspaceSlugValues(slugs []graph.WorkspaceSlug) (string, []any) {
	var values strings.Builder
	values.Grow(len(slugs) * len("(?,?,?),"))
	args := make([]any, 0, len(slugs)*3)
	for i, slug := range slugs {
		if i > 0 {
			values.WriteByte(',')
		}
		values.WriteString("(?,?,?)")
		args = append(args, slug.RepoPrefix, slug.Workspace, slug.Project)
	}
	return values.String(), args
}

// The count and the update below share one generation predicate. The updates
// relation is a constant VALUES table with no generation of its own, so pairing
// means holding the joined node row to the writing handle's generation: a
// backfill through generation 1 must neither count nor fill generation 0's rows.
func workspaceSlugResolutionImpact(viewGen int64, slugs []graph.WorkspaceSlug) (string, []any) {
	values, args := workspaceSlugValues(slugs)
	// Builtin stubs are target-only synthetic sentinels. They are physically
	// stamped like every repository node, but the cross-repository resolver
	// never selects KindBuiltin as a function, method, file, or import target;
	// filling their boundary columns therefore cannot change resolution.
	args = append(args, graph.KindBuiltin, viewGen)
	query := `WITH updates(repo_prefix, workspace_id, project_id) AS (VALUES ` + values + `)
	SELECT COUNT(*)
	FROM nodes AS n
	JOIN updates AS u ON n.repo_prefix = u.repo_prefix
	WHERE n.workspace_id = ''
		AND u.workspace_id <> ''
		AND u.workspace_id <> n.repo_prefix
		AND n.kind <> ?
		AND n.view_gen = ?`
	return query, args
}

func workspaceSlugUpdate(viewGen int64, slugs []graph.WorkspaceSlug) (string, []any) {
	values, args := workspaceSlugValues(slugs)
	args = append(args, viewGen)
	query := `WITH updates(repo_prefix, workspace_id, project_id) AS (VALUES ` + values + `)
	UPDATE nodes AS n
	SET workspace_id = CASE
			WHEN n.workspace_id = '' AND u.workspace_id <> '' THEN u.workspace_id
			ELSE n.workspace_id
		END,
		project_id = CASE
			WHEN n.project_id = '' AND u.project_id <> '' THEN u.project_id
			ELSE n.project_id
		END
	FROM updates AS u
	WHERE n.repo_prefix = u.repo_prefix
		AND n.view_gen = ?
		AND ((n.workspace_id = '' AND u.workspace_id <> '')
			OR (n.project_id = '' AND u.project_id <> ''))`
	return query, args
}
