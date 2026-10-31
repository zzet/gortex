package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEditProbeCensoredSetupKeepsCompleteOfferedDenominator(t *testing.T) {
	corpus := editProbeCorpus()
	plan, err := makeEditProbePlan(corpus, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	report := newEditProbeReport(plan, corpus, true)
	if len(report.Records) != 100 {
		t.Fatal("startup failure lost planned edits")
	}
	for i, row := range report.Records {
		if row.Ordinal != i || row.Outcome != "phase_censored" || row.Ack.Value != nil || len(row.Errors) == 0 {
			t.Fatalf("fabricated startup record%+v", row)
		}
	}
	root := t.TempDir()
	file := filepath.Join(root, corpus.File)
	if err := os.WriteFile(file, []byte("package p\nfunc Example() int { return 1 }\n"+corpus.EditAnchor+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	phase := &Phase{Metrics: make(map[string]Metric)}
	p := &privateRun{Root: root, Socket: filepath.Join(root, "absent.sock"), Repos: []privateRepo{{Path: root, Corpus: corpus}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	run, clients, err := prepareEditProbe(ctx, runOptions{Seed: 1, EditCount: 100, Editors: 1}, p, []*socketClient{nil}, phase)
	if err == nil || run == nil || len(clients) != 0 {
		t.Fatalf("setup cancellation missingrun=%v clients=%d err=%v", run != nil, len(clients), err)
	}
	_ = run.close() // expected censored records are a diagnostic failure
	if len(phase.EditProbe.Records) != 100 {
		t.Fatal("partial session setup dropped offered edits")
	}
	for _, row := range phase.EditProbe.Records {
		if row.Outcome != "phase_censored" || row.StartedNS != nil || row.Ack.Value != nil {
			t.Fatalf("unstarted edit fabricated timing%+v", row)
		}
	}
}

func TestEditProbeBlockedProbeResponsesCannotDelayEditDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	reached, edited, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var probeOnce, editOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Connection == 2 {
			probeOnce.Do(func() { close(reached) })
			<-release
		}
		if frame.Params["name"] == "edit" {
			editOnce.Do(func() { close(edited) })
			return stubContent(map[string]any{"mutation_receipt": "commit-test"}), nil
		}
		return stubContent(map[string]any{}), nil
	})
	editor, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer editor.Close()
	prober, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer prober.Close()
	corpus := editProbeCorpus()
	plan, err := makeEditProbePlan(corpus, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	plan.Steps[0].ActivateNS = 0
	plan.Steps[0].DueNS = int64(25 * time.Millisecond)
	file, err := os.CreateTemp(t.TempDir(), "probe-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	record := newEditProbeRecord(corpus, plan.Steps[0])
	record.Outcome = "not_started"
	record.Errors = nil
	state := &editProbeState{step: plan.Steps[0], record: record, client: prober, sink: file}
	state.startRaw()
	defer state.finishRaw()
	phase := &Phase{Metrics: make(map[string]Metric), EditProbe: newEditProbeReport(plan, corpus, true)}
	run := &editProbeRuntime{options: runOptions{Seed: 1, Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Prefix: "sample", Corpus: corpus}}}, phase: phase, plan: plan, editors: []*socketClient{editor}, states: []*editProbeState{state}, origin: time.Now(), requireAccounting: true}
	done := make(chan error, 1)
	go func() { done <- runEditProbeArrivals(ctx, plan, run.origin, run.activate, run.doEdit, waitContext) }()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("prober never dispatched")
	}
	select {
	case <-edited:
	case <-ctx.Done():
		t.Fatal("blocked probe response gated actual edit")
	}
	cancel()
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("owned edit/probers did not join")
	}
	state.stopProbes()
	if state.record.StartedNS == nil {
		t.Fatal("edit start not retained")
	}
}

type editProbeResponseClockStub struct {
	finished time.Time
	err      error
}

func (s *editProbeResponseClockStub) call(context.Context, string, any) (json.RawMessage, error) {
	s.finished = time.Now()
	if s.err != nil {
		return nil, s.err
	}
	return json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": "{}"}}})
}
func (s *editProbeResponseClockStub) responseReturnedAt() time.Time { return s.finished }

func TestEditProbeAttemptLatencyUsesResponseEndpoint(t *testing.T) {
	client := &editProbeResponseClockStub{}
	req := request{Tool: "read", Operation: "file"}
	_, attempts, err := callEditProbeTool(t.Context(), client, req, nil, "probe", 0, 0, 0, time.Second, waitContext)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("call: %v %+v", err, attempts)
	}
	want := float64(client.finished.Sub(attempts[0].Started)) / float64(time.Millisecond)
	if attempts[0].DurationMS != want {
		t.Fatalf("response latency includes observer work: got=%v want=%v", attempts[0].DurationMS, want)
	}
}

func TestEditProbeRawWriterDrainsBeforeEvidenceCompletion(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "raw-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	state := &editProbeState{sink: file}
	state.startRaw()
	for i := 0; i < 8; i++ {
		if err := state.emit(map[string]any{"ordinal": i}); err != nil {
			t.Fatal(err)
		}
	}
	state.finishRaw()
	data, err := os.ReadFile(file.Name())
	if err != nil || strings.Count(string(data), "\n") != 8 || state.rawBytes != len(data) || state.sinkErr != nil {
		t.Fatalf("writer not drained: bytes=%d raw=%d err=%v sink=%v", len(data), state.rawBytes, err, state.sinkErr)
	}
}

func TestEditProbeEvidenceFailureIsNotProductFailure(t *testing.T) {
	client := &editProbeResponseClockStub{err: errEditProbeEvidenceIncomplete}
	req := request{Tool: "edit", Operation: "file"}
	_, rows, err := callEditProbeTool(t.Context(), client, req, nil, "workload", 0, 0, 0, time.Second, waitContext)
	if !errors.Is(err, errEditProbeEvidenceIncomplete) || len(rows) != 1 || rows[0].Outcome != "harness_incomplete" {
		t.Fatalf("evidence classification: %+v %v", rows, err)
	}
	metrics := summarize(rows, 1, "workload")["edit.file"]
	if metrics.HarnessIncomplete != 1 || metrics.Successes != 0 || metrics.ProductExpired != 0 || len(metrics.Errors) != 0 {
		t.Fatalf("evidence failure charged to product: %+v", metrics)
	}
}

func TestEditProbeAggregatesDoNotTurnMissingACKOrCensoringIntoSuccess(t *testing.T) {
	corpus := editProbeCorpus()
	plan, err := makeEditProbePlan(corpus, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	report := newEditProbeReport(plan, corpus, true)
	report.Records[0].Outcome = "edit_error"
	report.Records[1].Outcome = "invalid_ack"
	report.Records[2].Outcome = "diagnostic_failure" // decoded response without receipt
	report.Records[3].Outcome = "complete"
	report.Records[3].Ack = measured(12, "ms")
	report.Records[3].FirstCorrect["search_visible"] = measured(15, "ms")
	report.Records[4].Outcome = "phase_censored"
	report.Records[4].Ack = measured(20, "ms") // genuine ACK before phase cancellation
	report.Records[5].Outcome = "product_deadline"
	report.Records[5].Ack = measured(30, "ms")
	phase := &Phase{EditProbe: report}
	finishEditProbeMetrics(phase)
	if len(report.Records) != 100 || phase.EditLatency.Count != 3 || phase.Visibility.Count != 1 || phase.NeverSeen != 1 || phase.Censored != 95 || len(phase.Edits) != 0 {
		t.Fatalf("invalid aggregate denominator: ack=%d visible=%d never=%d censored=%d legacy=%d", phase.EditLatency.Count, phase.Visibility.Count, phase.NeverSeen, phase.Censored, len(phase.Edits))
	}
}

func TestEditProbeRawCapIsExplicitFailure(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "cap-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	state := &editProbeState{sink: file, rawAccepted: editProbeRawLimit}
	state.startRaw()
	defer state.finishRaw()
	if err := state.emit(map[string]any{"required": "proof"}); !errors.Is(err, errEditProbeEvidenceIncomplete) || !strings.Contains(err.Error(), "evidence limit") {
		t.Fatal("dropped evidence silently passed")
	}
	if err := state.emit(nil); !errors.Is(err, state.sinkErr) {
		t.Fatal("evidence failure was cleared")
	}
}

func TestEditProbeEditsAndOwnReadbacksUseTheirFixedEditorConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const editors = 3
	var mu sync.Mutex
	frames := map[int][]stubFrame{}
	edited := make(chan int, 2*editors)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Method != "tools/call" {
			return stubContent(map[string]any{}), nil
		}
		mu.Lock()
		frames[frame.Connection] = append(frames[frame.Connection], frame)
		mu.Unlock()
		if frame.Params["name"] == "edit" {
			edited <- frame.Connection
			if frame.Connection == 1 {
				// Editor 0's ACK stays blocked until both later editors sent.
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return stubContent(map[string]any{"mutation_receipt": "commit-test"}), nil
		}
		return stubContent(map[string]any{}), nil
	})
	corpus := editProbeCorpus()
	full, err := makeEditProbeTrafficPlan(corpus, 7, 100, editors)
	if err != nil {
		t.Fatal(err)
	}
	plan := full
	plan.Steps = nil
	for editor := range editors {
		for _, step := range full.Steps {
			if step.Editor == editor {
				step.Ordinal = len(plan.Steps)
				step.ActivateNS = int64(len(plan.Steps)) * int64(10*time.Millisecond)
				step.DueNS = step.ActivateNS + int64(editProbePeriod)
				plan.Steps = append(plan.Steps, step)
				break
			}
		}
	}
	var sessions []*socketClient // editors on connections 1..3, probers on 4..6
	for range 2 * editors {
		client, err := openMCP(ctx, socket, "")
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		sessions = append(sessions, client)
	}
	phase := &Phase{Metrics: make(map[string]Metric), EditProbe: newEditProbeReport(plan, corpus, true)}
	run := &editProbeRuntime{options: runOptions{Seed: 1, Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Prefix: "sample", Corpus: corpus}}}, phase: phase, plan: plan, editors: sessions[:editors], origin: time.Now(), requireAccounting: true}
	for i, step := range plan.Steps {
		file, err := os.CreateTemp(t.TempDir(), "probe-*.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		record := newEditProbeRecord(corpus, step)
		record.Outcome, record.Errors = "not_started", nil
		state := &editProbeState{step: step, record: record, client: sessions[editors+i], sink: file}
		state.startRaw()
		run.states = append(run.states, state)
	}
	done := make(chan error, 1)
	go func() { done <- runEditProbeArrivals(ctx, plan, run.origin, run.activate, run.doEdit, waitContext) }()
	seen := map[int]bool{}
	for !seen[2] || !seen[3] {
		select {
		case connection := <-edited:
			seen[connection] = true
		case <-ctx.Done():
			t.Fatal("a blocked editor delayed another editor's offered arrival")
		}
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = run.finish() // stub receipts never turn terminal
	mu.Lock()
	defer mu.Unlock()
	for i, step := range plan.Steps {
		if record := run.phase.EditProbe.Records[i]; record.Editor != step.Editor || record.Receipt != "commit-test" || record.PhasesMS["derived"].Reason != "status exposes no phases_ms" {
			t.Fatalf("record lost its fixed editor, ACK or unavailable phases: %+v", record)
		}
		edits, reads := 0, 0
		for _, frame := range frames[step.Editor+1] {
			args, _ := json.Marshal(frame.Params["arguments"])
			for _, other := range plan.Steps {
				if other.Ordinal != step.Ordinal && strings.Contains(string(args), other.Marker) {
					t.Fatalf("editor %d carried another ordinal's call: %s", step.Editor, args)
				}
			}
			switch frame.Params["name"] {
			case "edit":
				edits++
			case "read":
				reads++
			default:
				t.Fatalf("editor session carried a probe/status call: %v", frame.Params["name"])
			}
		}
		if edits != 1 || reads < 2 {
			t.Fatalf("editor %d: edits=%d immediate readbacks=%d", step.Editor, edits, reads)
		}
		for _, frame := range frames[editors+i+1] {
			if frame.Params["name"] == "edit" {
				t.Fatal("prober session carried an edit")
			}
		}
	}
	// Cleanup rewrites the shared file, so it waits for every editor's receipt.
	for i := range run.phase.EditProbe.Records {
		run.phase.EditProbe.Records[i].GraphTerminal = run.phase.EditProbe.Records[i].Editor != 1
	}
	if err := run.restore(ctx); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("restore raced another editor's pending receipt: %v", err)
	}
	if err := run.close(); err != nil {
		t.Fatal(err)
	}
}
