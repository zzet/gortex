package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEditProbeNoEditWindowPrecedesActivationWithoutShift(t *testing.T) {
	c := editProbeCorpus()
	base, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	withReaders, err := planEditProbe(c, runOptions{Seed: 42, EditCount: 100, Editors: 1, Agents: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withReaders.Steps, base.Steps) || withReaders.Readers != 4 || withReaders.NoEditWindow != int64(editProbeNoEditWindow) {
		t.Fatalf("readers shifted the offered edits or lost the window: readers=%d window=%d", withReaders.Readers, withReaders.NoEditWindow)
	}
	baseBudget, err := base.budget(time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if budget, err := withReaders.budget(time.Minute, 1); err != nil || budget != baseBudget+editProbeNoEditWindow {
		t.Fatalf("no-edit window not budgeted: %v %v", budget, err)
	}
	if none, err := planEditProbe(c, runOptions{Seed: 42, EditCount: 100, Editors: 1}); err != nil || !reflect.DeepEqual(none, base) {
		t.Fatal("zero readers changed the default plan", err)
	}
	if _, err := planEditProbe(c, runOptions{Seed: 42, EditCount: 100, Editors: 1, Agents: editProbeMaxReaders + 1}); err == nil {
		t.Fatal("reader cap not enforced")
	}
	args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--edit-agents", "2", "--worktrees", "0"}
	if _, err := parseRun(append(args, "--agents", "4")); err != nil {
		t.Fatal(err)
	}
	if _, err := parseRun(append(args, "--agents", "33")); err == nil {
		t.Fatal("33 readers accepted")
	}
	paired := []string{"--build", "main=/private/main", "--build", "candidate=/private/candidate", "--manifest", "/private/manifest.json", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "1", "--edit-agents", "1", "--worktrees", "0"}
	if _, _, err := parsePaired(paired); err == nil {
		t.Fatal("paired no-regression cell accepted readers")
	}
}

func TestEditProbeReadsClassifyByPendingEditsAndNeverSampleFailures(t *testing.T) {
	r := &editProbeRuntime{origin: time.Now().Add(time.Hour)}
	if got := r.readClass(time.Now()); got != "no_edit_baseline" {
		t.Fatal(got)
	}
	r.origin = time.Now().Add(-time.Second)
	r.pendingEdits.Add(1)
	if got := r.readClass(time.Now()); got != "under_edit" {
		t.Fatal(got)
	}
	r.pendingEdits.Add(-1)
	if got := r.readClass(time.Now()); got != "between_edits" {
		t.Fatal(got)
	}
	reads := []editProbeRead{
		{"read.summary", "under_edit", "success", 7},
		{"read.summary", "under_edit", "success", 5},
		{"read.summary", "under_edit", "busy", 0},
		{"read.summary", "under_edit", "phase_censored", 0},
		{"read.summary", "under_edit", "product_deadline", 0},
		{"read.summary", "no_edit_baseline", "success", 3},
	}
	summary := summarizeEditProbeReads(reads, 1)
	under := summary["read.summary"]["under_edit"]
	// Deadline and run-end cuts rank above both successes; the busy read has
	// no latency at all.
	if under.Calls != 5 || under.Busy != 1 || under.Censored != 1 || under.Errors != 1 || under.Latency.Count != 2 || under.Latency.Censored != 2 || under.Latency.NotApplicable != 1 || *under.Latency.P50.Value != 7 || under.Latency.P95.Value != nil {
		t.Fatalf("failed reads entered the latency sample: %+v", under)
	}
	if baseline := summary["read.summary"]["no_edit_baseline"]; baseline.Calls != 1 || *baseline.Latency.Max.Value != 3 {
		t.Fatalf("baseline cell: %+v", baseline)
	}
	if summarizeEditProbeReads(nil, 1) != nil {
		t.Fatal("no readers reported an empty read table")
	}
}

// Reads are under edit while a sent edit's receipt is pending. Once its own
// status is terminal, reads issued while the edit still waits for its probes
// are between edits.
func TestEditProbeReadsUnderEditEndWhenTheReceiptTurnsTerminal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c := editProbeCorpus()
	plan, err := makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, c.File), []byte("package sample\n\n"+c.EditAnchor+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var terminal atomic.Bool
	run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(frame stubFrame) (any, *rpcError) {
		switch frame.Params["name"] {
		case "edit":
			return stubContent(map[string]any{"mutation_receipt": "receipt-1"}), nil
		case "change":
			return stubContent(map[string]any{"mutation_id": plan.Steps[0].MutationID, "graph_status": "fresh", "graph_status_terminal": terminal.Load()}), nil
		}
		return stubContent(map[string]any{}), nil // probes and readbacks never answer correctly
	})
	run.options.Deadline = time.Second
	run.activate(ctx, plan.Steps[0])
	awaitEditProbeFirstProbe(t, run.states[0])
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.doEdit(ctx, plan.Steps[0])
	}()
	await := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); run.readClass(time.Now()) != want; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("read class never became %s", want)
			}
		}
	}
	await("under_edit")
	terminal.Store(true)
	await("between_edits")
	select {
	case <-done:
		t.Fatal("the edit ended before its probes; a terminal receipt was not observed mid-loop")
	default:
	}
	<-done
	if got := run.readClass(time.Now()); got != "between_edits" || run.pendingEdits.Load() != 0 {
		t.Fatalf("pending edits after the loop: %s %d", got, run.pendingEdits.Load())
	}
}

func TestEditProbeReadersUseOwnSessionsAndClassAtIssue(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	connections := map[int]int{}
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Method == "tools/call" {
			mu.Lock()
			connections[frame.Connection]++
			mu.Unlock()
		}
		return stubContent(map[string]any{}), nil
	})
	var readers []*socketClient
	for range 2 {
		client, err := openMCP(ctx, socket, "")
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		readers = append(readers, client)
	}
	// The saturation profile removes think time so the test stays short.
	r := &editProbeRuntime{options: runOptions{Profile: "saturation", Seed: 1, Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Prefix: "sample", Corpus: editProbeCorpus()}}}, phase: &Phase{}, readers: readers}
	r.origin = time.Now().Add(150 * time.Millisecond)
	readCtx, stop := context.WithTimeout(ctx, 600*time.Millisecond)
	defer stop()
	var group sync.WaitGroup
	r.startReaders(readCtx, &group)
	group.Wait()
	classes := map[string]int{}
	for _, read := range r.reads {
		classes[read.class]++
	}
	if classes["no_edit_baseline"] == 0 || classes["between_edits"] == 0 || classes["under_edit"] != 0 {
		t.Fatalf("reads not classed by issue time: %v", classes)
	}
	for _, attempt := range r.phase.Attempts {
		if attempt.Role != "reader" || attempt.Agent < 0 || attempt.Agent > 1 {
			t.Fatalf("reader attempt identity: %+v", attempt)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connections) != 2 || connections[1] == 0 || connections[2] == 0 {
		t.Fatalf("readers did not use their own sessions: %v", connections)
	}
}
