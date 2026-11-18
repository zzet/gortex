package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// FindFoldedNameCandidatesContext retains the field grammar's Go Unicode
// predicate before the horizon. Compact selection and bounded hydration use
// the same physical read transaction; no cache token certifies exhaustion.
func (s *Store) FindFoldedNameCandidatesContext(ctx context.Context, needle string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, bool, error) {
	ctx = liveNameContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if s.coreless() || s.db == nil {
		return nil, false, sql.ErrConnDone
	}
	if needle == "" || limit <= 0 {
		return nil, false, fmt.Errorf("store_sqlite: invalid folded-name candidate head")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	keys, more, err := selectFoldedNameCandidates(ctx, tx, s.viewGen, needle, limit, filter)
	if err != nil {
		return nil, false, err
	}
	ids := make([]string, len(keys))
	for i, key := range keys {
		ids[i] = key.ID
	}
	nodes, err := hydrateNameCandidateIDs(ctx, tx, s.viewGen, ids)
	if err != nil {
		return nil, false, err
	}
	byID := make(map[string]*graph.Node, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	out := make([]*graph.Node, 0, len(keys))
	for _, key := range keys {
		node := byID[key.ID]
		if node == nil || node.Name != key.Name {
			return nil, false, fmt.Errorf("store_sqlite: inconsistent folded-name snapshot")
		}
		out = append(out, node)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return out, more, nil
}

// foldedNameLiteralGlob uses case classes instead of SQL lower/LIKE. GLOB is
// independent of case_sensitive_like; user metacharacters are always literal.
// Unsafe needles and expanded patterns beyond SQLite's existing pattern limit
// fall back to the original unfiltered scan, never a new refusal or SQL cap.
func foldedNameLiteralGlob(needle string) (string, bool) {
	folded := strings.ToLower(needle)
	if len(folded) > sqliteLikePatternLimit-2 {
		return "", false
	}
	var pattern strings.Builder
	pattern.Grow(min(4*len(folded)+2, sqliteLikePatternLimit))
	_ = pattern.WriteByte('*')
	for i := range len(folded) {
		c := folded[i]
		if c < 0x20 || c > 0x7e {
			return "", false
		}
		width := 1
		if c >= 'a' && c <= 'z' {
			width = 4
		} else if c == '*' || c == '?' || c == '[' || c == ']' {
			width = 3
		}
		if pattern.Len()+width+1 > sqliteLikePatternLimit {
			return "", false
		}
		switch {
		case c >= 'a' && c <= 'z':
			_ = pattern.WriteByte('[')
			_ = pattern.WriteByte(c)
			_ = pattern.WriteByte(c - ('a' - 'A'))
			_ = pattern.WriteByte(']')
		case c == '*':
			_, _ = pattern.WriteString("[*]")
		case c == '?':
			_, _ = pattern.WriteString("[?]")
		case c == '[':
			_, _ = pattern.WriteString("[[]")
		case c == ']':
			_, _ = pattern.WriteString("[]]")
		default:
			_ = pattern.WriteByte(c)
		}
	}
	_ = pattern.WriteByte('*')
	return pattern.String(), true
}

// foldedNameCandidatesSQL deliberately has no visit order: the bounded Go
// set supplies the authoritative total head order and overflow bit. Let the
// planner use an existing covering index, or fall back if it is absent.
func foldedNameCandidatesSQL(generation int64, needle string) (string, []any) {
	query := `SELECT ` + nameCandidateCols + ` FROM nodes WHERE view_gen = ?`
	args := []any{generation}
	// A nonempty Go-lower needle cannot match an empty TEXT name. This
	// explicit predicate makes the existing partial covering index eligible.
	// Empty internal needles retain the old full population.
	if needle != "" {
		query += ` AND name <> ''`
	}
	if pattern, usable := foldedNameLiteralGlob(needle); usable {
		// Only printable ASCII TEXT can be excluded here. Unicode, NUL,
		// malformed bytes and legacy storage types still reach Go matching.
		query += ` AND (
			typeof(name) <> 'text'
			OR name GLOB ?
			OR length(CAST(name AS BLOB)) <> length(name)
			OR name GLOB '*[^ -~]*'
		)`
		args = append(args, pattern)
	}
	return query, args
}

func selectFoldedNameCandidates(ctx context.Context, tx *sql.Tx, generation int64, needle string, limit int, filter graph.NameSearchFilter) ([]graph.NodeSearchKey, bool, error) {
	query, args := foldedNameCandidatesSQL(generation, needle)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	set := graph.NewFoldedNameCandidateSet(needle, limit)
	for rows.Next() {
		var key graph.NodeSearchKey
		if err := rows.Scan(&key.ID, &key.Name, &key.Kind, &key.FilePath, &key.RepoPrefix, &key.WorkspaceID, &key.ProjectID); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		if err := ctx.Err(); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		compact := &graph.Node{ID: key.ID, Name: key.Name, Kind: key.Kind, FilePath: key.FilePath, RepoPrefix: key.RepoPrefix, WorkspaceID: key.WorkspaceID, ProjectID: key.ProjectID}
		if filter.Allows(compact) {
			set.Add(key)
		}
	}
	rowsErr, closeErr := rows.Err(), rows.Close()
	if rowsErr != nil {
		return nil, false, rowsErr
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	return set.Keys(), set.More(), ctx.Err()
}

var _ graph.FoldedNameCandidateReader = (*Store)(nil)
