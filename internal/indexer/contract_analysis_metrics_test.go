package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestContractJournalLogsBeginAndAcceptBrackets pins the Debug bracket lines
// that time each path's unaccepted window (the harness's read window).
func TestContractJournalLogsBeginAndAcceptBrackets(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	backend := &contractCoreBackendProbe{known: true}
	journal, err := newContractCoreInputJournal(context.Background(), backend)
	require.NoError(t, err)
	core, logs := observer.New(zapcore.DebugLevel)
	journal.logger = zap.New(core)
	source := []byte("package fixture\nfunc Value() int { return 2 }\n")
	require.NoError(t, journal.prepare(context.Background(), idx, "fixture/value.go", "go", source, nil, false))
	require.NoError(t, journal.begin([]string{"fixture/value.go"}))
	require.NoError(t, journal.accept(nil))
	begin := logs.FilterMessage("contract journal begin").All()
	accept := logs.FilterMessage("contract journal accept").All()
	require.Len(t, begin, 1)
	require.Len(t, accept, 1)
	require.Equal(t, []any{"fixture/value.go"}, begin[0].ContextMap()["paths"])
	require.Equal(t, []any{"fixture/value.go"}, accept[0].ContextMap()["paths"])
	require.True(t, begin[0].Time.Compare(accept[0].Time) <= 0)
}

func contractCounter(key string) int64 {
	return viewmetrics.Default().Snapshot().Counters[key]
}

// TestContractAnalysisCountsRequestsAndJobs pins the job and request series
// at their call sites: an admitted request, the attached answer once the attachment exists,
// one published cold job with its body time, and the job's Info line.
func TestContractAnalysisCountsRequestsAndJobs(t *testing.T) {
	f := newContractWorkerFixture(t)
	a := f.request(t, "repo-a", "metrics-a")
	capture := func(ctx context.Context, _ *ContractCaptureSource, _ *graphview.SelectedContractInputs, _, _ string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		snapshot := a.Snapshot
		snapshot.Release = func() {}
		snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
			return ContractAcceptedSource{Bytes: append([]byte(nil), f.sources[file.Path]...), SourceFingerprint: file.SourceFingerprint, Policy: file.Policy}, ctx.Err()
		}
		return snapshot, []ContractFollowupTarget{{Key: a.Snapshot.Key, Work: a.Snapshot.Work, Catalog: f.store}}, nil
	}
	core, logs := observer.New(zapcore.InfoLevel)
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: capture, Logger: zap.New(core), Yield: func(ctx context.Context) error { return ctx.Err() }})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	admittedBefore := contractCounter("views_contract_analysis_requests_total{result=admitted}")
	attachedBefore := contractCounter("views_contract_analysis_requests_total{result=attached}")
	publishedBefore := contractCounter("views_contract_analysis_jobs_total{mode=fill,outcome=published}")
	secondsBefore := viewmetrics.Default().Snapshot().Durations["views_contract_analysis_job_seconds{mode=fill}"].Count

	inputs := &graphview.SelectedContractInputs{State: graph.ContractInputState{Accepted: true, InputVersion: a.Snapshot.Key.InputVersion, InputFingerprint: a.Snapshot.Key.InputFingerprint}}
	// The cohort's repository announced an accepted mutation before the
	// request: the start line times the job from it.
	c.Published(t.Context(), "repo-a", "")
	// A hang guard over one two-file job, not a latency bound.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	scheduled, status := scheduleAndWait(t, ctx, c, []string{"repo-a"}, "repo-a", inputs)
	require.Equal(t, ContractScheduled, scheduled.Result)
	require.Equal(t, ContractActorPublished, status.Result, "job logs: %v", logs.All())
	header, err := f.store.GetContractAttachmentContext(ctx, a.Snapshot.Key)
	require.NoError(t, err)
	require.NotNil(t, header)
	require.Eventually(t, func() bool {
		return contractCounter("views_contract_analysis_jobs_total{mode=fill,outcome=published}") > publishedBefore
	}, 30*time.Second, 10*time.Millisecond, "the published job is counted once its body returns")
	again, status := scheduleAndWait(t, ctx, c, []string{"repo-a"}, "repo-a", inputs)
	require.Equal(t, ContractNoop, again.Result, "a request at the published inputs is answered at once")
	require.Equal(t, ContractActorPublished, status.Result)
	c.mu.Lock()
	retained := c.deltaCaches.get(scheduled.Actor)
	c.mu.Unlock()
	require.NotNil(t, retained, "an attachment Noop preserves the admitted checkpoint")
	require.True(t, retained.complete)
	require.Len(t, logs.FilterMessage("contract cache borrow").All(), 1, "the Noop never borrows/discards warm state")
	admissions := logs.FilterMessage("contract cache admission").All()
	require.Len(t, admissions, 1)
	require.Equal(t, "admitted", admissions[0].ContextMap()["reason"])

	require.Greater(t, contractCounter("views_contract_analysis_requests_total{result=admitted}"), admittedBefore)
	require.Greater(t, contractCounter("views_contract_analysis_requests_total{result=attached}"), attachedBefore)
	require.Greater(t, viewmetrics.Default().Snapshot().Durations["views_contract_analysis_job_seconds{mode=fill}"].Count, secondsBefore)
	starts := logs.FilterMessage("contract analysis job started").All()
	require.NotEmpty(t, starts, "every job leaves a start line")
	require.Equal(t, viewmetrics.ContractModeFill, starts[0].ContextMap()["mode"])
	require.Equal(t, "repo-a", starts[0].ContextMap()["repo"])
	require.True(t, starts[0].Time.Compare(logs.FilterMessage("contract analysis job").All()[0].Time) <= 0, "the start line precedes the end line")
	start := starts[0].ContextMap()
	require.Equal(t, "interest", start["trigger"], "a request's interest queued the job")
	for _, name := range []string{"since_trigger_ms", "since_accept_ms", "capture_ms"} {
		require.GreaterOrEqual(t, start[name], 0.0, "the start line times %s", name)
	}
	lines := logs.FilterMessage("contract analysis job").All()
	require.NotEmpty(t, lines, "every finished job leaves one phase line")
	fields := lines[0].ContextMap()
	require.Equal(t, start["since_trigger_ms"], fields["dispatch_ms"], "the job line carries the dispatch time")
	require.Equal(t, start["capture_ms"], fields["capture_ms"])
	for _, name := range []string{"w_need_sum", "w_iterations", "window_p10", "window_p50", "window_p90", "window_n"} {
		require.Contains(t, fields, name)
	}
	require.Equal(t, viewmetrics.ContractModeFill, fields["mode"])
	require.Equal(t, viewmetrics.ContractJobPublished, fields["outcome"])
	for _, phase := range []string{"census", "stamps", "scratch", "extract", "rounds", "pp", "payload", "publish"} {
		require.Contains(t, fields, phase, "the phase line names every phase")
	}
	for _, count := range []string{"pp_runs", "pp_reruns", "pp_rechecks", "pp_recheck_reads", "revalidated"} {
		require.Contains(t, fields, count, "the job line counts enrichment runs, re-checks and revalidations")
	}
	require.EqualValues(t, 1, fields["pp_runs"])
	require.EqualValues(t, 0, fields["pp_reruns"])
}
