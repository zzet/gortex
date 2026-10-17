package viewmetrics_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// contractSeries is every contract analysis series and its whole vocabulary.
// The real-daemon harness and the daemon tests read these flattened keys out
// of `gortex daemon status`, so a declaration that drops a value must fail
// here rather than collapse silently into {…=other}.
func contractSeries() []struct {
	name   string
	labels []string
	key    string
} {
	type row = struct {
		name   string
		labels []string
		key    string
	}
	var rows []row
	modes := []string{viewmetrics.ContractModeCold, viewmetrics.ContractModeFill, viewmetrics.ContractModeDelta}
	outcomes := []string{
		viewmetrics.ContractJobPublished, viewmetrics.ContractJobFillContinued, viewmetrics.ContractJobSupersededKeyMoved,
		viewmetrics.ContractJobSupersededInputsMoving, viewmetrics.ContractJobSupersededReadUnsettled,
		viewmetrics.ContractJobSupersededPublishRefused, viewmetrics.ContractJobPreempted,
		viewmetrics.ContractJobFailed, viewmetrics.ContractJobPanic, viewmetrics.ContractJobDeferred,
	}
	for _, mode := range modes {
		for _, outcome := range outcomes {
			rows = append(rows, row{viewmetrics.ContractAnalysisJobsTotal, []string{mode, outcome},
				fmt.Sprintf("views_contract_analysis_jobs_total{mode=%s,outcome=%s}", mode, outcome)})
		}
	}
	for _, result := range []string{
		viewmetrics.ContractRequestAdmitted, viewmetrics.ContractRequestCoalesced,
		viewmetrics.ContractRequestAttached, viewmetrics.ContractRequestInputsPending,
		viewmetrics.ContractRequestBaselineAdmitted, viewmetrics.ContractRequestRefused,
		viewmetrics.ContractRequestQueueFull, viewmetrics.ContractRequestError,
	} {
		rows = append(rows, row{viewmetrics.ContractAnalysisRequestsTotal, []string{result},
			"views_contract_analysis_requests_total{result=" + result + "}"})
	}
	for _, result := range []string{viewmetrics.ContractCaptureOK, viewmetrics.ContractCaptureStale, viewmetrics.ContractCaptureError} {
		rows = append(rows, row{viewmetrics.ContractAnalysisCapturesTotal, []string{result},
			"views_contract_analysis_captures_total{result=" + result + "}"})
	}
	for _, reason := range []string{
		viewmetrics.ContractFilesCold, viewmetrics.ContractFilesFill, viewmetrics.ContractFilesDelta,
		viewmetrics.ContractFilesRetry, viewmetrics.ContractFilesStale, viewmetrics.ContractFilesKeyCheck,
		viewmetrics.ContractFilesGoMod, viewmetrics.ContractFilesRouter,
	} {
		rows = append(rows, row{viewmetrics.ContractAnalysisFilesTotal, []string{reason},
			"views_contract_analysis_files_total{reason=" + reason + "}"})
	}
	for _, outcome := range []string{
		viewmetrics.ContractBaselineAdmitted, viewmetrics.ContractBaselineCoalesced,
		viewmetrics.ContractBaselineSaturated, viewmetrics.ContractBaselineCurrent,
		viewmetrics.ContractBaselinePublished, viewmetrics.ContractBaselineRefused,
		viewmetrics.ContractBaselineFailed, viewmetrics.ContractBaselinePanic,
	} {
		rows = append(rows, row{viewmetrics.ContractBaselineTotal, []string{outcome},
			"views_contract_baseline_total{outcome=" + outcome + "}"})
	}
	for _, kind := range []string{viewmetrics.ContractRepairPendingHeader, viewmetrics.ContractRepairStuckReceipt, viewmetrics.ContractRepairStuckEnvelope} {
		rows = append(rows, row{viewmetrics.ContractRepairTotal, []string{kind},
			"views_contract_repair_total{kind=" + kind + "}"})
	}
	for name, key := range map[string]string{
		viewmetrics.ContractAnalysisRoundsTotal:             "views_contract_analysis_rounds_total",
		viewmetrics.ContractAnalysisIterationsTotal:         "views_contract_analysis_iterations_total",
		viewmetrics.ContractAnalysisRequeuedTotal:           "views_contract_analysis_requeued_total",
		viewmetrics.ContractAnalysisUnstampedReadsTotal:     "views_contract_analysis_unstamped_reads_total",
		viewmetrics.ContractAnalysisPredecessorRetiredTotal: "views_contract_analysis_predecessor_retired_total",
	} {
		rows = append(rows, row{name, nil, key})
	}
	return rows
}

// TestContractAnalysisSeriesAreDeclaredWithTheirWholeVocabulary records one
// sample per declared value and asserts the exact flattened key.
//
// Revert-red: drop a value from any contract vocabulary and its row lands
// under {…=other}; drop a series and its rows record nothing.
func TestContractAnalysisSeriesAreDeclaredWithTheirWholeVocabulary(t *testing.T) {
	for _, series := range contractSeries() {
		t.Run(series.key, func(t *testing.T) {
			r := viewmetrics.New()
			r.Count(series.name, series.labels...)
			counters := r.Snapshot().Counters
			require.Len(t, counters, 1, "an undeclared series records nothing")
			require.Equal(t, int64(1), counters[series.key], "the sample landed under %v", counters)
		})
	}
}

// TestContractAnalysisJobSecondsIsADurationPerMode pins the timing series'
// kind and label: a job's body time is observed per mode, never counted.
func TestContractAnalysisJobSecondsIsADurationPerMode(t *testing.T) {
	r := viewmetrics.New()
	r.Count(viewmetrics.ContractAnalysisJobSeconds, viewmetrics.ContractModeCold)
	require.Empty(t, r.Snapshot().Counters, "a duration series is not a counter")
	r.Observe(viewmetrics.ContractAnalysisJobSeconds, 0, viewmetrics.ContractModeDelta)
	require.Len(t, r.Snapshot().Durations, 1)
	for key := range r.Snapshot().Durations {
		require.Equal(t, "views_contract_analysis_job_seconds{mode=delta}", key)
	}
}
