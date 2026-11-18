package store_sqlite

import (
	"context"
	"encoding/json"
	"github.com/zzet/gortex/internal/graph"
	"unicode/utf8"
)

const exactNameDeclarationColumns = "n.id,n.name,n.kind,n.file_path,n.repo_prefix"
const exactNameDeclarationSeekSQL = `SELECT ` + exactNameDeclarationColumns + `
 FROM json_each(?) AS w CROSS JOIN nodes AS n
 WHERE n.name = w.value AND n.view_gen = ?`
const exactRawNameDeclarationSeekSQL = `SELECT ` + exactNameDeclarationColumns + `
 FROM nodes AS n WHERE n.name = ? AND n.view_gen = ?`

// VisitNodeDeclarationsByNamesContext skips opaque/enrichment columns for
// consumers needing only exact-name declaration facts. Generation, raw name
// spelling, duplicate suppression and cursor errors match the full visitor.
func (s *Store) VisitNodeDeclarationsByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	return s.visitNodeDeclarationsByNames(ctx, names, nil, yield)
}

// VisitNodeDeclarationsByNamesInRepoContext applies ownership before decoding.
func (s *Store) VisitNodeDeclarationsByNamesInRepoContext(ctx context.Context, names []string, repo string, yield func(*graph.Node) bool) error {
	return s.visitNodeDeclarationsByNames(ctx, names, &repo, yield)
}

func (s *Store) visitNodeDeclarationsByNames(ctx context.Context, names []string, repo *string, yield func(*graph.Node) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil || len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	uniq := make([]string, 0, len(names))
	raw := false
	for _, name := range names {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			uniq = append(uniq, name)
			raw = raw || !utf8.ValidString(name)
		}
	}
	if raw {
		// JSON replaces malformed UTF-8, so bind the original bytes directly.
		for _, name := range uniq {
			query, args := exactRawNameDeclarationSeekSQL, []any{name, s.viewGen}
			if repo != nil {
				query += " AND +n.repo_prefix = ?"
				args = append(args, *repo)
			}
			stopped, err := s.visitNameDeclarationQuery(ctx, query, args, yield)
			if err != nil {
				return err
			}
			if stopped {
				break
			}
		}
		return ctx.Err()
	}
	for start := 0; start < len(uniq); start += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, err := json.Marshal(uniq[start:min(start+lookupChunkSize, len(uniq))])
		if err != nil {
			return err
		}
		query, args := exactNameDeclarationSeekSQL, []any{string(payload), s.viewGen}
		if repo != nil {
			query += " AND +n.repo_prefix = ?"
			args = append(args, *repo)
		}
		stopped, err := s.visitNameDeclarationQuery(ctx, query, args, yield)
		if err != nil {
			return err
		}
		if stopped {
			break
		}
	}
	return ctx.Err()
}
func (s *Store) visitNameDeclarationQuery(ctx context.Context, query string, args []any, yield func(*graph.Node) bool) (stopped bool, err error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	defer func() {
		if e := rows.Close(); err == nil && e != nil {
			err = e
		}
	}()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n := new(graph.Node)
		if err := rows.Scan(&n.ID, &n.Name, &n.Kind, &n.FilePath, &n.RepoPrefix); err != nil {
			return false, err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !yield(n) {
			return true, ctx.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, ctx.Err()
}
