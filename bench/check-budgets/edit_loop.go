package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/zzet/gortex/bench/receiptproof"
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
	PhysicalWriteScope *struct {
		Kind    string `json:"kind"`
		File    string `json:"file"`
		Editors int    `json:"editors"`
	} `json:"physical_write_scope"`
	Plan struct {
		Kind   string `json:"kind"`
		Tool   string `json:"tool"`
		File   string `json:"file"`
		Sender string `json:"sender"`
		Routes []struct {
			ID       string `json:"id"`
			Repo     string `json:"repo"`
			Commit   string `json:"commit"`
			File     string `json:"file"`
			Kind     string `json:"kind"`
			Tool     string `json:"tool"`
			Language string `json:"language"`
			Mode     string `json:"mode"`
		} `json:"routes"`
		LinkedWorktrees int    `json:"linked_worktrees"`
		MeanNS          *int64 `json:"mean_interarrival_ns"`
		Arrival         string `json:"arrival"`
		GateModes       bool   `json:"gate_modes"`
		Count           int    `json:"count"`
		Editors         int    `json:"editors"`
		Mode            string `json:"mode"`
		Session         string `json:"session_model"`
		Steps           []struct {
			Editor  int    `json:"editor"`
			Route   string `json:"route"`
			Ordinal int    `json:"ordinal"`
			Mode    string `json:"probe_mode"`
		} `json:"steps"`
	} `json:"plan"`
	Edits []struct {
		Superseded          *receiptproof.Superseded `json:"superseded_receipt"`
		CoalescedFunctional *receiptproof.Coalesced  `json:"coalesced_functional_receipt"`
		FirstCorrect        map[string]metric        `json:"first_correct_ms"`
		Readback            metric                   `json:"t_readback_ms"`
		GraphTerminal       *bool                    `json:"graph_terminal"`
		StaleClaimedFresh   *bool                    `json:"stale_claimed_fresh"`
		EvidenceBytes       int                      `json:"evidence_bytes"`
		EvidenceLimit       int                      `json:"evidence_limit_bytes"`
		Receipt             string                   `json:"mutation_receipt"`
		Accounting          struct {
			State     string   `json:"state"`
			Reasons   []string `json:"reasons"`
			RPCID     string   `json:"bound_rpc_id"`
			Requested uint64   `json:"requested_generation"`
			Applied   uint64   `json:"applied_generation"`
			A         *struct {
				WallNS            int64 `json:"wall_ns"`
				SumNS             int64 `json:"sum_ns"`
				ResidualNS        int64 `json:"residual_ns"`
				WithinFivePercent bool  `json:"within_five_percent"`
			} `json:"acknowledgement"`
			P      json.RawMessage `json:"physical"`
			B      json.RawMessage `json:"requested_background"`
			R      json.RawMessage `json:"read_assisted_publisher"`
			Shared json.RawMessage `json:"shared_owner"`
		} `json:"accounting"`
		CheckoutID       string `json:"checkout_id"`
		MutationID       string `json:"mutation_id"`
		ACKStatus        string `json:"ack_status"`
		ACKDiskStatus    string `json:"ack_disk_status"`
		ACKMutationID    string `json:"ack_mutation_id"`
		ACKCommitSeq     uint64 `json:"ack_commit_seq"`
		ACKContentSHA256 string `json:"ack_content_sha256"`
		Editor           int    `json:"editor"`
		Route            string `json:"route"`
		TargetLines      *int   `json:"admitted_target_lines"`
		Ordinal          int    `json:"ordinal"`
		Mode             string `json:"probe_mode"`
		Outcome          string `json:"outcome"`
		ACK              metric `json:"t_ack_ms"`
		OneRead          metric `json:"t_gate_readback_ms"`
		ACKExact         *bool  `json:"ack_readback_exact"`
		Readbacks        []struct {
			Kind            string `json:"kind"`
			Correct         *bool  `json:"correct"`
			Latency         metric `json:"latency_ms"`
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
	if *scenario == "multirepoedit-6" {
		row := assessSixRepoReadObservation(dataset, loadErr)
		rows = append(rows, row)
		if editLoopRowBlocks(row) {
			pass = false
		}
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
	if wire.PhysicalWriteScope != nil && (len(r.Corpus) != 1 || !sharedPhysicalEditLoop(wire)) {
		return errors.New("shared ACK scope lacks the admitted single-worktree/file topology")
	}
	editors := wire.Plan.Editors
	if editors == 0 && wire.Plan.Session == "one_editor_plus_one_separate_prober_per_edit" {
		editors = 1
	}
	if editors != expected || wire.Plan.Session != session {
		return errors.New("plan editor population differs from workload")
	}
	if input.Scenario == "loadedit-4-largefile" {
		for _, record := range wire.Edits {
			if record.TargetLines == nil || *record.TargetLines < 10000 {
				return errors.New("large-file row lacks actual >=10000-line admitted target evidence")
			}
		}
	}
	if input.Scenario == "multirepoedit-6" {
		if len(wire.Plan.Routes) != 6 || len(r.Corpus) != 6 || wire.Plan.LinkedWorktrees != 0 {
			return errors.New("six-repository cell requires six independent pinned corpus routes")
		}
		corpora := map[string]string{}
		for _, corpus := range r.Corpus {
			label, _ := corpus["label"].(string)
			commit, _ := corpus["commit"].(string)
			if label == "" || corpora[label] != "" || !revisionPattern.MatchString(commit) {
				return errors.New("invalid six-repository corpus pins")
			}
			corpora[label] = commit
		}
		seen := map[string]bool{}
		for _, route := range wire.Plan.Routes {
			if route.ID == "" || seen[route.ID] || route.Repo != route.ID || corpora[route.ID] != route.Commit || route.File == "" || route.Kind != "add_intrafile_call" || route.Tool != "edit.file" || route.Language != "go" || route.Mode != "json" {
				return errors.New("six-repository route differs from admitted corpus pin")
			}
			seen[route.ID] = true
		}
		used := map[string]bool{}
		for i, record := range wire.Edits {
			step := wire.Plan.Steps[i]
			if record.Editor < 0 || record.Editor >= 6 || step.Editor != record.Editor || record.Route != step.Route || record.Route != wire.Plan.Routes[record.Editor].ID {
				return errors.New("edit borrowed a foreign six-repository editor/route")
			}
			used[record.Route] = true
		}
		if len(used) != 6 {
			return errors.New("six-repository cell did not actually edit all routes")
		}
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
		if record.Ordinal != i || wire.Plan.Steps[i].Ordinal != i || record.Mode != modes[i%5] || wire.Plan.Steps[i].Mode != record.Mode || record.Outcome != "complete" && record.Outcome != receiptproof.SupersededOutcome && record.Outcome != receiptproof.CoalescedFunctionalOutcome || len(record.Errors) != 0 {
			return wire, errors.New("missing, duplicate, failed or incorrectly routed gate edit")
		}

		if record.Outcome == receiptproof.CoalescedFunctionalOutcome && !coalescedFunctionalEditLoopRecord(wire, i) {
			return wire, errors.New("qualified coalesced receipt lacks independent functional/status proof")
		}
		if record.Outcome == receiptproof.SupersededOutcome {
			a := record.Accounting.A
			digest, digestErr := hex.DecodeString(record.ACKContentSHA256)
			if !sharedPhysicalEditLoop(wire) || !record.Superseded.Matches(record.Receipt, record.MutationID, record.Accounting.RPCID, record.Accounting.Requested, record.Accounting.Applied) ||
				record.Accounting.State != "terminal_unavailable_background" || a == nil || !a.WithinFivePercent || a.WallNS < 0 || a.SumNS < 0 || a.SumNS > a.WallNS ||
				a.ResidualNS != a.WallNS-a.SumNS || a.ResidualNS > a.WallNS/20 ||
				string(record.Accounting.P) != "null" || string(record.Accounting.B) != "null" || !absentEditLoopTiming(record.Accounting.R) || !absentEditLoopTiming(record.Accounting.Shared) ||
				record.ACKExact == nil || record.ACKStatus != "applied" || record.ACKDiskStatus != "committed" || record.ACKMutationID != record.MutationID ||
				record.ACKCommitSeq == 0 || digestErr != nil || len(digest) != 32 {
				return wire, errors.New("superseded receipt lacks exact shared-worktree status/guard/ACK proof")
			}
		}
	}
	return wire, nil
}

func absentEditLoopTiming(value json.RawMessage) bool {
	return len(value) == 0 || string(value) == "null"
}

// Only the runtime's physical binding observation qualifies this exception.
// Editor count or a common branch name alone never relaxes isolated checkouts.
func sharedPhysicalEditLoop(wire editLoopWire) bool {
	scope := wire.PhysicalWriteScope
	if scope == nil || scope.Kind != "shared_worktree_file" || scope.File == "" || scope.File != wire.Plan.File || scope.Editors < 2 || scope.Editors != wire.Plan.Editors || wire.Plan.LinkedWorktrees != 0 || len(wire.Plan.Routes) != 0 || wire.Plan.Kind != "add_intrafile_call" || wire.Plan.Tool != "edit.file" || wire.Plan.Sender != "" {
		return false
	}
	used := make(map[int]bool)
	for i, record := range wire.Edits {
		if record.Editor < 0 || record.Editor >= scope.Editors || record.Editor != wire.Plan.Steps[i].Editor || record.Route != "" || wire.Plan.Steps[i].Route != "" || record.CheckoutID != "" {
			return false
		}
		used[record.Editor] = true
	}
	return len(used) > 1
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
	if row.Scenario == "multirepoedit-6" && row.Metric == "readback_p95" && row.State == "OBSERVATIONAL" && row.Budget == nil && row.Value != nil && finiteNonnegative(*row.Value) && row.Unit == "s" {
		return false
	}
	if row.Metric == "silent_stale_compact" && row.State == "UNOBSERVABLE" && row.Value == nil {
		return false
	}
	return row.State != "PASS"
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
	sharedPhysical := sharedPhysicalEditLoop(wire)
	for _, record := range wire.Edits {
		if name == "edit_ack_p95" || name == "edit_ack_max" {
			if record.ACKExact == nil {
				return 0, errors.New("ACK lacks exact readback proof")
			}
			if sharedPhysical {
				digest, digestErr := hex.DecodeString(record.ACKContentSHA256)
				if record.ACKStatus != "applied" || record.ACKDiskStatus != "committed" || record.MutationID == "" || record.ACKMutationID != record.MutationID || record.ACKCommitSeq == 0 || digestErr != nil || len(digest) != 32 {
					return 0, errors.New("ACK lacks confirmed shared-write commit evidence")
				}
			} else if !*record.ACKExact {
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

// This population has no dedicated p95 target in the accepted contract. Retain
// its actual value without borrowing a single-repository limit or a PASS.
func assessSixRepoReadObservation(d dataset, loadErr error) editLoopRow {
	row := editLoopRow{Scenario: "multirepoedit-6", Metric: "readback_p95", State: "UNAVAILABLE", Unit: "s"}
	if loadErr != nil {
		row.Reason = loadErr.Error()
		return row
	}
	values := []float64{}
	for repeat := 1; repeat <= d.config.repeats(row.Scenario); repeat++ {
		found := false
		for _, sample := range d.runs {
			if sample.input.Role != "candidate" || sample.input.HostClass != "reference_linux" || sample.input.Scenario != row.Scenario || sample.input.Repeat != repeat {
				continue
			}
			if found {
				row.Reason = "duplicate candidate repeat"
				return row
			}
			found = true
			value, err := measureEditLoopRow(sample.data.Phases[0], row.Metric)
			if err != nil {
				row.Reason = err.Error()
				return row
			}
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
	row.Value = &value
	row.State = "OBSERVATIONAL"
	row.Reason = "unbudgeted: no dedicated six-repository p95 target; universal readback max remains blocking"
	return row
}
