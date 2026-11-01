package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEditProbePhasesAreUnavailableWithReasonsNeverZero(t *testing.T) {
	decode := func(raw string) map[string]any {
		status, err := decodeEditProbeResult(outputWire(raw, nil))
		if err != nil {
			t.Fatal(err)
		}
		return status
	}
	// A baseline build that predates phases_ms reports every lap unavailable.
	for name, metric := range editProbePhases(decode(`{"receipt":"r","graph_status":"fresh"}`), "status exposes no phases_ms") {
		if metric.Value != nil || metric.Reason != "status exposes no phases_ms" || metric.Unit != "ms" {
			t.Fatalf("absent phase %s became %+v", name, metric)
		}
	}
	phases := editProbePhases(decode(`{"phases_ms":{"parse":1.25,"debounce":0,"derived":null,"apply":null,"resolve":"x"},"phase_states":{"derived":"pending","debounce":"measured"}}`), "unused")
	if len(phases) != len(editProbePhaseNames) {
		t.Fatalf("expected every ACK and patch lap: %d", len(phases))
	}
	for name, want := range map[string]string{"derived": "producer phase state pending", "apply": "producer phase state unreported", "resolve": "invalid phases_ms value", "mtimes": "status phases_ms omits mtimes"} {
		if phases[name].Value != nil || phases[name].Reason != want {
			t.Fatalf("%s: %+v, want reason %q", name, phases[name], want)
		}
	}
	// A producer's measured zero stays a measurement; only absence is unavailable.
	if phases["parse"].Value == nil || *phases["parse"].Value != 1.25 || phases["debounce"].Value == nil || *phases["debounce"].Value != 0 {
		t.Fatalf("measured laps lost: %+v %+v", phases["parse"], phases["debounce"])
	}
	corpus := editProbeCorpus()
	plan, err := makeEditProbePlan(corpus, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	baseline, candidate := newEditProbeReport(plan, corpus, false), newEditProbeReport(plan, corpus, true)
	if baseline.Role != "baseline" || candidate.Role != "candidate" {
		t.Fatalf("roles: %q %q", baseline.Role, candidate.Role)
	}
	data, err := json.Marshal(baseline.Records[0].PhasesMS["parse"])
	if err != nil || string(data) != `{"value":null,"unit":"ms","reason":"no receipt status observed"}` {
		t.Fatalf("unsent edit phase is not explicitly unavailable: %s %v", data, err)
	}
}

func TestEditProbeRoleFlagIsEditProbeOnlyAndNotPaired(t *testing.T) {
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", "1", "--worktrees", "0"}
	for role, baseline := range map[string]bool{"": false, "candidate": false, "baseline": true} {
		args := base
		if role != "" {
			args = append(append([]string{}, base...), "--edit-probe-role", role)
		}
		o, err := parseRun(args)
		if err != nil || o.editProbeBaseline != baseline {
			t.Fatalf("role %q: baseline=%v err=%v", role, o.editProbeBaseline, err)
		}
	}
	for _, extra := range [][]string{{"--edit-probe-role", "main"}, {"--edit-probe-role", "baseline", "--scenario", "edit"}} {
		if _, err := parseRun(append(append([]string{}, base...), extra...)); err == nil {
			t.Fatalf("invalid role accepted: %v", extra)
		}
	}
	paired := []string{"--build", "main=/private/main", "--build", "candidate=/private/candidate", "--manifest", "/private/manifest.json", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", "1", "--worktrees", "0", "--edit-probe-role", "baseline"}
	if _, _, err := parsePaired(paired); err == nil || !strings.Contains(err.Error(), "build order") {
		t.Fatalf("paired accepted an explicit role: %v", err)
	}
}
