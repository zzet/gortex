package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sampleReport(t *testing.T) Report {
	t.Helper()
	c := sampleCorpus()
	operations, err := readingPlan(c)
	if err != nil {
		t.Fatal(err)
	}
	w := Workload{Scenario: "read", SessionModel: "per_agent", Agents: 8, DurationSec: 60, DeadlineSec: 120, Seed: 42, Profile: "agent", Operations: operations}
	hash, err := hashJSON(w)
	if err != nil {
		t.Fatal(err)
	}
	r := Report{
		SchemaVersion: reportVersion, RunID: "sample-run", GitSHA: strings.Repeat("b", 40), Binary: "/private/gortex", DaemonVersion: "sample", Started: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Profile: "agent", Seed: 42, Workload: w, Corpus: []Corpus{c}, RequestPlan: hash,
		Host:    Host{Machine: "sample-host", OS: "linux", Arch: "amd64", Kernel: "sample", Profile: "native", Limits: map[string]Metric{"cpu": measured(8, "cores")}},
		Phases:  []Phase{{Name: "read", Operations: operations, Gate: map[string]json.RawMessage{"readiness": json.RawMessage(`{"ready":true,"enriched":true}`)}, Status: []json.RawMessage{json.RawMessage(`{"views":{},"storage":{"wal_pending_frames":0}}`)}, Stats: []json.RawMessage{json.RawMessage(`{"total_nodes":2,"total_edges":1}`)}, Tools: summarize([]Attempt{{Tool: "search.symbols", Role: "workload", Outcome: "success", LogicalMS: 1}}, 60, "workload"), Started: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), DurationSec: 60, EditLatency: distribution(nil, "ms"), Visibility: distribution(nil, "ms")}},
		Metrics: map[string]Metric{"stop_seconds": measured(1, "s"), "daemon_cpu_cores": measured(2, "cores"), "pss": unavailable("bytes", "not available on this OS")}, Complete: true,
	}
	r.Phases[0].SessionModel = "per_agent"
	return r
}

func TestReportSchemaGolden(t *testing.T) {
	argumentSchema := schemaFor(reflect.TypeFor[map[string]any]())
	if property, ok := argumentSchema["additionalProperties"].(map[string]any); !ok || len(property) != 0 {
		t.Fatalf("heterogeneous request values wrongly constrained: %v", argumentSchema)
	}
	data, err := json.MarshalIndent(reportSchema(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", "report-v1.schema.json")
	if os.Getenv("UPDATE_LOAD_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatal("report schema changed; review compatibility and regenerate golden explicitly")
	}
}

func TestReportRoundTripAndDelta(t *testing.T) {
	before := sampleReport(t)
	after := before
	after.Metrics = map[string]Metric{"stop_seconds": measured(1.2, "s"), "daemon_cpu_cores": measured(1, "cores"), "pss": unavailable("bytes", "not available on this OS")}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeReport(path, before); err != nil {
		t.Fatal(err)
	}
	got, err := readReport(path)
	beforeJSON, _ := json.Marshal(before)
	gotJSON, _ := json.Marshal(got)
	if err != nil || !bytes.Equal(beforeJSON, gotJSON) {
		t.Fatalf("report round trip: %+v %v", got, err)
	}
	var output bytes.Buffer
	if err := printDelta(&output, before, after); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+20.00%", "-50.00%", "unavailable", "Timing metrics more than 15% worse: run.stop_seconds"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("delta missing %q:\n%s", want, output.String())
		}
	}
	for _, data := range []string{`{"schema_version":99}`, `{"schema_version":1}`, `{"schema_version":1,"unexpected":true}`, `{"schema_version":1} {}`} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readReport(path); err == nil {
			t.Fatalf("accepted invalid report %s", data)
		}
	}
}

func TestDeltaRejectsIncomparableOfferedLoads(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Report)
	}{
		{"agents", func(r *Report) { r.Workload.Agents = 16 }},
		{"duration", func(r *Report) { r.Workload.DurationSec++ }},
		{"deadline", func(r *Report) { r.Workload.DeadlineSec++ }},
		{"seed", func(r *Report) { r.Seed++ }},
		{"profile", func(r *Report) { r.Profile = "saturation" }},
		{"corpus", func(r *Report) { r.Corpus[0].Commit = strings.Repeat("c", 40) }},
		{"target", func(r *Report) { r.Corpus[0].Symbol = "other.go::Other" }},
		{"host", func(r *Report) { r.Host.Machine = "other-host" }},
		{"limits", func(r *Report) { r.Host.Limits["cpu"] = measured(4, "cores") }},
		{"session model", func(r *Report) { r.Phases[0].SessionModel = "per_operation_isolated" }},
		{"sweep repetition count", func(r *Report) { r.Workload.SweepRepetitions++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, b := sampleReport(t), sampleReport(t)
			test.mutate(&b)
			var out bytes.Buffer
			if err := printDelta(&out, a, b); err == nil || out.Len() != 0 {
				t.Fatalf("incomparable inputs produced a verdict: %v %s", err, out.String())
			}
		})
	}
	a, b := sampleReport(t), sampleReport(t)
	b.GitSHA, b.Binary, b.Corpus[0].Source = strings.Repeat("c", 40), "/other/gortex", "/other/clone"
	if err := comparable(a, b); err != nil {
		t.Fatalf("build/source differences should be comparable: %v", err)
	}
}

func TestRepresentativeReportGoldenAndValidation(t *testing.T) {
	r := sampleReport(t)
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", "report-v1.json")
	if os.Getenv("UPDATE_LOAD_GOLDEN") == "1" {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, want) {
		t.Fatalf("representative report shape changed: %v", err)
	}
	if err := validateReport(data, r); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Report){
		func(r *Report) { r.RunID = "" },
		func(r *Report) { r.RequestPlan = "wrong" },
		func(r *Report) { r.Host.Profile, r.Host.Validated = "reference", false },
		func(r *Report) { r.Metrics["pss"] = Metric{Unit: "bytes"} },
		func(r *Report) { r.Phases[0].EditLatency.Count = 1 },
		func(r *Report) { r.Phases = nil },
		func(r *Report) { r.Errors = []string{"incomplete"} },
	} {
		broken := sampleReport(t)
		mutate(&broken)
		if err := validateReport(data, broken); err == nil {
			t.Fatal("malformed observation accepted")
		}
	}
}

func TestDistributionDoesNotModifyObservations(t *testing.T) {
	values := []float64{100, 1, 20, 4}
	d := distribution(values, "ms")
	if *d.P50.Value != 4 || *d.P95.Value != 100 || values[0] != 100 {
		t.Fatalf("distribution: %+v input=%v", d, values)
	}
}

func TestReportValidationPreservesEditorOnlyPopulationAndHash(t *testing.T) {
	for _, test := range []struct {
		scenario         string
		readers, editors int
		floor, valid     bool
	}{
		{"edit", 0, 1, false, true}, {"edit", 0, 4, false, true}, {"edit", 0, 8, false, true},
		{"many-edit", 0, 4, false, true}, {"edit", 4, 4, false, true},
		{"edit", 0, 4, true, true}, {"edit", 0, 8, true, false}, {"edit", 1, 4, true, false},
		{"edit", 0, 0, false, false}, {"edit", 4, 0, false, false}, {"many-edit", 0, 0, false, false},
		{"read", 0, 4, false, false}, {"worktrees", 0, 4, false, false}, {"sweep", 0, 4, false, false},
		{"all", 0, 4, false, false}, {"all", 4, 0, false, false}, {"edit", -1, 4, false, false},
	} {
		r := sampleReport(t)
		r.Workload.Scenario, r.Workload.Agents, r.Workload.EditAgents = test.scenario, test.readers, test.editors
		r.Phases[0].Name = test.scenario
		if test.floor {
			r.Host.Profile, r.Host.Validated = "floor", true
		}
		r.RequestPlan, _ = hashJSON(r.Workload)
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateReport(data, r); (err == nil) != test.valid {
			t.Fatalf("%s readers%d editors%d floor%t valid%t: %v", test.scenario, test.readers, test.editors, test.floor, test.valid, err)
		}
		if test.valid {
			pureReaderAdded := r.Workload
			pureReaderAdded.Agents++
			hash, _ := hashJSON(pureReaderAdded)
			if hash == r.RequestPlan {
				t.Fatal("adding a pure reader did not change explicit workload identity")
			}
		}
	}
}
