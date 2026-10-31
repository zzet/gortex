package mcp

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

func (s *Server) deadCodeRepoPrefixes(ctx context.Context) []string {
	narrow := repoAllowFromContext(ctx)
	opts, active := s.scopeOptionsWithRepoNarrow(ctx, narrow)
	if !active {
		return nil
	}
	if strings.HasPrefix(opts.WorkspaceID, unresolvedWorkspacePrefix) {
		return []string{}
	}
	if len(opts.RepoAllow) == 0 {
		// A workspace slug is not a repository prefix. Without a resolved
		// repo set, retain the metadata-filtered fallback rather than guess
		// ownership from a registry that may be mid-untrack/reconciliation.
		return nil
	}
	prefixes := make([]string, 0, len(opts.RepoAllow)+1)
	for prefix, allowed := range opts.RepoAllow {
		if allowed {
			prefixes = append(prefixes, prefix)
		}
	}
	// Unlike outline's NodeScope, dead_code's explicit analyze repo narrow
	// excludes the unowned bucket. A session ceiling alone still admits it.
	if len(narrow) == 0 && !opts.RepoAllow[""] {
		prefixes = append(prefixes, "")
	}
	sort.Strings(prefixes)
	return prefixes
}

func (s *Server) deadCodeProcesses(ctx context.Context, reader graph.Reader) (*analysis.ProcessResult, error) {
	if s.scopeFiltersActive(ctx) {
		if query, header, current := s.activeAnalysisQueryForAnswer(); current {
			// Query the published base analysis, but admit entry points through the
			// selected view. Steps of an admitted process remain global evidence.
			if selected, ok := query.(graph.Reader); ok {
				if bound, ok := graph.BindReadContext(selected, ctx).(graph.AnalysisQueryStore); ok {
					query = bound
				}
			}
			result, err := s.readDeadCodeProcesses(ctx, reader, query, header)
			if errors.Is(err, graph.ErrAnalysisGenerationInactive) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			_, after, current := s.activeAnalysisQueryForAnswer()
			if !current || after.GenerationID != header.GenerationID || after.GraphRevision != header.GraphRevision {
				return nil, nil
			}
			return result, nil
		}
	}

	processes := s.getProcesses()
	if processes == nil || !s.scopeFiltersActive(ctx) {
		return processes, nil
	}
	ids := make([]string, 0, len(processes.Processes))
	for _, process := range processes.Processes {
		ids = append(ids, process.EntryPoint)
	}
	nodes, err := deadCodeEntryNodes(ctx, reader, ids)
	if err != nil {
		return nil, err
	}
	filtered := *processes
	filtered.Processes = nil
	for _, process := range processes.Processes {
		if s.analyzeNodeVisible(ctx, nodes[process.EntryPoint]) {
			filtered.Processes = append(filtered.Processes, process)
		}
	}
	return &filtered, nil
}

// readDeadCodeProcesses pages compact headers for admission and never reads
// steps of a foreign-entry process. No scalar endpoint lookups or full-process
// compatibility map is needed by the dead-code consumer.
func (s *Server) readDeadCodeProcesses(ctx context.Context, reader graph.Reader, query graph.AnalysisQueryStore, header graph.AnalysisGenerationHeader) (*analysis.ProcessResult, error) {
	result := &analysis.ProcessResult{Truncated: header.ProcessesTruncated, TruncationReason: header.ProcessesTruncationReason}
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, next, err := query.ListAnalysisProcessSummaries(header.GenerationID, analysisGenerationQueryPage, cursor)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.EntryPoint)
		}
		nodes, err := deadCodeEntryNodes(ctx, reader, ids)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if !s.analyzeNodeVisible(ctx, nodes[row.EntryPoint]) {
				continue
			}
			process := analysis.Process{ID: row.ID, Name: row.Name, EntryPoint: row.EntryPoint, StepCount: row.StepCount, Score: row.Score, Truncated: row.Truncated, Files: append([]string(nil), row.Files...)}
			ordinal := -1
			for {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				steps, nextOrdinal, err := query.AnalysisProcessSteps(header.GenerationID, row.ID, analysisGenerationQueryPage, ordinal)
				if err != nil {
					return nil, err
				}
				for _, step := range steps {
					process.Steps = append(process.Steps, analysis.Step{ID: step.NodeID, Depth: step.Depth})
				}
				if len(steps) == 0 || nextOrdinal <= ordinal {
					break
				}
				ordinal = nextOrdinal
			}
			result.Processes = append(result.Processes, process)
		}
		if len(rows) == 0 || next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return result, ctx.Err()
}

// deadCodeEntryNodes reads process entry points for scope admission under the
// request: a cancellable batch lookup when the reader has one, the reader
// bound to ctx otherwise. A bound lookup that stops with the request returns
// no nodes, so an ended request reports ctx's error instead of admitting none.
func deadCodeEntryNodes(ctx context.Context, reader graph.Reader, ids []string) (map[string]*graph.Node, error) {
	var nodes map[string]*graph.Node
	if checked, ok := reader.(interface {
		GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error)
	}); ok {
		var err error
		if nodes, err = checked.GetNodesByIDsContext(ctx, ids); err != nil {
			return nil, err
		}
	} else {
		nodes = graph.BindReadContext(reader, ctx).GetNodesByIDs(ids)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}
