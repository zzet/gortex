package indexer

import (
	"time"

	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// countContractRequest records what one scheduling request did. Every return
// path of the coordinator's Schedule records exactly one result; a worker
// capture that fails other than stale records one error.
func countContractRequest(result string) {
	viewmetrics.Count(viewmetrics.ContractAnalysisRequestsTotal, result)
}

// countContractCapture records one scheduler capture of an actor's inputs.
func countContractCapture(result string) {
	viewmetrics.Count(viewmetrics.ContractAnalysisCapturesTotal, result)
}

// countContractRequeue records one job put back on its ring after an end
// that left its key current and unpublished.
func countContractRequeue() {
	viewmetrics.Count(viewmetrics.ContractAnalysisRequeuedTotal)
}

// countContractJob records one finished attachment job and its body time.
func countContractJob(mode, outcome string, elapsed time.Duration) {
	viewmetrics.Count(viewmetrics.ContractAnalysisJobsTotal, mode, outcome)
	viewmetrics.Observe(viewmetrics.ContractAnalysisJobSeconds, elapsed, mode)
}

// countContractBaseline records a baseline admission answer or job outcome.
func countContractBaseline(outcome string) {
	viewmetrics.Count(viewmetrics.ContractBaselineTotal, outcome)
}

// countContractFiles records n per-file extractions for one reason.
func countContractFiles(reason string, n int) {
	if n > 0 {
		viewmetrics.Add(viewmetrics.ContractAnalysisFilesTotal, int64(n), reason)
	}
}

// contractJobPhases is the per-job phase ledger logged once when the job
// ends. A phase the job never entered stays zero.
type contractJobPhases struct {
	Census, Stamps, Scratch, Extract, Rounds, PP, Payload, Publish time.Duration
}

func (p contractJobPhases) fields() []zap.Field {
	return []zap.Field{
		zap.Duration("census", p.Census), zap.Duration("stamps", p.Stamps),
		zap.Duration("scratch", p.Scratch), zap.Duration("extract", p.Extract),
		zap.Duration("rounds", p.Rounds), zap.Duration("pp", p.PP),
		zap.Duration("payload", p.Payload), zap.Duration("publish", p.Publish),
	}
}

// logContractJob writes the one Info line a finished job leaves behind.
func logContractJob(logger *zap.Logger, mode, outcome string, elapsed time.Duration, report ContractFollowupBatchReport, extra ...zap.Field) {
	if logger == nil {
		return
	}
	published := 0
	cohort := report.Cohort
	phases := contractJobPhases{
		Census: cohort.CensusDuration, Stamps: cohort.StampsDuration, Scratch: cohort.ScratchDuration,
		Extract: cohort.ExtractDuration, Rounds: cohort.RoundsDuration, PP: cohort.EnrichDuration,
	}
	// Payload assembly and publication are per target; the line sums them.
	for _, target := range report.Targets {
		if target.Report.Published {
			published++
		}
		phases.Payload += target.Report.PayloadDuration
		phases.Publish += target.Report.PublishDuration
	}
	fields := []zap.Field{
		zap.String("mode", mode), zap.String("outcome", outcome), zap.Duration("elapsed", elapsed),
		zap.Int("targets", len(report.Targets)), zap.Int("published", published),
		zap.Int("files", cohort.Files), zap.Int("source_reads", cohort.SourceReads),
		zap.Int("iteration_count", cohort.Iterations), zap.Int("round_count", cohort.Rounds),
		zap.Int("pp_runs", cohort.EnrichRuns), zap.Int("pp_reruns", cohort.EnrichReruns), zap.Int("revalidated", cohort.Revalidated),
		zap.Int("pp_rechecks", cohort.EnrichRechecks), zap.Int("pp_recheck_reads", cohort.EnrichRecheckReads),
		zap.Int("content_kept", cohort.ContentKept),
		// What the iterations after the first needed of a writer's quiet
		// gap, and the gaps the writer left on the paths they waited on.
		zap.Duration("w_need_sum", cohort.NeedSum), zap.Int("w_iterations", cohort.NeedIterations),
		zap.Duration("window_p10", contractQuantile(cohort.WindowGaps, 0.1)),
		zap.Duration("window_p50", contractQuantile(cohort.WindowGaps, 0.5)),
		zap.Duration("window_p90", contractQuantile(cohort.WindowGaps, 0.9)),
		zap.Int("window_n", len(cohort.WindowGaps)),
	}
	fields = append(fields, phases.fields()...)
	fields = append(fields, extra...)
	logger.Info("contract analysis job", fields...)
}

// countContractRepair records one stuck-input repair by kind.
func countContractRepair(kind string) {
	viewmetrics.Count(viewmetrics.ContractRepairTotal, kind)
}
