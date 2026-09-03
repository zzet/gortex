package store_sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.BoundedExactNameReader = (*Store)(nil)
	_ graph.BoundedFileNodeReader  = (*Store)(nil)
)

// lookupLocalizationNodeCols carries identity, kind, location, and scope plus
// the opaque metadata blob needed to preserve the exact anchor's is_test gate.
// Unlike lookupNodeCols it excludes docs, signatures, retrieval text, and every
// other promoted payload. Raw metadata work is capped to 256 rows per closed
// keyset page; pages continue only until limit+1 eligible declarations prove
// saturation. Promoting is_test later would remove the remaining skipped-row
// blob decoding without changing this projection contract.
var lookupLocalizationNodeCols = lookupNodeSummaryCols + `, meta`

// FindNodesByNameBounded performs the exact-name localization lookup inside
// one short read-only transaction. Indexed scope predicates and keyset pages
// keep every cursor and allocation bounded. is_test remains in legacy metadata,
// so each 256-row page is decoded and closed before the next; scanning stops as
// soon as limit+1 production declarations prove ambiguity.
func (s *Store) FindNodesByNameBounded(
	ctx context.Context,
	name string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	if s.coreless() || s.db == nil || name == "" || limit <= 0 {
		return graph.BoundedNodeProjection{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedNodeProjection{}, err
	}

	predicate, args := localizationNodePredicate(name, scope)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.BoundedNodeProjection{}, err
	}
	defer func() { _ = tx.Rollback() }()

	const rawPageSize = 256
	sentinel := limit + 1
	nodes := make([]*graph.Node, 0, sentinel)
	lastID := ""
	for len(nodes) < sentinel {
		if err := ctx.Err(); err != nil {
			return graph.BoundedNodeProjection{}, err
		}
		pageArgs := append(append([]any(nil), args...), s.viewGen, lastID, rawPageSize)
		rows, queryErr := tx.QueryContext(
			ctx,
			`SELECT `+lookupLocalizationNodeCols+` FROM nodes WHERE `+predicate+` AND view_gen = ? AND id > ? ORDER BY id LIMIT ?`,
			pageArgs...,
		)
		if queryErr != nil {
			return graph.BoundedNodeProjection{}, queryErr
		}
		rawRows := 0
		for rows.Next() {
			node, scanErr := scanLocalizationNode(rows)
			if scanErr != nil {
				_ = rows.Close()
				return graph.BoundedNodeProjection{}, scanErr
			}
			rawRows++
			lastID = node.ID
			if !scope.Allows(node) {
				continue
			}
			nodes = append(nodes, node)
			if len(nodes) == sentinel {
				break
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return graph.BoundedNodeProjection{}, err
		}
		if err := rows.Close(); err != nil {
			return graph.BoundedNodeProjection{}, err
		}
		if len(nodes) == sentinel || rawRows < rawPageSize {
			break
		}
	}
	if err := tx.Commit(); err != nil {
		return graph.BoundedNodeProjection{}, err
	}

	total := len(nodes)
	truncated := total > limit
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	return graph.BoundedNodeProjection{Nodes: nodes, Total: total, Truncated: truncated}, nil
}

// FindFileNodesBounded reads one file through the identity/location projection.
// Scope and kind predicates are pushed before the sentinel cap. The normal
// declaration path transfers no metadata column at all; callers that explicitly
// exclude tests pay bounded RawBytes decoding because is_test still lives in the
// legacy metadata blob. Every keyset cursor is closed before the next page.
func (s *Store) FindFileNodesBounded(
	ctx context.Context,
	filePath string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	if s.coreless() || s.db == nil || filePath == "" || limit <= 0 {
		return graph.BoundedNodeProjection{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedNodeProjection{}, err
	}
	if scope.ExcludesFile(filePath) {
		return graph.BoundedNodeProjection{}, nil
	}

	predicate, args := localizationFileNodePredicate(filePath, scope)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.BoundedNodeProjection{}, err
	}
	defer func() { _ = tx.Rollback() }()

	columns := lookupNodeSummaryCols
	if scope.ExcludeTests {
		columns = lookupLocalizationNodeCols
	}
	const rawPageSize = 256
	sentinel := limit + 1
	nodes := make([]*graph.Node, 0, sentinel)
	lastID := ""
	for len(nodes) < sentinel {
		if err := ctx.Err(); err != nil {
			return graph.BoundedNodeProjection{}, err
		}
		pageArgs := append(append([]any(nil), args...), s.viewGen, lastID, rawPageSize)
		rows, queryErr := tx.QueryContext(
			ctx,
			`SELECT `+columns+` FROM nodes WHERE `+predicate+` AND view_gen = ? AND id > ? ORDER BY id LIMIT ?`,
			pageArgs...,
		)
		if queryErr != nil {
			return graph.BoundedNodeProjection{}, queryErr
		}
		rawRows := 0
		for rows.Next() {
			var node *graph.Node
			var scanErr error
			if scope.ExcludeTests {
				node, scanErr = scanLocalizationNode(rows)
			} else {
				node, scanErr = scanNodeSummary(rows)
			}
			if scanErr != nil {
				_ = rows.Close()
				return graph.BoundedNodeProjection{}, scanErr
			}
			rawRows++
			lastID = node.ID
			if !scope.Allows(node) {
				continue
			}
			nodes = append(nodes, node)
			if len(nodes) == sentinel {
				break
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return graph.BoundedNodeProjection{}, err
		}
		if err := rows.Close(); err != nil {
			return graph.BoundedNodeProjection{}, err
		}
		if len(nodes) == sentinel || rawRows < rawPageSize {
			break
		}
	}
	if err := tx.Commit(); err != nil {
		return graph.BoundedNodeProjection{}, err
	}

	total := len(nodes)
	truncated := total > limit
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	// Match the in-memory projection's ownership boundary: callers cannot
	// reslice a bounded page into the sentinel slot.
	nodes = nodes[:len(nodes):len(nodes)]
	return graph.BoundedNodeProjection{Nodes: nodes, Total: total, Truncated: truncated}, nil
}

func localizationNodePredicate(name string, scope graph.LocalizationNodeScope) (string, []any) {
	clauses, args := localizationScopePredicate(scope)
	clauses = append([]string{`name = ?`}, clauses...)
	args = append([]any{name}, args...)
	return strings.Join(clauses, ` AND `), args
}

func localizationFileNodePredicate(filePath string, scope graph.LocalizationNodeScope) (string, []any) {
	clauses, args := localizationScopePredicate(scope)
	clauses = append([]string{`file_path = ?`}, clauses...)
	args = append([]any{filePath}, args...)
	return strings.Join(clauses, ` AND `), args
}

func localizationScopePredicate(scope graph.LocalizationNodeScope) ([]string, []any) {
	var clauses []string
	var args []any
	if len(scope.Kinds) > 0 {
		kinds := make([]string, 0, len(scope.Kinds))
		for kind, allowed := range scope.Kinds {
			if allowed {
				kinds = append(kinds, string(kind))
			}
		}
		sort.Strings(kinds)
		if len(kinds) == 0 {
			clauses = append(clauses, `0`)
		} else {
			clauses = append(clauses, `kind IN (`+inPlaceholders(len(kinds))+`)`)
			for _, kind := range kinds {
				args = append(args, kind)
			}
		}
	}
	if len(scope.ExcludeKinds) > 0 {
		kinds := make([]string, 0, len(scope.ExcludeKinds))
		for kind, excluded := range scope.ExcludeKinds {
			if excluded {
				kinds = append(kinds, string(kind))
			}
		}
		sort.Strings(kinds)
		if len(kinds) > 0 {
			clauses = append(clauses, `kind NOT IN (`+inPlaceholders(len(kinds))+`)`)
			for _, kind := range kinds {
				args = append(args, kind)
			}
		}
	}
	if scope.WorkspaceID != "" {
		clauses = append(clauses, `COALESCE(NULLIF(workspace_id, ''), repo_prefix) = ?`)
		args = append(args, scope.WorkspaceID)
		if scope.ProjectID != "" {
			clauses = append(clauses, `COALESCE(NULLIF(project_id, ''), repo_prefix) = ?`)
			args = append(args, scope.ProjectID)
		}
	}
	if len(scope.RepoAllow) > 0 {
		repos := make([]string, 0, len(scope.RepoAllow))
		for repo, allowed := range scope.RepoAllow {
			if allowed {
				repos = append(repos, repo)
			}
		}
		sort.Strings(repos)
		if len(repos) == 0 {
			clauses = append(clauses, `repo_prefix = ''`)
		} else {
			clauses = append(clauses, `(repo_prefix = '' OR repo_prefix IN (`+inPlaceholders(len(repos))+`))`)
			for _, repo := range repos {
				args = append(args, repo)
			}
		}
	}
	return clauses, args
}

func scanLocalizationNode(scanner interface{ Scan(...any) error }) (*graph.Node, error) {
	var (
		node     graph.Node
		metaBlob sql.RawBytes
	)
	if err := scanner.Scan(
		&node.ID, &node.Kind, &node.Name, &node.QualName, &node.FilePath,
		&node.StartLine, &node.EndLine, &node.StartColumn, &node.EndColumn,
		&node.Language, &node.RepoPrefix, &node.WorkspaceID, &node.ProjectID,
		&metaBlob,
	); err != nil {
		return nil, err
	}
	if len(metaBlob) == 0 {
		return &node, nil
	}
	meta, err := decodeMeta([]byte(metaBlob))
	if err != nil {
		return nil, err
	}
	if isTest, _ := meta["is_test"].(bool); isTest {
		node.Meta = map[string]any{"is_test": true}
	}
	return &node, nil
}
