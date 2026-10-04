package store_sqlite

import (
	"context"
	"database/sql"
	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ContractRepoProjectionReader = (*Store)(nil)
var _ graph.OverlayLayerContractRepoProjectionReader = (*Store)(nil)

func (s *Store) LoadContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	return graph.CompleteContractRepoProjection(ctx, s, repo)
}

func (s *Store) LayerContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.ContractFileProjection{}, err
	}
	if s.coreless() || s.db == nil {
		return graph.ContractFileProjection{}, sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	p := graph.ContractFileProjection{Targets: make(map[string]*graph.Node), SourceNodes: make(map[string]*graph.Node), FileNodes: make(map[string][]*graph.Node)}
	budget := graph.ContractProjectionRowLimit
	query := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE repo_prefix=? AND kind='contract' AND view_gen=? LIMIT ?`
	p.ScalarNodes, err = s.contractProjectionNodes(ctx, tx, query, []any{repo, s.viewGen, budget + 1}, &budget)
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	// Sparse kind-first ownership projection also retains edge-only physical
	// generations whose source node is supplied by a lower selected layer.
	query = `SELECT COALESCE(n.repo_prefix,''), ` + contractProjectionEdgeColumns() + `
 FROM edges AS e LEFT JOIN nodes AS n ON n.id=e.from_id AND n.view_gen=e.view_gen
 WHERE e.kind IN ` + contractOwnerKindsSQL + ` AND e.view_gen=?
 AND (n.repo_prefix=? OR n.id IS NULL) LIMIT ?`
	p.OwnerRows, err = s.contractProjectionEdges(ctx, tx, query, []any{s.viewGen, repo, budget + 1}, &budget)
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	// Meta is persisted using typed binary/legacy codecs, not SQL JSON.
	// Decode through the ordinary checked scanner before filtering explicit
	// namespaces of edge-only seeds; inherited-source attribution stays composed.
	selected := p.OwnerRows[:0]
	for _, row := range p.OwnerRows {
		if row.Edge == nil {
			return graph.ContractFileProjection{}, graph.ErrContractProjectionIncomplete
		}
		if explicit, ok := row.Edge.Meta["contract_owner_repo_prefix"].(string); ok {
			if explicit != repo {
				continue
			}
			row.RepoPrefix = explicit
		}
		selected = append(selected, row)
	}
	p.OwnerRows = selected
	if err := tx.Commit(); err != nil {
		return graph.ContractFileProjection{}, err
	}
	return p, nil
}
