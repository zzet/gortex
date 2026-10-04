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
	query = `SELECT COALESCE(json_extract(e.meta,'$.contract_owner_repo_prefix'),n.repo_prefix,''), ` + contractProjectionEdgeColumns() + `
 FROM edges AS e LEFT JOIN nodes AS n ON n.id=e.from_id AND n.view_gen=e.view_gen
 WHERE e.kind IN ` + contractOwnerKindsSQL + ` AND e.view_gen=?
 AND (COALESCE(json_extract(e.meta,'$.contract_owner_repo_prefix'),n.repo_prefix,'')=?
 OR (n.id IS NULL AND json_type(e.meta,'$.contract_owner_repo_prefix') IS NULL)) LIMIT ?`
	p.OwnerRows, err = s.contractProjectionEdges(ctx, tx, query, []any{s.viewGen, repo, budget + 1}, &budget)
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	if err := tx.Commit(); err != nil {
		return graph.ContractFileProjection{}, err
	}
	return p, nil
}
