package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A typed refusal that says the product changed no files is a known outcome.
// The restore proceeds past it, rewrites only checkouts whose bytes moved, and
// sends nothing for a checkout whose edits were all refused. If the bytes of
// such a checkout moved anyway, the byte-guarded rewrite restores them and its
// route records the contradiction. An abandoned tool deadline, a busy reply
// whose delivery is unknown, or a request still outstanding at the deadline
// can still hide a write, so those records keep blocking the restore.
func TestEditProbeRestoreProceedsPastTypedNoMutationRefusals(t *testing.T) {
	const checkout = "expected-linked-checkout"
	toolError := func(text string) func() (any, *rpcError) {
		return func() (any, *rpcError) {
			return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": text}}}, nil
		}
	}
	// Exact product texts from a linked-checkout smoke run.
	routeMoved := toolError("view_read_only: checkout mutation was not admitted; no files were changed: indexer: checkout mutation view is stale: the selected route moved: checkout route changed; read the current exact view and retry. Native fallback status was not established for this checkout. Wait for a fresh view and retry the Gortex edit, or reconnect the MCP session.")
	notRouted := toolError(`view_building: checkout "` + checkout + `" is not fully routed yet`)
	abandoned := toolError(`tool "edit" exceeded its 55.071287166s deadline and was abandoned so the session stays responsive. The work may still complete in the background, so treat any side effect as unknown. This usually means the graph is busy (indexing, enrichment, or a slow store) — retry, or check ` + "`gortex daemon status`.")
	busy := func() (any, *rpcError) {
		return nil, &rpcError{Code: -32002, Message: "MCP dispatcher is busy", Data: map[string]any{"outcome": "server_busy", "retryable": true, "retry_after_ms": 1, "delivery_unknown": false}}
	}
	unknownBusy := func() (any, *rpcError) {
		return nil, &rpcError{Code: -32002, Message: "MCP dispatcher is busy", Data: map[string]any{"outcome": "server_busy", "retryable": true, "delivery_unknown": true}}
	}
	for _, cell := range []struct {
		name    string
		routes  int                       // 2: an applied primary edit beside the refused linked edit
		replies []func() (any, *rpcError) // refused edit's replies in order; the last repeats
		refusal string                    // the record's not-admitted refusal; empty means unknown
		// admitted_late: after the replies, the product admits the edit but
		// replies, and writes, only after the logical deadline, so the request
		// is outstanding when the call ends. changed_anyway: the refused reply
		// still changed the checkout's bytes.
		effect string
	}{
		{"view_read_only_route_moved", 2, []func() (any, *rpcError){routeMoved}, "view_read_only", ""},
		{"view_building_not_routed", 2, []func() (any, *rpcError){notRouted}, "view_building", ""},
		{"busy_then_typed_refusal", 2, []func() (any, *rpcError){busy, notRouted}, "view_building", ""},
		{"busy_until_deadline", 2, []func() (any, *rpcError){busy}, "server_busy", ""},
		{"single_checkout_all_refused", 1, []func() (any, *rpcError){routeMoved}, "view_read_only", ""},
		{"tool_deadline_abandoned", 2, []func() (any, *rpcError){abandoned}, "", ""},
		{"busy_delivery_unknown", 2, []func() (any, *rpcError){unknownBusy}, "", ""},
		{"busy_then_request_outstanding_at_deadline", 2, []func() (any, *rpcError){busy, busy}, "", "admitted_late"},
		{"refusal_but_bytes_changed", 2, []func() (any, *rpcError){routeMoved}, "view_read_only", "changed_anyway"},
	} {
		t.Run(cell.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			c := editProbeCorpus()
			c.Label = "fixture"
			p := &privateRun{Root: t.TempDir()}
			labels := []string{"fixture", "linked-1"}[:cell.routes]
			originals := make([]string, len(labels))
			for i, label := range labels {
				root := filepath.Join(p.Root, label)
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				originals[i] = fmt.Sprintf("package sample\n// checkout%d\nfunc Example() int { return 1 }\n%s\n", i, c.EditAnchor)
				if err := os.WriteFile(filepath.Join(root, c.File), []byte(originals[i]), 0o600); err != nil {
					t.Fatal(err)
				}
				corpus := c
				corpus.Label = label
				p.Repos = append(p.Repos, privateRepo{Path: root, File: filepath.Join(root, c.File), Prefix: "fixture", Corpus: corpus})
			}
			refused := len(p.Repos) - 1 // the linked checkout, or the only one
			disks := make(map[string]func(stubFrame) (any, *rpcError))
			for _, repo := range p.Repos {
				disks[repo.Path] = editProbeDiskStub(t, c, repo.Path)
			}
			var mu sync.Mutex
			refusedCalls := 0
			batches := make(map[string]int)
			mutations := make(map[string]string)
			apply := func(frame stubFrame, repo int) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				options, _ := args["options"].(map[string]any)
				data, err := os.ReadFile(p.Repos[repo].File)
				if err != nil {
					t.Error(err)
					return nil, &rpcError{Code: -32000, Message: "read failed"}
				}
				match, _ := args["match"].(string)
				replacement, _ := args["replacement"].(string)
				if match == "" || strings.Count(string(data), match) != 1 {
					return nil, &rpcError{Code: -32000, Message: "exact match missing"}
				}
				if err := os.WriteFile(p.Repos[repo].File, []byte(strings.Replace(string(data), match, replacement, 1)), 0o600); err != nil {
					t.Error(err)
					return nil, &rpcError{Code: -32000, Message: "write failed"}
				}
				mutation, _ := options["mutation_id"].(string)
				mu.Lock()
				mutations[frame.Cwd] = mutation
				mu.Unlock()
				return stubContent(map[string]any{"mutation_receipt": "applied-receipt"}), nil
			}
			lateSent, release, landed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var lateOnce, releaseOnce sync.Once
			p.Socket = startStub(t, func(frame stubFrame) (any, *rpcError) {
				if frame.Kind == "search_symbols" {
					return map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "repo_prefix": "fixture", "checkout_id": checkout}}, nil
				}
				if frame.Method != "tools/call" {
					return stubContent(map[string]any{}), nil
				}
				repo := -1
				for i, candidate := range p.Repos {
					if frame.Cwd == candidate.Path {
						repo = i
					}
				}
				if repo < 0 {
					return nil, &rpcError{Code: -32000, Message: "unknown checkout"}
				}
				args, _ := frame.Params["arguments"].(map[string]any)
				switch {
				case frame.Params["name"] == "edit" && args["operation"] == "file" && repo == refused:
					mu.Lock()
					call := refusedCalls
					refusedCalls++
					mu.Unlock()
					if cell.effect == "admitted_late" && call >= len(cell.replies) {
						lateOnce.Do(func() { close(lateSent) })
						<-release // held past the logical deadline, and past the restore
						defer close(landed)
						return apply(frame, repo)
					}
					if cell.effect == "changed_anyway" {
						if _, fault := apply(frame, repo); fault != nil {
							return nil, fault
						}
					}
					return cell.replies[min(call, len(cell.replies)-1)]()
				case frame.Params["name"] == "edit" && args["operation"] == "file":
					return apply(frame, repo)
				case frame.Params["name"] == "change" && args["operation"] == "receipt":
					mu.Lock()
					mutation := mutations[frame.Cwd]
					mu.Unlock()
					return stubContent(map[string]any{"found": true, "receipt": "applied-receipt", "mutation_id": mutation, "graph_status_terminal": true, "graph_status": "fresh"}), nil
				case frame.Params["name"] == "edit" && args["operation"] == "batch":
					// The product refuses a rewrite whose old and new bytes match.
					changes, _ := args["changes"].([]any)
					change, _ := changes[0].(map[string]any)
					current, err := os.ReadFile(p.Repos[repo].File)
					if err != nil || len(changes) != 1 || change["old_string"] != string(current) {
						return nil, &rpcError{Code: -32000, Message: "restore guard differs from disk"}
					}
					if change["old_string"] == change["new_string"] {
						return toolError("old_string and new_string are identical")()
					}
					mu.Lock()
					batches[frame.Cwd]++
					mu.Unlock()
				}
				return disks[frame.Cwd](frame)
			})
			// Runs before the stub's cleanup waits for its handlers.
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			phase := &Phase{Metrics: map[string]Metric{}, Gate: map[string]json.RawMessage{}}
			if cell.routes > 1 {
				gate, err := json.Marshal(map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "repo_prefix": "fixture", "checkout_id": checkout}})
				if err != nil {
					t.Fatal(err)
				}
				phase.Gate["linked-1.view"] = gate
			}
			var clients []*socketClient
			t.Cleanup(func() { closeClients(clients) })
			for _, repo := range p.Repos {
				client, err := openMCP(ctx, p.Socket, repo.Path)
				if err != nil {
					t.Fatal(err)
				}
				clients = append(clients, client)
			}
			run, probers, err := prepareEditProbe(ctx, runOptions{Seed: 42, EditCount: 100, Editors: len(p.Repos), Worktrees: len(p.Repos) - 1, Deadline: 2 * time.Second, editProbeBaseline: true}, p, nil, clients, phase)
			clients = append(clients, probers...)
			if run != nil {
				t.Cleanup(func() { _ = run.close() })
			}
			if err != nil {
				t.Fatal(err)
			}
			run.origin = time.Now()
			phase.EditProbe.Records = nil
			for editor := range p.Repos {
				var step editProbeStep
				for _, candidate := range run.plan.Steps {
					if candidate.Editor == editor {
						step = candidate
						break
					}
				}
				state := run.states[step.Ordinal]
				run.activate(ctx, step)
				awaitEditProbeFirstProbe(t, state)
				run.doEdit(ctx, step)
				record := state.record
				if editor != refused {
					if record.Outcome != "complete" || !record.GraphTerminal {
						t.Fatalf("applied primary edit did not complete: %+v", record)
					}
				} else if record.Outcome != "edit_error" || record.Receipt != "" {
					t.Fatalf("refused edit gained a receipt or another outcome: %+v", record)
				}
				phase.EditProbe.Records = append(phase.EditProbe.Records, record)
			}
			before := make([]string, len(p.Repos))
			for i, repo := range p.Repos {
				data, err := os.ReadFile(repo.File)
				if err != nil {
					t.Fatal(err)
				}
				before[i] = string(data)
			}
			changed := cell.effect == "changed_anyway"
			if (before[refused] != originals[refused]) != changed {
				t.Fatalf("refused edit changed its checkout %t, want %t: %q", !changed, changed, before[refused])
			}
			if cell.effect == "admitted_late" {
				select {
				case <-lateSent:
				default:
					t.Fatal("the admitted request was not sent before the logical deadline")
				}
			}
			restoreErr := run.restore(ctx)
			mu.Lock()
			sent, refusedBatches := 0, batches[p.Repos[refused].Path]
			for _, count := range batches {
				sent += count
			}
			mu.Unlock()
			// The report names why a receipt-less record did not block the restore.
			wire, err := json.Marshal(phase.EditProbe.Records[refused])
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			refusal, _ := fields["not_admitted"].(string)
			if cell.refusal == "" {
				if restoreErr == nil || !strings.Contains(restoreErr.Error(), "unknown/pending") {
					t.Fatalf("an unknown edit outcome permitted the restore: %v", restoreErr)
				}
				for i, repo := range p.Repos {
					if data, err := os.ReadFile(repo.File); err != nil || string(data) != before[i] {
						t.Fatalf("a refused restore changed checkout%d: %v", i, err)
					}
				}
				if sent != 0 || refusal != "" {
					t.Fatalf("a refused restore sent %d rewrites; record names not_admitted %q", sent, refusal)
				}
				if cell.effect == "admitted_late" {
					// The write the restore must not race lands now.
					releaseOnce.Do(func() { close(release) })
					select {
					case <-landed:
					case <-ctx.Done():
						t.Fatal("the late admitted write never landed")
					}
					if data, err := os.ReadFile(p.Repos[refused].File); err != nil || string(data) == originals[refused] {
						t.Fatalf("the late admitted write did not land: %v", err)
					}
				}
				return
			}
			if restoreErr != nil {
				t.Fatalf("a typed no-mutation refusal blocked the restore: %v (%s)", restoreErr, phase.EditProbe.Restore)
			}
			for i, repo := range p.Repos {
				if data, err := os.ReadFile(repo.File); err != nil || string(data) != originals[i] {
					t.Fatalf("checkout%d not at its original after restore: %q %v", i, data, err)
				}
			}
			wantRefused := 0
			if changed {
				wantRefused = 1 // the refused checkout's bytes moved, so it is rewritten
			}
			if refusedBatches != wantRefused || sent != len(p.Repos)-1+wantRefused {
				t.Fatalf("restore rewrote %d checkouts (%d refused), want %d refused beside the applied one", sent, refusedBatches, wantRefused)
			}
			if strings.Contains(string(phase.EditProbe.Restore), "unchanged") == changed {
				t.Fatalf("restore evidence names an unchanged checkout %t, want %t: %s", !changed, !changed, phase.EditProbe.Restore)
			}
			if cell.routes > 1 {
				// Recorded, not a failure: every edit on the route was refused
				// without a receipt, yet its bytes moved.
				var rows []map[string]any
				if err := json.Unmarshal(phase.EditProbe.Restore, &rows); err != nil || len(rows) != len(p.Repos) {
					t.Fatalf("routed restore evidence %s: %v", phase.EditProbe.Restore, err)
				}
				for i, row := range rows {
					if flagged, _ := row["not_admitted_but_changed"].(bool); flagged != (changed && i == refused) {
						t.Fatalf("route %d records not_admitted_but_changed %t: %s", i, flagged, phase.EditProbe.Restore)
					}
				}
			}
			if refusal != cell.refusal {
				t.Fatalf("refused record names not_admitted %q, want %q: %s", refusal, cell.refusal, wire)
			}
		})
	}
}

// A single-checkout move whose every edit was refused before admission leaves
// the source and the destination at their originals. The product refuses a
// rewrite of identical bytes, so the restore sends nothing and says so.
func TestEditProbeMoveRestoreSendsNothingWhenEveryEditWasRefused(t *testing.T) {
	const sourceBefore, targetBefore = "package fixture\n\nfunc Moved() int { return 1 }\n", "package fixture\n"
	root := t.TempDir()
	for name, content := range map[string]string{"source.go": sourceBefore, "target.go": targetBefore} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	calls := 0
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Method != "tools/call" {
			return stubContent(map[string]any{}), nil
		}
		mu.Lock()
		calls++
		mu.Unlock()
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "old_string and new_string are identical"}}}, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	refused := editProbeRecord{Outcome: "edit_error", NotAdmitted: "server_busy"}
	run := &editProbeRuntime{options: runOptions{Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Path: root, Prefix: "fixture", Corpus: Corpus{EditDestination: "target.go"}}}}, editors: []*socketClient{client}, plan: editProbePlan{Tool: "refactor.move", File: "source.go"}, original: []byte(sourceBefore), moveOriginal: []byte(targetBefore), requireAccounting: true, phase: &Phase{EditProbe: &editProbeReport{Records: []editProbeRecord{refused, refused}}}}
	if err := run.restore(ctx); err != nil {
		t.Fatalf("a move whose every edit was refused failed its restore: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("restore sent %d calls for a source and destination already at their originals", calls)
	}
	if got := string(run.phase.EditProbe.Restore); got != `{"status":"unchanged","write_sent":false}` {
		t.Fatalf("restore evidence %s does not record the unchanged checkout", got)
	}
}
