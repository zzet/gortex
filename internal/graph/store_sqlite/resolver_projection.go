package store_sqlite

import (
	"container/heap"
	"database/sql"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

const resolverProjectionPageSize = 256

// ScopeBindingNodesSeq projects only lexical binding fields. Pages are fully
// consumed and closed before yielding, so consumers may safely re-enter Store.
func (s *Store) ScopeBindingNodesSeq(kinds ...graph.NodeKind) iter.Seq[graph.ScopeBindingNode] {
	return resolverNodeProjectionSeq(
		s,
		kinds,
		nil,
		`id, kind, name, start_line`,
		func(rows *sql.Rows) (graph.ScopeBindingNode, string, error) {
			var row graph.ScopeBindingNode
			err := rows.Scan(&row.ID, &row.Kind, &row.Name, &row.StartLine)
			return row, row.ID, err
		},
	)
}

// CallableBindingNodesSeq projects only callable binding fields. Pages are
// fully consumed and closed before yielding, so consumers may safely re-enter.
func (s *Store) CallableBindingNodesSeq(kinds ...graph.NodeKind) iter.Seq[graph.CallableBindingNode] {
	return resolverNodeProjectionSeq(
		s,
		kinds,
		nil,
		`id, kind, name, file_path`,
		func(rows *sql.Rows) (graph.CallableBindingNode, string, error) {
			var row graph.CallableBindingNode
			err := rows.Scan(&row.ID, &row.Kind, &row.Name, &row.FilePath)
			return row, row.ID, err
		},
	)
}

func resolverNodeProjectionSeq[T any](
	s *Store,
	kinds []graph.NodeKind,
	repoPrefixes []string,
	columns string,
	scan func(*sql.Rows) (T, string, error),
) iter.Seq[T] {
	uniq, _ := aggDedupeNodeKinds(kinds)
	if repoPrefixes != nil {
		prefixes := resolverProjectionRepoPrefixes(repoPrefixes)
		return resolverScopedNodeProjectionSeq(s, uniq, prefixes, columns, scan)
	}
	return func(yield func(T) bool) {
		for _, kind := range uniq {
			baseArgs := []any{kind}
			var highWater string
			err := s.db.QueryRow(
				`SELECT id FROM nodes WHERE kind = ? AND view_gen = ? ORDER BY id DESC LIMIT 1`,
				kind, s.viewGen,
			).Scan(&highWater)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				panicOnFatal(err)
				return
			}

			after := ""
			firstPage := true
			page := make([]T, 0, resolverProjectionPageSize)
			for {
				query := `SELECT ` + columns + `
FROM nodes
WHERE kind = ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`
				args := append([]any(nil), baseArgs...)
				args = append(args, highWater, s.viewGen, resolverProjectionPageSize)
				if !firstPage {
					query = `SELECT ` + columns + `
FROM nodes
WHERE kind = ? AND id > ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`
					args = append([]any(nil), baseArgs...)
					args = append(args, after, highWater, s.viewGen, resolverProjectionPageSize)
				}
				rows, queryErr := s.db.Query(query, args...)
				if queryErr != nil {
					panicOnFatal(queryErr)
					return
				}
				page = page[:0]
				var lastID string
				for rows.Next() {
					row, id, scanErr := scan(rows)
					if scanErr != nil {
						_ = rows.Close()
						panicOnFatal(scanErr)
						return
					}
					page = append(page, row)
					lastID = id
				}
				rowsErr := rows.Err()
				closeErr := rows.Close()
				if rowsErr != nil {
					panicOnFatal(rowsErr)
					return
				}
				if closeErr != nil {
					panicOnFatal(closeErr)
					return
				}
				if len(page) == 0 {
					break
				}
				after = lastID
				for _, row := range page {
					if !yield(row) {
						return
					}
				}
				if len(page) < resolverProjectionPageSize || after == highWater {
					break
				}
				firstPage = false
			}
		}
	}
}

const resolverProjectionBufferBudget = 4096

type resolverProjectionPageRow[T any] struct {
	value T
	id    string
}

type resolverProjectionPrefixState[T any] struct {
	prefix    string
	highWater string
	after     string
	page      []resolverProjectionPageRow[T]
	next      int
	started   bool
	exhausted bool
}

type resolverProjectionHeapItem[T any] struct {
	row   resolverProjectionPageRow[T]
	state int
}

type resolverProjectionHeap[T any] []resolverProjectionHeapItem[T]

func (h resolverProjectionHeap[T]) Len() int { return len(h) }
func (h resolverProjectionHeap[T]) Less(i, j int) bool {
	if h[i].row.id == h[j].row.id {
		return h[i].state < h[j].state
	}
	return h[i].row.id < h[j].row.id
}
func (h resolverProjectionHeap[T]) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *resolverProjectionHeap[T]) Push(value any) {
	*h = append(*h, value.(resolverProjectionHeapItem[T]))
}
func (h *resolverProjectionHeap[T]) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	old[last] = resolverProjectionHeapItem[T]{}
	*h = old[:last]
	return value
}

// resolverScopedNodeProjectionSeq reads one indexed keyset stream per
// repository and merges the closed page buffers by ID. This preserves the
// previous global ORDER BY id contract without making SQLite rebuild a
// temporary sort for every multi-repository page.
func resolverScopedNodeProjectionSeq[T any](
	s *Store,
	kinds []graph.NodeKind,
	prefixes []string,
	columns string,
	scan func(*sql.Rows) (T, string, error),
) iter.Seq[T] {
	return func(yield func(T) bool) {
		if len(kinds) == 0 || len(prefixes) == 0 {
			return
		}
		for _, kind := range kinds {
			if !streamResolverScopedKindProjection(s, kind, prefixes, columns, scan, yield) {
				return
			}
		}
	}
}

const resolverScopedProjectionHighWaterQuery = `SELECT id FROM nodes WHERE repo_prefix = ? AND kind = ? AND view_gen = ? ORDER BY id DESC LIMIT 1`

func resolverScopedProjectionPageQuery(columns string, started bool) string {
	if started {
		return `SELECT ` + columns + `
FROM nodes
WHERE repo_prefix = ? AND kind = ? AND id > ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`
	}
	return `SELECT ` + columns + `
FROM nodes
WHERE repo_prefix = ? AND kind = ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`
}

func streamResolverScopedKindProjection[T any](
	s *Store,
	kind graph.NodeKind,
	prefixes []string,
	columns string,
	scan func(*sql.Rows) (T, string, error),
	yield func(T) bool,
) bool {
	states := make([]resolverProjectionPrefixState[T], 0, len(prefixes))
	for _, prefix := range prefixes {
		var highWater string
		err := s.db.QueryRow(
			resolverScopedProjectionHighWaterQuery, prefix, kind, s.viewGen,
		).Scan(&highWater)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			panicOnFatal(err)
			return false
		}
		states = append(states, resolverProjectionPrefixState[T]{
			prefix: prefix, highWater: highWater,
		})
	}
	if len(states) == 0 {
		return true
	}

	pageSize := resolverProjectionPageSize
	if perPrefix := resolverProjectionBufferBudget / len(states); perPrefix < pageSize {
		pageSize = perPrefix
	}
	if pageSize < 1 {
		pageSize = 1
	}
	for i := range states {
		states[i].page = make([]resolverProjectionPageRow[T], 0, pageSize)
	}

	pending := make(resolverProjectionHeap[T], 0, len(states))
	for i := range states {
		if !fillResolverProjectionPage(s, kind, columns, scan, pageSize, &states[i]) {
			return false
		}
		if len(states[i].page) > 0 {
			pending = append(pending, resolverProjectionHeapItem[T]{row: states[i].page[0], state: i})
			states[i].next = 1
		}
	}
	heap.Init(&pending)

	for pending.Len() > 0 {
		item := heap.Pop(&pending).(resolverProjectionHeapItem[T])
		if !yield(item.row.value) {
			return false
		}

		state := &states[item.state]
		if state.next >= len(state.page) && !state.exhausted {
			if !fillResolverProjectionPage(s, kind, columns, scan, pageSize, state) {
				return false
			}
		}
		if state.next < len(state.page) {
			heap.Push(&pending, resolverProjectionHeapItem[T]{
				row: state.page[state.next], state: item.state,
			})
			state.next++
		}
	}
	return true
}

func fillResolverProjectionPage[T any](
	s *Store,
	kind graph.NodeKind,
	columns string,
	scan func(*sql.Rows) (T, string, error),
	pageSize int,
	state *resolverProjectionPrefixState[T],
) bool {
	query := resolverScopedProjectionPageQuery(columns, state.started)
	args := []any{state.prefix, kind, state.highWater, s.viewGen, pageSize}
	if state.started {
		args = []any{state.prefix, kind, state.after, state.highWater, s.viewGen, pageSize}
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		panicOnFatal(err)
		return false
	}

	state.page = state.page[:0]
	state.next = 0
	var lastID string
	for rows.Next() {
		value, id, scanErr := scan(rows)
		if scanErr != nil {
			_ = rows.Close()
			panicOnFatal(scanErr)
			return false
		}
		state.page = append(state.page, resolverProjectionPageRow[T]{value: value, id: id})
		lastID = id
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil {
		panicOnFatal(rowsErr)
		return false
	}
	if closeErr != nil {
		panicOnFatal(closeErr)
		return false
	}

	state.started = true
	if len(state.page) == 0 {
		state.exhausted = true
		return true
	}
	state.after = lastID
	state.exhausted = len(state.page) < pageSize || state.after == state.highWater
	return true
}

func resolverProjectionRepoPrefixes(prefixes []string) []string {
	seen := make(map[string]struct{}, len(prefixes))
	uniq := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		if _, duplicate := seen[prefix]; duplicate {
			continue
		}
		seen[prefix] = struct{}{}
		uniq = append(uniq, prefix)
	}
	return uniq
}

// FileLanguageNodesSeq projects the fixed file ID/language pair.
func (s *Store) FileLanguageNodesSeq() iter.Seq[graph.FileLanguageNode] {
	return resolverNodeProjectionSeq(
		s, []graph.NodeKind{graph.KindFile}, nil, `id, language`,
		func(rows *sql.Rows) (graph.FileLanguageNode, string, error) {
			var row graph.FileLanguageNode
			err := rows.Scan(&row.ID, &row.Language)
			return row, row.ID, err
		},
	)
}

// RepoNodeIdentitiesSeq projects IDs and repository owners for exact kinds.
func (s *Store) RepoNodeIdentitiesSeq(repoPrefixes []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	return resolverNodeProjectionSeq(
		s, kinds, repoPrefixes, `id, repo_prefix`,
		func(rows *sql.Rows) (graph.RepoNodeIdentity, string, error) {
			var row graph.RepoNodeIdentity
			err := rows.Scan(&row.ID, &row.RepoPrefix)
			return row, row.ID, err
		},
	)
}

// NamedLanguageNodesSeq projects fixed name/language binding fields.
func (s *Store) NamedLanguageNodesSeq(kinds ...graph.NodeKind) iter.Seq[graph.NamedLanguageNode] {
	return resolverNodeProjectionSeq(
		s, kinds, nil, `id, name, language`,
		func(rows *sql.Rows) (graph.NamedLanguageNode, string, error) {
			var row graph.NamedLanguageNode
			err := rows.Scan(&row.ID, &row.Name, &row.Language)
			return row, row.ID, err
		},
	)
}

// QualifiedNodeIdentitiesSeq projects fixed qualified target fields.
func (s *Store) QualifiedNodeIdentitiesSeq(kinds ...graph.NodeKind) iter.Seq[graph.QualifiedNodeIdentity] {
	return resolverNodeProjectionSeq(
		s, kinds, nil, `id, name, qual_name, language, file_path, repo_prefix`,
		func(rows *sql.Rows) (graph.QualifiedNodeIdentity, string, error) {
			var row graph.QualifiedNodeIdentity
			err := rows.Scan(
				&row.ID, &row.Name, &row.QualName,
				&row.Language, &row.FilePath, &row.RepoPrefix,
			)
			return row, row.ID, err
		},
	)
}

// FileNodeIdentitiesSeq keeps resolver directory indexes on a metadata-free
// file projection. Nil prefixes mean global; non-nil empty means no rows. The
// ID keyset preserves the prior deterministic candidate order, freezes an
// upper boundary, and closes each bounded page before yielding.
func (s *Store) FileNodeIdentitiesSeq(repoPrefixes []string) iter.Seq[graph.FileNodeIdentity] {
	return resolverNodeProjectionSeq(
		s, []graph.NodeKind{graph.KindFile}, repoPrefixes,
		`id, file_path, repo_prefix, workspace_id`,
		func(rows *sql.Rows) (graph.FileNodeIdentity, string, error) {
			var row graph.FileNodeIdentity
			err := rows.Scan(&row.ID, &row.FilePath, &row.RepoPrefix, &row.WorkspaceID)
			return row, row.ID, err
		},
	)
}

// GetInEdgeIdentitiesByNodeIDs resolves an explicit incoming-adjacency batch
// without transferring or decoding edge payload, provenance, or metadata.
func (s *Store) GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]graph.EdgeIdentity {
	uniq := dedupeNonEmpty(ids)
	out := make(map[string][]graph.EdgeIdentity, len(uniq))
	for start := 0; start < len(uniq); start += lookupChunkSize {
		end := minInt(start+lookupChunkSize, len(uniq))
		chunk := uniq[start:end]
		query := `SELECT from_id, to_id, kind, file_path, line FROM edges WHERE to_id IN (` +
			inPlaceholders(len(chunk)) + `) AND view_gen = ?`
		rows, err := s.db.Query(query, append(toAnyArgs(chunk), s.viewGen)...)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		for rows.Next() {
			var identity graph.EdgeIdentity
			if err := rows.Scan(
				&identity.From,
				&identity.To,
				&identity.Kind,
				&identity.FilePath,
				&identity.Line,
			); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return out
			}
			out[identity.To] = append(out[identity.To], identity)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		if err := rows.Close(); err != nil {
			panicOnFatal(err)
			return out
		}
	}
	return out
}

// NodePlacementsByIDs resolves an explicit endpoint batch without decoding
// names, text, promoted metadata, or the residual metadata blob.
func (s *Store) NodePlacementsByIDs(ids []string) map[string]graph.NodePlacement {
	uniq := dedupeNonEmpty(ids)
	out := make(map[string]graph.NodePlacement, len(uniq))
	for start := 0; start < len(uniq); start += lookupChunkSize {
		end := minInt(start+lookupChunkSize, len(uniq))
		chunk := uniq[start:end]
		query := `SELECT id, kind, file_path, repo_prefix FROM nodes WHERE id IN (` +
			inPlaceholders(len(chunk)) + `) AND view_gen = ?`
		rows, err := s.db.Query(query, append(toAnyArgs(chunk), s.viewGen)...)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		for rows.Next() {
			var id string
			var placement graph.NodePlacement
			if err := rows.Scan(&id, &placement.Kind, &placement.FilePath, &placement.RepoPrefix); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return out
			}
			out[id] = placement
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		if err := rows.Close(); err != nil {
			panicOnFatal(err)
			return out
		}
	}
	return out
}

var (
	_ graph.ScopeBindingNodeSequencer      = (*Store)(nil)
	_ graph.CallableBindingNodeSequencer   = (*Store)(nil)
	_ graph.FileLanguageNodeSequencer      = (*Store)(nil)
	_ graph.RepoNodeIdentitySequencer      = (*Store)(nil)
	_ graph.NamedLanguageNodeSequencer     = (*Store)(nil)
	_ graph.QualifiedNodeIdentitySequencer = (*Store)(nil)
	_ graph.FileNodeIdentitySequencer      = (*Store)(nil)
	_ graph.NodePlacementBatchReader       = (*Store)(nil)
	_ graph.InEdgeIdentityBatchReader      = (*Store)(nil)
)
