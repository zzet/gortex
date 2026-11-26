package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// Ownership, ordinal, ACK, missing-evidence and censor controls for the
// relation-consumer census at one, eight and thirty-two fixed editors.
func TestEditConsumerOwnershipBindsEveryOrdinalToItsOwnEdit(t *testing.T) {
	corpus, _ := editConsumerTestFixture(t)
	for _, editors := range []int{1, 8, 32} {
		t.Run(fmt.Sprintf("editors=%d", editors), func(t *testing.T) {
			// Seed 43 gives every one of 32 editors an edit among 100 offers;
			// a plan that leaves an editor idle is refused below.
			seed := int64(42)
			if editors == 32 {
				seed = 43
			}
			plan, err := planEditProbe(corpus, runOptions{Seed: seed, EditCount: 100, Editors: editors, EditGateModes: true, EditConsumerProbes: true})
			if err != nil {
				t.Fatal(err)
			}
			if !editConsumerPlannedEditors(plan, editors) {
				t.Fatal("planned editor population refused")
			}
			if editConsumerPlannedEditors(plan, editors+1) {
				t.Fatal("plan accepted for another editor count")
			}
			for name, mutate := range map[string]func([]editProbeStep){
				"foreign_editor":    func(steps []editProbeStep) { steps[0].Editor = editors },
				"misassigned_order": func(steps []editProbeStep) { steps[0].Ordinal = 1 },
				"missing_editors": func(steps []editProbeStep) {
					for i := range steps {
						steps[i].Editor = 0
					}
				},
			} {
				wrong := plan
				wrong.Steps = slices.Clone(plan.Steps)
				mutate(wrong.Steps)
				refused := !editConsumerPlannedEditors(wrong, editors)
				// One editor owns every ordinal; its plan names no order beyond that.
				if editors == 1 && name != "foreign_editor" {
					refused = !refused
				}
				if !refused {
					t.Errorf("%s accepted", name)
				}
			}

			records := make([]editProbeRecord, len(plan.Steps))
			for i, step := range plan.Steps {
				record := newEditProbeRecord(corpus, step)
				record.Outcome, record.Evidence = "diagnostic_failure", fmt.Sprintf("edit-%03d.jsonl", step.Ordinal)
				record.Receipt, record.ACKCommitSeq = fmt.Sprintf("receipt-%d", step.Ordinal), uint64(step.Ordinal+1)
				record.ACKDiskStatus, record.ACKStatus, record.ACKMutationID = "committed", "applied", step.MutationID
				ack := map[string]any{"mutation_id": step.MutationID, "mutation_receipt": record.Receipt, "commit_seq": json.Number(fmt.Sprint(step.Ordinal + 1)), "ack_mode": "local", "disk_status": "committed", "status": "applied"}
				own := editConsumerOwnedACK(step, record, ack)
				record.ConsumerOwnACK = &own
				records[i] = record
			}
			step, record := plan.Steps[0], records[0]
			if !editConsumerOwnedRecord(step, record) || !*record.ConsumerOwnACK {
				t.Fatal("own committed record or ACK refused")
			}
			for name, mutate := range map[string]func(*editProbeRecord){
				"wrong_editor":     func(r *editProbeRecord) { r.Editor = (r.Editor + 1) % max(2, editors) },
				"foreign_mutation": func(r *editProbeRecord) { r.ACKMutationID = "foreign" },
				"no_receipt":       func(r *editProbeRecord) { r.Receipt = "" },
				"uncommitted":      func(r *editProbeRecord) { r.ACKDiskStatus = "pending" },
				"censored":         func(r *editProbeRecord) { r.Outcome = "phase_censored" },
			} {
				bad := record
				mutate(&bad)
				if editConsumerOwnedRecord(step, bad) {
					t.Errorf("%s record accepted", name)
				}
			}
			ack := map[string]any{"mutation_id": step.MutationID, "mutation_receipt": record.Receipt, "commit_seq": json.Number("1"), "disk_status": "committed", "status": "applied"}
			for name, mutate := range map[string]func(map[string]any){
				"foreign_receipt":  func(a map[string]any) { a["mutation_receipt"] = "foreign-receipt" },
				"foreign_mutation": func(a map[string]any) { a["mutation_id"] = "foreign" },
				"other_commit":     func(a map[string]any) { a["commit_seq"] = json.Number("2") },
				"zero_commit":      func(a map[string]any) { a["commit_seq"] = json.Number("0") },
				"not_applied":      func(a map[string]any) { a["status"] = "refused" },
			} {
				bad := map[string]any{}
				for k, v := range ack {
					bad[k] = v
				}
				mutate(bad)
				if editConsumerOwnedACK(step, record, bad) {
					t.Errorf("%s ACK accepted", name)
				}
			}

			census := summarizeEditConsumerOwnership(plan, editors, records)
			if census == nil || !census.Complete || census.OwnedACKs != 100 || census.RawEvidence != 100 || census.Censored != 0 || len(census.UnownedOrdinals) != 0 {
				t.Fatalf("complete sample refused: %+v", census)
			}
			for name, mutate := range map[string]func([]editProbeRecord) []editProbeRecord{
				"missing_raw":    func(r []editProbeRecord) []editProbeRecord { r[3].Outcome = "harness_evidence_incomplete"; return r },
				"foreign_ack":    func(r []editProbeRecord) []editProbeRecord { r[3].ConsumerOwnACK = new(false); return r },
				"censored":       func(r []editProbeRecord) []editProbeRecord { r[3].Outcome = "phase_censored"; return r },
				"missing_record": func(r []editProbeRecord) []editProbeRecord { return slices.Delete(r, 3, 4) },
			} {
				got := summarizeEditConsumerOwnership(plan, editors, mutate(slices.Clone(records)))
				if got.Complete || name != "missing_raw" && !slices.Equal(got.UnownedOrdinals, []int{3}) {
					t.Errorf("%s accepted or misattributed: %+v", name, got)
				}
			}
			plain := plan
			plain.ConsumerProbes = false
			if summarizeEditConsumerOwnership(plain, editors, records) != nil {
				t.Fatal("a plan without consumer probes gained a census")
			}
		})
	}
}
