package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

type scopedDeadCodeProcessQuery struct {
	graph.AnalysisQueryStore
	rows      []graph.AnalysisProcessSummary
	steps     map[string][]graph.AnalysisProcessStep
	stepReads map[string]int
	fail      error
	cancel    context.CancelFunc
}

func (q *scopedDeadCodeProcessQuery) ListAnalysisProcessSummaries(_ int64, limit int, cursor string) ([]graph.AnalysisProcessSummary, string, error) {
	start := 0
	if cursor != "" {
		start = slices.IndexFunc(q.rows, func(row graph.AnalysisProcessSummary) bool { return row.ID == cursor }) + 1
	}
	end := min(start+limit, len(q.rows))
	next := ""
	if end < len(q.rows) {
		next = q.rows[end-1].ID
	}
	if q.cancel != nil {
		q.cancel()
	}
	return q.rows[start:end], next, nil
}
func (q *scopedDeadCodeProcessQuery) AnalysisProcessSteps(_ int64, id string, limit, cursor int) ([]graph.AnalysisProcessStep, int, error) {
	q.stepReads[id]++
	if q.fail != nil {
		return nil, cursor, q.fail
	}
	steps := q.steps[id]
	start := cursor + 1
	end := min(start+limit, len(steps))
	if start >= len(steps) {
		return nil, cursor, nil
	}
	return steps[start:end], end - 1, nil
}

type deadCodeProcessNodeSpy struct {
	graph.Reader
	t               *testing.T
	batches, scalar int
}

func (r *deadCodeProcessNodeSpy) GetNode(id string) *graph.Node {
	r.scalar++
	return r.Reader.GetNode(id)
}
func (r *deadCodeProcessNodeSpy) GetNodesByIDs(ids []string) map[string]*graph.Node {
	r.batches++
	require.LessOrEqual(r.t, len(ids), analysisGenerationQueryPage)
	return r.Reader.GetNodesByIDs(ids)
}

func TestDeadCodeGenerationProcessesReadOnlyAdmittedSteps(t *testing.T) {
	g := graph.New()
	query := &scopedDeadCodeProcessQuery{steps: make(map[string][]graph.AnalysisProcessStep), stepReads: make(map[string]int)}
	for i := 0; i < 1100; i++ {
		repo := "b"
		if i == 0 {
			repo = "a"
		}
		id := fmt.Sprintf("process%04d", i)
		entry := fmt.Sprintf("entry%04d", i)
		g.AddNode(&graph.Node{ID: entry, RepoPrefix: repo})
		query.rows = append(query.rows, graph.AnalysisProcessSummary{ID: id, Name: id, EntryPoint: entry, StepCount: 2, Score: .5, Files: []string{"file.go"}})
		query.steps[id] = []graph.AnalysisProcessStep{{NodeID: entry, Ordinal: 0}, {NodeID: "foreign-step", Ordinal: 1, Depth: 1}}
	}
	g.AddNode(&graph.Node{ID: "foreign-step", Name: "foreignStep", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/f.go", Language: "go"})
	s := &Server{graph: g, session: newSessionState()}
	ctx := withRepoAllow(t.Context(), map[string]bool{"a": true})
	spy := &deadCodeProcessNodeSpy{Reader: g, t: t}
	result, err := s.readDeadCodeProcesses(ctx, spy, query, graph.AnalysisGenerationHeader{GenerationID: 7})
	require.NoError(t, err)
	require.Len(t, result.Processes, 1)
	require.Equal(t, analysis.Process{ID: "process0000", Name: "process0000", EntryPoint: "entry0000", StepCount: 2, Score: .5, Files: []string{"file.go"}, Steps: []analysis.Step{{ID: "entry0000"}, {ID: "foreign-step", Depth: 1}}}, result.Processes[0])
	withoutProcess := analysis.FindDeadCode(g, nil, nil)
	require.Contains(t, withoutProcess[0].ID, "foreign-step", "fixture: the foreign node needs process reachability evidence")
	require.Empty(t, analysis.FindDeadCode(g, result, nil), "foreign steps of an admitted process remain reachable")
	require.Len(t, query.stepReads, 1, "foreign entry points must never cause step reads")
	require.Equal(t, 3, spy.batches)
	require.Zero(t, spy.scalar)
	sentinel := errors.New("process query unavailable")
	query.fail = sentinel
	_, err = s.readDeadCodeProcesses(ctx, spy, query, graph.AnalysisGenerationHeader{GenerationID: 7})
	require.ErrorIs(t, err, sentinel, "transient query errors must not become partial/full-hydrated success")
	cancelled, cancel := context.WithCancel(ctx)
	query.cancel = cancel
	_, err = s.readDeadCodeProcesses(cancelled, spy, query, graph.AnalysisGenerationHeader{GenerationID: 7})
	require.ErrorIs(t, err, context.Canceled)
}
