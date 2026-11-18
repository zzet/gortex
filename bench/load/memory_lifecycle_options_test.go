package main

import (
	"strings"
	"testing"
)

func TestMemoryGaugesRefuseIdleStop(t *testing.T) {
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir()}
	idle := append(append([]string(nil), base...), "--scenario", "idle-stop", "--agents", "0", "--edit-agents", "0", "--worktrees", "0")
	if _, err := parseRun(idle); err != nil {
		t.Fatalf("ordinary idle-stop refused: %v", err)
	}
	for _, profiles := range []bool{false, true} {
		memory := append(append([]string(nil), idle...), "--gauges")
		if profiles {
			memory = append(memory, "--heap-profiles")
		}
		if _, err := parseRun(memory); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("idle-stop accepted an extra live health session: profiles=%v err=%v", profiles, err)
		}
	}
	// Ordinary restart starts its series only after the new process is
	// ready; all uses fresh per-scenario processes and excludes idle-stop.
	// Step mode enables its supported gauge series implicitly.
	for _, extra := range [][]string{
		{"--scenario", "read", "--gauges"},
		{"--scenario", "edit", "--gauges", "--heap-profiles"},
		{"--scenario", "restart", "--gauges"},
		{"--scenario", "restart", "--gauges", "--heap-profiles"},
		{"--scenario", "all", "--gauges"},
		{"--scenario", "all", "--gauges", "--heap-profiles"},
		{"--scenario", "worktrees", "--worktrees", "2", "--worktree-steps", "0,2"},
	} {
		if _, err := parseRun(append(append([]string(nil), base...), extra...)); err != nil {
			t.Errorf("supported memory measurement refused: %v: %v", extra, err)
		}
	}
}

func TestEditProbeMemoryGaugesRefuseRestartSender(t *testing.T) {
	corpus := editProbeCorpus()
	corpus.EditSender = editProbeRestart
	options := runOptions{Scenario: editProbeScenario, Seed: 42, EditCount: 1, Editors: 1}
	if _, err := planEditProbe(corpus, options); err != nil {
		t.Fatalf("restart cell without memory telemetry refused: %v", err)
	}
	for _, profiles := range []bool{false, true} {
		options.Gauges, options.HeapProfiles = true, profiles
		if _, err := planEditProbe(corpus, options); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("restart cell accepted stale memory telemetry: profiles=%v err=%v", profiles, err)
		}
		// Optional series and heap captures do not disable ordinary edit
		// probes or native editor saves on one continuous daemon.
		for _, supported := range []Corpus{editProbeCorpus(), editProbeEditorSaveCorpus(t, 1)} {
			if _, err := planEditProbe(supported, options); err != nil {
				t.Errorf("steady edit measurement refused: sender=%q profiles=%v: %v", supported.EditSender, profiles, err)
			}
		}
	}
}

func TestMemoryGaugesRefuseStartupProbeWithoutARecordedSeries(t *testing.T) {
	if err := validateMemoryOptions(runOptions{Scenario: startupScenario}); err != nil {
		t.Fatalf("ordinary startup probe refused: %v", err)
	}
	for _, profiles := range []bool{false, true} {
		err := validateMemoryOptions(runOptions{Scenario: startupScenario, Gauges: true, HeapProfiles: profiles})
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("startup probe silently accepted an unrecorded series: profiles=%v err=%v", profiles, err)
		}
	}
}
