package store_sqlite

import (
	"context"
	"database/sql"
	"github.com/zzet/gortex/internal/graph"
)

// Sparse ownership reads must seek both the selected generation and owner kind.
// The kind-only index also contains every historical generation's owner rows.
const contractOwnerGenerationIndexName = "edges_contract_owner_generation"
const contractOwnerGenerationIndexDDL = `CREATE INDEX IF NOT EXISTS edges_contract_owner_generation ON edges(view_gen, kind) WHERE kind IN ` + contractOwnerKindsSQL

var _ graph.ContractRepoProjectionReader = (*Store)(nil)
var _ graph.OverlayLayerContractRepoProjectionReader = (*Store)(nil)
var _ graph.OverlayLayerContractRepoOwnerProjectionReader = (*Store)(nil)

func (s *Store) LoadContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	return graph.CompleteContractRepoProjection(ctx, s, repo)
}

func (s *Store) ContractRepoOwnerProjectionSource() any { return s }

func (s *Store) LayerContractRepoOwnerProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	return s.layerContractRepoProjectionContext(ctx, repo, false)
}

func (s *Store) LayerContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	return s.layerContractRepoProjectionContext(ctx, repo, true)
}

func (s *Store) layerContractRepoProjectionContext(ctx context.Context, repo string, scalars bool) (graph.ContractFileProjection, error) {
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
	if scalars {
		query := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE repo_prefix=? AND kind='contract' AND view_gen=? LIMIT ?`
		p.ScalarNodes, err = s.contractProjectionNodes(ctx, tx, query, []any{repo, s.viewGen, budget + 1}, &budget)
		if err != nil {
			return graph.ContractFileProjection{}, err
		}
	} else {
		// Keep every scalar completion key and its mask path. A selected node
		// replacement can reattribute a lower off-file owner edge into this repo.
		// The kind/generation seek avoids scanning historical repo payloads;
		// canonical completion still checks the complete selected ID groups.
		query := `SELECT id,file_path FROM nodes WHERE +repo_prefix=? AND kind='contract' AND view_gen=? LIMIT ?`
		p.ScalarNodes, err = s.contractProjectionScalarIdentities(ctx, tx, query, []any{repo, s.viewGen, budget + 1}, repo, &budget)
		if err != nil {
			return graph.ContractFileProjection{}, err
		}
	}
	// Sparse kind-first ownership projection also retains edge-only physical
	// generations whose source node is supplied by a lower selected layer.
	query := contractRepoOwnerRowsQuery()
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

// Keep both the physical generation and the three ownership kinds in the
// driving seek. Neither unrelated edge kinds nor historical owner rows belong
// to this projection. All selected rows, joins and metadata checks are unchanged.
func contractRepoOwnerRowsQuery() string {
	return `SELECT COALESCE(n.repo_prefix,''), ` + contractProjectionEdgeColumns() + `
 FROM edges AS e INDEXED BY ` + contractOwnerGenerationIndexName + ` LEFT JOIN nodes AS n ON n.id=e.from_id AND n.view_gen=e.view_gen
 WHERE e.kind IN ` + contractOwnerKindsSQL + ` AND e.view_gen=?
 AND (n.repo_prefix=? OR n.id IS NULL) LIMIT ?`
}

// These rows are completion keys, never hydrated answers. Keep the same bounded
// read transaction and failure semantics as the full scalar projection.
func (s *Store) contractProjectionScalarIdentities(ctx context.Context, tx *sql.Tx, query string, args []any, repo string, budget *int) ([]*graph.Node, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*graph.Node
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		if *budget == 0 {
			return nil, graph.ErrContractProjectionLimit
		}
		*budget--
		out = append(out, &graph.Node{ID: id, Kind: graph.KindContract, FilePath: path, RepoPrefix: repo})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}
