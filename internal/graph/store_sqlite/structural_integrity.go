package store_sqlite

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	structuralExactDefaultSamples = 20
	structuralExactMaxSamples     = 100
	structuralAuditRepoExpr       = `COALESCE(NULLIF(n.repo_prefix, ''), 'unknown')`
	structuralAuditReasonExpr     = `CASE WHEN instr(e.to_id, '#param:') > 0 THEN 'parameter_target' ELSE 'local_target' END`
	structuralAuditOriginExpr     = `CASE WHEN trim(COALESCE(e.origin, '')) = '' THEN 'unknown' ELSE lower(substr(trim(e.origin), 1, 48)) END`
	// The LEFT JOIN pairs generations: an edge whose source node is missing
	// from this generation must audit as dangling, not borrow a same-id node
	// from another generation and read as attributed.
	structuralAuditFromSQL = ` FROM edges e LEFT JOIN nodes n ON n.id = e.from_id AND n.view_gen = e.view_gen WHERE e.kind IN (?, ?, ?, ?, ?) AND (instr(e.to_id, '#param:') > 0 OR instr(e.to_id, '#local:') > 0) AND e.view_gen = ?`
)

var structuralAuditKinds = []any{
	string(graph.EdgeImplements),
	string(graph.EdgeExtends),
	string(graph.EdgeOverrides),
	string(graph.EdgeInstantiates),
	string(graph.EdgeMemberOf),
}

var (
	_ graph.StructuralIntegrityEventRecorder = (*Store)(nil)
	_ graph.StructuralIntegritySnapshotter   = (*Store)(nil)
	_ graph.StructuralIntegrityAuditor       = (*Store)(nil)
)

func (s *Store) RecordStructuralIntegrityEvent(event graph.StructuralIntegrityEvent) {
	if s.coreless() {
		return
	}
	s.structuralIntegrity.Record(event)
	switch event.Direction {
	case graph.StructuralDropWrite:
		if s.structuralWriteWarned.CompareAndSwap(false, true) {
			log.Printf("store_sqlite: structurally invalid edge rejected; run analyze(kind=edge_audit) for attributed diagnostics")
		}
	case graph.StructuralDropRead:
		if s.structuralReadWarned.CompareAndSwap(false, true) {
			log.Printf("store_sqlite: store contains structurally invalid legacy edges; suppressing them on read — rebuild or run analyze(kind=edge_audit)")
		}
	}
}

func (s *Store) StructuralIntegritySnapshot(opts graph.StructuralIntegritySnapshotOptions) graph.StructuralIntegritySnapshot {
	if s.coreless() {
		return graph.StructuralIntegritySnapshot{}
	}
	return s.structuralIntegrity.Snapshot(opts)
}

func (s *Store) recordStructuralEdge(direction graph.StructuralDropDirection, path graph.StructuralDropPath, repo string, edge *graph.Edge) {
	if event, ok := graph.StructuralIntegrityEventForEdge(direction, path, repo, edge); ok {
		s.RecordStructuralIntegrityEvent(event)
	}
}

func structuralInputNodeRepos(nodes []*graph.Node) map[string]string {
	var repos map[string]string
	for _, node := range nodes {
		if node == nil || node.ID == "" || node.RepoPrefix == "" {
			continue
		}
		if repos == nil {
			repos = make(map[string]string)
		}
		repos[node.ID] = node.RepoPrefix
	}
	return repos
}

func (s *Store) structuralWriteRepo(edge *graph.Edge, inputRepos map[string]string) string {
	if edge == nil {
		return ""
	}
	if repo := inputRepos[edge.From]; repo != "" {
		return repo
	}
	if source := s.GetNode(edge.From); source != nil {
		return source.RepoPrefix
	}
	return ""
}

func (s *Store) noteStructuralReadDrop(path graph.StructuralDropPath, edge *graph.Edge) {
	// Scanner rows do not carry authoritative source ownership. Do not guess a
	// repository from the edge ID while the cursor is open; exact audit joins
	// the source node explicitly after the routine scan has completed.
	s.recordStructuralEdge(graph.StructuralDropRead, path, "", edge)
}

func normalizedStructuralAuditRepos(repos []string) []string {
	set := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			repo = "unknown"
		}
		set[repo] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for repo := range set {
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

func structuralAuditScopeSQL(repos []string, active bool) (string, []any) {
	repos = normalizedStructuralAuditRepos(repos)
	if len(repos) == 0 {
		if active {
			return " AND 1 = 0", nil
		}
		return "", nil
	}
	placeholders := make([]string, len(repos))
	args := make([]any, len(repos))
	for i, repo := range repos {
		placeholders[i] = "?"
		args[i] = repo
	}
	return " AND " + structuralAuditRepoExpr + " IN (" + strings.Join(placeholders, ", ") + ")", args
}

// structuralAuditArgs follows the placeholder order of structuralAuditFromSQL
// plus the appended repository scope: kinds, generation, then scope.
func structuralAuditArgs(viewGen int64, scopeArgs []any) []any {
	args := make([]any, 0, len(structuralAuditKinds)+len(scopeArgs)+2)
	args = append(args, structuralAuditKinds...)
	args = append(args, viewGen)
	args = append(args, scopeArgs...)
	return args
}

// AuditStructuralIntegrity performs the explicit persisted-row audit. Routine
// status never calls this capability.
func (s *Store) AuditStructuralIntegrity(ctx context.Context, opts graph.StructuralIntegrityAuditOptions) (graph.StructuralIntegrityExactAudit, error) {
	out := graph.StructuralIntegrityExactAudit{Status: graph.StructuralAuditSupported}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	scopeSQL, scopeArgs := structuralAuditScopeSQL(opts.RepoPrefixes, opts.RepoScopeActive)
	groupSQL := `SELECT ` + structuralAuditRepoExpr + `, e.kind, ` + structuralAuditReasonExpr + `, ` + structuralAuditOriginExpr + `, COUNT(*)` +
		structuralAuditFromSQL + scopeSQL + ` GROUP BY 1, 2, 3, 4 ORDER BY 1, 2, 3, 4`
	rows, err := s.db.QueryContext(ctx, groupSQL, structuralAuditArgs(s.viewGen, scopeArgs)...)
	if err != nil {
		return out, fmt.Errorf("store_sqlite: structural integrity grouping: %w", err)
	}
	for rows.Next() {
		var (
			group  graph.StructuralIntegrityExactGroup
			kind   string
			reason string
		)
		if err := rows.Scan(&group.Repo, &kind, &reason, &group.Origin, &group.Count); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("store_sqlite: structural integrity group scan: %w", err)
		}
		group.Kind = graph.EdgeKind(kind)
		group.Reason = graph.StructuralDropReason(reason)
		out.TotalRows += group.Count
		out.Groups = append(out.Groups, group)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, fmt.Errorf("store_sqlite: structural integrity group rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return out, fmt.Errorf("store_sqlite: structural integrity group close: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}

	limit := opts.SampleLimit
	if limit <= 0 {
		limit = structuralExactDefaultSamples
	}
	if limit > structuralExactMaxSamples {
		limit = structuralExactMaxSamples
	}
	sampleSQL := `SELECT ` + structuralAuditRepoExpr + `, e.kind, ` + structuralAuditReasonExpr + `, ` + structuralAuditOriginExpr + `, e.from_id, e.to_id, e.file_path, e.line` +
		structuralAuditFromSQL + scopeSQL + ` ORDER BY 1, 2, 3, 4, e.from_id, e.to_id, e.file_path, e.line, e.id LIMIT ?`
	args := structuralAuditArgs(s.viewGen, scopeArgs)
	args = append(args, limit+1)
	rows, err = s.db.QueryContext(ctx, sampleSQL, args...)
	if err != nil {
		return out, fmt.Errorf("store_sqlite: structural integrity samples: %w", err)
	}
	for rows.Next() {
		var (
			sample graph.StructuralIntegrityExactSample
			kind   string
			reason string
		)
		if err := rows.Scan(&sample.Repo, &kind, &reason, &sample.Origin, &sample.From, &sample.To, &sample.FilePath, &sample.Line); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("store_sqlite: structural integrity sample scan: %w", err)
		}
		sample.Kind = graph.EdgeKind(kind)
		sample.Reason = graph.StructuralDropReason(reason)
		out.Samples = append(out.Samples, sample)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, fmt.Errorf("store_sqlite: structural integrity sample rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return out, fmt.Errorf("store_sqlite: structural integrity sample close: %w", err)
	}
	if len(out.Samples) > limit {
		out.Samples = out.Samples[:limit]
		out.Truncated = true
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}
