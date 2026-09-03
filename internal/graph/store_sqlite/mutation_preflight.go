package store_sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

type sqliteEdgeIdentity struct {
	from     string
	to       string
	kind     graph.EdgeKind
	filePath string
	line     int
}

func sqliteIdentityForEdge(e *graph.Edge) sqliteEdgeIdentity {
	return sqliteEdgeIdentity{
		from: e.From, to: e.To, kind: e.Kind, filePath: e.FilePath, line: e.Line,
	}
}

// batchContainsNewEdgeLocked determines whether AddBatch can change topology.
// It runs only while an active analysis generation exists; ordinary indexing
// pays no preflight cost. Composite keys are checked in bounded queries so an
// idempotent enrichment batch preserves the expensive warm analysis cache.
func (s *Store) batchContainsNewEdgeLocked(edges []*graph.Edge) (bool, int, error) {
	const keysPerQuery = 180 // five bound values per key plus the generation; remain below SQLite's 999 limit
	unique := make(map[sqliteEdgeIdentity]struct{}, len(edges))
	keys := make([]sqliteEdgeIdentity, 0, len(edges))
	for _, edge := range edges {
		if edge == nil || graph.IsProxyID(edge.From) || graph.IsProxyID(edge.To) {
			continue
		}
		key := sqliteIdentityForEdge(edge)
		if _, exists := unique[key]; exists {
			continue
		}
		unique[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return false, 0, nil
	}

	found := make(map[sqliteEdgeIdentity]struct{}, len(keys))
	statements := 0
	for start := 0; start < len(keys); start += keysPerQuery {
		end := start + keysPerQuery
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		var values strings.Builder
		args := make([]any, 0, len(chunk)*5+1)
		for i, key := range chunk {
			if i > 0 {
				values.WriteByte(',')
			}
			values.WriteString(`(?,?,?,?,?)`)
			args = append(args, key.from, key.to, string(key.kind), key.filePath, key.line)
		}
		// The insert this gates writes at the handle's generation, so an
		// identical edge in another one is still a new edge here.
		args = append(args, s.viewGen)
		rows, err := s.queryActiveWriteLocked(context.Background(),
			`WITH wanted(from_id, to_id, kind, file_path, line) AS (VALUES `+values.String()+`)
             SELECT e.from_id, e.to_id, e.kind, e.file_path, e.line
             FROM wanted AS w
             JOIN edges AS e
               ON e.from_id = w.from_id AND e.to_id = w.to_id
              AND e.kind = w.kind AND e.file_path = w.file_path AND e.line = w.line
              AND e.view_gen = ?`,
			args...,
		)
		if err != nil {
			return false, statements, err
		}
		statements++
		for rows.Next() {
			var key sqliteEdgeIdentity
			var kind string
			if err := rows.Scan(&key.from, &key.to, &kind, &key.filePath, &key.line); err != nil {
				_ = rows.Close()
				return false, statements, err
			}
			key.kind = graph.EdgeKind(kind)
			found[key] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return false, statements, err
		}
		_ = rows.Close()
	}
	return len(found) != len(keys), statements, nil
}

// batchContainsAnalysisNodeChangeLocked compares every analysis-facing node
// field in bounded VALUES joins. The old AddBatch path called the single-node
// preflight once per input row, yielding one SELECT + Meta decode per node.
func (s *Store) batchContainsAnalysisNodeChangeLocked(nodes []*graph.Node) (bool, int, error) {
	const idsPerQuery = 450
	incoming := make(map[string][]*graph.Node, len(nodes))
	ordered := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node == nil || node.ID == "" || graph.IsProxyNode(node) {
			continue
		}
		if _, exists := incoming[node.ID]; !exists {
			ordered = append(ordered, node.ID)
		}
		incoming[node.ID] = append(incoming[node.ID], node)
	}
	if len(ordered) == 0 {
		return false, 0, nil
	}

	statements := 0
	for start := 0; start < len(ordered); start += idsPerQuery {
		end := start + idsPerQuery
		if end > len(ordered) {
			end = len(ordered)
		}
		ids := ordered[start:end]
		var values strings.Builder
		args := make([]any, 0, len(ids)+1)
		for i, id := range ids {
			if i > 0 {
				values.WriteByte(',')
			}
			values.WriteString(`(?)`)
			args = append(args, id)
		}
		// Same generation the upsert lands on: a row missing from this
		// generation is a new node even when another generation carries its id.
		args = append(args, s.viewGen)
		rows, err := s.queryActiveWriteLocked(context.Background(), `
WITH wanted(id) AS (VALUES `+values.String()+`)
SELECT n.id, n.kind, n.name, n.qual_name, n.file_path,
       n.start_line, n.end_line, n.start_column, n.end_column,
       n.language, n.repo_prefix, n.workspace_id, n.project_id,
       n.visibility, n.entry_point, n.entry_point_kind
FROM wanted AS w
JOIN nodes AS n ON n.id = w.id AND n.view_gen = ?`, args...)
		if err != nil {
			return false, statements, err
		}
		statements++
		found := make(map[string]struct{}, len(ids))
		for rows.Next() {
			var (
				id, kind, name, qualName, filePath   string
				language, repo, workspace, project   string
				startLine, endLine, startCol, endCol int
				visibility, entryPointKind           sql.NullString
				entryPoint                           sql.NullBool
			)
			if err := rows.Scan(
				&id, &kind, &name, &qualName, &filePath,
				&startLine, &endLine, &startCol, &endCol,
				&language, &repo, &workspace, &project,
				&visibility, &entryPoint, &entryPointKind,
			); err != nil {
				_ = rows.Close()
				return false, statements, err
			}
			// Promoted columns only — see invalidateAnalysisBeforeNodeMutationLocked
			// for the pre-promotion-row over-invalidation trade.
			storedEntry := entryPoint.Valid && entryPoint.Bool
			storedEntryKind := entryPointKind.String
			for _, node := range incoming[id] {
				newVisibility, _ := node.Meta["visibility"].(string)
				newEntry, _ := node.Meta["entry_point"].(bool)
				newEntryKind, _ := node.Meta["entry_point_kind"].(string)
				if kind != string(node.Kind) || name != node.Name || qualName != node.QualName ||
					filePath != node.FilePath || startLine != node.StartLine || endLine != node.EndLine ||
					startCol != node.StartColumn || endCol != node.EndColumn || language != node.Language ||
					repo != node.RepoPrefix || workspace != node.WorkspaceID || project != node.ProjectID ||
					visibility.String != newVisibility || storedEntry != newEntry ||
					(storedEntry && storedEntryKind != newEntryKind) {
					_ = rows.Close()
					return true, statements, nil
				}
			}
			found[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return false, statements, err
		}
		if err := rows.Close(); err != nil {
			return false, statements, err
		}
		if len(found) != len(ids) {
			return true, statements, nil
		}
	}
	return false, statements, nil
}
