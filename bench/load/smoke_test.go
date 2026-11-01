package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestSmokeRestoresOnlyOwnedCloneBytesBeforeReuse(t *testing.T) {
	c := gitFixture(t)
	p, err := newPrivateRun(context.Background(), t.TempDir(), "/owned/build", []Corpus{c})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(p.SocketDir) })
	if err := validateSmokeOwner(p, "/other/build", []Corpus{c}); err == nil {
		t.Fatal("main/candidate store ownership mixed")
	}
	if err := os.WriteFile(p.Repos[0].File, []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Repos[0].Path, "untracked.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.process = &exec.Cmd{}
	if err := restoreSmokeClone(context.Background(), p); err == nil {
		t.Fatal("restored files while an owned daemon was still live")
	}
	p.process = nil
	if err := restoreSmokeClone(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.Repos[0].File)
	if err != nil || string(data) != "package sample\nfunc Example() {}\n" {
		t.Fatal("exact pinned bytes were not restored")
	}
	if _, err := os.Stat(filepath.Join(p.Repos[0].Path, "untracked.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("untracked edits bled into later repetitions")
	}
	before, err := os.ReadFile(filepath.Join(c.Source, c.File))
	if err != nil {
		t.Fatal(err)
	}
	p.Repos[0].Path = c.Source
	if err := restoreSmokeClone(context.Background(), p); err == nil {
		t.Fatal("restoration escaped into the read-only fixture")
	}
	after, err := os.ReadFile(filepath.Join(c.Source, c.File))
	if err != nil || string(before) != string(after) {
		t.Fatal("source fixture changed")
	}
}

type idleUnavailableSampler struct{}

func (idleUnavailableSampler) sample(int) map[string]Metric {
	return absentProcessMetrics("unit fixture has no real process counters")
}

func TestIdleStopClosesSessionsAndRetainsCanceledInterval(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "parent-canceled"}[canceled], func(t *testing.T) {
			socket := startStub(t, func(stubFrame) (any, *rpcError) { return map[string]any{}, nil })
			client, err := openMCP(context.Background(), socket, "/fixture")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			}
			p := &privateRun{Store: filepath.Join(t.TempDir(), "store.db"), process: &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}}
			phase := Phase{Gate: map[string]json.RawMessage{}, Metrics: map[string]Metric{}}
			err = measureIdleStop(ctx, p, []*socketClient{client}, &phase, idleUnavailableSampler{}, "")
			if canceled && !errors.Is(err, context.Canceled) || !canceled && err != nil {
				t.Fatalf("wrong idle interval result: %v", err)
			}
			var receipt struct {
				Complete bool `json:"complete"`
				Sessions int  `json:"closed_mcp_sessions"`
			}
			if err := json.Unmarshal(phase.Gate["idle_stop"], &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Complete == canceled || receipt.Sessions != 1 || !canceled && phase.DurationSec < 2 || len(phase.Attempts) != 0 {
				t.Fatal("idle evidence invented work or waived cancellation/closed sessions")
			}
			select {
			case <-client.done:
			default:
				t.Fatal("idle measurement retained a live MCP session")
			}
		})
	}
}

func TestIdleScenarioDoesNotChangeFrozenAllProfile(t *testing.T) {
	actual, err := selectedScenarios(runOptions{Scenario: "all", Host: "reference"})
	if err != nil || !reflect.DeepEqual(actual, []string{"read", "edit", "many", "many-edit", "worktrees", "cold", "restart", "sweep"}) {
		t.Fatal("adding explicit idle-stop changed the frozen all profile")
	}
	if err := validateAgentCounts("idle-stop", 0, 0); err != nil {
		t.Fatal(err)
	}
	for _, counts := range [][2]int{{1, 0}, {0, 1}} {
		if err := validateAgentCounts("idle-stop", counts[0], counts[1]); err == nil {
			t.Fatal("idle-stop admitted a workload population")
		}
	}
}

func TestSmokeLogCensusDoesNotReplayPriorProcessLifetime(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{"daemon-1.stderr.log": "panic: old failure\n", "daemon-2.stderr.log": "healthy second process\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, crashes, err := collectRunLogs(root, t.TempDir(), 2)
	if err != nil || crashes["panic_lines"] != 0 || files["daemon-2.stderr.log"] == "" || files["daemon-1.stderr.log"] != "" {
		t.Fatal("retained store's prior foreground log was replayed into a new phase")
	}
	_, legacyCrashes, err := collectLogs(root)
	if err != nil || legacyCrashes["panic_lines"] != 1 {
		t.Fatal("default standalone crash census changed")
	}
}

func TestSmokeRestorationWaitsForPublicMarkerRemoval(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(map[bool]string{false: "publication-catches-up", true: "stale-until-deadline"}[permanent], func(t *testing.T) {
			var calls atomic.Int64
			socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
				if frame.Method != "tools/call" {
					return map[string]any{}, nil
				}
				count := calls.Add(1)
				text := `[]`
				if count == 1 || permanent {
					text = `[{"id":"sample.go::Marker","name":"Marker"}]`
				}
				return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil
			})
			client, err := openMCP(context.Background(), socket, "/fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			bound := time.Second
			if permanent {
				bound = 30 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), bound)
			defer cancel()
			phase := Phase{Gate: map[string]json.RawMessage{}}
			p := &privateRun{Repos: []privateRepo{{Corpus: sampleCorpus(), Prefix: "sample"}}}
			err = awaitSmokeRestoration(ctx, runOptions{Deadline: time.Second, smokeMarkers: []string{"Marker"}}, p, client, &phase)
			if permanent && err == nil || !permanent && err != nil {
				t.Fatalf("wrong restoration/publication admission: %v", err)
			}
			if permanent && len(phase.Gate["pr_smoke_restoration"]) != 0 || !permanent && (calls.Load() < 2 || len(phase.Gate["pr_smoke_restoration"]) == 0) {
				t.Fatal("a pending marker became a successful restoration fence")
			}
		})
	}
}
