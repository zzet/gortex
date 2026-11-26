package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// A linked checkout that is still routing, or whose route moved under an
// edit, refuses the edit before admission and asks for a retry. LOAD re-sends
// the edit within the record's own deadline and counts each re-send on the
// record by refusal code, instead of failing the edit on first sight. Other
// refusals, and a tool deadline that may have left work running, are never
// re-sent.
func TestEditProbeRetriesTransientCheckoutRefusalsWithinDeadline(t *testing.T) {
	const checkout = "expected-linked-checkout"
	// Exact product texts from a linked-checkout smoke run.
	notRouted := `view_building: checkout "` + checkout + `" is not fully routed yet`
	routeMoved := "view_read_only: checkout mutation was not admitted; no files were changed: indexer: checkout mutation view is stale: the selected route moved: checkout route changed; read the current exact view and retry. Native fallback status was not established for this checkout. Wait for a fresh view and retry the Gortex edit, or reconnect the MCP session."
	readOnly := "view_read_only: a pinned commit view only supports reads"
	abandoned := `tool "edit" exceeded its 55.071287166s deadline and was abandoned so the session stays responsive. The work may still complete in the background, so treat any side effect as unknown.`
	const deadline = 2 * time.Second
	for _, cell := range []struct {
		name    string
		refuse  []string // edit replies before the edit applies; the last repeats when apply is false
		apply   bool
		calls   int // edit requests the product saw; 0 means at least two
		retries map[string]int
		outcome string
	}{
		{"building_then_moved_then_applied", []string{notRouted, routeMoved}, true, 3, map[string]int{"view_building": 1, "view_read_only": 1}, "complete"},
		{"building_until_deadline", []string{notRouted}, false, 0, nil, "edit_error"},
		{"other_read_only_is_terminal", []string{readOnly}, false, 1, nil, "edit_error"},
		{"abandoned_tool_deadline_is_terminal", []string{abandoned}, false, 1, nil, "edit_error"},
	} {
		t.Run(cell.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			c := editProbeCorpus()
			c.Label = "fixture"
			p := &privateRun{Root: t.TempDir()}
			for i, label := range []string{"fixture", "linked-1"} {
				root := filepath.Join(p.Root, label)
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				content := fmt.Sprintf("package sample\n// checkout%d\nfunc Example() int { return 1 }\n%s\n", i, c.EditAnchor)
				if err := os.WriteFile(filepath.Join(root, c.File), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				corpus := c
				corpus.Label = label
				p.Repos = append(p.Repos, privateRepo{Path: root, File: filepath.Join(root, c.File), Prefix: "fixture", Corpus: corpus})
			}
			linked := p.Repos[1]
			disk := editProbeDiskStub(t, c, linked.Path)
			var mu sync.Mutex
			calls := 0
			mutation := ""
			p.Socket = startStub(t, func(frame stubFrame) (any, *rpcError) {
				if frame.Kind == "search_symbols" {
					return map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "repo_prefix": "fixture", "checkout_id": checkout}}, nil
				}
				if frame.Method != "tools/call" {
					return stubContent(map[string]any{}), nil
				}
				args, _ := frame.Params["arguments"].(map[string]any)
				if frame.Params["name"] == "edit" && args["operation"] == "file" {
					mu.Lock()
					call := calls
					calls++
					mu.Unlock()
					if call < len(cell.refuse) || !cell.apply {
						text := cell.refuse[min(call, len(cell.refuse)-1)]
						return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": text}}}, nil
					}
					data, err := os.ReadFile(linked.File)
					if err != nil {
						t.Error(err)
						return nil, &rpcError{Code: -32000, Message: "read failed"}
					}
					match, _ := args["match"].(string)
					replacement, _ := args["replacement"].(string)
					if match == "" || strings.Count(string(data), match) != 1 {
						return nil, &rpcError{Code: -32000, Message: "exact match missing"}
					}
					if err := os.WriteFile(linked.File, []byte(strings.Replace(string(data), match, replacement, 1)), 0o600); err != nil {
						t.Error(err)
						return nil, &rpcError{Code: -32000, Message: "write failed"}
					}
					options, _ := args["options"].(map[string]any)
					mu.Lock()
					mutation, _ = options["mutation_id"].(string)
					mu.Unlock()
					return stubContent(map[string]any{"mutation_receipt": "linked-receipt"}), nil
				}
				if frame.Params["name"] == "change" && args["operation"] == "receipt" {
					mu.Lock()
					defer mu.Unlock()
					return stubContent(map[string]any{"found": true, "receipt": "linked-receipt", "mutation_id": mutation, "checkout_id": checkout, "graph_status_terminal": true, "graph_status": "fresh"}), nil
				}
				return disk(frame)
			})
			gate, err := json.Marshal(map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "repo_prefix": "fixture", "checkout_id": checkout}})
			if err != nil {
				t.Fatal(err)
			}
			phase := &Phase{Metrics: map[string]Metric{}, Gate: map[string]json.RawMessage{"linked-1.view": gate}}
			var clients []*socketClient
			t.Cleanup(func() { closeClients(clients) })
			for _, repo := range p.Repos {
				client, err := openMCP(ctx, p.Socket, repo.Path)
				if err != nil {
					t.Fatal(err)
				}
				clients = append(clients, client)
			}
			run, probers, err := prepareEditProbe(ctx, runOptions{Seed: 42, EditCount: 100, Editors: 2, Worktrees: 1, Deadline: deadline, editProbeBaseline: true}, p, nil, clients, phase)
			clients = append(clients, probers...)
			if run != nil {
				t.Cleanup(func() { _ = run.close() })
			}
			if err != nil {
				t.Fatal(err)
			}
			run.origin = time.Now()
			var step editProbeStep
			for _, candidate := range run.plan.Steps {
				if candidate.Editor == 1 {
					step = candidate
					break
				}
			}
			state := run.states[step.Ordinal]
			run.activate(ctx, step)
			awaitEditProbeFirstProbe(t, state)
			started := time.Now()
			run.doEdit(ctx, step)
			elapsed := time.Since(started)
			record := state.record
			mu.Lock()
			seen := calls
			mu.Unlock()
			// The report carries the re-sends by refusal code.
			wire, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			var fields struct {
				Retries map[string]int `json:"edit_refusal_retries"`
			}
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			if record.Outcome != cell.outcome {
				t.Fatalf("edit outcome %q after %d requests, want %q: %v", record.Outcome, seen, cell.outcome, record.Errors)
			}
			if cell.calls > 0 && seen != cell.calls || cell.calls == 0 && seen < 2 {
				t.Fatalf("product saw %d edit requests, want %d (0: a retry within the deadline)", seen, cell.calls)
			}
			if elapsed > deadline+time.Second {
				t.Fatalf("retries outlived the record's %v deadline: %v", deadline, elapsed)
			}
			want := cell.retries
			if cell.calls == 0 {
				want = map[string]int{"view_building": seen - 1}
			}
			if !reflect.DeepEqual(fields.Retries, want) {
				t.Fatalf("record retries %v, want %v: %s", fields.Retries, want, wire)
			}
			if cell.apply && (!record.GraphTerminal || record.Receipt != "linked-receipt") {
				t.Fatalf("retried edit lost its receipt: %+v", record)
			}
		})
	}
}
