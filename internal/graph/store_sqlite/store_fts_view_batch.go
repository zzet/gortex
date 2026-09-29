package store_sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// SymbolSearchViewGeneration identifies the generation this search handle is
// pinned to. It is intentionally narrower than exposing Store's payload state.
func (s *Store) SymbolSearchViewGeneration() int64 {
	if s == nil {
		return 0
	}
	return s.viewGen
}

// SharesSymbolSearchCore reports whether other is a Store handle backed by
// the same open database core. Derived handles have different addresses, so
// pointer or path equality cannot establish that they are safe to batch.
func (s *Store) SharesSymbolSearchCore(other any) bool {
	peer, ok := other.(*Store)
	return ok && !s.coreless() && !peer.coreless() && s.storeCore == peer.storeCore
}

// symbolFTSStreamObserver, when set by a test, is told which generations a
// batch search sends through the shared unbounded rank stream. nil in
// production.
var symbolFTSStreamObserver func(generations []int64)

func symbolFTSViewBatchQuery(generations, repos int) string {
	q := `SELECT symbol_fts_rowid.view_gen, symbol_fts.node_id, bm25(symbol_fts)
FROM symbol_fts
CROSS JOIN symbol_fts_rowid
WHERE symbol_fts MATCH ?
  AND symbol_fts_rowid.fts_rowid = symbol_fts.rowid
  AND symbol_fts_rowid.view_gen IN (?` + strings.Repeat(`,?`, generations-1) + `)`
	if repos > 0 {
		q += ` AND symbol_fts.repo_prefix IN ('', ?` + strings.Repeat(`,?`, repos-1) + `)`
	}
	return q + ` AND symbol_fts.rank MATCH 'bm25()' ORDER BY symbol_fts.rank`
}

// SearchSymbolsViewGenerationsRepoScopedContext returns the same ranked page
// SearchSymbolsRepoScopedContext would return for each requested generation,
// but scans the shared FTS rank stream once. Results are accumulated as
// independent per-generation subsequences; the stream is closed only after
// every generation has filled its own limit or the shared corpus is exhausted.
//
// Duplicate generations are harmless. Every distinct requested generation is
// present in a successful result, with a nil slice when it has no hits.
func (s *Store) SearchSymbolsViewGenerationsRepoScopedContext(
	ctx context.Context,
	query string,
	repoAllow []string,
	viewGens []int64,
	limit int,
) (map[int64][]graph.SymbolHit, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}

	hitsByGeneration := make(map[int64][]graph.SymbolHit, len(viewGens))
	generations := make([]int64, 0, len(viewGens))
	for _, generation := range viewGens {
		if _, exists := hitsByGeneration[generation]; exists {
			continue
		}
		hitsByGeneration[generation] = nil
		generations = append(generations, generation)
	}
	if query == "" || len(generations) == 0 {
		return hitsByGeneration, nil
	}

	var allowed map[string]struct{}
	if len(repoAllow) > 0 {
		allowed = make(map[string]struct{}, len(repoAllow))
		for _, repo := range repoAllow {
			allowed[repo] = struct{}{}
		}
	}

	// Preserve the per-generation exact-name short circuit. A generation with
	// an admissible exact symbol is complete; generations without one continue
	// together through the shared FTS stream.
	unresolved := make([]int64, 0, len(generations))
	if isIdentifierQuery(query) {
		for _, generation := range generations {
			generationStore := *s
			generationStore.viewGen = generation
			nodes, err := generationStore.symbolExactNodesContext(ctx, query)
			if err != nil {
				return nil, err
			}
			for _, node := range nodes {
				if node.ID == "" || !tier0ShortCircuitKind(node.Kind) {
					continue
				}
				if allowed != nil && node.RepoPrefix != "" {
					if _, ok := allowed[node.RepoPrefix]; !ok {
						continue
					}
				}
				hitsByGeneration[generation] = append(
					hitsByGeneration[generation],
					graph.SymbolHit{NodeID: node.ID, Score: 100.0},
				)
				if len(hitsByGeneration[generation]) >= limit {
					break
				}
			}
			if len(hitsByGeneration[generation]) == 0 {
				unresolved = append(unresolved, generation)
			}
		}
	} else {
		unresolved = append(unresolved, generations...)
	}
	if len(unresolved) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return hitsByGeneration, nil
	}

	match := s.buildFTSMatch(query, true)
	if match == "" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return hitsByGeneration, nil
	}

	// A derived generation whose documents form a dense rowid run is ranked
	// inside that run on its own; one with no documents answers nothing.
	// Only the rest (the base corpus, a scattered generation) share the
	// unbounded rank stream below. See symbolFTSSpanFraction.
	streamed := unresolved[:0:0]
	for _, generation := range unresolved {
		span, measured, err := s.symbolFTSGenerationSpan(ctx, generation)
		if err != nil {
			return nil, err
		}
		switch {
		case measured && span.empty():
			continue
		case measured && span.dense():
			hits, err := s.searchSymbolFTSSpan(ctx, match, span, repoAllow, limit, false)
			if err != nil {
				return nil, err
			}
			hitsByGeneration[generation] = hits
		default:
			streamed = append(streamed, generation)
		}
	}
	unresolved = streamed
	if len(unresolved) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return hitsByGeneration, nil
	}

	if observe := symbolFTSStreamObserver; observe != nil {
		observe(append([]int64(nil), unresolved...))
	}

	// CROSS JOIN fixes symbol_fts as the single outer rank stream. The UNIQUE
	// rowid sidecar lookup then assigns each streamed document to at most one
	// requested generation without changing BM25 score or order.
	q := symbolFTSViewBatchQuery(len(unresolved), len(repoAllow))
	args := make([]any, 0, 1+len(unresolved)+len(repoAllow))
	args = append(args, match)
	for _, generation := range unresolved {
		args = append(args, generation)
	}
	if len(repoAllow) > 0 {
		for _, repo := range repoAllow {
			args = append(args, repo)
		}
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	remaining := len(unresolved)
	filled := make(map[int64]struct{}, len(unresolved))
	for rows.Next() {
		var (
			generation int64
			id         string
			score      float64
		)
		if err := rows.Scan(&generation, &id, &score); err != nil {
			return nil, err
		}
		if id == "" || len(hitsByGeneration[generation]) >= limit {
			continue
		}
		hitsByGeneration[generation] = append(
			hitsByGeneration[generation],
			graph.SymbolHit{NodeID: id, Score: -score},
		)
		if len(hitsByGeneration[generation]) == limit {
			if _, alreadyFilled := filled[generation]; !alreadyFilled {
				filled[generation] = struct{}{}
				remaining--
			}
			if remaining == 0 {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close batched symbol search rows: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return hitsByGeneration, nil
}
