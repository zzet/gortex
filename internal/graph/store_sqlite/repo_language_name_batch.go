package store_sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ResolverNameScopeFinder = (*Store)(nil)

type resolverNameScopePayload struct {
	ScopeID    int      `json:"scope_id"`
	RepoPrefix string   `json:"repo_prefix"`
	AllRepos   bool     `json:"all_repos"`
	Languages  []string `json:"languages"`
	Names      []string `json:"names"`
}

// resolverNameScopeQuery expands one bounded JSON payload into correlated
// (scope, repository, language, name) rows inside SQLite. The four UNION arms
// keep wildcard predicates out of indexed joins: exact repository+language
// requests use nodes_by_repo_language_name; global or unknown-language requests
// use nodes_by_name and apply only their remaining cheap filters. CROSS JOIN
// fixes the wanted rows as the outer loop, so each request performs an index
// seek instead of inviting a scan of nodes.
//
// Bind order is the payload followed by one generation per UNION arm: the
// wanted rows come from JSON and carry no generation, so each arm has to
// constrain the nodes side itself. resolverNameScopeArgs builds the list.
var resolverNameScopeQuery = `
WITH raw_scopes AS MATERIALIZED (
    SELECT
        CAST(json_extract(j.value, '$.scope_id') AS INTEGER) AS scope_id,
        CAST(json_extract(j.value, '$.repo_prefix') AS TEXT) AS repo_prefix,
        CAST(json_extract(j.value, '$.all_repos') AS INTEGER) AS all_repos,
        json_extract(j.value, '$.languages') AS languages_json,
        json_extract(j.value, '$.names') AS names_json
    FROM json_each(?) AS j
),
scope_names AS MATERIALIZED (
    SELECT r.scope_id, r.repo_prefix, r.all_repos, r.languages_json,
           CAST(n.value AS TEXT) AS name
    FROM raw_scopes AS r
    CROSS JOIN json_each(r.names_json) AS n
    WHERE n.type = 'text' AND CAST(n.value AS TEXT) <> ''
),
wanted AS MATERIALIZED (
    SELECT s.scope_id, s.repo_prefix, s.all_repos, 0 AS all_languages,
           CAST(l.value AS TEXT) AS language, s.name
    FROM scope_names AS s
    CROSS JOIN json_each(s.languages_json) AS l
    WHERE l.type = 'text'
    UNION ALL
    SELECT s.scope_id, s.repo_prefix, s.all_repos, 1 AS all_languages,
           '' AS language, s.name
    FROM scope_names AS s
    WHERE json_array_length(s.languages_json) = 0
)
SELECT w.scope_id,
       CASE WHEN w.all_repos = 1 THEN n.repo_prefix ELSE '' END AS sort_repo,
       CASE WHEN w.all_languages = 1 THEN '' ELSE n.language END AS sort_language,
       ` + qualifiedNodeColumns("n", lookupNodeCols) + `
FROM wanted AS w
CROSS JOIN nodes AS n INDEXED BY nodes_by_repo_language_name
WHERE w.all_repos = 0 AND w.all_languages = 0
  AND n.repo_prefix = w.repo_prefix AND n.language = w.language
  AND n.name = w.name AND n.name <> ''
  AND n.view_gen = ?
UNION ALL
SELECT w.scope_id, n.repo_prefix, n.language,
       ` + qualifiedNodeColumns("n", lookupNodeCols) + `
FROM wanted AS w
CROSS JOIN nodes AS n INDEXED BY nodes_by_name
WHERE w.all_repos = 1 AND w.all_languages = 0
  AND n.name = w.name AND n.language = w.language
  AND n.view_gen = ?
UNION ALL
SELECT w.scope_id, '', '',
       ` + qualifiedNodeColumns("n", lookupNodeCols) + `
FROM wanted AS w
CROSS JOIN nodes AS n INDEXED BY nodes_by_name
WHERE w.all_repos = 0 AND w.all_languages = 1
  AND n.name = w.name AND n.repo_prefix = w.repo_prefix
  AND n.view_gen = ?
UNION ALL
SELECT w.scope_id, n.repo_prefix, '',
       ` + qualifiedNodeColumns("n", lookupNodeCols) + `
FROM wanted AS w
CROSS JOIN nodes AS n INDEXED BY nodes_by_name
WHERE w.all_repos = 1 AND w.all_languages = 1
  AND n.name = w.name
  AND n.view_gen = ?
ORDER BY 1, 2, 3, 4`

// resolverNameScopeUnionArms is the number of UNION arms in
// resolverNameScopeQuery, each of which binds the generation once.
const resolverNameScopeUnionArms = 4

// resolverNameScopeArgs orders the payload bind ahead of one generation bind
// per UNION arm, matching the placeholders' textual order.
func resolverNameScopeArgs(payload []byte, viewGen int64) []any {
	args := make([]any, 0, 1+resolverNameScopeUnionArms)
	args = append(args, payload)
	for i := 0; i < resolverNameScopeUnionArms; i++ {
		args = append(args, viewGen)
	}
	return args
}

// FindNodesByResolverNameScopes performs one correlated read for every logical
// resolver scope in a pending page. It uses one bind regardless of the number
// of names, so a large page cannot cross SQLITE_LIMIT_VARIABLE_NUMBER. Query,
// scan, metadata-decode, and rows-finalization errors are returned to the
// resolver; an incomplete read is never converted into authoritative misses.
func (s *Store) FindNodesByResolverNameScopes(scopes []graph.ResolverNameScope) ([]map[string][]*graph.Node, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	payloadScopes := make([]resolverNameScopePayload, 0, len(scopes))
	for i, scope := range scopes {
		names := uniqueNonEmptyStrings(scope.Names)
		if len(names) == 0 {
			continue
		}
		payloadScopes = append(payloadScopes, resolverNameScopePayload{
			ScopeID:    i,
			RepoPrefix: scope.RepoPrefix,
			AllRepos:   scope.AllRepos,
			Languages:  uniqueStrings(scope.Languages),
			Names:      names,
		})
	}
	out := make([]map[string][]*graph.Node, len(scopes))
	if len(payloadScopes) == 0 {
		return out, nil
	}
	payload, err := json.Marshal(payloadScopes)
	if err != nil {
		return nil, fmt.Errorf("sqlite resolver name scopes encode: %w", err)
	}
	rows, err := s.db.Query(resolverNameScopeQuery, resolverNameScopeArgs(payload, s.viewGen)...)
	if err != nil {
		return nil, fmt.Errorf("sqlite resolver name scopes query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		scopeID, node, scanErr := scanResolverNameScopeNode(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("sqlite resolver name scopes scan: %w", scanErr)
		}
		if scopeID < 0 || scopeID >= len(out) {
			return nil, fmt.Errorf("sqlite resolver name scopes invalid scope id %d", scopeID)
		}
		if node == nil {
			continue
		}
		if out[scopeID] == nil {
			out[scopeID] = make(map[string][]*graph.Node)
		}
		out[scopeID][node.Name] = append(out[scopeID][node.Name], node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite resolver name scopes rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("sqlite resolver name scopes close: %w", err)
	}
	return out, nil
}

func scanResolverNameScopeNode(scanner interface {
	Scan(...any) error
}) (int, *graph.Node, error) {
	var (
		scopeID            int
		sortRepo, sortLang string
		n                  graph.Node
		metaBlob           sql.RawBytes
		p                  promotedNodeMeta
	)
	err := scanner.Scan(
		&scopeID, &sortRepo, &sortLang,
		&n.ID, &n.Kind, &n.Name, &n.QualName, &n.FilePath,
		&n.StartLine, &n.EndLine, &n.StartColumn, &n.EndColumn, &n.Language,
		&n.RepoPrefix, &n.WorkspaceID, &n.ProjectID,
		&p.sig, &p.vis, &p.doc, &p.external, &p.returnType,
		&p.isAsync, &p.isStatic, &p.isAbstract, &p.isExported, &p.updatedAt,
		&p.dataClass, &p.semanticType, &p.semanticSource, &p.cloneSig,
		&p.entryPoint, &p.entryPointKind, &metaBlob,
		&p.searchSig, &p.searchQualName, &p.searchDoc, &p.searchSuppressed, &p.sectionText,
	)
	if err != nil {
		return 0, nil, err
	}
	if len(metaBlob) > 0 {
		meta, decodeErr := decodeMeta(metaBlob)
		if decodeErr != nil {
			return 0, nil, decodeErr
		}
		n.Meta = meta
	}
	restorePromotedMeta(&n, p)
	return scopeID, &n, nil
}

// FindNodesByNamesInRepoLanguages keeps the resolver's three strongest cheap
// predicates in SQLite. The matching compound index is ordered by repository,
// language, then name so every bounded IN page is seek-driven. An empty
// languages slice is the conservative unknown-source fallback and delegates to
// the repository-scoped batch lookup rather than issuing one query per name.
func (s *Store) FindNodesByNamesInRepoLanguages(names []string, repoPrefix string, languages []string) map[string][]*graph.Node {
	uniqNames := uniqueNonEmptyStrings(names)
	if len(uniqNames) == 0 {
		return nil
	}
	uniqLanguages := uniqueStrings(languages)
	if len(uniqLanguages) == 0 {
		return s.FindNodesByNamesInRepo(uniqNames, repoPrefix)
	}

	// lookupChunkSize stays well below SQLite's variable limit. Reserve one
	// binding for repo_prefix, one for the generation, and one for every
	// compatible language; bound values do not expand the SQL text, so the
	// placeholder string stays small.
	nameChunkSize := lookupChunkSize - len(uniqLanguages) - 2
	if nameChunkSize < 1 {
		nameChunkSize = 1
	}
	out := make(map[string][]*graph.Node, len(uniqNames))
	languagePlaceholders := strings.Repeat(",?", len(uniqLanguages))[1:]
	for start := 0; start < len(uniqNames); start += nameChunkSize {
		end := minInt(start+nameChunkSize, len(uniqNames))
		chunk := uniqNames[start:end]
		namePlaceholders := strings.Repeat(",?", len(chunk))[1:]
		query := `SELECT ` + lookupNodeCols + ` FROM nodes
WHERE repo_prefix = ?
  AND language IN (` + languagePlaceholders + `)
  AND name IN (` + namePlaceholders + `)
  AND name <> ''
  AND view_gen = ?`
		args := make([]any, 0, 2+len(uniqLanguages)+len(chunk))
		args = append(args, repoPrefix)
		for _, language := range uniqLanguages {
			args = append(args, language)
		}
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

func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
