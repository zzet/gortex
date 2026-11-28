package main

import "slices"

// Relation-consumer answers are scored per planned ordinal, and with several
// fixed editors an ordinal's answers count only when its acknowledgement is
// its own. The census binds every planned ordinal to its planned editor and
// mutation, an applied and committed ACK with a positive commit sequence and
// a receipt, retained raw evidence, and an uncensored record. Attribution
// only: no outcome, error or consumer verdict changes.
type editConsumerOwnership struct {
	Editors         int   `json:"editors"`
	PlannedOrdinals int   `json:"planned_ordinals"`
	PlanBound       bool  `json:"plan_bound"`
	OwnedACKs       int   `json:"owned_acks"`
	RawEvidence     int   `json:"raw_evidence"`
	Censored        int   `json:"censored"`
	UnownedOrdinals []int `json:"unowned_ordinals,omitempty"`
	Complete        bool  `json:"complete"`
}

// The plan assigns each ordinal in order to one of the run's fixed editors,
// and a multi-editor plan uses every one of them. One editor is the singleton
// plan, which names no editor count.
func editConsumerPlannedEditors(plan editProbePlan, editors int) bool {
	if editors == 1 {
		if plan.Editors != 0 {
			return false
		}
		for _, step := range plan.Steps {
			if step.Editor != 0 {
				return false
			}
		}
		return true
	}
	if editors < 2 || editors > editProbeMaxEditors || plan.Editors != editors || plan.SessionModel != editProbeFixedEditorModel || len(plan.Steps) == 0 {
		return false
	}
	present := make([]bool, editors)
	for ordinal, step := range plan.Steps {
		if step.Ordinal != ordinal || step.Editor < 0 || step.Editor >= editors {
			return false
		}
		present[step.Editor] = true
	}
	return !slices.Contains(present, false)
}

// The record is the planned step's own and its ACK identity is complete.
func editConsumerOwnedRecord(step editProbeStep, record editProbeRecord) bool {
	return record.Ordinal == step.Ordinal && record.Editor == step.Editor &&
		record.PlannedDueNS == step.DueNS && record.Marker == step.Marker &&
		step.MutationID != "" && record.MutationID == step.MutationID &&
		record.ACKMutationID == step.MutationID && record.Receipt != "" &&
		record.ACKCommitSeq > 0 && record.ACKDiskStatus == "committed" &&
		record.ACKStatus == "applied" && record.Outcome != "phase_censored"
}

// The decoded ACK body names the step's own mutation and the record's
// receipt and commit sequence, applied and committed.
func editConsumerOwnedACK(step editProbeStep, record editProbeRecord, ack map[string]any) bool {
	seq, ok := editConsumerUint(ack["commit_seq"])
	return ok && seq > 0 && seq == record.ACKCommitSeq && step.MutationID != "" &&
		ack["mutation_id"] == step.MutationID && record.Receipt != "" && ack["mutation_receipt"] == record.Receipt &&
		ack["disk_status"] == "committed" && ack["status"] == "applied"
}

func editConsumerCompleteOwnership(o editConsumerOwnership) bool {
	return o.PlanBound && o.PlannedOrdinals > 0 && o.OwnedACKs == o.PlannedOrdinals &&
		o.RawEvidence == o.PlannedOrdinals && o.Censored == 0
}

func summarizeEditConsumerOwnership(plan editProbePlan, editors int, records []editProbeRecord) *editConsumerOwnership {
	if !plan.ConsumerProbes {
		return nil
	}
	out := &editConsumerOwnership{Editors: editors, PlannedOrdinals: len(plan.Steps), PlanBound: editConsumerPlannedEditors(plan, editors)}
	byOrdinal := make(map[int]editProbeRecord, len(records))
	for _, record := range records {
		if _, duplicate := byOrdinal[record.Ordinal]; duplicate {
			out.PlanBound = false
		}
		byOrdinal[record.Ordinal] = record
	}
	for _, step := range plan.Steps {
		record, present := byOrdinal[step.Ordinal]
		if !present || record.Outcome == "phase_censored" {
			out.Censored++
		}
		if present && record.Evidence != "" && record.Outcome != "harness_evidence_incomplete" {
			out.RawEvidence++
		}
		if present && record.ConsumerOwnACK != nil && *record.ConsumerOwnACK && editConsumerOwnedRecord(step, record) {
			out.OwnedACKs++
		} else {
			out.UnownedOrdinals = append(out.UnownedOrdinals, step.Ordinal)
		}
	}
	out.Complete = editConsumerCompleteOwnership(*out)
	return out
}
