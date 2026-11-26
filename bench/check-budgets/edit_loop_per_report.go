package main

import "errors"

// Single-report tables expose the already-validated observations without
// inventing repetitions or relaxing the existing aggregate input contract.
const editLoopPerReportScope = "PER_VALIDATED_REPORT_ONLY_NOT_THREE_RUN_STABILITY"

const compactEditLoopQualifiedReason = "compact freshness metadata is unobservable under the accepted contract; actual supported content/tool and unsupported-refusal proofs passed; no numeric silent-stale claim"

// The original per-report compact checks are shared by the diagnostic table
// and the aggregate's unchanged loop over every required actual repeat.
func validateCompactEditLoopPhase(phase phase) error {
	wire, err := decodeEditLoopWire(phase)
	if err != nil {
		return err
	}
	expected := 0
	for _, record := range wire.Edits {
		if record.Mode == "compact" {
			expected++
		}
	}
	mode, exists := wire.Modes["compact"]
	if !exists || expected == 0 || mode.ObservedEdits == nil || *mode.ObservedEdits != expected || mode.Answers < expected || mode.DecodeFailures != 0 || mode.Observable || mode.ClaimsFresh == nil || *mode.ClaimsFresh != 0 || mode.Stale.Value != nil || mode.Stale.Reason == "" {
		return errors.New("compact actual assigned-edit/decoder proof missing or contradicts known unobservable contract")
	}
	for _, name := range []string{"read.summary", "search.symbols", "relations.callers", "trace.call_chain"} {
		tool, exists := mode.Tools[name]
		if !exists || tool.ObservedEdits == nil || *tool.ObservedEdits != expected || tool.CorrectEdits == nil || *tool.CorrectEdits != expected || tool.Answers < expected || tool.DecodeFailures != 0 {
			return errors.New("compact supported tool lacks post-ACK semantic witness: " + name)
		}
	}
	for _, name := range []string{"read.source", "read.editing_context", "search.text"} {
		tool, exists := mode.Tools[name]
		if !exists || tool.State != "UNSUPPORTED" || tool.Reason == "" || tool.RefusedEdits == nil || *tool.RefusedEdits != expected || tool.FallbackAnswers == nil || *tool.FallbackAnswers < expected || tool.ObservedEdits == nil || *tool.ObservedEdits != 0 || tool.DecodeFailures != 0 {
			return errors.New("compact unsupported operation lacks explicit format refusal and actual fallback proof: " + name)
		}
	}
	return nil
}

type editLoopSingleReportRows struct {
	Role      string        `json:"role"`
	HostClass string        `json:"host_class"`
	Scenario  string        `json:"scenario"`
	Repeat    int           `json:"repeat"`
	Report    string        `json:"report"`
	RunID     string        `json:"run_id"`
	Revision  string        `json:"git_sha"`
	Pass      bool          `json:"pass"`
	Rows      []editLoopRow `json:"rows"`
}

func writeEditLoopPerReportRows(path string, budgets []budget, data dataset, validationErr error) error {
	result := struct {
		Version         int                        `json:"schema_version"`
		Scope           string                     `json:"scope"`
		ValidationError string                     `json:"validation_error,omitempty"`
		Reports         []editLoopSingleReportRows `json:"reports"`
	}{Version: 1, Scope: editLoopPerReportScope, Reports: []editLoopSingleReportRows{}}
	if validationErr != nil {
		// loadInputsChecked can retain a successfully read prefix. Do not
		// give that favorable prefix credit when the input set is invalid.
		result.ValidationError = validationErr.Error()
		return writeNewJSON(path, result)
	}
	for _, observation := range data.runs {
		if observation.input.Role != "candidate" {
			continue
		}
		entry := editLoopSingleReportRows{Role: observation.input.Role, HostClass: observation.input.HostClass, Scenario: observation.input.Scenario, Repeat: observation.input.Repeat, Report: observation.input.Report, RunID: observation.data.RunID, Revision: observation.data.Revision, Pass: true, Rows: []editLoopRow{}}
		for _, b := range budgets {
			if b.Scenario != entry.Scenario || b.HostClass != entry.HostClass {
				continue
			}
			row := editLoopRow{Metric: b.Metric, Scenario: b.Scenario, State: "UNAVAILABLE", Budget: b.Target, Unit: b.Unit, Baseline: editLoopBaseline(b, data)}
			if b.Metric == "reclaim_hold_p95" {
				row = assessEditLoopHoldSample(row, b, observation.data.Phases[0])
			} else if b.Metric == "silent_stale_compact" {
				if err := validateCompactEditLoopPhase(observation.data.Phases[0]); err != nil {
					row.Reason = err.Error()
				} else {
					row.State = "UNOBSERVABLE"
					row.Reason = compactEditLoopQualifiedReason
				}
			} else if value, err := measureEditLoopRow(observation.data.Phases[0], b.Metric); err != nil {
				row.Reason = err.Error()
			} else if b.Target == nil {
				row.Reason = "budget target unavailable"
			} else {
				row.Value = &value
				if row.Baseline != nil {
					delta := value - *row.Baseline
					row.Delta = &delta
				}
				row.State = "FAIL"
				if editLoopCompare(value, *b.Target, b.Compare) {
					row.State = "PASS"
				}
			}
			if editLoopRowBlocks(row) {
				entry.Pass = false
			}
			entry.Rows = append(entry.Rows, row)
		}
		if len(entry.Rows) > 0 {
			result.Reports = append(result.Reports, entry)
		}
	}
	return writeNewJSON(path, result)
}
