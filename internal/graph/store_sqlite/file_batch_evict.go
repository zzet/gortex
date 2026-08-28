package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

const (
	evictFilePredicate         = `file_path = ?`
	evictFilesPredicate        = `file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	evictRepoPredicate         = `repo_prefix = ?`
	evictNonEmptyRepoPredicate = `repo_prefix = ? AND repo_prefix <> ''`
)

// evictScope selects whether an eviction is bound to the handle's payload view
// generation. File replacement and ordinary repository reindexing retire only
// rows written through the calling handle. Authoritative untrack is the sole
// repository path that selects every generation, matching store_purge.go.
type evictScope bool

const (
	evictThisGeneration evictScope = true
	evictAllGenerations evictScope = false
)

// EvictFiles removes a bounded file replacement set in one transaction. The
// two edge deletes deliberately use indexed from_id/to_id predicates instead
// of an OR expression, and keep the candidate node set inside SQLite rather
// than materialising every node ID in Go.
func (s *Store) EvictFiles(filePaths []string) (nodesRemoved, edgesRemoved int) {
	paths := make([]string, 0, len(filePaths))
	seen := make(map[string]struct{}, len(filePaths))
	for _, filePath := range filePaths {
		if filePath == "" {
			continue
		}
		if _, duplicate := seen[filePath]; duplicate {
			continue
		}
		seen[filePath] = struct{}{}
		paths = append(paths, filePath)
	}
	if len(paths) == 0 {
		return 0, 0
	}
	pathsJSON, ok := projectionJSON(paths)
	if !ok {
		return 0, 0
	}
	return s.evictByPredicate(evictFilesPredicate, pathsJSON, evictThisGeneration)
}

// evictByPredicate is the common SQLite-native scope eviction path. The
// predicate is always one of the package constants above, never caller SQL.
func (s *Store) evictByPredicate(predicate string, arg any, scope evictScope) (nodesRemoved, edgesRemoved int) {
	nodesRemoved, edgesRemoved, err := s.evictByPredicateResult(predicate, arg, scope)
	if err != nil {
		panicOnFatal(err)
		return 0, 0
	}
	return nodesRemoved, edgesRemoved
}

// evictByPredicateResult keeps the entire binding/edge/node change in one
// IMMEDIATE transaction. Candidate node IDs remain in SQLite: the two indexed
// edge deletes consume the same predicate subquery directly, so scope size
// never creates a Go ID frontier or a DELETE-per-node loop.
func (s *Store) evictByPredicateResult(predicate string, arg any, scope evictScope) (nodesRemoved, edgesRemoved int, retErr error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	ctx := context.Background()
	// A generation-scoped eviction appends the residual conjunct to every
	// predicate and binds the handle's generation once per placeholder. A
	// repo-administration eviction leaves all three statements byte-identical
	// to what they were before generations existed, so it still reaches every
	// generation's rows in one pass.
	// The edge delete carries the conjunct twice: once inside the candidate-node
	// subquery and once on the edge row itself, so an edge cannot be dragged out
	// of another generation by a node id this one also uses.
	scoped, scopeArgs := predicate, []any{arg}
	edgeScope, edgeArgs := "", []any{arg}
	if scope == evictThisGeneration {
		scoped += ` AND view_gen = ?`
		scopeArgs = append(scopeArgs, s.viewGen)
		edgeScope = ` AND view_gen = ?`
		edgeArgs = append(edgeArgs, s.viewGen, s.viewGen)
	}
	// The binding sidecar follows the node/edge scope: an unscoped sweep would
	// strand bindings against nodes that no longer exist, and a scoped one must
	// not touch another generation's bindings for the same path.
	if _, err := tx.ExecContext(ctx, `DELETE FROM semantic_binding_types WHERE `+scoped, scopeArgs...); err != nil {
		return 0, 0, err
	}
	scopedNodes := `SELECT id FROM nodes WHERE ` + scoped
	for _, column := range []string{"from_id", "to_id"} {
		result, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE `+column+` IN (`+scopedNodes+`)`+edgeScope, edgeArgs...)
		if err != nil {
			return 0, 0, err
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		edgesRemoved += int(removed)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE `+scoped, scopeArgs...)
	if err != nil {
		return 0, 0, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	nodesRemoved = int(removed)
	changed := nodesRemoved > 0 || edgesRemoved > 0
	invalidatedAnalysis := false
	if changed && s.analysisGenerationPresent {
		if err := invalidateAnalysisGenerationTx(tx); err != nil {
			return 0, 0, err
		}
		invalidatedAnalysis = true
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}

	if invalidatedAnalysis {
		s.analysisGenerationPresent = false
	}
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return nodesRemoved, edgesRemoved, nil
}

var (
	_ graph.FileBatchEvicter                 = (*Store)(nil)
	_ graph.CurrentGenerationRepoEvicter     = (*Store)(nil)
	_ graph.AllGenerationsRepoEvicter        = (*Store)(nil)
	_ graph.CheckedAllGenerationsRepoEvicter = (*Store)(nil)
)
