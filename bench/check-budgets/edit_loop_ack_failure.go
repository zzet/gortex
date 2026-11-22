package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Keep loading after an independently proved seeded ACK report fails the full
// validator. Keep its original error for every decision about passage. An
// invalid set (including a missing file or repeated identity) yields no
// diagnostic evidence from a partially loaded prefix.
func loadEditLoopInputs(path string) (dataset, error) {
	var strictErr error
	d, err := loadInputsChecked(path, func(config inputs, input runInput, r report) error {
		err := validateEditLoopReport(config, input, r)
		if err == nil {
			return nil
		}
		if strictErr == nil {
			strictErr = fmt.Errorf("%s/%s/%s/%d: %w", input.Role, input.HostClass, input.Scenario, input.Repeat, err)
		}
		if _, ackErr := seededACKFailureSamples(config, input, r); ackErr != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if strictErr != nil && strictErr.Error() != err.Error() {
			err = errors.Join(strictErr, err)
		}
		return dataset{}, err
	}
	return d, strictErr
}

func editLoopBudgetRow(b budget, d dataset, loadErr error) editLoopRow {
	if loadErr == nil {
		return assessEditLoopRow(b, d)
	}
	row := editLoopRow{Metric: b.Metric, Scenario: b.Scenario, State: "UNAVAILABLE", Budget: b.Target, Unit: b.Unit, Reason: loadErr.Error()}
	if (b.Metric != "edit_ack_p95" && b.Metric != "edit_ack_max") || b.Compare != "le" || b.Target == nil {
		return row
	}
	repeats := d.config.repeats(b.Scenario)
	if repeats < 3 {
		return row
	}
	var worst float64
	count := 0
	var sameCorpus [3]string
	for _, obs := range d.runs {
		got := obs.input
		if got.Role != "candidate" || got.HostClass != b.HostClass || got.Scenario != b.Scenario {
			continue
		}
		evidence, err := seededACKFailureSamples(d.config, got, obs.data)
		if err != nil {
			return row
		}
		if count == 0 {
			sameCorpus = evidence.corpus
		}
		if evidence.corpus != sameCorpus {
			return row
		}
		value, err := editLoopLatencyRank(b.Metric, evidence.ackSeconds)
		if err != nil || editLoopCompare(value, *b.Target, b.Compare) {
			return row
		}
		// The seed waits inside the successful RPC. The independently
		// retained wire-call rank must establish the same breach.
		wire, err := editLoopLatencyRank(b.Metric, evidence.wireSeconds)
		if err != nil || editLoopCompare(wire, *b.Target, b.Compare) {
			return row
		}
		worst = max(worst, value)
		count++
	}
	if count != repeats {
		return row
	}
	row.State, row.Value = "FAIL", &worst
	row.Reason = fmt.Sprintf("ACK exceeded the budget in all %d planned repeats. The full input still fails validation: %s", repeats, loadErr)
	return row
}

type seededACKEvidence struct {
	ackSeconds  []float64
	wireSeconds []float64
	corpus      [3]string
}

func seededACKFailureSamples(config inputs, input runInput, r report) (seededACKEvidence, error) {
	var evidence seededACKEvidence
	var workload struct {
		Seed  int64  `json:"seed"`
		Fault string `json:"edit_gate_seed"`
	}
	if input.Role != "candidate" || input.HostClass != "reference_linux" || input.Scenario != "loadedit-4" ||
		json.Unmarshal(r.Workload, &workload) != nil || workload.Fault != "ack-wait" || workload.Seed != r.Seed || r.Seed != int64(input.Repeat) {
		return evidence, errors.New("not an original seeded ACK smoke repeat")
	}
	if err := validateEditLoopReportEnvelope(config, input, r, false); err != nil {
		return evidence, err
	}
	phase := r.Phases[0]
	wire, err := decodeEditLoopPopulation(phase)
	if err != nil {
		return evidence, err
	}
	if len(r.Corpus) != 1 || r.Corpus[0]["file"] != wire.Plan.File || !sharedPhysicalEditLoop(wire) ||
		wire.Plan.Seed != r.Seed || wire.Plan.Editors != editLoopEditors(input.Scenario) || wire.Plan.Session != phase.SessionModel {
		return evidence, errors.New("seeded ACK repeat lacks the intended shared-file edit population")
	}
	label, _ := r.Corpus[0]["label"].(string)
	commit, _ := r.Corpus[0]["commit"].(string)
	if label == "" || !revisionPattern.MatchString(commit) {
		return evidence, errors.New("seeded ACK repeat lacks a pinned corpus identity")
	}
	evidence.corpus = [3]string{label, commit, wire.Plan.File}
	samples, err := editLoopACKSamples(wire)
	if err != nil {
		return evidence, err
	}
	last, err := finalSeededACKAttempts(phase.Attempts, wire)
	if err != nil {
		return evidence, err
	}
	mutations, receipts, sequences := map[string]bool{}, map[string]bool{}, map[uint64]bool{}
	evidence.ackSeconds = samples
	evidence.wireSeconds = make([]float64, 0, len(samples))
	for i, edit := range wire.Edits {
		rpc := last[i]
		if edit.MutationID != wire.Plan.Steps[i].MutationID || mutations[edit.MutationID] ||
			edit.Receipt == "" || receipts[edit.Receipt] || sequences[edit.ACKCommitSeq] ||
			rpc.Retry == nil || rpc.Outcome != "success" || rpc.Error != "" || rpc.Code == nil || *rpc.Code != 0 ||
			rpc.DurationMS == nil || rpc.LogicalMS == nil || !finiteNonnegative(*rpc.DurationMS) ||
			!finiteNonnegative(*rpc.LogicalMS) || *rpc.DurationMS > *rpc.LogicalMS || *rpc.LogicalMS > *edit.ACK.Value {
			return evidence, fmt.Errorf("seeded ACK ordinal %d lacks its unique successful committed request and timing", i)
		}
		mutations[edit.MutationID], receipts[edit.Receipt], sequences[edit.ACKCommitSeq] = true, true, true
		evidence.wireSeconds = append(evidence.wireSeconds, *rpc.DurationMS/1000)
	}
	return evidence, nil
}

func finalSeededACKAttempts(attempts []attempt, wire editLoopWire) ([]attempt, error) {
	last := make([]attempt, len(wire.Edits))
	for _, rpc := range attempts {
		if rpc.Role != "workload" || rpc.Tool != "edit.file" {
			continue
		}
		if rpc.Step == nil || *rpc.Step < 0 || *rpc.Step >= len(last) || rpc.Slot == nil || *rpc.Slot != 0 ||
			rpc.Agent == nil || *rpc.Agent != 0 || rpc.Retry == nil || rpc.Started.IsZero() ||
			!planPattern.MatchString(rpc.Request) || rpc.Request != wire.Plan.Steps[*rpc.Step].RequestHash {
			return nil, errors.New("seeded ACK request differs from its owned ordinal or plan")
		}
		previous := last[*rpc.Step]
		next := 0
		if previous.Retry != nil {
			if (previous.Outcome != "busy" && previous.Outcome != "retryable") || rpc.Started.Before(previous.Started) {
				return nil, errors.New("seeded ACK retried an ineligible attempt")
			}
			next = *previous.Retry + 1
		}
		if *rpc.Retry != next {
			return nil, errors.New("seeded ACK has duplicate or noncontiguous attempts")
		}
		last[*rpc.Step] = rpc
	}
	return last, nil
}
