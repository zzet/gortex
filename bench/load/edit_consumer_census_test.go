package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A written report whose consumer rows errored is a complete report: the run
// exits 1 under its any-error rule, and the offline census still scores the
// assessed answers and discloses the errors.
func TestConsumerCensusScoresAReportWithConsumerErrors(t *testing.T) {
	type censusRow struct {
		Consumer           string `json:"consumer"`
		Ordinals           int    `json:"ordinals"`
		AssessedOrdinals   int    `json:"assessed_ordinals"`
		Errors             int    `json:"errors"`
		UnassessedOrdinals []int  `json:"unassessed_ordinals"`
		ErrorOrdinals      []int  `json:"error_ordinals"`
	}
	type census struct {
		State                string      `json:"state"`
		Reason               string      `json:"reason"`
		LoadComplete         bool        `json:"load_complete"`
		ConsumerRuleFailures int         `json:"consumer_rule_failed_edits"`
		ScoredOrdinals       int         `json:"scored_ordinals"`
		UnownedOrdinals      []int       `json:"unowned_ordinals"`
		UnownedSilentStale   int         `json:"unowned_silent_stale"`
		Assessed             int         `json:"post_ack_assessed"`
		SilentStale          int         `json:"post_ack_assessed_silent_stale"`
		Unassessed           int         `json:"post_ack_unassessed"`
		Errors               int         `json:"errors"`
		Consumers            []censusRow `json:"consumers"`
	}
	staleRename := func(record *editProbeRecord) {
		row := record.Consumers["consumer_rename"]
		row.PostACKAttempts, row.Assessed, row.SilentStale = 3, 3, measured(1, "answers")
		record.Consumers["consumer_rename"] = row
	}
	// Every scored case keeps edit 1's errors: 398 clean rows of two assessed
	// answers plus verify's two, and five unassessed answers from seven errors.
	for _, test := range []struct {
		name                           string
		mutate                         func(*editProbeReport)
		state, reason                  string
		code, failed, scored, assessed int
		stale, unownedStale, dead      int
		unowned                        []int
	}{
		{name: "consumer errors", state: "errors_present", failed: 1, scored: 100, assessed: 798, dead: 100},
		{name: "assessed silent stale", state: "silent_stale", code: 1, failed: 2, scored: 100, assessed: 799, stale: 1, dead: 100,
			mutate: func(p *editProbeReport) { staleRename(&p.Records[2]) }},
		// An answer counts only for an ordinal whose ACK is its own; the stale
		// answer of an unowned ordinal is disclosed, not claimed.
		{name: "unowned ordinal", state: "errors_present", failed: 2, scored: 99, assessed: 790, unownedStale: 1, dead: 99, unowned: []int{3},
			mutate: func(p *editProbeReport) {
				staleRename(&p.Records[3])
				p.Records[3].ConsumerOwnACK = new(false)
			}},
		{name: "no assessed answer", state: "no_result", code: 2, reason: "no assessed", mutate: func(p *editProbeReport) {
			for _, record := range p.Records {
				for name := range record.Consumers {
					record.Consumers[name] = editConsumerObservation{State: "error", Attempts: 2, PostACKAttempts: 2, Unassessed: 2, Errors: 2, SilentStale: unavailable("answers", "consumer coverage missing, unsupported, failed or unassessed")}
				}
			}
		}},
		{name: "no consumer probes", state: "no_result", code: 2, reason: "did not probe", mutate: func(p *editProbeReport) { p.Plan.ConsumerProbes = false }},
		// An assessed answer the post-ACK attempts do not account for.
		{name: "inconsistent counts", state: "no_result", code: 2, reason: "inconsistent", mutate: func(p *editProbeReport) {
			row := p.Records[4].Consumers["consumer_detect"]
			row.Assessed, row.SubjectMatches, row.SilentStale = 3, 3, measured(0, "answers")
			p.Records[4].Consumers["consumer_detect"] = row
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := editConsumerTestFixture(t)
			plan, err := planEditProbe(c, runOptions{Seed: 1, EditCount: 100, Editors: 1, EditConsumerProbes: true})
			if err != nil {
				t.Fatal(err)
			}
			probe := newEditProbeReport(plan, c, true)
			probe.ConsumerBaseline = &editConsumerBaseline{State: "OBSERVED_DEAD_BEFORE_EDIT", Symbol: "owned/" + c.Symbol, Evidence: "baseline.jsonl"}
			// The report already holds one unsent record per planned step.
			for i, step := range plan.Steps {
				record := &probe.Records[i]
				record.Outcome, record.Errors, record.Evidence = "complete", nil, fmt.Sprintf("edit-%03d.jsonl", step.Ordinal)
				record.Receipt, record.ACKCommitSeq = fmt.Sprintf("receipt-%d", step.Ordinal), uint64(step.Ordinal+1)
				record.ACKDiskStatus, record.ACKStatus, record.ACKMutationID = "committed", "applied", step.MutationID
				ack := map[string]any{"mutation_id": step.MutationID, "mutation_receipt": record.Receipt, "commit_seq": json.Number(fmt.Sprint(step.Ordinal + 1)), "disk_status": "committed", "status": "applied"}
				own := editConsumerOwnedACK(step, *record, ack)
				record.ConsumerOwnACK = &own
				for name, row := range record.Consumers {
					record.Consumers[name] = editConsumerObservation{State: "subject_observed", RequestedMode: row.RequestedMode, Attempts: 3, PostACKAttempts: 2, Assessed: 2, SubjectMatches: 2, SilentStale: measured(0, "answers")}
				}
			}
			// Edit 1: verify has two assessed and two errored post-ACK answers
			// plus one pre-ACK error; dead code never got an assessed answer.
			unassessed := unavailable("answers", "consumer coverage missing, unsupported, failed or unassessed")
			verify := probe.Records[1].Consumers["consumer_verify"]
			verify.Attempts, verify.PostACKAttempts, verify.Assessed, verify.Pending, verify.SubjectMatches = 6, 4, 2, 1, 1
			verify.Unassessed, verify.Errors, verify.SilentStale = 2, 3, unassessed
			probe.Records[1].Consumers["consumer_verify"] = verify
			probe.Records[1].Consumers["consumer_dead_code"] = editConsumerObservation{State: "error", RequestedMode: verify.RequestedMode, Attempts: 4, PostACKAttempts: 3, Unassessed: 3, Errors: 4, SilentStale: unassessed}
			if !editConsumersFailure(probe.Records[1].Consumers) {
				t.Fatal("fixture edit 1 does not fail the any-error consumer rule")
			}
			if test.mutate != nil {
				test.mutate(probe)
			}

			failure := "edit 1: relation consumer observations incomplete, unsupported or incorrect"
			report := sampleReport(t)
			report.Workload.Scenario, report.Workload.Agents, report.Workload.EditAgents = editProbeScenario, 0, 1
			report.Workload.SessionModel = plan.SessionModel
			report.RequestPlan, err = hashJSON(report.Workload)
			if err != nil {
				t.Fatal(err)
			}
			report.Phases[0].Name, report.Phases[0].SessionModel = editProbeScenario, plan.SessionModel
			report.Phases[0].Errors, report.Phases[0].EditProbe = []string{failure}, probe
			report.Errors, report.Complete = []string{editProbeScenario + ": " + failure}, false
			dir := t.TempDir()
			path, out := filepath.Join(dir, "report.json"), filepath.Join(dir, "census.json")
			if err := writeReport(path, report); err != nil {
				t.Fatal(err)
			}

			err = command([]string{"consumer-census", "--out", out, path})
			code := 0
			if err != nil {
				var status interface{ ExitCode() int }
				if !errors.As(err, &status) {
					t.Fatalf("census did not score the report: %v", err)
				}
				code = status.ExitCode()
			}
			data, readErr := os.ReadFile(out)
			if readErr != nil {
				t.Fatalf("census wrote nothing: %v (command: %v)", readErr, err)
			}
			var got census
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.State != test.state || code != test.code || !strings.Contains(got.Reason, test.reason) {
				t.Fatalf("state %q exit %d, want %q exit %d (reason %q):\n%s", got.State, code, test.state, test.code, test.reason, data)
			}
			if test.state == "no_result" {
				return
			}
			if got.LoadComplete || got.ConsumerRuleFailures != test.failed || got.ScoredOrdinals != test.scored || got.Assessed != test.assessed ||
				got.SilentStale != test.stale || got.Unassessed != 5 || got.Errors != 7 ||
				!slices.Equal(got.UnownedOrdinals, test.unowned) || got.UnownedSilentStale != test.unownedStale {
				t.Fatalf("census did not score and disclose the errors:\n%s", data)
			}
			rows := map[string]censusRow{}
			for _, row := range got.Consumers {
				rows[row.Consumer] = row
			}
			dead, verifyRow := rows["consumer_dead_code"], rows["consumer_verify"]
			if len(rows) != 4 || dead.Ordinals != test.dead || dead.AssessedOrdinals != test.dead-1 || !slices.Equal(dead.UnassessedOrdinals, []int{1}) ||
				verifyRow.Errors != 3 || !slices.Equal(verifyRow.ErrorOrdinals, []int{1}) || len(verifyRow.UnassessedOrdinals) != 0 {
				t.Fatalf("per-consumer disclosure lost:\n%s", data)
			}
		})
	}
}
