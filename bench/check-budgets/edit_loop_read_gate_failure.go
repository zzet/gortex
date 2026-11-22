package main

import (
	"errors"
	"fmt"
)

func assessSeededReadGateFailure(row editLoopRow, b budget, d dataset, loadErr error) editLoopRow {
	if b.HostClass != "reference_linux" || b.Scenario != "loadedit-4" ||
		b.Unit != "count" || b.Compare != "eq" || b.Target == nil || *b.Target != 0 {
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
		input := obs.input
		if input.Role != "candidate" || input.HostClass != b.HostClass || input.Scenario != b.Scenario {
			continue
		}
		evidence, err := seededReadGateFailure(d.config, input, obs.data)
		if err != nil || count > 0 && evidence.corpus != sameCorpus {
			return row
		}
		sameCorpus = evidence.corpus
		worst = max(worst, evidence.closures)
		count++
	}
	if count != repeats {
		return row
	}
	row.State, row.Value = "FAIL", &worst
	row.Reason = fmt.Sprintf("Owned daemon status confirms read gate closures in all %d planned repeats. The full input still fails validation: %s", repeats, loadErr)
	return row
}

type seededReadGateEvidence struct {
	closures float64
	corpus   [3]string
}

func seededReadGateFailure(config inputs, input runInput, r report) (seededReadGateEvidence, error) {
	var evidence seededReadGateEvidence
	wire, corpus, err := seededSmokePopulation(config, input, r, "closed-read-gate")
	if err != nil {
		return evidence, err
	}
	if wire.Plan.Count != 100 {
		return evidence, errors.New("seeded read gate repeat lacks the original 100-edit population")
	}
	mutations, receipts, sequences := map[string]bool{}, map[string]bool{}, map[uint64]bool{}
	for i, edit := range wire.Edits {
		step := wire.Plan.Steps[i]
		if edit.MutationID == "" || edit.MutationID != step.MutationID || edit.ACKMutationID != edit.MutationID ||
			mutations[edit.MutationID] || !planPattern.MatchString(step.RequestHash) ||
			edit.Receipt == "" || receipts[edit.Receipt] || edit.ACKCommitSeq == 0 || sequences[edit.ACKCommitSeq] ||
			edit.ACKStatus != "applied" || edit.ACKDiskStatus != "committed" || !planPattern.MatchString(edit.ACKContentSHA256) {
			return evidence, fmt.Errorf("seeded read gate ordinal %d lacks its unique committed request", i)
		}
		mutations[edit.MutationID], receipts[edit.Receipt], sequences[edit.ACKCommitSeq] = true, true, true
	}
	closures, err := measureReadGateClosures(r.Phases[0])
	if err != nil {
		return evidence, err
	}
	if closures <= 0 {
		return evidence, errors.New("seeded repeat has no proved read gate closure")
	}
	return seededReadGateEvidence{closures: closures, corpus: corpus}, nil
}
