package store_sqlite

import (
	"context"
	"database/sql"
	"hash/fnv"
	"strconv"
)

// SymbolFTSScoringSnapshot returns global BM25 statistics and every requested
// prefix's matching-document count from one read-only transaction. Counts
// include all generations, just as SQLite's shared symbol_fts BM25 does.
// No detached stamp check or in-memory document total may name this snapshot.
func (s *Store) SymbolFTSScoringSnapshot(ctx context.Context, prefixes []string) (SymbolFTSStats, map[string]int64, error) {
	if s.coreless() {
		return SymbolFTSStats{}, nil, errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SymbolFTSStats{}, nil, err
	}
	return s.symbolFTSScoringSnapshot(ctx, prefixes, s.rowCountersReady.Load())
}

// symbolFTSStatsTx uses the exact averages/structure blocks hashed by the
// existing corpus stamp, in the same snapshot as the prefix counts.
func symbolFTSStatsTx(ctx context.Context, tx *sql.Tx) (SymbolFTSStats, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, block FROM symbol_fts_data WHERE id IN (1, 10) ORDER BY id`)
	if err != nil {
		return SymbolFTSStats{}, err
	}
	defer rows.Close()
	var stats SymbolFTSStats
	hash := fnv.New64a()
	found := 0
	for rows.Next() {
		var id int64
		var block []byte
		if err := rows.Scan(&id, &block); err != nil {
			return SymbolFTSStats{}, err
		}
		_, _ = hash.Write([]byte(strconv.FormatInt(id, 10)))
		_, _ = hash.Write(block)
		found++
		if id == 1 {
			values := sqliteVarints(block)
			if len(values) > 0 {
				stats.Rows = int64(values[0])
				for _, v := range values[1:] {
					stats.Tokens += int64(v)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return SymbolFTSStats{}, err
	}
	if err := rows.Close(); err != nil {
		return SymbolFTSStats{}, err
	}
	if err := ctx.Err(); err != nil {
		return SymbolFTSStats{}, err
	}
	if found > 0 {
		stats.Stamp = strconv.FormatUint(hash.Sum64(), 16)
	}
	return stats, nil
}
