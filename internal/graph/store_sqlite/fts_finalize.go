package store_sqlite

import (
	"context"
	"time"
)

// mergeLiveFTSLocked performs one bounded best-effort finalize. Callers hold
// writeMu and supply a constant table/statement pair; generation and coordinated
// cold-load short circuits remain at the existing public entry points.
func (s *Store) mergeLiveFTSLocked(table, statement string) bulkFinalizeEvent {
	args := []any{coldFTSMergePages}
	if s.ftsMaintenanceObserver != nil {
		s.ftsMaintenanceObserver(statement, append([]any(nil), args...))
	}
	started := time.Now()
	_, err := s.execActiveWriteLocked(context.Background(), statement, args...)
	return bulkFinalizeEvent{Stage: "fts_merge", Name: table, Elapsed: time.Since(started), Err: err}
}

// FTSSegmentCounts describes physical FTS indexes, not an individual view or
// repository. Zero is a valid empty index; a failed read returns an error.
type FTSSegmentCounts struct {
	Symbols int64
	Content int64
}

// FTSSegments explicitly reads only distinct segment IDs in the two FTS idx
// sidecars, in one SQLite read snapshot. It performs no maintenance or automatic
// sampling and does not add a second gauge beside future checkpoint telemetry.
func (s *Store) FTSSegments(ctx context.Context) (FTSSegmentCounts, error) {
	var counts FTSSegmentCounts
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(DISTINCT segid) FROM symbol_fts_idx),
		(SELECT COUNT(DISTINCT segid) FROM content_fts_idx)`).Scan(&counts.Symbols, &counts.Content)
	if err != nil {
		return FTSSegmentCounts{}, err
	}
	return counts, nil
}
