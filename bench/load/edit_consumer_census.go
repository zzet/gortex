package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
)

// The consumer census scores a written edit-and-probe report offline. The run
// keeps its strict rule: an edit whose consumer rows errored, stayed
// unassessed or never got an assessed answer fails, and `load run` exits 1. That
// exit does not mean there is no result. The report is written in full, so the
// census counts the silent-stale answers among the assessed post-ACK answers
// and discloses everything it could not assess.
const editConsumerCensusVersion = 1

type editConsumerCensusRow struct {
	Consumer           string `json:"consumer"`
	RequestedMode      string `json:"requested_mode"`
	Ordinals           int    `json:"ordinals"`
	AssessedOrdinals   int    `json:"assessed_ordinals"`
	PostACKAttempts    int    `json:"post_ack_attempts"`
	Assessed           int    `json:"post_ack_assessed"`
	Pending            int    `json:"post_ack_pending"`
	SubjectMatches     int    `json:"post_ack_subject_matches"`
	SilentStale        int    `json:"post_ack_assessed_silent_stale"`
	Unassessed         int    `json:"post_ack_unassessed"`
	Errors             int    `json:"errors"`
	UnassessedOrdinals []int  `json:"unassessed_ordinals,omitempty"`
	ErrorOrdinals      []int  `json:"error_ordinals,omitempty"`
}

// State is clean, errors_present (no silent stale among the assessed answers,
// with errors or unassessed answers disclosed), silent_stale or no_result.
type editConsumerCensus struct {
	Version              int                     `json:"census_version"`
	State                string                  `json:"state"`
	Reason               string                  `json:"reason,omitempty"`
	RunID                string                  `json:"run_id,omitempty"`
	GitSHA               string                  `json:"git_sha,omitempty"`
	PlanSHA256           string                  `json:"plan_sha256,omitempty"`
	LoadComplete         bool                    `json:"load_complete"`
	ConsumerRuleFailures int                     `json:"consumer_rule_failed_edits"`
	Editors              int                     `json:"editors"`
	PlannedOrdinals      int                     `json:"planned_ordinals"`
	ScoredOrdinals       int                     `json:"scored_ordinals"`
	UnownedOrdinals      []int                   `json:"unowned_ordinals,omitempty"`
	UnownedSilentStale   int                     `json:"unowned_silent_stale"`
	Assessed             int                     `json:"post_ack_assessed"`
	SilentStale          int                     `json:"post_ack_assessed_silent_stale"`
	Unassessed           int                     `json:"post_ack_unassessed"`
	Errors               int                     `json:"errors"`
	Consumers            []editConsumerCensusRow `json:"consumers"`
}

// exitStatus carries a command's own exit code through main.
type exitStatus struct {
	code int
	err  error
}

func (e exitStatus) Error() string { return e.err.Error() }
func (e exitStatus) Unwrap() error { return e.err }
func (e exitStatus) ExitCode() int { return e.code }

// Exit 0 when the census scored the report with no silent stale among the
// assessed answers (clean or errors_present), 1 for silent stale, 2 for no
// result. The census JSON is written in every case the arguments allow.
func consumerCensusCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("load consumer-census", flag.ContinueOnError)
	out := flags.String("out", "", "write the census to this new file instead of stdout")
	if err := flags.Parse(args); err != nil {
		return exitStatus{2, err}
	}
	if flags.NArg() != 1 {
		return exitStatus{2, errors.New("usage: load consumer-census [--out census.json] <report.json>")}
	}
	census := editConsumerCensus{Version: editConsumerCensusVersion, State: "no_result", Consumers: []editConsumerCensusRow{}}
	if report, err := readReport(flags.Arg(0)); err != nil {
		census.Reason = "report cannot be read: " + err.Error()
	} else {
		census = censusEditConsumers(report)
	}
	data, err := json.MarshalIndent(census, "", "  ")
	if err != nil {
		return exitStatus{2, err}
	}
	data = append(data, '\n')
	if *out == "" {
		_, err = stdout.Write(data)
	} else {
		err = writeNewFile(*out, data)
	}
	if err != nil {
		return exitStatus{2, err}
	}
	switch census.State {
	case "clean", "errors_present":
		return nil
	case "silent_stale":
		return exitStatus{1, fmt.Errorf("consumer census: %d silent-stale answers among %d assessed post-ACK answers", census.SilentStale, census.Assessed)}
	default:
		return exitStatus{2, fmt.Errorf("consumer census: no result: %s", census.Reason)}
	}
}

func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

func censusEditConsumers(r Report) editConsumerCensus {
	base := editConsumerCensus{Version: editConsumerCensusVersion, State: "no_result", RunID: r.RunID, GitSHA: r.GitSHA, LoadComplete: r.Complete, Editors: r.Workload.EditAgents, Consumers: []editConsumerCensusRow{}}
	noResult := func(reason string) editConsumerCensus {
		base.Reason = reason
		return base
	}
	var probe *editProbeReport
	for _, phase := range r.Phases {
		if phase.Name == editProbeScenario {
			probe = phase.EditProbe
		}
	}
	switch {
	case probe == nil:
		return noResult("report has no edit-and-probe phase")
	case !probe.Plan.ConsumerProbes:
		return noResult("run did not probe relation consumers")
	case probe.ConsumerBaseline == nil || probe.ConsumerBaseline.State != "OBSERVED_DEAD_BEFORE_EDIT":
		return noResult("run has no observed pre-edit dead-code baseline")
	}
	base.PlanSHA256 = probe.PlanHash
	if !slices.ContainsFunc(probe.Records, func(record editProbeRecord) bool { return record.ConsumerOwnACK != nil }) {
		return noResult("no edit carries its own-ACK binding (consumer_own_ack): the run writes it only in gate-mode or own-ACK-readback runs, once an edit's ACK decodes, and a report written before that binding has none")
	}
	// Answers count only for an ordinal whose acknowledgement is its own.
	ownership := summarizeEditConsumerOwnership(probe.Plan, r.Workload.EditAgents, probe.Records)
	base.PlannedOrdinals = ownership.PlannedOrdinals
	if !ownership.PlanBound {
		return noResult("planned ordinals are not bound to the run's editors")
	}
	out := base
	out.UnownedOrdinals = ownership.UnownedOrdinals
	rows := make(map[[2]string]*editConsumerCensusRow)
	for _, record := range probe.Records {
		if record.Ordinal < 0 || record.Ordinal >= len(probe.Plan.Steps) || !editConsumerCensusRowSet(record.Consumers) {
			return noResult(fmt.Sprintf("edit %d has no planned ordinal or consumer row set", record.Ordinal))
		}
		owned := !slices.Contains(ownership.UnownedOrdinals, record.Ordinal)
		consumers := maps.Clone(record.Consumers)
		for name, observed := range record.Consumers {
			stale, consistent := editConsumerCensusStale(observed)
			if !consistent {
				return noResult(fmt.Sprintf("edit %d %s has inconsistent answer counts", record.Ordinal, name))
			}
			observed.stale = stale
			consumers[name] = observed
			if !owned {
				out.UnownedSilentStale += stale
				continue
			}
			key := [2]string{name, observed.RequestedMode}
			row := rows[key]
			if row == nil {
				row = &editConsumerCensusRow{Consumer: name, RequestedMode: observed.RequestedMode}
				rows[key] = row
			}
			row.Ordinals++
			row.PostACKAttempts += observed.PostACKAttempts
			row.Assessed += observed.Assessed
			row.Pending += observed.Pending
			row.SubjectMatches += observed.SubjectMatches
			row.SilentStale += stale
			row.Unassessed += observed.Unassessed
			row.Errors += observed.Errors
			if observed.Assessed > 0 {
				row.AssessedOrdinals++
			} else {
				row.UnassessedOrdinals = append(row.UnassessedOrdinals, record.Ordinal)
			}
			if observed.Errors > 0 {
				row.ErrorOrdinals = append(row.ErrorOrdinals, record.Ordinal)
			}
		}
		// The run's own any-error rule, reproduced to disclose how many edits it failed.
		if editConsumersFailure(consumers) {
			out.ConsumerRuleFailures++
		}
		if owned {
			out.ScoredOrdinals++
		}
	}
	for _, row := range rows {
		slices.Sort(row.UnassessedOrdinals)
		slices.Sort(row.ErrorOrdinals)
		out.Assessed += row.Assessed
		out.SilentStale += row.SilentStale
		out.Unassessed += row.Unassessed
		out.Errors += row.Errors
		out.Consumers = append(out.Consumers, *row)
	}
	order := append(slices.Clone(editConsumerNames), editConsumerGCXNames...)
	slices.SortFunc(out.Consumers, func(a, b editConsumerCensusRow) int {
		return cmp.Or(cmp.Compare(slices.Index(order, a.Consumer), slices.Index(order, b.Consumer)), cmp.Compare(a.RequestedMode, b.RequestedMode))
	})
	switch {
	case out.ScoredOrdinals == 0:
		out.State, out.Reason = "no_result", "no ordinal carries its own committed ACK"
	case out.Assessed == 0:
		out.State, out.Reason = "no_result", "no assessed post-ACK consumer answer"
	case out.SilentStale > 0:
		out.State = "silent_stale"
	case out.ConsumerRuleFailures > 0 || len(out.UnownedOrdinals) > 0:
		out.State = "errors_present"
	default:
		out.State = "clean"
	}
	return out
}

// Every record of a consumer run carries the four ordinary rows, or those
// and the three GCX companions, each under its own oracle name.
func editConsumerCensusRowSet(rows map[string]editConsumerObservation) bool {
	names := editConsumerNames
	if len(rows) == len(editConsumerNames)+len(editConsumerGCXNames) {
		names = append(slices.Clone(names), editConsumerGCXNames...)
	}
	if len(rows) != len(names) {
		return false
	}
	for _, name := range names {
		if _, exists := rows[name]; !exists {
			return false
		}
	}
	return true
}

// The run counts every post-ACK answer once, as assessed (pending, subject
// observed or silent stale) or unassessed, and an error at most once per
// attempt. The silent-stale answers are the assessed ones that were neither
// pending nor showed the subject. Counts that break this were not written by
// the run's consumer observer, and a measured row total must agree.
func editConsumerCensusStale(row editConsumerObservation) (int, bool) {
	for _, count := range []int{row.Attempts, row.PostACKAttempts, row.Assessed, row.Pending, row.SubjectMatches, row.Unassessed, row.Errors} {
		if count < 0 {
			return 0, false
		}
	}
	stale := row.Assessed - row.Pending - row.SubjectMatches
	if stale < 0 || row.PostACKAttempts != row.Assessed+row.Unassessed || row.PostACKAttempts > row.Attempts || row.Errors > row.Attempts {
		return 0, false
	}
	if value := row.SilentStale.Value; value != nil && *value != float64(stale) {
		return 0, false
	}
	return stale, true
}
