package main

import "testing"

func TestEditProbeGateWitnessRequirements(t *testing.T) {
	for _, mode := range []string{"json", "toon", "facade", "cli", "compact"} {
		t.Run(mode, func(t *testing.T) {
			r := &editProbeRuntime{plan: editProbePlan{GateModes: true}, modeCounts: map[string]editProbeModeCounts{}}
			step := editProbeStep{Ordinal: 7, ProbeMode: mode}
			if r.gateModeComplete(step) {
				t.Fatal("empty witness set completed")
			}
			for _, tool := range editProbeGateTools {
				output := mode
				if mode == "compact" && !editProbeCompactOperations[tool] {
					output = "json"
				}
				// Pre-ACK correctness, a failed/cancelled call, an undecoded
				// answer and another ordinal must never lend this edit proof.
				r.countGateMode(step, output, tool, true, true, true, false, false, true, false)
				r.countGateMode(step, output, tool, false, true, true, false, false, true, true)
				r.countGateMode(step, output, tool, true, false, false, false, false, false, true)
				other := step
				other.Ordinal++
				r.countGateMode(other, output, tool, true, true, true, false, false, true, true)
			}
			if r.gateModeComplete(step) {
				t.Fatal("pre-ACK/cancelled/undecoded/other-edit evidence completed")
			}
			for i, tool := range editProbeGateTools {
				output := mode
				if mode == "compact" && !editProbeCompactOperations[tool] {
					output = "json"
				}
				r.countGateMode(step, output, tool, true, true, true, false, false, true, true)
				if r.gateModeComplete(step) != (i == len(editProbeGateTools)-1) {
					t.Fatalf("completion after %s ignored missing own tool", tool)
				}
			}
			// The helper does not create new receipt counts or require a
			// freshness field on compact output; it reads original witnesses.
			if !r.gateModeComplete(step) {
				t.Fatal("complete original own post-ACK witnesses refused")
			}
		})
	}
	r := &editProbeRuntime{}
	if !r.gateModeComplete(editProbeStep{}) {
		t.Fatal("ordinary non-gate collector changed")
	}
}

func TestEditProbeGateCompactRequiresCorrectSupportedWitness(t *testing.T) {
	r := &editProbeRuntime{plan: editProbePlan{GateModes: true}, modeCounts: map[string]editProbeModeCounts{}}
	step := editProbeStep{Ordinal: 0, ProbeMode: "compact"}
	for _, tool := range editProbeGateTools {
		mode := "compact"
		if !editProbeCompactOperations[tool] {
			mode = "json"
		}
		r.countGateMode(step, mode, tool, true, true, false, false, false, tool != "read.summary", true)
	}
	if r.gateModeComplete(step) {
		t.Fatal("wrong compact summary became semantic proof")
	}
	r.countGateMode(step, "compact", "read.summary", true, true, false, false, false, true, true)
	if !r.gateModeComplete(step) {
		t.Fatal("actual correct compact summary not retained")
	}
}
