package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.AtomicVectorCorpusInstaller = (*Store)(nil)

var (
	errVectorCorpusInvalidDims   = errors.New("store_sqlite: vector corpus dims must be positive")
	errVectorCorpusAmbiguousDims = errors.New("store_sqlite: durable vector corpus has multiple dimensions")
)

// ReplaceVectorCorpus atomically replaces every durable vector owned by one
// repository. All input validation and cross-repository ownership checks occur
// before the first DELETE. The returned stats describe the complete committed
// corpus at dims, not just the replaced repository.
func (s *Store) ReplaceVectorCorpus(
	ctx context.Context,
	repoPrefix string,
	dims int,
	items []graph.VectorCorpusItem,
) (graph.VectorCorpusStats, error) {
	if err := validateVectorCorpus(ctx, dims, items); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	defer s.writeMu.Unlock()

	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return graph.VectorCorpusStats{}, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	if err := validateVectorCorpusOwnershipTx(ctx, tx, s.viewGen, repoPrefix, items); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM vectors WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	if err := insertVectorCorpusTx(ctx, tx, s.viewGen, repoPrefix, dims, items); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	stats, err := vectorCorpusStatsForRepoDims(ctx, tx, s.viewGen, repoPrefix, dims)
	if err != nil {
		return graph.VectorCorpusStats{}, err
	}
	if err := ctx.Err(); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	if err := tx.Commit(); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	return stats, nil
}

// VectorCorpusStats reports the durable corpus without materialising vectors.
// A non-positive dims value is discovery mode for warm restart: it succeeds
// only when the store is empty or has exactly one positive dimension.
func (s *Store) VectorCorpusStats(ctx context.Context, dims int) (graph.VectorCorpusStats, error) {
	if err := ctx.Err(); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	if dims > 0 {
		return vectorCorpusStatsForDims(ctx, s.db, s.viewGen, dims)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT dims,
       COUNT(*),
       COALESCE(SUM(CASE WHEN parent_id <> '' THEN 1 ELSE 0 END), 0)
FROM vectors
WHERE view_gen = ?
GROUP BY dims
ORDER BY dims
LIMIT 2`, s.viewGen)
	if err != nil {
		return graph.VectorCorpusStats{}, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return graph.VectorCorpusStats{}, err
		}
		return graph.VectorCorpusStats{}, nil
	}
	var stats graph.VectorCorpusStats
	if err := rows.Scan(&stats.Dims, &stats.VectorCount, &stats.ChunkCount); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	if stats.Dims <= 0 {
		return graph.VectorCorpusStats{}, fmt.Errorf("store_sqlite: durable vector corpus has invalid dimension %d", stats.Dims)
	}
	if rows.Next() {
		return graph.VectorCorpusStats{}, errVectorCorpusAmbiguousDims
	}
	if err := rows.Err(); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	return stats, nil
}

// VectorCorpusStatsForRepo returns global publication counts together with the
// selected repository's contribution. Discovery mode resolves the sole stored
// dimension in one aggregate query, avoiding a cross-query generation race.
func (s *Store) VectorCorpusStatsForRepo(
	ctx context.Context,
	repoPrefix string,
	dims int,
) (graph.VectorCorpusStats, error) {
	if err := ctx.Err(); err != nil {
		return graph.VectorCorpusStats{}, err
	}
	if dims > 0 {
		return vectorCorpusStatsForRepoDims(ctx, s.db, s.viewGen, repoPrefix, dims)
	}

	var stats graph.VectorCorpusStats
	var distinctDims int
	err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(MIN(dims), 0),
       COUNT(DISTINCT dims),
       COUNT(*),
       COALESCE(SUM(CASE WHEN parent_id <> '' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN repo_prefix = ? THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN repo_prefix = ? AND parent_id <> '' THEN 1 ELSE 0 END), 0)
FROM vectors
WHERE view_gen = ?`, repoPrefix, repoPrefix, s.viewGen).Scan(
		&stats.Dims,
		&distinctDims,
		&stats.VectorCount,
		&stats.ChunkCount,
		&stats.RepositoryVectorCount,
		&stats.RepositoryChunkCount,
	)
	if err != nil {
		return graph.VectorCorpusStats{}, err
	}
	if distinctDims > 1 {
		return graph.VectorCorpusStats{}, errVectorCorpusAmbiguousDims
	}
	if stats.VectorCount > 0 && stats.Dims <= 0 {
		return graph.VectorCorpusStats{}, fmt.Errorf("store_sqlite: durable vector corpus has invalid dimension %d", stats.Dims)
	}
	return stats, nil
}

func validateVectorCorpus(ctx context.Context, dims int, items []graph.VectorCorpusItem) error {
	if dims <= 0 {
		return errVectorCorpusInvalidDims
	}
	seen := make(map[string]struct{}, len(items))
	for i, item := range items {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if strings.TrimSpace(item.NodeID) == "" {
			return fmt.Errorf("store_sqlite: vector corpus item %d has empty node ID", i)
		}
		if _, ok := seen[item.NodeID]; ok {
			return fmt.Errorf("store_sqlite: vector corpus contains duplicate node ID %q", item.NodeID)
		}
		seen[item.NodeID] = struct{}{}
		if len(item.Vec) != dims {
			return fmt.Errorf("store_sqlite: vector corpus item %q has %d dimensions, want %d", item.NodeID, len(item.Vec), dims)
		}
		for _, value := range item.Vec {
			f := float64(value)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return fmt.Errorf("store_sqlite: vector corpus item %q contains a non-finite value", item.NodeID)
			}
		}
	}
	return ctx.Err()
}

func validateVectorCorpusOwnershipTx(
	ctx context.Context,
	tx *sql.Tx,
	viewGen int64,
	repoPrefix string,
	items []graph.VectorCorpusItem,
) error {
	if len(items) == 0 {
		return nil
	}

	// Every vector must be anchored in the durable graph. Ordinary vectors use
	// their own node ID; synthetic chunks use their durable parent symbol.
	ownershipTargets := make(map[string]struct{}, len(items))
	for _, item := range items {
		targetID := item.NodeID
		if item.ParentID != "" {
			if err := validateCanonicalVectorChunkID(item.NodeID, item.ParentID); err != nil {
				return err
			}
			targetID = item.ParentID
		}
		ownershipTargets[targetID] = struct{}{}
	}
	targetIDs := make([]string, 0, len(ownershipTargets))
	for id := range ownershipTargets {
		targetIDs = append(targetIDs, id)
	}
	sort.Strings(targetIDs)

	for start := 0; start < len(targetIDs); start += vectorChunk {
		end := start + vectorChunk
		if end > len(targetIDs) {
			end = len(targetIDs)
		}
		batch := targetIDs[start:end]
		stmt := make([]byte, 0, 72+len(batch)*2)
		stmt = append(stmt, "SELECT id, repo_prefix FROM nodes WHERE id IN ("...)
		args := make([]any, 0, len(batch))
		for i, id := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, id)
		}
		stmt = append(stmt, ") AND view_gen = ?"...)
		args = append(args, viewGen)

		rows, err := tx.QueryContext(ctx, string(stmt), args...)
		if err != nil {
			return err
		}
		owners := make(map[string]string, len(batch))
		for rows.Next() {
			var id, owner string
			if err := rows.Scan(&id, &owner); err != nil {
				_ = rows.Close()
				return err
			}
			owners[id] = owner
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range batch {
			owner, ok := owners[id]
			if !ok {
				return fmt.Errorf("store_sqlite: vector ownership node %q does not exist", id)
			}
			if owner != repoPrefix {
				return fmt.Errorf("store_sqlite: vector ownership node %q belongs to repository %q, not %q", id, owner, repoPrefix)
			}
		}
	}

	// Keep the vector-table ownership check as a second line of defence. It
	// prevents a malformed historical row from being reassigned through the
	// global node_id primary key even when the durable node anchor is valid.
	for start := 0; start < len(items); start += vectorChunk {
		end := start + vectorChunk
		if end > len(items) {
			end = len(items)
		}
		batch := items[start:end]
		stmt := make([]byte, 0, 96+len(batch)*2)
		stmt = append(stmt, "SELECT node_id, repo_prefix FROM vectors WHERE view_gen = ? AND node_id IN ("...)
		args := make([]any, 0, len(batch)+2)
		args = append(args, viewGen)
		for i, item := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, item.NodeID)
		}
		stmt = append(stmt, ") AND repo_prefix <> ? LIMIT 1"...)
		args = append(args, repoPrefix)

		var nodeID, owner string
		err := tx.QueryRowContext(ctx, string(stmt), args...).Scan(&nodeID, &owner)
		switch {
		case err == nil:
			return fmt.Errorf("store_sqlite: vector %q is owned by repository %q, not %q", nodeID, owner, repoPrefix)
		case errors.Is(err, sql.ErrNoRows):
			continue
		default:
			return err
		}
	}
	return nil
}

func validateCanonicalVectorChunkID(nodeID, parentID string) error {
	const marker = "#chunk"
	suffix, ok := strings.CutPrefix(nodeID, parentID+marker)
	if !ok || suffix == "" {
		return fmt.Errorf("store_sqlite: vector chunk %q is not derived from parent %q", nodeID, parentID)
	}
	ordinal, err := strconv.Atoi(suffix)
	if err != nil || ordinal < 0 || strconv.Itoa(ordinal) != suffix {
		return fmt.Errorf("store_sqlite: vector chunk %q has a non-canonical ordinal", nodeID)
	}
	return nil
}

func insertVectorCorpusTx(
	ctx context.Context,
	tx *sql.Tx,
	viewGen int64,
	repoPrefix string,
	dims int,
	items []graph.VectorCorpusItem,
) error {
	for start := 0; start < len(items); start += vectorChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + vectorChunk
		if end > len(items) {
			end = len(items)
		}
		batch := items[start:end]

		stmt := make([]byte, 0, 96+len(batch)*20)
		stmt = append(stmt, "INSERT INTO vectors (view_gen, node_id, repo_prefix, parent_id, dims, vec) VALUES "...)
		args := make([]any, 0, len(batch)*6)
		for i, item := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?, ?, ?)"...)
			args = append(args, viewGen, item.NodeID, repoPrefix, item.ParentID, dims, encodeVec(item.Vec))
		}
		if _, err := tx.ExecContext(ctx, string(stmt), args...); err != nil {
			return err
		}
	}
	return nil
}

type vectorCorpusStatsQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func vectorCorpusStatsForDims(
	ctx context.Context,
	q vectorCorpusStatsQuerier,
	viewGen int64,
	dims int,
) (graph.VectorCorpusStats, error) {
	stats := graph.VectorCorpusStats{Dims: dims}
	err := q.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN parent_id <> '' THEN 1 ELSE 0 END), 0)
FROM vectors
WHERE view_gen = ? AND dims = ?`, viewGen, dims).Scan(&stats.VectorCount, &stats.ChunkCount)
	if err != nil {
		return graph.VectorCorpusStats{}, err
	}
	return stats, nil
}

func vectorCorpusStatsForRepoDims(
	ctx context.Context,
	q vectorCorpusStatsQuerier,
	viewGen int64,
	repoPrefix string,
	dims int,
) (graph.VectorCorpusStats, error) {
	stats := graph.VectorCorpusStats{Dims: dims}
	err := q.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN parent_id <> '' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN repo_prefix = ? THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN repo_prefix = ? AND parent_id <> '' THEN 1 ELSE 0 END), 0)
FROM vectors
WHERE view_gen = ? AND dims = ?`, repoPrefix, repoPrefix, viewGen, dims).Scan(
		&stats.VectorCount,
		&stats.ChunkCount,
		&stats.RepositoryVectorCount,
		&stats.RepositoryChunkCount,
	)
	if err != nil {
		return graph.VectorCorpusStats{}, err
	}
	return stats, nil
}
