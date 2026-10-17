package store_sqlite

import (
	"context"
	"database/sql"
	"strings"
)

// These are transient scan bounds, not the retained-row cache threshold.
// All attempts share one query's allowance; a decline uses the existing MATCH.
const (
	symbolFTSStreamRows      = 20000
	symbolFTSStreamBytes     = 8 * 1024 * 1024
	symbolFTSStreamSingleRow = 1024 * 1024
	symbolFTSStreamPrefixes  = 32
)

type symbolFTSStreamBudget struct {
	rows  int
	bytes int64
}

// A nil completed entry records an unsupported/over-budget generation, never
// an empty count certificate. Successful entries contain every
// eligible prefix's count after the entire owned generation reached EOF.
type symbolFTSAddedStreams struct {
	tx        *sql.Tx
	prefixes  []string
	baselines map[string]*symbolFTSPrefixBaseline
	revs      map[int64]int64
	rows      int64
	docs      map[int64]int64
	completed map[int64]map[string]int64
	budget    symbolFTSStreamBudget
}

func newSymbolFTSAddedStreams(tx *sql.Tx, prefixes []string, baselines map[string]*symbolFTSPrefixBaseline, revs map[int64]int64, rows int64, docs map[int64]int64) *symbolFTSAddedStreams {
	var eligible []string
	seen := map[string]bool{}
	for _, prefix := range prefixes {
		folded := strings.ToLower(strings.TrimSpace(prefix))
		if !asciiFoldable(folded) || seen[folded] {
			continue
		}
		seen[folded] = true
		eligible = append(eligible, folded)
		if len(eligible) > symbolFTSStreamPrefixes {
			eligible = nil
			break
		}
	}
	return &symbolFTSAddedStreams{tx: tx, prefixes: eligible, baselines: baselines,
		revs: revs, rows: rows, docs: docs, completed: map[int64]map[string]int64{},
		budget: symbolFTSStreamBudget{rows: symbolFTSStreamRows, bytes: symbolFTSStreamBytes}}
}

func (s *symbolFTSAddedStreams) count(ctx context.Context, generation int64, folded string) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if !asciiFoldable(folded) {
		return 0, false, nil
	}
	if counts, attempted := s.completed[generation]; attempted {
		n, found := counts[folded]
		return n, found, nil
	}
	// The existing snapshot loop already obtained this exact owned-doc count.
	n := s.docs[generation]
	if n <= symbolFTSSmallGeneration || n > int64(s.budget.rows) || s.budget.bytes <= 0 || len(s.prefixes) < 2 {
		return 0, false, nil
	}
	var prefixes []string
	for _, prefix := range s.prefixes {
		base := s.baselines[prefix]
		if base == nil {
			continue // Missing baselines still require the unchanged global count.
		}
		if _, known := base.revs[generation]; known {
			continue
		}
		fresh := false
		for g, revision := range base.revs {
			if s.revs[g] != revision {
				fresh = true
				break
			}
		}
		if fresh {
			continue
		}
		rows := base.rows
		for g := range s.revs {
			if _, known := base.revs[g]; known {
				continue
			}
			docs, known := s.docs[g]
			if !known {
				if err := s.tx.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts_rowid WHERE view_gen = ?`, g).Scan(&docs); err != nil {
					return 0, false, err
				}
				s.docs[g] = docs
			}
			rows += docs
		}
		if rows == s.rows {
			prefixes = append(prefixes, prefix)
		}
	}
	if len(prefixes) < 2 {
		return 0, false, nil
	}
	counts, err := symbolFTSStreamAddedCounts(ctx, s.tx, generation, n, prefixes, &s.budget)
	if err != nil {
		return 0, false, err
	}
	s.completed[generation] = counts
	value, found := counts[folded]
	return value, found, nil
}

// The CASE prevents materializing an oversized token string into Go. SQLite
// retains its ordinary cell-read behavior. The +1 row distinguishes EOF from
// a truncated scan; exact ownership and the expected row count are both kept.
const symbolFTSAddedStreamSQL = `SELECT CASE WHEN length(CAST(f.tokens AS BLOB)) <= ? THEN f.tokens END
 FROM symbol_fts_rowid m INDEXED BY symbol_fts_rowid_by_generation
 CROSS JOIN symbol_fts f
 WHERE m.view_gen = ? AND f.rowid = m.fts_rowid
 ORDER BY m.fts_rowid LIMIT ?`

func symbolFTSStreamAddedCounts(ctx context.Context, tx *sql.Tx, generation, expectedRows int64, prefixes []string, budget *symbolFTSStreamBudget) (counts map[string]int64, retErr error) {
	if expectedRows > int64(budget.rows) || budget.rows <= 0 || budget.bytes <= 0 {
		return nil, ctx.Err()
	}
	if len(prefixes) < 2 || len(prefixes) > symbolFTSStreamPrefixes {
		return nil, ctx.Err()
	}
	seen := map[string]bool{}
	for _, prefix := range prefixes {
		if !asciiFoldable(prefix) || seen[prefix] {
			return nil, ctx.Err()
		}
		seen[prefix] = true
	}
	cur, err := tx.QueryContext(ctx, symbolFTSAddedStreamSQL, symbolFTSStreamSingleRow, generation, budget.rows+1)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := cur.Close(); err != nil {
			counts = nil
			if retErr == nil {
				retErr = err
			}
		}
	}()
	counts = make(map[string]int64, len(prefixes))
	var rows int64
	for cur.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var text sql.NullString
		if err := cur.Scan(&text); err != nil {
			return nil, err
		}
		rows++
		budget.rows--
		if !text.Valid {
			budget.bytes = 0
			return nil, ctx.Err()
		}
		budget.bytes -= int64(len(text.String))
		if budget.rows < 0 || budget.bytes < 0 {
			return nil, ctx.Err()
		}
		for _, prefix := range prefixes {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			has, ascii := asciiTokensHavePrefix(text.String, prefix)
			if !ascii {
				return nil, ctx.Err() // No partial ASCII counts survive a Unicode row.
			}
			if has {
				counts[prefix]++
			}
		}
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	if err := cur.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if rows != expectedRows {
		return nil, ctx.Err()
	}
	// Missing keys must denote a proved zero, not an unattempted prefix.
	for _, prefix := range prefixes {
		if _, present := counts[prefix]; !present {
			counts[prefix] = 0
		}
	}
	return counts, nil
}
