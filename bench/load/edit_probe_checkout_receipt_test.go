package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A same-prefix view must not borrow another checkout's terminal status even
// when the mutation ID, source bytes and all graph answers otherwise match.
func TestEditProbeLinkedReceiptCannotBorrowAnotherCheckoutIdentity(t *testing.T) {
	for _, cell := range []string{"matching", "foreign", "missing", "anonymous_admission", "duplicate_admission", "missing_checkout"} {
		t.Run(cell, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := editProbeCorpus()
			c.Label = "fixture"
			p := &privateRun{Root: t.TempDir()}
			labels := []string{"fixture", "linked-1"}
			if cell == "missing_checkout" {
				labels = labels[:1]
			}
			if cell == "duplicate_admission" {
				labels = append(labels, "linked-2")
			}
			for i, label := range labels {
				root := filepath.Join(p.Root, label)
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				content := fmt.Sprintf("package sample\n// checkout%d\nfunc Example() int { return 1 }\n%s\n", i, c.EditAnchor)
				if err := os.WriteFile(filepath.Join(root, c.File), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				corpus := c
				corpus.Label = label
				p.Repos = append(p.Repos, privateRepo{Path: root, File: filepath.Join(root, c.File), Prefix: "fixture", Corpus: corpus})
			}
			editors, linkedCount := len(labels), len(labels)-1
			if cell == "missing_checkout" {
				editors, linkedCount = 2, 1
			}
			full, err := makeEditProbeTrafficPlan(c, 42, 100, editors)
			if err != nil {
				t.Fatal(err)
			}
			var selected editProbeStep
			for _, step := range full.Steps {
				if step.Editor == 1 {
					selected = step
					break
				}
			}
			disk := editProbeDiskStub(t, c, p.Repos[len(p.Repos)-1].Path)
			viewID := "expected-linked-checkout"
			if cell == "anonymous_admission" {
				viewID = ""
			}
			statusID := "expected-linked-checkout"
			if cell == "foreign" {
				statusID = "foreign-checkout"
			}
			if cell == "missing" {
				statusID = ""
			}
			p.Socket = startStub(t, func(frame stubFrame) (any, *rpcError) {
				if frame.Kind == "search_symbols" {
					return map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "repo_prefix": "fixture", "checkout_id": viewID}}, nil
				}
				if frame.Method != "tools/call" {
					return stubContent(map[string]any{}), nil
				}
				args, _ := frame.Params["arguments"].(map[string]any)
				if frame.Params["name"] == "edit" && args["operation"] == "file" {
					data, err := os.ReadFile(p.Repos[1].File)
					if err != nil {
						t.Error(err)
						return nil, &rpcError{Code: -32000, Message: "read failed"}
					}
					match, _ := args["match"].(string)
					replacement, _ := args["replacement"].(string)
					if match == "" || strings.Count(string(data), match) != 1 {
						return nil, &rpcError{Code: -32000, Message: "exact match missing"}
					}
					if err := os.WriteFile(p.Repos[1].File, []byte(strings.Replace(string(data), match, replacement, 1)), 0600); err != nil {
						t.Error(err)
						return nil, &rpcError{Code: -32000, Message: "write failed"}
					}
					return stubContent(map[string]any{"mutation_receipt": "own-receipt"}), nil
				}
				if frame.Params["name"] == "change" && args["operation"] == "receipt" {
					status := map[string]any{"found": true, "receipt": "own-receipt", "mutation_id": selected.MutationID, "graph_status_terminal": true, "graph_status": "fresh"}
					if statusID != "" {
						status["checkout_id"] = statusID
					}
					return stubContent(status), nil
				}
				return disk(frame)
			})
			gate, err := json.Marshal(map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "repo_prefix": "fixture", "checkout_id": viewID}})
			if err != nil {
				t.Fatal(err)
			}
			phase := &Phase{Metrics: map[string]Metric{}, Gate: map[string]json.RawMessage{}}
			for _, label := range labels[1:] {
				phase.Gate[label+".view"] = gate
			}
			var clients []*socketClient
			t.Cleanup(func() { closeClients(clients) })
			for i := range editors {
				repo := p.Repos[i%len(p.Repos)]
				client, err := openMCP(ctx, p.Socket, repo.Path)
				if err != nil {
					t.Fatal(err)
				}
				clients = append(clients, client)
			}
			run, probers, err := prepareEditProbe(ctx, runOptions{Seed: 42, EditCount: 100, Editors: editors, Worktrees: linkedCount, Deadline: 2 * time.Second, editProbeBaseline: true}, p, nil, clients, phase)
			clients = append(clients, probers...)
			if run != nil {
				t.Cleanup(func() { _ = run.close() })
			}
			if cell == "anonymous_admission" || cell == "duplicate_admission" || cell == "missing_checkout" {
				if err == nil {
					t.Fatal("anonymous exact/shared-prefix view admitted without a checkout identity")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			run.origin = time.Now()
			step := run.plan.Steps[selected.Ordinal]
			state := run.states[step.Ordinal]
			run.activate(ctx, step)
			awaitEditProbeFirstProbe(t, state)
			run.doEdit(ctx, step)
			if state.record.CheckoutID != "expected-linked-checkout" {
				t.Fatalf("record lost its admitted checkout identity: %+v", state.record)
			}
			if cell == "matching" {
				if state.record.Outcome != "complete" || !state.record.GraphTerminal {
					t.Fatalf("own checkout receipt refused: %+v", state.record)
				}
			} else if state.record.Outcome == "complete" || state.record.GraphTerminal || !strings.Contains(strings.Join(state.record.Errors, ";"), "checkout") {
				t.Fatalf("%s checkout identity borrowed a terminal mutation: %+v", cell, state.record)
			}
		})
	}
}
