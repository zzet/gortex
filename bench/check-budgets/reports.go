package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

type inputs struct {
	Version           int               `json:"schema_version"`
	CandidateRevision string            `json:"candidate_revision"`
	MainRevision      string            `json:"main_revision"`
	TagRevision       string            `json:"tag_revision"`
	Repeats           int               `json:"repeats"`
	ScenarioRepeats   map[string]int    `json:"scenario_repeats"`
	Hosts             map[string]string `json:"hosts"` // reviewed host class -> exact observed machine
	Runs              []runInput        `json:"runs"`
}

type runInput struct {
	Role      string `json:"role"`
	HostClass string `json:"host_class"`
	Scenario  string `json:"scenario"`
	Repeat    int    `json:"repeat"`
	Report    string `json:"report"`
}

type metric struct {
	Value  *float64 `json:"value"`
	Unit   string   `json:"unit"`
	Reason string   `json:"reason"`
}

type distribution struct {
	Count int    `json:"count"`
	P50   metric `json:"p50"`
	P95   metric `json:"p95"`
	P99   metric `json:"p99"`
	Max   metric `json:"max"`
}

type toolResult struct {
	Latency       distribution    `json:"latency_ms"`
	Errors        map[string]*int `json:"errors"`
	PhaseCensored *int            `json:"phase_censored"`
	Expired       *int            `json:"product_expired"`
}

type attempt struct {
	Tool      string   `json:"tool"`
	Role      string   `json:"role"`
	Outcome   string   `json:"outcome"`
	Code      *int     `json:"code"`
	LogicalMS *float64 `json:"logical_ms"`
}

type phase struct {
	SessionModel string                     `json:"session_model"`
	Name         string                     `json:"name"`
	Duration     float64                    `json:"duration_seconds"`
	Gate         map[string]json.RawMessage `json:"gate"`
	Tools        map[string]toolResult      `json:"tools"`
	Attempts     []attempt                  `json:"attempts"`
	Edit         distribution               `json:"edit_latency_ms"`
	Metrics      map[string]metric          `json:"metrics"`
	Errors       []string                   `json:"errors"`
	NeverSeen    *int                       `json:"never_visible"`
	Censored     *int                       `json:"visibility_censored"`
}

type report struct {
	Started  time.Time       `json:"started"`
	Version  int             `json:"schema_version"`
	RunID    string          `json:"run_id"`
	Revision string          `json:"git_sha"`
	Profile  string          `json:"profile"`
	Plan     string          `json:"request_plan_sha256"`
	Seed     int64           `json:"seed"`
	Workload json.RawMessage `json:"workload"`
	Host     struct {
		Machine   string            `json:"machine"`
		OS        string            `json:"os"`
		Arch      string            `json:"arch"`
		Kernel    string            `json:"kernel"`
		Profile   string            `json:"profile"`
		Validated bool              `json:"validated"`
		Limits    map[string]metric `json:"limits"`
	} `json:"host"`
	Corpus   []map[string]any `json:"corpus"`
	Phases   []phase          `json:"phases"`
	Crashes  map[string]*int  `json:"crashes"`
	Errors   []string         `json:"errors"`
	Complete bool             `json:"complete"`
}

type observation struct {
	input runInput
	data  report
}

type dataset struct {
	config inputs
	runs   []observation
}

func loadInputs(path string) (dataset, error) {
	var out dataset
	data, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&out.config); err != nil {
		return out, err
	}
	if d.Decode(new(any)) != io.EOF {
		return out, errors.New("inputs must contain exactly one JSON document")
	}
	c := out.config
	if c.Version != 1 || c.Repeats < 3 || len(c.Hosts) == 0 || len(c.Runs) == 0 || !revisionPattern.MatchString(c.CandidateRevision) || !revisionPattern.MatchString(c.MainRevision) || c.TagRevision != shippedRevision {
		return out, errors.New("inputs need schema1, >=3 planned repeats, reviewed hosts and full candidate/main/shipped revisions")
	}
	for scenario, repeats := range c.ScenarioRepeats {
		if _, ok := scenarioSpecs[scenario]; !ok || repeats < 3 {
			return out, errors.New("invalid scenario-specific planned repeat count")
		}
	}
	machines := make(map[string]bool)
	for class, machine := range c.Hosts {
		if (class != "reference_linux" && class != "reference_mac" && class != "large_mac") || machine == "" || machines[machine] {
			return out, errors.New("reviewed host classes need distinct nonempty machine bindings")
		}
		machines[machine] = true
	}
	paths, ids, cells := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, input := range c.Runs {
		if err := validateRunInput(c, input); err != nil {
			return out, err
		}
		file := input.Report
		if !filepath.IsAbs(file) {
			file = filepath.Join(filepath.Dir(path), file)
		}
		file, err = filepath.EvalSymlinks(file)
		if err != nil {
			return out, err
		}
		cell := fmt.Sprintf("%s/%s/%s/%d", input.Role, input.HostClass, input.Scenario, input.Repeat)
		if paths[file] || cells[cell] {
			return out, fmt.Errorf("reused report or duplicate repeat %s", cell)
		}
		paths[file], cells[cell] = true, true
		data, err = os.ReadFile(file)
		if err != nil {
			return out, err
		}
		r, err := decodeReport(data)
		if err != nil {
			return out, fmt.Errorf("%s: %w", cell, err)
		}
		if err := validateReport(c, input, r); err != nil {
			return out, fmt.Errorf("%s: %w", cell, err)
		}
		if ids[r.RunID] {
			return out, fmt.Errorf("reused run identity %s", r.RunID)
		}
		ids[r.RunID] = true
		out.runs = append(out.runs, observation{input: input, data: r})
	}
	return out, nil
}

func validateRunInput(c inputs, r runInput) error {
	if r.Role != "candidate" && r.Role != "main" && r.Role != "tag" {
		return fmt.Errorf("unknown role %s", r.Role)
	}
	if c.Hosts[r.HostClass] == "" || (r.HostClass != "reference_linux" && r.HostClass != "reference_mac" && r.HostClass != "large_mac") || r.Repeat < 1 || r.Repeat > c.repeats(r.Scenario) || r.Report == "" {
		return errors.New("run lacks reviewed host binding, planned repeat or report")
	}
	if _, ok := scenarioSpecs[r.Scenario]; !ok {
		return fmt.Errorf("unknown scenario %s", r.Scenario)
	}
	return nil
}

func (c inputs) repeats(scenario string) int {
	if n, ok := c.ScenarioRepeats[scenario]; ok {
		return n
	}
	return c.Repeats
}

func validateReport(c inputs, input runInput, r report) error {
	want := map[string]string{"candidate": c.CandidateRevision, "main": c.MainRevision, "tag": c.TagRevision}[input.Role]
	if r.Version != 1 || !r.Complete || r.RunID == "" || r.Revision != want || r.Profile != "agent" || !planPattern.MatchString(r.Plan) || len(r.Errors) != 0 || len(r.Phases) != 1 {
		return errors.New("need complete standalone report-v1 with pinned identity and agent profile; paired/saturation evidence is not capacity evidence")
	}
	if err := validateHost(input.HostClass, c.Hosts[input.HostClass], r); err != nil {
		return err
	}
	for _, name := range []string{"panic_lines", "sigabrt_lines", "oom_log_lines"} {
		n, ok := r.Crashes[name]
		if !ok || n == nil || *n != 0 {
			return fmt.Errorf("missing or nonzero safety count %s", name)
		}
	}
	p := r.Phases[0]
	if _, coupled := p.Gate["paired_measurement_window"]; coupled {
		return errors.New("paired child report is coupled latency evidence, not a standalone capacity run")
	}
	if len(p.Errors) != 0 || p.Duration <= 0 || p.NeverSeen == nil || *p.NeverSeen != 0 || p.Censored == nil || *p.Censored != 0 {
		return errors.New("incomplete, censored or failed phase")
	}
	if len(p.Attempts) == 0 {
		return errors.New("missing attempts")
	}
	for _, a := range p.Attempts {
		validOutcome := a.Code != nil && (a.Outcome == "success" && *a.Code == 0 || a.Outcome == "busy" && *a.Code == -32002)
		if a.LogicalMS == nil || !finiteNonnegative(*a.LogicalMS) || !validOutcome {
			return errors.New("failed, censored or unrecognized attempt retained: run cannot pass")
		}
	}
	if forced, err := number(p.Metrics["force_killed"], "boolean"); err != nil || forced != 0 {
		return errors.New("missing stop evidence or force-killed daemon")
	}
	for _, t := range p.Tools {
		if t.PhaseCensored == nil || *t.PhaseCensored != 0 || t.Expired == nil || *t.Expired != 0 || t.Errors == nil {
			return errors.New("censored/expired workload calls retained: run cannot pass")
		}
		for _, n := range t.Errors {
			if n == nil || *n != 0 {
				return errors.New("workload errors retained: run cannot pass")
			}
		}
	}
	return validateScenario(input.Scenario, r)
}

func validateHost(class, machine string, r report) error {
	if r.Host.Machine != machine || machine == "" || r.Host.Arch == "" || r.Host.Kernel == "" {
		return errors.New("host identity does not match reviewed inventory")
	}
	if class != "reference_linux" {
		if r.Host.OS != "darwin" || r.Host.Arch != "arm64" || r.Host.Profile != "native" {
			return errors.New("mac inventory requires native Darwin ARM64 evidence")
		}
		return nil // Mac class assignment is an explicit external inventory prerequisite.
	}
	if r.Host.OS != "linux" || r.Host.Profile != "reference" || !r.Host.Validated {
		return errors.New("reference Linux needs observed validated cgroup evidence")
	}
	for key, want := range map[string]float64{"cpu_quota": 8, "memory_max": 16 * 1024 * 1024 * 1024} {
		unit := "cores"
		if key == "memory_max" {
			unit = "bytes"
		}
		if v, err := number(r.Host.Limits[key], unit); err != nil || v != want {
			return fmt.Errorf("incorrect reference cgroup %s", key)
		}
	}
	for key, minimum := range map[string]float64{"logical_cpus": 8, "host_memory": 16 * 1024 * 1024 * 1024} {
		unit := "cores"
		if key == "host_memory" {
			unit = "bytes"
		}
		if v, err := number(r.Host.Limits[key], unit); err != nil || v < minimum {
			return fmt.Errorf("reference lacks observed host capacity %s", key)
		}
	}
	disk, err := number(r.Host.Limits["disk_capacity_bytes"], "bytes")
	if err != nil || disk < .95*60*1024*1024*1024 || disk > 60*1024*1024*1024 {
		return errors.New("reference scratch must be observed 57-60 GiB filesystem")
	}
	return nil
}

func comparable(a, b report) bool {
	var aw, bw any
	if decodeNumbers(a.Workload, &aw) != nil || decodeNumbers(b.Workload, &bw) != nil || !reflect.DeepEqual(aw, bw) || a.Plan != b.Plan || !reflect.DeepEqual(a.Host, b.Host) {
		return false
	}
	logical := func(corpus []map[string]any) []map[string]any {
		out := make([]map[string]any, 0, len(corpus))
		for _, c := range corpus {
			clone := make(map[string]any)
			for k, v := range c {
				if k != "source" { // independent owned source paths are expected
					clone[k] = v
				}
			}
			out = append(out, clone)
		}
		return out
	}
	return reflect.DeepEqual(logical(a.Corpus), logical(b.Corpus))
}

func comparableReadLoads(a, b report) bool {
	normalize := func(r report) report {
		var w map[string]any
		if decodeNumbers(r.Workload, &w) != nil {
			return r
		}
		w["agents"] = 0 // the one intentional difference in load32/load8
		r.Workload, _ = json.Marshal(w)
		r.Plan = "read-load reference with count normalized"
		return r
	}
	return comparable(normalize(a), normalize(b))
}

func comparableRepeats(a, b report) bool {
	normalize := func(r report) report {
		var w map[string]any
		if decodeNumbers(r.Workload, &w) != nil {
			return r
		}
		delete(w, "seed") // repetition seeds may vary; roles in that repetition must match exactly
		r.Workload, _ = json.Marshal(w)
		r.Plan = "repetition with seed normalized"
		return r
	}
	return comparable(normalize(a), normalize(b))
}

func decodeNumbers(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return d.Decode(value)
}

// Only the fixed report-v1 records whose zero values could hide failures are
// checked here. This is presence validation, not a general JSON schema engine.
func decodeReport(data []byte) (report, error) {
	var r report
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return r, err
	}
	for _, name := range []string{"schema_version", "run_id", "git_sha", "profile", "seed", "request_plan_sha256", "complete", "errors", "crashes", "phases", "host", "corpus", "workload"} {
		if _, ok := root[name]; !ok {
			return r, fmt.Errorf("missing report field %s", name)
		}
	}
	if err := requireScalars(root["workload"], "scenario", "agents", "edit_agents", "linked_worktrees", "seed", "profile", "duration_seconds", "product_deadline_seconds", "session_model"); err != nil {
		return r, err
	}
	var phases []map[string]json.RawMessage
	if err := json.Unmarshal(root["phases"], &phases); err != nil {
		return r, err
	}
	for _, p := range phases {
		for _, name := range []string{"errors", "tools", "attempts", "gate", "metrics"} {
			if _, ok := p[name]; !ok {
				return r, fmt.Errorf("missing phase field %s", name)
			}
		}
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	return r, nil
}

func requireScalars(data []byte, names ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range names {
		v, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("missing/null required scalar %s", name)
		}
	}
	return nil
}
