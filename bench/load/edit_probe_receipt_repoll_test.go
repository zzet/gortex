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

// A receipt that was not graph-terminal when its edit's deadline ended is read
// once more before the restore, inside the restore's own deadline. Terminal at
// that read, it no longer blocks the restore, and the record keeps the read's
// tiers as post-workload evidence while its workload fields stay as observed.
// Still pending, terminal for another checkout, or unanswered before the
// restore deadline, it keeps blocking the restore and no checkout is rewritten.
func TestEditProbeRestoreRepollsReceiptsPendingAtWorkloadEnd(t *testing.T) {
	const (
		checkout    = "expected-linked-checkout"
		ackAt       = "2026-01-01T00:00:00.000000001Z"
		fileGraphAt = "2026-01-01T00:00:00.000000002Z"
		relationsAt = "2026-01-01T00:00:00.000000003Z"
		derivedAt   = "2026-01-01T00:00:00.000000004Z"
	)
	pending := map[string]any{"graph_status_terminal": false, "graph_status": "pending", "ack_at": ackAt, "file_graph_at": fileGraphAt, "tier_state": map[string]any{"file_graph": "fresh", "relations": "pending", "derived": "pending"}}
	terminal := map[string]any{"graph_status_terminal": true, "graph_status": "fresh", "ack_at": ackAt, "file_graph_at": fileGraphAt, "relations_at": relationsAt, "derived_at": derivedAt, "tier_state": map[string]any{"file_graph": "fresh", "relations": "fresh", "derived": "fresh"}}
	for _, cell := range []struct {
		name     string
		repoll   map[string]any // the linked receipt's status after the workload; nil holds the reply past the restore deadline
		checkout string         // the checkout that status names
		state    string         // the record's post-workload receipt state
	}{
		{"terminal_at_repoll", terminal, checkout, "terminal"},
		{"still_pending_at_repoll", pending, checkout, "pending"},
		{"terminal_for_another_checkout", terminal, "another-checkout", "unavailable"},
		{"unanswered_within_restore_deadline", nil, checkout, "unavailable"},
	} {
		t.Run(cell.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			c := editProbeCorpus()
			c.Label = "fixture"
			p := &privateRun{Root: t.TempDir()}
			labels := []string{"fixture", "linked-1"}
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
			const linked = 1 // its receipt stays pending for the whole workload
			disks := make(map[string]func(stubFrame) (any, *rpcError))
			for _, repo := range p.Repos {
				disks[repo.Path] = editProbeDiskStub(t, c, repo.Path)
			}
			held := make(chan struct{})
			var mu sync.Mutex
			workloadOver := false
			repolls := make(map[string]int)
			batches := make(map[string]int)
			mutations := make(map[string]string)
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
				options, _ := args["options"].(map[string]any)
				receipt := p.Repos[repo].Corpus.Label + "-receipt"
				switch {
				case frame.Params["name"] == "edit" && args["operation"] == "file":
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
					return stubContent(map[string]any{"mutation_receipt": receipt}), nil
				case frame.Params["name"] == "change" && args["operation"] == "receipt":
					if options["receipt"] != receipt {
						return nil, &rpcError{Code: -32000, Message: "unknown receipt"}
					}
					mu.Lock()
					mutation, over := mutations[frame.Cwd], workloadOver
					if over {
						repolls[frame.Cwd]++
					}
					mu.Unlock()
					status := map[string]any{"found": true, "receipt": receipt, "mutation_id": mutation, "graph_status_terminal": true, "graph_status": "fresh"}
					if repo == linked {
						fields := pending
						if over {
							if cell.repoll == nil {
								<-held
							}
							fields = cell.repoll
						}
						status = map[string]any{"found": true, "receipt": receipt, "mutation_id": mutation, "checkout_id": checkout}
						if over {
							status["checkout_id"] = cell.checkout
						}
						for key, value := range fields {
							status[key] = value
						}
					}
					return stubContent(status), nil
				case frame.Params["name"] == "edit" && args["operation"] == "batch":
					mu.Lock()
					batches[frame.Cwd]++
					mu.Unlock()
				}
				return disks[frame.Cwd](frame)
			})
			t.Cleanup(func() { close(held) }) // before the stub's own cleanup waits for its handlers
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
			const deadline = 2 * time.Second
			run, probers, err := prepareEditProbe(ctx, runOptions{Seed: 42, EditCount: 100, Editors: len(p.Repos), Worktrees: len(p.Repos) - 1, Deadline: deadline, editProbeBaseline: true}, p, nil, clients, phase)
			clients = append(clients, probers...)
			if run != nil {
				t.Cleanup(func() { _ = run.close() })
			}
			if err != nil {
				t.Fatal(err)
			}
			run.origin = time.Now()
			phase.EditProbe.Records = nil
			var linkedProber *socketClient
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
				if editor != linked && (record.Outcome != "complete" || !record.GraphTerminal) {
					t.Fatalf("primary edit did not complete: %+v", record)
				}
				if editor == linked && (record.Outcome != "product_deadline" || record.GraphTerminal || record.Receipt != "linked-1-receipt") {
					t.Fatalf("linked receipt was not left pending by the workload: %+v", record)
				}
				if editor == linked {
					linkedProber = state.client
				}
				phase.EditProbe.Records = append(phase.EditProbe.Records, record)
			}
			before := make([]string, len(p.Repos))
			for i, repo := range p.Repos {
				data, err := os.ReadFile(repo.File)
				if err != nil {
					t.Fatal(err)
				}
				if before[i] = string(data); before[i] == originals[i] {
					t.Fatalf("checkout%d was not edited by the workload", i)
				}
			}
			// A probe canceled mid-write when its edit ended closes the edit's
			// prober session for good, so the read must not depend on it.
			_ = linkedProber.Close() // it may already be closed that way
			mu.Lock()
			workloadOver = true
			mu.Unlock()
			restoreDeadline := deadline
			if cell.repoll == nil {
				restoreDeadline = deadline / 4 // shorter than the status read's own budget
			}
			restoreCtx, stop := context.WithTimeout(ctx, restoreDeadline)
			defer stop()
			started := time.Now()
			restoreErr := run.restore(restoreCtx)
			elapsed := time.Since(started)
			mu.Lock()
			repolled, primaryRepolled, sent := repolls[p.Repos[linked].Path], repolls[p.Repos[0].Path], 0
			for _, count := range batches {
				sent += count
			}
			mu.Unlock()
			if cell.state == "terminal" && restoreErr != nil {
				t.Fatalf("a receipt terminal at the post-workload read still blocked the restore: %v", restoreErr)
			}
			if cell.state != "terminal" && (restoreErr == nil || !strings.Contains(restoreErr.Error(), "unknown/pending")) {
				t.Fatalf("a receipt that is not terminal after the workload permitted the restore: %v", restoreErr)
			}
			if repolled != 1 || primaryRepolled != 0 {
				t.Fatalf("after the workload the linked receipt was read %d times and the terminal primary receipt %d times; want 1 and 0", repolled, primaryRepolled)
			}
			wire, err := json.Marshal(phase.EditProbe.Records[linked])
			if err != nil {
				t.Fatal(err)
			}
			var fields struct {
				Outcome       string `json:"outcome"`
				GraphTerminal bool   `json:"graph_terminal"`
				MutationTiers *struct {
					Tiers map[string]string `json:"tier_state"`
				} `json:"mutation_tiers"`
				Post *struct {
					State         string `json:"state"`
					GraphStatus   string `json:"graph_status"`
					MutationTiers *struct {
						State   string            `json:"observation_state"`
						Derived string            `json:"derived_at"`
						Tiers   map[string]string `json:"tier_state"`
					} `json:"mutation_tiers"`
				} `json:"post_workload_receipt"`
			}
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			// The workload's own observation is kept as it was.
			if fields.Outcome != "product_deadline" || fields.GraphTerminal || fields.MutationTiers == nil || fields.MutationTiers.Tiers["relations"] != "pending" {
				t.Fatalf("post-workload read changed the workload fields: %s", wire)
			}
			if fields.Post == nil || fields.Post.State != cell.state {
				t.Fatalf("post-workload receipt state is not %q: %s", cell.state, wire)
			}
			if cell.state != "terminal" {
				if elapsed >= deadline {
					t.Fatalf("restore took %s: the post-workload read outlived the restore deadline", elapsed)
				}
				for i, repo := range p.Repos {
					if data, err := os.ReadFile(repo.File); err != nil || string(data) != before[i] {
						t.Fatalf("a refused restore changed checkout%d: %v", i, err)
					}
				}
				if sent != 0 {
					t.Fatalf("a refused restore sent %d rewrites", sent)
				}
				return
			}
			for i, repo := range p.Repos {
				if data, err := os.ReadFile(repo.File); err != nil || string(data) != originals[i] {
					t.Fatalf("checkout%d not at its original after restore: %q %v", i, data, err)
				}
			}
			if sent != len(p.Repos) {
				t.Fatalf("restore sent %d rewrites, want one per checkout", sent)
			}
			tiers := fields.Post.MutationTiers
			if fields.Post.GraphStatus != "fresh" || tiers == nil || tiers.State != "observed" || tiers.Derived != derivedAt || tiers.Tiers["file_graph"] != "fresh" || tiers.Tiers["relations"] != "fresh" || tiers.Tiers["derived"] != "fresh" {
				t.Fatalf("post-workload receipt does not carry the read's tiers: %s", wire)
			}
		})
	}
}

// The branches the fake daemon above does not send: a primary record has no
// checkout to match, a terminal reply for another mutation never counts, and
// any terminal graph state, fresh or not, ends the pending mutation.
func TestEditProbePostWorkloadStatusNeedsTheEditsOwnMutation(t *testing.T) {
	primary := editProbeRecord{MutationID: "own"}
	linked := editProbeRecord{MutationID: "own", CheckoutID: "checkout"}
	for _, cell := range []struct {
		name   string
		record editProbeRecord
		reply  any
		state  string
		graph  string
	}{
		{"primary_terminal", primary, stubContent(map[string]any{"mutation_id": "own", "graph_status_terminal": true, "graph_status": "fresh"}), "terminal", "fresh"},
		{"terminal_failed_graph", linked, stubContent(map[string]any{"mutation_id": "own", "checkout_id": "checkout", "graph_status_terminal": true, "graph_status": "failed"}), "terminal", "failed"},
		{"terminal_for_another_mutation", primary, stubContent(map[string]any{"mutation_id": "other", "graph_status_terminal": true, "graph_status": "fresh"}), "unavailable", ""},
		{"not_a_json_object", primary, map[string]any{"content": []any{map[string]any{"type": "text", "text": "receipt not found"}}}, "unavailable", ""},
	} {
		t.Run(cell.name, func(t *testing.T) {
			raw, err := json.Marshal(cell.reply)
			if err != nil {
				t.Fatal(err)
			}
			poll := classifyEditProbePostWorkloadStatus(&editProbePostWorkloadReceipt{State: "unavailable"}, cell.record, raw)
			if poll.State != cell.state || poll.GraphStatus != cell.graph || poll.terminal() != (cell.state == "terminal") {
				t.Fatalf("got %+v, want state %q graph %q", poll, cell.state, cell.graph)
			}
			if cell.state == "unavailable" && poll.Reason == "" {
				t.Fatal("an unusable reply names no reason")
			}
		})
	}
}
