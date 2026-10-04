package store_sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ContractFileProjectionReader = (*Store)(nil)

const contractOwnerKindsSQL = `('provides','consumes','handles_route')`

func contractProjectionEdgeColumns() string {
	columns := strings.Split(lookupEdgeCols, ", ")
	for i := range columns {
		columns[i] = "e." + columns[i]
	}
	return strings.Join(columns, ", ")
}

// LoadContractFileProjectionContext reads one physical generation. File paths
// are already repository-qualified; sibling owner rows intentionally retain all
// source scopes, so the contracts decoder can preserve shared canonical IDs.
func (s *Store) LoadContractFileProjectionContext(ctx context.Context, repo string, paths []string) (graph.ContractFileProjection, error) {
	return graph.CompleteContractFileProjection(ctx, s, repo, paths)
}

func (s *Store) LoadContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	return graph.CompleteContractIDProjection(ctx, s, ids)
}

func (s *Store) LayerContractFileProjectionContext(ctx context.Context, _ string, paths []string) (graph.ContractFileProjection, error) {
	return s.loadContractProjection(ctx, dedupeNonEmpty(paths), nil, true)
}
func (s *Store) LayerContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	return s.loadContractProjection(ctx, nil, dedupeNonEmpty(ids), false)
}

func (s *Store) loadContractProjection(ctx context.Context, paths, ids []string, byFile bool) (graph.ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.ContractFileProjection{}, err
	}
	if s.coreless() || s.db == nil {
		return graph.ContractFileProjection{}, sql.ErrConnDone
	}
	// Even an empty request checks the connection lifetime. A closed physical
	// layer must not certify an empty composed projection.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	p := graph.ContractFileProjection{FileNodes: make(map[string][]*graph.Node), Targets: make(map[string]*graph.Node), SourceNodes: make(map[string]*graph.Node)}
	budget := graph.ContractProjectionRowLimit
	// Probe inside this read transaction so an optional index is never pinned
	// based on a stale presence memo across a cold bulk schema change.
	var composite bool
	if err := tx.QueryRowContext(ctx, edgesByFileGenerationIndexPresenceSQL, edgesByFileGenerationIndexName).Scan(&composite); err != nil {
		return graph.ContractFileProjection{}, err
	}
	edgeFileIndex := "edges_by_file"
	if composite {
		edgeFileIndex = edgesByFileGenerationIndexName
	}

	if byFile {
		for start := 0; start < len(paths); start += lookupChunkSize {
			chunk := paths[start:min(start+lookupChunkSize, len(paths))]
			args := append(toAnyArgs(chunk), s.viewGen, budget+1)
			query := `SELECT ` + lookupNodeCols + ` FROM nodes INDEXED BY nodes_by_file WHERE file_path IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ? LIMIT ?`
			nodes, err := s.contractProjectionNodes(ctx, tx, query, args, &budget)
			if err != nil {
				return graph.ContractFileProjection{}, err
			}
			for _, node := range nodes {
				p.FileNodes[node.FilePath] = append(p.FileNodes[node.FilePath], node)
				if node.Kind == graph.KindContract {
					p.ScalarNodes = append(p.ScalarNodes, node)
				}
			}
			query = `SELECT COALESCE(n.repo_prefix,''), ` + contractProjectionEdgeColumns() + ` FROM edges AS e INDEXED BY ` + edgeFileIndex + ` LEFT JOIN nodes AS n ON n.id = e.from_id AND n.view_gen = e.view_gen WHERE e.file_path IN (` + inPlaceholders(len(chunk)) + `) AND e.view_gen = ? AND e.kind IN ` + contractOwnerKindsSQL + ` LIMIT ?`
			args[len(args)-1] = budget + 1
			rows, err := s.contractProjectionEdges(ctx, tx, query, args, &budget)
			if err != nil {
				return graph.ContractFileProjection{}, err
			}
			p.OwnerRows = append(p.OwnerRows, rows...)

		}
	}
	ids = dedupeNonEmpty(ids)
	sort.Strings(ids)
	for start := 0; start < len(ids); start += lookupChunkSize {
		chunk := ids[start:min(start+lookupChunkSize, len(ids))]
		args := append(toAnyArgs(chunk), s.viewGen, budget+1)
		query := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE id IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ? LIMIT ?`
		nodes, err := s.contractProjectionNodes(ctx, tx, query, args, &budget)
		if err != nil {
			return graph.ContractFileProjection{}, err
		}
		for _, node := range nodes {
			p.SourceNodes[node.ID] = node
			if node.Kind == graph.KindContract {
				p.Targets[node.ID] = node
			}
		}
		query = `SELECT COALESCE(n.repo_prefix,''), ` + contractProjectionEdgeColumns() + ` FROM edges AS e INDEXED BY edges_by_to LEFT JOIN nodes AS n ON n.id = e.from_id AND n.view_gen = e.view_gen WHERE e.to_id IN (` + inPlaceholders(len(chunk)) + `) AND e.view_gen = ? AND e.kind IN ` + contractOwnerKindsSQL + ` LIMIT ?`
		args[len(args)-1] = budget + 1
		rows, err := s.contractProjectionEdges(ctx, tx, query, args, &budget)
		if err != nil {
			return graph.ContractFileProjection{}, err
		}
		p.OwnerRows = append(p.OwnerRows, rows...)
		query = `SELECT COALESCE(n.repo_prefix,''), ` + contractProjectionEdgeColumns() + ` FROM edges AS e INDEXED BY edges_by_from LEFT JOIN nodes AS n ON n.id = e.from_id AND n.view_gen = e.view_gen WHERE e.from_id IN (` + inPlaceholders(len(chunk)) + `) AND e.view_gen = ? AND e.kind IN ` + contractOwnerKindsSQL + ` LIMIT ?`
		args[len(args)-1] = budget + 1
		rows, err = s.contractProjectionEdges(ctx, tx, query, args, &budget)
		if err != nil {
			return graph.ContractFileProjection{}, err
		}
		p.OutgoingOwnerRows = append(p.OutgoingOwnerRows, rows...)

	}
	if err := tx.Commit(); err != nil {
		return graph.ContractFileProjection{}, err
	}
	return p, nil
}

func (s *Store) contractProjectionNodes(ctx context.Context, tx *sql.Tx, query string, args []any, budget *int) ([]*graph.Node, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*graph.Node
	for rows.Next() {
		node, err := scanNodeCursor(rows)
		if err != nil {
			return nil, err
		}
		if *budget == 0 {
			return nil, graph.ErrContractProjectionLimit
		}
		*budget--
		out = append(out, node)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) contractProjectionEdges(ctx context.Context, tx *sql.Tx, query string, args []any, budget *int) ([]graph.RepoEdgeRow, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []graph.RepoEdgeRow
	for rows.Next() {
		var repo string
		edge, err := s.scanEdgeCursor(repoPrefixedEdgeScanner{scanner: rows, repo: &repo})
		if err != nil {
			return nil, err
		}
		if *budget == 0 {
			return nil, graph.ErrContractProjectionLimit
		}
		*budget--
		out = append(out, graph.RepoEdgeRow{RepoPrefix: repo, Edge: edge})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}
