package indexer

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestContractIterationLineCarriesItsNeed pins what each iteration line says
// it needs of a writer's quiet gap: the p90 lead and span of its re-asks, its
// enrichment re-check and its fence, the fence's census included, and their
// sum. A re-ask refused because an input moved while it asked is one of the
// iteration's re-asks: its span is what the writer's gap must fit. The job
// line sums the need of the iterations after the first.
func TestContractIterationLineCarriesItsNeed(t *testing.T) {
	const fenceCensus = 25 * time.Millisecond
	const movedSpan = 40 * time.Millisecond
	f := newRouteConstantFixture(t)
	core, logs := observer.New(zapcore.InfoLevel)
	f.logger = zap.New(core)
	var fencing atomic.Bool
	f.adjust = func(snapshot *ContractFollowupSnapshot) {
		scan := snapshot.Scan
		snapshot.Scan = func(ctx context.Context, paths []string) (ContractFollowupScan, error) {
			out, err := scan(ctx, paths)
			if paths == nil && fencing.Load() {
				// The fence's census takes its time.
				time.Sleep(fenceCensus)
				out.CensusDuration += fenceCensus
			}
			return out, err
		}
	}
	consts := jobConsts
	stage := 0
	var firstPPAt time.Time
	hooks := &contractConvergenceHooks{
		roundWait: 2 * time.Second,
		// After the first enrichment round the owner declares one more
		// function: the route's unit is revalidated in a second iteration.
		afterPP: func() {
			if stage == 0 {
				firstPPAt = time.Now()
				stage = 1
				consts = jobConsts + "func Extra() int { return 1 }\n"
				f.saveKeepingKey("consts.go", consts)
			}
		},
		beforeFence: func(int) { fencing.Store(true) },
		fenced:      func(int, []string) { fencing.Store(false) },
		// An apply of the owner opens its window while the second
		// iteration's first re-ask asks, which takes its time: the re-ask is
		// refused for the move and waits for the window to close.
		beforeRevalidateR1: func() {
			if stage == 1 {
				stage = 2
				time.Sleep(movedSpan)
				f.stageInFlight("fixture/consts.go")
			}
		},
	}
	// The apply ends as the job waits for it: the wake carries the next
	// re-ask's lead.
	f.onWait = func() {
		if stage == 2 {
			stage = 3
			f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{"fixture/consts.go"}))
		}
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 3, stage, "the owner's apply opened its window during a re-ask and ended at the wait")
	require.Len(t, report.WindowGaps, 1, "the quiet gap before the window the job waited on")
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": consts, "helper.go": jobHelper})), contractIDs(f.contracts()))

	reasks := logs.FilterMessage("contract analysis reask").All()
	iterations := logs.FilterMessage("contract analysis iteration").All()
	t.Cleanup(func() {
		if t.Failed() {
			for _, line := range append(reasks, iterations...) {
				t.Logf("%s %v", line.Message, line.ContextMap())
			}
		}
	})
	require.NotEmpty(t, reasks)
	require.GreaterOrEqual(t, len(iterations), 2)
	movedIn := -1
	spans, leads := map[int]int{}, map[int]int{}
	for _, line := range reasks {
		fields := line.ContextMap()
		iteration := int(fields["iteration"].(int64))
		spans[iteration]++
		if fields["lead_ms"].(float64) >= 0 {
			leads[iteration]++
		}
		if fields["moved"].(bool) && fields["span_ms"].(float64) >= float64(movedSpan.Milliseconds()) && movedIn < 0 {
			movedIn = iteration
		}
	}
	require.GreaterOrEqual(t, movedIn, 1, "a re-ask refused for a move is logged with its span: %v", reasks)
	var needSum time.Duration
	needIterations := 0
	for _, line := range iterations {
		fields := line.ContextMap()
		iteration := int(fields["iteration"].(int64))
		lead, span := fields["w_lead_p90"].(time.Duration), fields["w_span_p90"].(time.Duration)
		pp, fence, need := fields["w_pp"].(time.Duration), fields["w_fence"].(time.Duration), fields["w_need"].(time.Duration)
		require.Equal(t, lead+span+pp+fence, need, "iteration %d: the need is the sum of its terms", iteration)
		require.Equal(t, fields["pp"], pp)
		require.GreaterOrEqual(t, fence, fenceCensus, "iteration %d: the fence's time includes its census", iteration)
		require.EqualValues(t, spans[iteration], fields["w_spans"], "iteration %d: every re-ask that asked is a span sample", iteration)
		require.EqualValues(t, leads[iteration], fields["w_leads"], "iteration %d: every woken re-ask is a lead sample", iteration)
		if iteration == movedIn {
			require.GreaterOrEqual(t, span, movedSpan, "the re-ask refused for a move counts in the span p90")
			require.Positive(t, fields["w_leads"], "the re-ask after the wake carries a lead")
		}
		if iteration >= 1 {
			needSum += need
			needIterations++
		}
	}
	require.Equal(t, needSum, report.NeedSum, "the job sums the need of the iterations after the first")
	require.Equal(t, needIterations, report.NeedIterations)
	scopes := logs.FilterMessage("contract analysis window scope").All()
	require.Len(t, scopes, 1)
	scope := scopes[0].ContextMap()
	require.NotEmpty(t, scope["window_process"])
	require.NotEmpty(t, scope["last_closes"])
	require.Positive(t, scope["window_job"])
	require.Contains(t, scope["blocked_paths"], "fixture/consts.go")
	require.Positive(t, scope["postextract_since_ns"])
	require.GreaterOrEqual(t, scope["postextract_since_ns"], firstPPAt.UnixNano(), "only iterations after the initial extraction qualify quiet gaps")
	require.GreaterOrEqual(t, scope["measurement_end_ns"], scope["postextract_since_ns"])
	for _, line := range reasks {
		require.Equal(t, scope["window_process"], line.ContextMap()["window_process"])
		require.Equal(t, scope["window_job"], line.ContextMap()["window_job"])
	}

	jobCore, jobLogs := observer.New(zapcore.InfoLevel)
	logContractJob(zap.New(jobCore), viewmetrics.ContractModeCold, viewmetrics.ContractJobPublished, time.Second, ContractFollowupBatchReport{Cohort: report})
	line := jobLogs.FilterMessage("contract analysis job").All()
	require.Len(t, line, 1)
	fields := line[0].ContextMap()
	require.Equal(t, needSum, fields["w_need_sum"])
	require.EqualValues(t, needIterations, fields["w_iterations"])
	require.EqualValues(t, 1, fields["window_n"])
	for _, name := range []string{"window_p10", "window_p50", "window_p90"} {
		require.Equal(t, report.WindowGaps[0], fields[name])
	}
}

// TestContractJournalLogsTheQuietGapBeforeEachWindow pins the window line: a
// save whose staging opens a file's core-write window after the previous
// save's window closed logs the quiet gap between them and how long the
// previous window was open.
func TestContractJournalLogsTheQuietGapBeforeEachWindow(t *testing.T) {
	f := newRouteConstantFixture(t)
	core, logs := observer.New(zapcore.InfoLevel)
	f.mi.mu.RLock()
	idx := f.mi.indexers["fixture"]
	f.mi.mu.RUnlock()
	require.NotNil(t, idx)
	idx.logger = zap.New(core)
	f.save("helper.go", jobHelper+"// first\n")
	logs.TakeAll()
	const quiet = 30 * time.Millisecond
	time.Sleep(quiet)
	f.save("helper.go", jobHelper+"// second\n")
	lines := logs.FilterMessage("contract receipt window").All()
	require.Len(t, lines, 1)
	fields := lines[0].ContextMap()
	require.Equal(t, "fixture", fields["repo"])
	require.Equal(t, filepath.ToSlash("fixture/helper.go"), fields["path"])
	require.Positive(t, fields["seq"])
	require.GreaterOrEqual(t, fields["quiet_ms"].(float64), float64(quiet.Milliseconds()))
	require.Positive(t, fields["open_prev_ms"].(float64))
	gaps := f.store.ContractCoreWindowGaps("fixture", []string{"fixture/helper.go"}, time.Time{})
	require.NotEmpty(t, gaps)
	require.Equal(t, fields["seq"], gaps[len(gaps)-1].Seq, "the line names the window the save opened")
	require.Equal(t, gaps[len(gaps)-1].OpenedAt.UnixNano(), fields["opened_at_ns"])
	require.Equal(t, gaps[len(gaps)-1].ClosedAt.UnixNano(), fields["closed_at_ns"])
	require.Equal(t, "", fields["checkout"])
	require.NotEmpty(t, fields["window_process"])

}
