package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type editProbeFakeRestarter struct {
	mu       sync.Mutex
	calls    []string
	startErr error
	onStart  func()
}

func (f *editProbeFakeRestarter) kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "kill")
	return nil
}

func (f *editProbeFakeRestarter) start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start")
	if f.onStart != nil {
		f.onStart()
	}
	return f.startErr
}

// The edit splices the fixture on disk. Its receipt reads pending (or
// terminal) before the kill and is unknown to the restarted daemon. Sessions
// opened before the kill (connections 1 and 2) must not be used after it.
func editProbeRestartStub(t *testing.T, c Corpus, dir string, terminal bool, restarted *atomic.Bool) func(stubFrame) (any, *rpcError) {
	disk := editProbeDiskStub(t, c, dir)
	return func(frame stubFrame) (any, *rpcError) {
		args, _ := frame.Params["arguments"].(map[string]any)
		if frame.Method == "tools/call" && restarted.Load() && frame.Connection <= 2 {
			t.Errorf("pre-kill session %d used after restart", frame.Connection)
		}
		switch {
		case frame.Params["name"] == "edit" && args["operation"] == "file":
			path := filepath.Join(dir, c.File)
			data, err := os.ReadFile(path)
			if err == nil {
				err = os.WriteFile(path, []byte(strings.Replace(string(data), args["match"].(string), args["replacement"].(string), 1)), 0o600)
			}
			if err != nil {
				t.Error(err)
			}
			return stubContent(map[string]any{"mutation_receipt": "receipt-1"}), nil
		case frame.Params["name"] == "change":
			if restarted.Load() {
				return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unknown receipt: receipt-1"}}}, nil
			}
			return stubContent(map[string]any{"graph_status_terminal": terminal, "graph_status": "pending"}), nil
		}
		return disk(frame)
	}
}

func runEditProbeRestartCell(t *testing.T, terminal bool, startErr error) (*editProbeRuntime, *editProbeFakeRestarter, []*socketClient) {
	t.Helper()
	c := editProbeCorpus()
	c.EditSender = editProbeRestart
	plan, err := makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, c.File), []byte("package sample\n\nfunc Example() int { return 1 }\n\n"+c.EditAnchor+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var restarted atomic.Bool
	run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, editProbeRestartStub(t, c, dir, terminal, &restarted))
	fake := &editProbeFakeRestarter{startErr: startErr, onStart: func() { restarted.Store(true) }}
	run.restarter = fake
	before := []*socketClient{run.editors[0], run.states[0].client}
	run.activate(ctx, plan.Steps[0])
	awaitEditProbeFirstProbe(t, run.states[0])
	run.doEdit(ctx, plan.Steps[0])
	t.Cleanup(func() {
		for _, client := range run.restarted {
			_ = client.Close()
		}
	})
	return run, fake, before
}

func TestEditProbeRestartReopensSessionsAndNeverReusesPreKillAnswers(t *testing.T) {
	run, fake, before := runEditProbeRestartCell(t, false, nil)
	record := run.states[0].record
	restart := record.Restart
	if record.Outcome != "complete" || len(record.Errors) != 0 || restart == nil || !restart.PendingBeforeKill || restart.Qualification != "" || !reflect.DeepEqual(fake.calls, []string{"kill", "start"}) {
		t.Fatalf("restart cell did not complete after a pending kill: %+v %+v %v", record, restart, fake.calls)
	}
	if restart.KillNS == nil || restart.ReadyNS == nil || *restart.ReadyNS < *restart.KillNS || restart.Downtime.Value == nil || !strings.Contains(restart.StatusAfterError, "unknown receipt") || len(restart.StatusAfterRestart) == 0 || !restart.DiskHasMarker {
		t.Fatalf("restart evidence missing: %+v", restart)
	}
	if record.Ack.Value == nil || record.Readback.Value == nil || record.Accounting.State != "not_applicable_restart" || record.Derived.Value != nil {
		t.Fatalf("restart record timings: %+v", record)
	}
	for name, lap := range record.PhasesMS {
		if lap.Value != nil {
			t.Fatal("restart invented a phase for the interrupted receipt", name)
		}
	}
	for name, value := range record.FirstCorrect {
		fromReady := restart.FromReady[name]
		if _, kept := restart.BeforeKill[name]; !kept || value.Value == nil || fromReady.Value == nil || *fromReady.Value < 0 {
			t.Fatalf("%s first-correct is not a post-restart answer: %+v %+v", name, value, fromReady)
		}
	}
	if run.editors[0] == before[0] || run.states[0].client == before[1] || len(run.restarted) != 2 {
		t.Fatal("restart did not reopen the editor and prober sessions")
	}
	// The killed daemon's acknowledged write is on disk, so restore proceeds.
	run.phase.EditProbe.Records = []editProbeRecord{record}
	if err := run.restore(t.Context()); err != nil || run.phase.EditProbe.RestoreMS.Value == nil {
		t.Fatalf("restore after restart: %v", err)
	}
}

func TestEditProbeRestartRequiresObservedPendingReceipt(t *testing.T) {
	run, fake, _ := runEditProbeRestartCell(t, true, nil)
	record := run.states[0].record
	if record.Outcome != "diagnostic_failure" || record.Restart.Qualification != "receipt terminal before kill" || !slices.Contains(record.Errors, "receipt terminal before kill") || len(fake.calls) != 2 {
		t.Fatalf("terminal receipt passed as a restart: %+v", record)
	}

	run, _, before := runEditProbeRestartCell(t, false, errors.New("socket never came back"))
	record = run.states[0].record
	if record.Outcome != "diagnostic_failure" || record.Restart.ReadyNS != nil || record.Restart.Downtime.Value != nil || run.editors[0] != before[0] || len(run.restarted) != 0 {
		t.Fatalf("failed restart reported as measured: %+v", record.Restart)
	}
	for name, value := range record.Restart.FromReady {
		if value.Value != nil || value.Reason != "daemon not restarted" {
			t.Fatalf("%s invented a time from a restart that never became ready", name)
		}
	}

	c := editProbeCorpus()
	c.EditSender = editProbeRestart
	toon, write, rename := c, editProbeWriteCorpus(t, "go"), editProbeRenameCorpus(t, "go", 1)
	toon.EditOutput = "toon"
	write.EditSender = editProbeRestart
	rename.EditTool, rename.EditSender = "edit.file", editProbeRestart
	for name, build := range map[string]func() (editProbePlan, error){
		"hundred_edits": func() (editProbePlan, error) { return makeEditProbePlan(c, 42, 100) },
		"readers": func() (editProbePlan, error) {
			return planEditProbe(c, runOptions{Seed: 42, EditCount: 1, Editors: 1, Agents: 1})
		},
		"toon":       func() (editProbePlan, error) { return makeEditProbePlan(toon, 42, 1) },
		"write_tool": func() (editProbePlan, error) { return makeEditProbePlan(write, 42, 1) },
		"rename":     func() (editProbePlan, error) { return makeEditProbePlan(rename, 42, 1) },
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s: restart cell planned", name)
		}
	}
}
