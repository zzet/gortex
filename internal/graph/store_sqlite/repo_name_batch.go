package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.RepoNamesNodeFinder = (*Store)(nil)
var _ graph.RepoLanguageSymbolCounter = (*Store)(nil)

// FindNodesByNamesInRepo keeps both predicates in SQLite. The compound
// repo/name index makes each bounded IN page seek-driven and prevents symbols
// from unrelated repositories crossing the driver boundary.
func (s *Store) FindNodesByNamesInRepo(names []string, repoPrefix string) map[string][]*graph.Node {
	seen := make(map[string]struct{}, len(names))
	uniq := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		uniq = append(uniq, name)
	}
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string][]*graph.Node, len(uniq))
	namesJSON, ok := projectionJSON(uniq)
	if !ok {
		return out
	}
	if observe := nameLookupSQLObserver; observe != nil {
		observe(repoNamesSeekSQL)
	}
	for _, node := range s.queryNodesSQL(repoNamesSeekSQL, namesJSON, s.viewGen, repoPrefix) {
		if node != nil {
			out[node.Name] = append(out[node.Name], node)
		}
	}
	return out
}

// repoNamesSeekSQL reads a repository's nodes of the given names in one
// generation, one nodes_by_name (name, view_gen) seek per name. The IN-list
// form flipped, from about two hundred names, to a scan of the repository's
// whole generation (nodes_by_repo (repo_prefix, view_gen)): 600k rows for the
// base generation. The json_each form fixed the order but left the index to
// the statistics: with none (a store fresh from a load) nodes_by_repo and
// nodes_by_name both key two equalities, and the planner took nodes_by_repo,
// a scan of the repository's generation per name (60 s an edit). The unary +
// keeps repo_prefix out of every index key, so nodes_by_name is the only
// two-column seek whatever the statistics say; the repository is checked on
// the rows the seek returns.
var repoNamesSeekSQL = `SELECT ` + qualifiedNodeColumns("n", lookupNodeCols) + `
  FROM json_each(?) AS w
  CROSS JOIN nodes AS n
 WHERE n.name = w.value AND n.view_gen = ? AND +n.repo_prefix = ?`

func (s *Store) CountRepoLanguageSymbols(repoPrefix string, languages []string) int {
	seen := make(map[string]struct{}, len(languages))
	uniq := make([]string, 0, len(languages))
	for _, language := range languages {
		if language == "" {
			continue
		}
		if _, duplicate := seen[language]; duplicate {
			continue
		}
		seen[language] = struct{}{}
		uniq = append(uniq, language)
	}
	if len(uniq) == 0 {
		return 0
	}
	query := `SELECT COUNT(*) FROM nodes
WHERE repo_prefix = ? AND language IN (` + strings.Repeat(",?", len(uniq))[1:] + `)
  AND kind <> ? AND kind <> ? AND view_gen = ?`
	args := make([]any, 0, len(uniq)+4)
	args = append(args, repoPrefix)
	for _, language := range uniq {
		args = append(args, language)
	}
	args = append(args, string(graph.KindFile), string(graph.KindImport), s.viewGen)
	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		panicOnFatal(err)
		return 0
	}
	return count
}

// nameLookupSQLObserver, when a test sets it, is told the statement each
// repository name read runs, so the plan the test checks is the one the read
// uses. nil in production.
var nameLookupSQLObserver func(query string)
