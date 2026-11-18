package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

type editingContextScopeKey struct{}
type editingContextTransaction struct {
	core   *storeCore
	tx     *sql.Tx
	parent *editingContextTransaction
	budget int
	err    error
}

// WithFileEditingContextReader keeps all full-row bundle reads in one read
// transaction. Selected generations on the same core reuse that transaction,
// avoiding a nested acquisition of SQLite's sole reader connection.
func (s *Store) WithFileEditingContextReader(ctx context.Context, fn func(context.Context, graph.FileEditingContextRows) error) error {
	if ctx == nil || s == nil || s.coreless() {
		return graph.ErrFileEditingContextUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.db == nil {
		return sql.ErrConnDone
	}
	revision, err := s.PayloadInputRevisionContext(ctx)
	if err != nil {
		return err
	}
	head, _ := ctx.Value(editingContextScopeKey{}).(*editingContextTransaction)
	scope := head
	for scope != nil && scope.core != s.storeCore {
		scope = scope.parent
	}
	owned := scope == nil
	if owned {
		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		scope = &editingContextTransaction{core: s.storeCore, tx: tx, parent: head, budget: graph.ContractProjectionRowLimit}
		ctx = context.WithValue(ctx, editingContextScopeKey{}, scope)
	}
	reader := &editingContextSQLReader{ctx: ctx, store: s, scope: scope, nodesByID: make(map[string]*graph.Node), knownIDs: make(map[string]bool)}
	if err := fn(ctx, reader); err != nil {
		return err
	}
	if scope.err != nil {
		return scope.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	after, err := s.PayloadInputRevisionContext(ctx)
	if err != nil {
		return err
	}
	if after != revision {
		return graph.ErrContractProjectionStale
	}
	if owned {
		return scope.tx.Commit()
	}
	return nil
}

// Only the synchronous editing-context reads are exposed; no live store reader
// is embedded, so a new operation cannot silently bypass this transaction.
type editingContextSQLReader struct {
	ctx       context.Context
	store     *Store
	scope     *editingContextTransaction
	nodesByID map[string]*graph.Node
	knownIDs  map[string]bool
}

func (r *editingContextSQLReader) nodes(column string, keys []string) []*graph.Node {
	if r.scope.err != nil {
		return nil
	}
	var result []*graph.Node
	keys = dedupeNonEmpty(keys)
	for start := 0; start < len(keys); start += lookupChunkSize {
		chunk := keys[start:min(start+lookupChunkSize, len(keys))]
		args := append(toAnyArgs(chunk), r.store.viewGen, r.scope.budget+1)
		query := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE ` + column + ` IN (` + inPlaceholders(len(chunk)) + `) AND view_gen=? LIMIT ?`
		rows, err := r.store.contractProjectionNodes(r.ctx, r.scope.tx, query, args, &r.scope.budget)
		if err != nil {
			r.scope.err = err
			return nil
		}
		for _, node := range rows {
			r.nodesByID[node.ID] = node
			r.knownIDs[node.ID] = true
		}
		result = append(result, rows...)
	}
	return result
}
func (r *editingContextSQLReader) GetFileNodes(path string) []*graph.Node {
	return r.nodes("file_path", []string{path})
}
func (r *editingContextSQLReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	out := make(map[string]*graph.Node)
	var missing []string
	for _, id := range dedupeNonEmpty(ids) {
		if !r.knownIDs[id] {
			missing = append(missing, id)
		}
	}
	rows := r.nodes("id", missing)
	if r.scope.err != nil {
		return nil
	}
	for _, id := range missing {
		r.knownIDs[id] = true
	}
	for _, n := range rows {
		r.nodesByID[n.ID] = n
	}
	for _, id := range ids {
		if n := r.nodesByID[id]; n != nil {
			out[id] = n
		}
	}
	return out
}

func (r *editingContextSQLReader) GetNode(id string) *graph.Node {
	return r.GetNodesByIDs([]string{id})[id]
}
func (r *editingContextSQLReader) edges(column string, ids []string) map[string][]*graph.Edge {
	if r.scope.err != nil {
		return nil
	}
	out := make(map[string][]*graph.Edge)
	ids = dedupeNonEmpty(ids)
	for start := 0; start < len(ids); start += lookupChunkSize {
		chunk := ids[start:min(start+lookupChunkSize, len(ids))]
		args := append(toAnyArgs(chunk), r.store.viewGen, r.scope.budget+1)
		query := `SELECT COALESCE(n.repo_prefix,''), ` + contractProjectionEdgeColumns() + ` FROM edges AS e LEFT JOIN nodes AS n ON n.id=e.from_id AND n.view_gen=e.view_gen WHERE e.` + column + ` IN (` + inPlaceholders(len(chunk)) + `) AND e.view_gen=? LIMIT ?`
		rows, err := r.store.contractProjectionEdges(r.ctx, r.scope.tx, query, args, &r.scope.budget)
		if err != nil {
			r.scope.err = err
			return nil
		}
		for _, row := range rows {
			id := row.Edge.From
			if column == "to_id" {
				id = row.Edge.To
			}
			out[id] = append(out[id], row.Edge)
		}
	}
	return out
}
func (r *editingContextSQLReader) GetOutEdges(id string) []*graph.Edge {
	return r.GetOutEdgesByNodeIDs([]string{id})[id]
}
func (r *editingContextSQLReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return r.edges("from_id", ids)
}
func (r *editingContextSQLReader) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return r.edges("to_id", ids)
}
