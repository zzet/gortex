package store_sqlite

import (
	"database/sql"
	"errors"
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// HasLanguage reports whether any indexed NAMED node carries the given
// language (backs the resolver's graphHasLanguage gate). nodes has no
// language-leading index, so a bare language predicate would scan the
// table exactly on the miss — the case a gate exists for. The recursive
// CTE instead skip-scans the distinct repo prefixes (one MIN seek each,
// empty solo-repo prefix included) and probes each through the
// nodes_by_repo_language_name partial index — the non-empty-name
// predicate is what makes it eligible. Unexpected errors answer true so
// a gated pass runs rather than being silently skipped.
//
// Measured on a 5.3 GB, 34-prefix store: 8ms on a miss and ~1ms on a hit,
// against 2.5s for the equivalent `WHERE language = ? LIMIT 1`.
//
// The non-empty-name predicate is load-bearing for the plan and narrows the
// question to "are there NAMED nodes in this language" — a language
// present only as unnamed nodes answers false. That is the right question
// for every caller today (each gates a pass over symbols), and no such
// language exists in practice: on the same store, every language with any
// node has at least one named node. Callers needing raw presence,
// including of nameless nodes, need a different probe — this one cannot
// answer it without giving up the index.
func (s *Store) HasLanguage(lang string) bool {
	if lang == "" {
		return false
	}
	var one int
	err := s.db.QueryRow(hasLanguageSQL, lang, s.viewGen).Scan(&one)
	// Bulk ingestion can defer this dense index. Preserve the previous read
	// behavior in that state rather than making an index hint a new fatal error.
	if err != nil && strings.Contains(err.Error(), "no such index: nodes_by_repo_language_name") {
		fallback := strings.Replace(hasLanguageSQL, " INDEXED BY nodes_by_repo_language_name", "", 1)
		err = s.db.QueryRow(fallback, lang, s.viewGen).Scan(&one)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		panicOnFatal(err)
		return true
	}
	return true
}

// Pin the covering probe: nodes_name_candidates can otherwise win costing
// despite having no language key, turning an absent-language check into a
// scan of every named node in each repository. The missing-index fallback above
// retains compatibility with bulk ingestion's deferred-index window.
const hasLanguageSQL = `
WITH RECURSIVE repo_prefixes(rp) AS (
    SELECT MIN(repo_prefix) FROM nodes
    UNION ALL
    SELECT (SELECT MIN(repo_prefix) FROM nodes WHERE repo_prefix > rp)
    FROM repo_prefixes WHERE rp IS NOT NULL
)
SELECT 1 FROM repo_prefixes
WHERE rp IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM nodes INDEXED BY nodes_by_repo_language_name
      WHERE repo_prefix = rp AND language = ? AND name <> '' AND view_gen = ?
  )
LIMIT 1`

// NodesByKindLang is NodesByKind with the language predicate pushed into
// SQL, so only the matching language's rows are decoded and cross the
// boundary. Satisfies the resolver's optional nodesByKindLang interface,
// which previously always fell back to NodesByKind + an in-Go filter.
func (s *Store) NodesByKindLang(kind graph.NodeKind, lang string) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		out := s.queryNodesSQL(`SELECT `+lookupNodeCols+` FROM nodes WHERE kind = ? AND language = ? AND view_gen = ?`,
			string(kind), lang, s.viewGen)
		for _, n := range out {
			if !yield(n) {
				return
			}
		}
	}
}
