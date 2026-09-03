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
	for start := 0; start < len(uniq); start += lookupChunkSize - 2 {
		end := minInt(start+lookupChunkSize-2, len(uniq))
		chunk := uniq[start:end]
		query := `SELECT ` + lookupNodeCols + ` FROM nodes
WHERE repo_prefix = ? AND name IN (` + strings.Repeat(",?", len(chunk))[1:] + `) AND view_gen = ?`
		args := make([]any, 0, len(chunk)+2)
		args = append(args, repoPrefix)
		for _, name := range chunk {
			args = append(args, name)
		}
		args = append(args, s.viewGen)
		for _, node := range s.queryNodesSQL(query, args...) {
			if node != nil {
				out[node.Name] = append(out[node.Name], node)
			}
		}
	}
	return out
}

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
