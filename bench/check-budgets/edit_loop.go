package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
)

type editLoopRow struct {
	Metric   string   `json:"metric"`
	Scenario string   `json:"scenario"`
	State    string   `json:"state"`
	Value    *float64 `json:"value"`
	Budget   *float64 `json:"budget"`
	Unit     string   `json:"unit"`
	Baseline *float64 `json:"baseline"`
	Delta    *float64 `json:"delta"`
	Reason   string   `json:"reason"`
}

type editLoopWire struct {
	Plan struct {
		GateModes bool   `json:"gate_modes"`
		Count     int    `json:"count"`
		Editors   int    `json:"editors"`
		Mode      string `json:"mode"`
		Session   string `json:"session_model"`
		Steps     []struct {
			Ordinal int    `json:"ordinal"`
			Mode    string `json:"probe_mode"`
		} `json:"steps"`
	} `json:"plan"`
	Edits []struct {
		Ordinal   int    `json:"ordinal"`
		Mode      string `json:"probe_mode"`
		Outcome   string `json:"outcome"`
		ACK       metric `json:"t_ack_ms"`
		OneRead   metric `json:"t_gate_readback_ms"`
		ACKExact  *bool  `json:"ack_readback_exact"`
		Readbacks []struct {
			Kind            string `json:"kind"`
			Correct         *bool  `json:"correct"`
			Error           string `json:"error"`
			ObservationOnly bool   `json:"observation_only"`
		} `json:"readbacks"`
		Errors []string `json:"errors"`
	} `json:"edits"`
	Modes map[string]struct {
		Answers        int    `json:"probe_answers"`
		DecodeFailures int    `json:"decode_failures"`
		Observable     bool   `json:"claims_fresh_observable"`
		Stale          metric `json:"stale_claimed_fresh"`
	} `json:"output_modes"`
}

func editLoopCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("check-budgets edit-loop", flag.ContinueOnError)
	flags.SetOutput(stderr)
	policyPath := flags.String("budgets", "bench/budgets.yaml", "budget policy")
	inputsPath := flags.String("inputs", "", "pinned repeated edit-probe reports")
	scenario := flags.String("scenario", "", "explicit edit-load cell")
	output := flags.String("out", "", "new JSON evidence output")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *inputsPath == "" || *scenario == "" {
		fmt.Fprintln(stderr, "--inputs and --scenario are required")
		return 2
	}
	data, err := os.ReadFile(*policyPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	policy, err := loadPolicy(data)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var budgets []budget
	for _, row := range policy.Budgets {
		if row.HostClass == "reference_linux" && row.Scenario == *scenario && slices.Contains(row.Lane, "edit-smoke") {
			budgets = append(budgets, row)
		}
	}
	if len(budgets) == 0 {
		fmt.Fprintln(stderr, "scenario has no declared edit-loop gate rows")
		return 2
	}
	dataset, loadErr := loadInputsChecked(*inputsPath, validateEditLoopReport)
	rows := make([]editLoopRow, 0, len(budgets))
	pass := true
	for _, b := range budgets {
		row := editLoopRow{Metric: b.Metric, Scenario: b.Scenario, State: "UNAVAILABLE", Budget: b.Target, Unit: b.Unit}
		if loadErr != nil {
			row.Reason = loadErr.Error()
		} else {
			row = assessEditLoopRow(b, dataset)
		}
		if row.State != "PASS" {
			pass = false
		}
		rows = append(rows, row)
	}
	writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "scenario\tmetric\tvalue\tbudget\tunit\tbaseline\tdelta\tstate\treason")
	for _, row := range rows {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.Scenario, row.Metric, editLoopNumber(row.Value), editLoopNumber(row.Budget), row.Unit, editLoopNumber(row.Baseline), editLoopNumber(row.Delta), row.State, row.Reason)
	}
	if err := writer.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *output != "" {
		if err := writeNewJSON(*output, struct {
			Pass bool          `json:"pass"`
			Rows []editLoopRow `json:"rows"`
		}{pass, rows}); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if !pass {
		return 1
	}
	return 0
}

func editLoopNumber(value *float64) string {
	if value == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.6g", *value)
}

func validateEditLoopReport(config inputs, input runInput, r report) error {
	if r.Version != 1 || !r.Complete || r.RunID == "" || r.Revision != map[string]string{"candidate": config.CandidateRevision, "main": config.MainRevision, "tag": config.TagRevision}[input.Role] || r.Profile != "agent" || !planPattern.MatchString(r.Plan) || len(r.Errors) != 0 || len(r.Phases) != 1 {
		return errors.New("incomplete or unpinned edit-loop report")
	}
	if err := validateHost(input.HostClass, config.Hosts[input.HostClass], r); err != nil {
		return err
	}
	for _, name := range []string{"panic_lines", "sigabrt_lines", "oom_log_lines"} {
		if r.Crashes[name] == nil || *r.Crashes[name] != 0 {
			return errors.New("missing or nonzero safety census")
		}
	}
	phase := r.Phases[0]
	if len(phase.Errors) != 0 || phase.Duration <= 0 || phase.Duration > 900 {
		return errors.New("edit-loop phase incomplete or exceeds the 15-minute gate")
	}
	if _, coupled := phase.Gate["paired_measurement_window"]; coupled {
		return errors.New("paired child cannot supply standalone gate evidence")
	}
	if forced, err := number(phase.Metrics["force_killed"], "boolean"); err != nil || forced != 0 {
		return errors.New("missing clean daemon-stop evidence")
	}
	var workload struct {
		Scenario string `json:"scenario"`
		Agents   int    `json:"agents"`
		Editors  int    `json:"edit_agents"`
		Session  string `json:"session_model"`
	}
	if err := json.Unmarshal(r.Workload, &workload); err != nil {
		return err
	}
	expected := scenarioSpecs[input.Scenario].editors
	if input.Scenario == "loadedit-4-largefile" {
		expected = 4
	}
	if workload.Scenario != "edit-and-probe" || workload.Agents != 0 || workload.Editors != expected || workload.Session != "fixed_editor_per_ordinal_plus_one_separate_prober_per_edit" {
		return errors.New("incorrect edit-loop workload population or session model")
	}
	wire, err := decodeEditLoopWire(phase)
	if err != nil {
		return err
	}
	if wire.Plan.Editors != expected {
		return errors.New("plan editor population differs from workload")
	}
	return nil
}

func decodeEditLoopWire(phase phase) (editLoopWire, error) {
	var wire editLoopWire
	if err := json.Unmarshal(phase.EditProbe, &wire); err != nil {
		return wire, fmt.Errorf("missing edit-probe evidence: %w", err)
	}
	if !wire.Plan.GateModes || wire.Plan.Count < 100 || len(wire.Edits) != wire.Plan.Count || len(wire.Plan.Steps) != wire.Plan.Count || wire.Plan.Mode != "json" {
		return wire, errors.New("gate needs the complete >=100-total JSON-ACK/five-mode denominator")
	}
	modes := []string{"json", "toon", "compact", "facade", "cli"}
	for i, record := range wire.Edits {
		if record.Ordinal != i || wire.Plan.Steps[i].Ordinal != i || record.Mode != modes[i%5] || wire.Plan.Steps[i].Mode != record.Mode || record.Outcome != "complete" || len(record.Errors) != 0 {
			return wire, errors.New("missing, duplicate, failed or incorrectly routed gate edit")
		}
	}
	return wire, nil
}

func assessEditLoopRow(b budget, dataset dataset) editLoopRow {
	row := editLoopRow{Metric: b.Metric, Scenario: b.Scenario, State: "UNAVAILABLE", Budget: b.Target, Unit: b.Unit}
	values := make([]float64, 0, dataset.config.repeats(b.Scenario))
	verdict := -1
	for repeat := 1; repeat <= dataset.config.repeats(b.Scenario); repeat++ {
		found := false
		for _, observation := range dataset.runs {
			if observation.input.Role != "candidate" || observation.input.HostClass != b.HostClass || observation.input.Scenario != b.Scenario || observation.input.Repeat != repeat {
				continue
			}
			found = true
			value, err := measureEditLoopRow(observation.data.Phases[0], b.Metric)
			if err != nil {
				row.Reason = err.Error()
				return row
			}
			current := 0
			if editLoopCompare(value, *b.Target, b.Compare) {
				current = 1
			}
			if verdict != -1 && verdict != current {
				row.State = "FAIL"
				row.Reason = "same-build repeat verdicts disagree"
				return row
			}
			verdict = current
			values = append(values, value)
		}
		if !found {
			row.Reason = "missing candidate repeat"
			return row
		}
	}
	if len(values) == 0 {
		row.Reason = "no actual observations"
		return row
	}
	value := slices.Max(values)
	if b.Compare == "ge" {
		value = slices.Min(values)
	}
	row.Value = &value
	row.State = "FAIL"
	if verdict == 1 {
		row.State = "PASS"
	}
	return row
}

func editLoopCompare(value, target float64, compare string) bool {
	switch compare {
	case "le":
		return value <= target
	case "lt":
		return value < target
	case "ge":
		return value >= target
	case "eq":
		return value == target
	}
	return false
}

func measureEditLoopRow(phase phase, name string) (float64, error) {
	wire, err := decodeEditLoopWire(phase)
	if err != nil {
		return 0, err
	}
	switch name {
	case "wal_p95_mib", "wal_peak_mib":
		key := "p95_wal_bytes"
		if name == "wal_peak_mib" {
			key = "peak_wal_bytes"
		}
		value, err := number(phase.Metrics[key], "bytes")
		return value / (1024 * 1024), err
	case "read_gate_closures":
		return number(phase.Metrics["reclaim_gate_closures"], "closures")
	case "reclaim_hold_p95":
		value, err := number(phase.Metrics["reclaim_writer_hold_p95_ms"], "ms")
		return value / 1000, err
	}
	if mode, ok := strings.CutPrefix(name, "silent_stale_"); ok {
		coverage, exists := wire.Modes[mode]
		if !exists || coverage.Answers < 20 || coverage.DecodeFailures != 0 || !coverage.Observable {
			return 0, errors.New("missing or unobservable actual output-mode coverage; absent freshness is never zero-stale")
		}
		return number(coverage.Stale, "answers")
	}
	var values []float64
	correct, total := 0, 0
	for _, record := range wire.Edits {
		if name == "edit_ack_p95" || name == "edit_ack_max" {
			if record.ACKExact == nil || !*record.ACKExact {
				return 0, errors.New("ACK lacks exact readback proof")
			}
			value, err := number(record.ACK, "ms")
			if err != nil {
				return 0, err
			}
			values = append(values, value/1000)
		} else if name == "readback_p95" || name == "readback_max" {
			value, err := number(record.OneRead, "ms")
			if err != nil {
				return 0, err
			}
			values = append(values, value/1000)
		} else if name == "readback_correct" {
			seen := map[string]bool{}
			for _, read := range record.Readbacks {
				if read.ObservationOnly {
					continue
				}
				if seen[read.Kind] {
					return 0, errors.New("duplicate immediate readback")
				}
				seen[read.Kind] = true
				if read.Correct == nil {
					return 0, errors.New("immediate readback correctness unavailable")
				}
				total++
				if *read.Correct && read.Error == "" {
					correct++
				}
			}
			for _, kind := range []string{"file", "source", "summary", "search_visible", "text"} {
				if !seen[kind] {
					return 0, fmt.Errorf("missing immediate %s readback", kind)
				}
			}
		}
	}
	if name == "readback_correct" {
		if total == 0 {
			return 0, errors.New("no readbacks")
		}
		return float64(correct) / float64(total), nil
	}
	if len(values) == 0 {
		return 0, errors.New("required edit-loop measure unavailable")
	}
	slices.Sort(values)
	if strings.HasSuffix(name, "_max") {
		return values[len(values)-1], nil
	}
	return values[(95*len(values)+99)/100-1], nil
}
