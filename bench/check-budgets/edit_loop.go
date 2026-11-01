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
		MeanNS    *int64 `json:"mean_interarrival_ns"`
		Arrival   string `json:"arrival"`
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
		Tools map[string]struct {
			State           string `json:"state"`
			Reason          string `json:"reason"`
			CorrectEdits    *int   `json:"correct_edits"`
			RefusedEdits    *int   `json:"refused_edits"`
			FallbackAnswers *int   `json:"fallback_answers"`
			ObservedEdits   *int   `json:"observed_edits"`
			Answers         int    `json:"probe_answers"`
			DecodeFailures  int    `json:"decode_failures"`
			ClaimsFresh     *int   `json:"claims_fresh"`
		} `json:"tools"`
		ObservedEdits  *int   `json:"observed_edits"`
		ClaimsFresh    *int   `json:"claims_fresh"`
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
		if editLoopRowBlocks(row) {
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
	if phase.Name != "edit-and-probe" || phase.SessionModel == "" || len(phase.Errors) != 0 || !finiteNonnegative(phase.Duration) || phase.Duration <= 0 || input.Scenario == "loadedit-4" && phase.Duration > 900 {
		return errors.New("edit-loop phase incomplete or exceeds the 15-minute gate")
	}
	var wall struct {
		Clock    string `json:"clock"`
		NS       *int64 `json:"wall_ns"`
		Complete bool   `json:"complete"`
	}
	if json.Unmarshal(phase.Gate["edit_loop_wall"], &wall) != nil || wall.Clock != "monotonic" || wall.NS == nil || *wall.NS <= 0 || input.Scenario == "loadedit-4" && *wall.NS > 900000000000 || float64(*wall.NS) < phase.Duration*1e9 || !wall.Complete {
		return errors.New("missing, incomplete or over-15-minute whole-run wall observation")
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
	session := "fixed_editor_per_ordinal_plus_one_separate_prober_per_edit"
	if expected == 1 {
		session = "one_editor_plus_one_separate_prober_per_edit"
	}
	if workload.Scenario != "edit-and-probe" || workload.Agents != 0 || workload.Editors != expected || workload.Session != session || phase.SessionModel != session {
		return errors.New("incorrect edit-loop workload population or session model")
	}
	wire, err := decodeEditLoopWire(phase)
	if err != nil {
		return err
	}
	editors := wire.Plan.Editors
	if editors == 0 && wire.Plan.Session == "one_editor_plus_one_separate_prober_per_edit" {
		editors = 1
	}
	if editors != expected || wire.Plan.Session != session {
		return errors.New("plan editor population differs from workload")
	}
	return nil
}

func decodeEditLoopWire(phase phase) (editLoopWire, error) {
	var wire editLoopWire
	if err := json.Unmarshal(phase.EditProbe, &wire); err != nil {
		return wire, fmt.Errorf("missing edit-probe evidence: %w", err)
	}
	if wire.Plan.MeanNS == nil || *wire.Plan.MeanNS != 15000000000 || wire.Plan.Arrival != "precomputed_open_loop_poisson" {
		return wire, errors.New("gate changed the offered 15-second-per-editor open-loop schedule")
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
	row := editLoopRow{Metric: b.Metric, Scenario: b.Scenario, State: "UNAVAILABLE", Budget: b.Target, Unit: b.Unit, Baseline: editLoopBaseline(b, dataset)}
	if b.Metric == "silent_stale_compact" {
		return assessCompactEditLoopRow(row, b, dataset)
	}
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
	if row.Baseline != nil {
		delta := value - *row.Baseline
		row.Delta = &delta
	}
	row.State = "FAIL"
	if verdict == 1 {
		row.State = "PASS"
	}
	return row
}

// The WAL policy explicitly adopts each shipped release peak as the p95
// baseline. Other rows use actual matched shipped reports when available;
// the historical development-machine ACK numbers are not reference-host data.
func editLoopBaseline(b budget, d dataset) *float64 {
	if b.Metric == "wal_p95_mib" {
		if value, exists := map[string]float64{"loadedit-1": 1007, "loadedit-4": 1059, "loadedit-8": 960, "multirepoedit-6": 760}[b.Scenario]; exists {
			return &value
		}
	}
	values := make([]float64, 0, d.config.repeats(b.Scenario))
	for repeat := 1; repeat <= d.config.repeats(b.Scenario); repeat++ {
		found := false
		for _, observation := range d.runs {
			if observation.input.Role != "tag" || observation.input.HostClass != b.HostClass || observation.input.Scenario != b.Scenario || observation.input.Repeat != repeat {
				continue
			}
			value, err := measureEditLoopRow(observation.data.Phases[0], b.Metric)
			if err != nil {
				return nil
			}
			values = append(values, value)
			found = true
		}
		if !found {
			return nil
		}
	}
	value, err := median(values)
	if err != nil {
		return nil
	}
	return &value
}

// Only the explicitly accepted compact metadata contract is qualified. Every
// supported compact tool still proves decoded post-ACK content for every
// assigned edit; unsupported operations prove the harness's format refusal
// and actual JSON fallback without receiving any compact-format credit.
func assessCompactEditLoopRow(row editLoopRow, b budget, d dataset) editLoopRow {
	if d.config.repeats(b.Scenario) < 3 {
		row.Reason = "at least three actual repeats required"
		return row
	}
	for repeat := 1; repeat <= d.config.repeats(b.Scenario); repeat++ {
		found := false
		for _, observation := range d.runs {
			if observation.input.Role != "candidate" || observation.input.HostClass != b.HostClass || observation.input.Scenario != b.Scenario || observation.input.Repeat != repeat {
				continue
			}
			found = true
			wire, err := decodeEditLoopWire(observation.data.Phases[0])
			if err != nil {
				row.Reason = err.Error()
				return row
			}
			expected := 0
			for _, record := range wire.Edits {
				if record.Mode == "compact" {
					expected++
				}
			}
			mode, exists := wire.Modes["compact"]
			if !exists || expected == 0 || mode.ObservedEdits == nil || *mode.ObservedEdits != expected || mode.Answers < expected || mode.DecodeFailures != 0 || mode.Observable || mode.ClaimsFresh == nil || *mode.ClaimsFresh != 0 || mode.Stale.Value != nil || mode.Stale.Reason == "" {
				row.Reason = "compact actual assigned-edit/decoder proof missing or contradicts known unobservable contract"
				return row
			}
			for _, name := range []string{"read.summary", "search.symbols", "relations.callers", "trace.call_chain"} {
				tool, exists := mode.Tools[name]
				if !exists || tool.ObservedEdits == nil || *tool.ObservedEdits != expected || tool.CorrectEdits == nil || *tool.CorrectEdits != expected || tool.Answers < expected || tool.DecodeFailures != 0 {
					row.Reason = "compact supported tool lacks post-ACK semantic witness: " + name
					return row
				}
			}
			for _, name := range []string{"read.source", "read.editing_context", "search.text"} {
				tool, exists := mode.Tools[name]
				if !exists || tool.State != "UNSUPPORTED" || tool.Reason == "" || tool.RefusedEdits == nil || *tool.RefusedEdits != expected || tool.FallbackAnswers == nil || *tool.FallbackAnswers < expected || tool.ObservedEdits == nil || *tool.ObservedEdits != 0 || tool.DecodeFailures != 0 {
					row.Reason = "compact unsupported operation lacks explicit format refusal and actual fallback proof: " + name
					return row
				}
			}
		}
		if !found {
			row.Reason = "missing candidate repeat"
			return row
		}
	}
	row.State = "UNOBSERVABLE"
	row.Reason = "compact freshness metadata is unobservable under the accepted contract; actual supported content/tool and unsupported-refusal proofs passed; no numeric silent-stale claim"
	return row
}

func editLoopRowBlocks(row editLoopRow) bool {
	return row.State != "PASS" && !(row.Metric == "silent_stale_compact" && row.State == "UNOBSERVABLE" && row.Value == nil)
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
		expected := 0
		for _, record := range wire.Edits {
			if record.Mode == mode {
				expected++
			}
		}
		if !exists || expected == 0 || coverage.ObservedEdits == nil || *coverage.ObservedEdits != expected || coverage.ClaimsFresh == nil || *coverage.ClaimsFresh <= 0 || coverage.Answers < expected || coverage.DecodeFailures != 0 || !coverage.Observable {
			return 0, errors.New("missing or unobservable actual output-mode coverage; absent freshness is never zero-stale")
		}
		for _, name := range []string{"read.source", "read.summary", "read.editing_context", "search.symbols", "search.text", "relations.callers", "trace.call_chain"} {
			tool, exists := coverage.Tools[name]
			if !exists || tool.ObservedEdits == nil || *tool.ObservedEdits != expected || tool.Answers < expected || tool.DecodeFailures != 0 || tool.ClaimsFresh == nil || *tool.ClaimsFresh <= 0 {
				return 0, fmt.Errorf("missing actual %s/%s coverage or explicit freshness claims", mode, name)
			}
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
