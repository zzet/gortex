package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSearchAcceptancePreparationBoundsActualInitializedClients(t *testing.T) {
	for _, search := range []bool{true, false} {
		name, count, expected := "search1000", 1000, 1
		if !search {
			name, count, expected = "ordinary100", 100, 100
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			corpus := editProbeRenameCorpus(t, "go", count)
			corpus.EditTool = "edit.file"
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, corpus.File), []byte(corpus.EditInitialContent), 0o600); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			connections := make(map[int]bool)
			// This control observes initialized transport topology, not repeated
			// AST parsing. Derive every immutable pre-edit reply once from the
			// actual fixture bytes written above; every offered admission still
			// makes its real framed request and verifies its named declaration.
			before := make(map[string]map[string]any)
			for i, line := range strings.Split(corpus.EditInitialContent, "\n") {
				if !strings.HasPrefix(line, "func ") {
					continue
				}
				name, _, found := strings.Cut(strings.TrimPrefix(line, "func "), "(")
				if found {
					id := "fixture/" + corpus.File + "::" + name
					before[id] = map[string]any{"id": id, "file_path": "fixture/" + corpus.File, "source": line, "start_line": i + 1, "end_line": i + 1}
				}
			}
			socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
				mu.Lock()
				connections[frame.Connection] = true
				mu.Unlock()
				if frame.Method == "ping" {
					return map[string]any{}, nil
				}
				args, _ := frame.Params["arguments"].(map[string]any)
				target, _ := args["target"].(map[string]any)
				id, _ := target["symbol"].(string)
				return stubContent(before[id]), nil
			})
			var editors []*socketClient
			for range 8 {
				c, err := openMCP(ctx, socket, root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				if _, err := c.call(ctx, "ping", map[string]any{}); err != nil {
					t.Fatal(err)
				}
				editors = append(editors, c)
			}
			phase := &Phase{Metrics: make(map[string]Metric)}
			private := &privateRun{Root: root, Socket: socket, Repos: []privateRepo{{Path: root, Prefix: "fixture", Corpus: corpus}}}
			options := runOptions{Seed: 42, EditCount: count, Editors: 8, EditMean: time.Second, Deadline: time.Second, SearchReadbackAcceptance: search, editProbeBaseline: true}
			run, clients, err := prepareEditProbe(ctx, options, private, nil, editors, phase)
			if run != nil {
				t.Cleanup(func() { _ = run.close() })
			}
			for _, c := range clients {
				t.Cleanup(func() { _ = c.Close() })
			}
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			actual := len(connections)
			mu.Unlock()
			if len(clients) != expected || actual != 8+expected || phase.Metrics["edit_probe_sessions"].Value == nil || *phase.Metrics["edit_probe_sessions"].Value != float64(expected) {
				t.Fatalf("actual initialized topology: editors=%d probers=%d observed_connections=%d expected_probers=%d", len(editors), len(clients), actual, expected)
			}
			for _, state := range run.states {
				if state.client == nil || state.client.version != "stub-v1" {
					t.Fatal("uninitialized or fabricated prober identity")
				}
				if search && state.client != clients[0] {
					t.Fatal("queued offer owns another prober connection")
				}
			}
			t.Logf("search=%v offers=%d actual_initialized_editor_connections=8 actual_initialized_prober_connections=%d actual_total=%d", search, count, len(clients), actual)
		})
	}
}

func TestSearchAcceptancePostTurnDiagnosticsAreSerialAndWindowBounded(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "serial24"
		if blocked {
			name = "original_window_clips_stage"
		}
		t.Run(name, func(t *testing.T) {
			window := 3 * time.Second
			if blocked {
				window = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), window)
			defer cancel()
			var mu sync.Mutex
			calls, active, peak := 0, 0, 0
			client := searchSequenceClient(t, func(frame stubFrame) any {
				if frame.Method != "tools/call" {
					return map[string]any{}
				}
				mu.Lock()
				calls++
				active++
				peak = max(peak, active)
				mu.Unlock()
				if blocked {
					<-ctx.Done()
				}
				mu.Lock()
				active--
				mu.Unlock()
				return stubContent(map[string]any{})
			})
			r := &editProbeRuntime{p: &privateRun{Repos: []privateRepo{{Prefix: "fixture", Corpus: Corpus{File: "main.go"}}}}, origin: time.Now(), editors: make([]*socketClient, 8)}
			for i := range 8 {
				sink := newEditProbeHeldSink(t)
				sink.unblock()
				s := startHeldEditProbeSpool(t, sink)
				s.client, s.step.Editor, s.step.Marker = client, i, "Marker"
				s.record.SearchACK, s.record.Receipt = searchACKFixture(), "owned"
				r.states = append(r.states, s)
			}
			started := time.Now()
			r.collectSearchAcceptanceDiagnostics(ctx)
			if blocked && time.Since(started) > 500*time.Millisecond {
				t.Fatal("diagnostics extended the original caller window")
			}
			mu.Lock()
			count, maximum := calls, peak
			mu.Unlock()
			if maximum != 1 || !blocked && count != 24 || blocked && count != 1 {
				t.Fatalf("diagnostic topology/retry changed: blocked=%v calls=%d peak=%d", blocked, count, maximum)
			}
			for _, state := range r.states {
				if state.record.GraphTerminal || state.record.Ack.Value != nil {
					t.Fatal("optional observation manufactured acceptance facts")
				}
			}
		})
	}
}

func TestSearchAcceptanceKeepStatePreservesNonPristineCloneWithoutInventedRetirement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	current := []byte("package p\nfunc Changed() {}\n")
	if err := os.WriteFile(path, current, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &editProbeRuntime{plan: editProbePlan{SearchReadbackAcceptance: true}, options: runOptions{KeepState: true}, phase: &Phase{EditProbe: &editProbeReport{Records: []editProbeRecord{{Receipt: "pending", GraphTerminal: false}}}}}
	if err := r.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	var retained map[string]any
	if json.Unmarshal(r.phase.EditProbe.Restore, &retained) != nil || retained["restored"] != false || retained["fresh_seed_reuse_allowed"] != false || r.phase.EditProbe.Records[0].GraphTerminal {
		t.Fatal("retention falsely claimed restoration/freshness")
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(current) {
		t.Fatal("owned after-image was overwritten")
	}
	r.plan.SearchReadbackAcceptance = false
	if err := r.restore(t.Context()); err == nil {
		t.Fatal("generic pending mutation restore was weakened")
	}
}
