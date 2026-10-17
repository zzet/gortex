package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// LayerContractFileNodesContext reads full node rows from this physical
// generation. It never reads canonical ownership groups or outgoing edges.
func (s *Store) LayerContractFileNodesContext(ctx context.Context, _ string, paths []string) (map[string][]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.coreless() || s.db == nil {
		return nil, sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out := make(map[string][]*graph.Node)
	budget := graph.ContractProjectionRowLimit
	paths = dedupeNonEmpty(paths)
	for start := 0; start < len(paths); start += lookupChunkSize {
		chunk := paths[start:min(start+lookupChunkSize, len(paths))]
		args := append(toAnyArgs(chunk), s.viewGen, budget+1)
		query := `SELECT ` + lookupNodeCols + ` FROM nodes INDEXED BY nodes_by_file WHERE file_path IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ? LIMIT ?`
		nodes, err := s.contractProjectionNodes(ctx, tx, query, args, &budget)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			out[node.FilePath] = append(out[node.FilePath], node)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
