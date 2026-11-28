package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The files in testdata/linked-full-ack are wire shapes, re-indented only:
//   - smoke-linked-ack.json: a linked checkout's edit.file ACK under the
//     default full ACK policy, from a live linked-worktree run. It returned
//     before the checkout's queued publication (reindex_pending, graph_status
//     pending) and names the checkout in its freshness view.
//   - smoke-linked-receipt.json: that edit's first receipt that read fresh.
//     Its ACK block reports reindex_wait unobserved with 0/0/0 counts.
//   - smoke-primary-ack.json, smoke-primary-receipt.json: the same run's
//     first primary edit and its first fresh receipt; that ACK waited.
//   - pr-linked-ack.json, pr-linked-receipt.json: a linked edit.file ACK and
//     its first fresh receipt once the linked ACK counts its publication
//     wait as one not-applicable lap (1/1/1, null). The capture sent no
//     mutation_id; the one LOAD sends is added to both.
func readLinkedFullACKFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "linked-full-ack", name))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// A linked full ACK that returned before its queued publication never waited,
// and its receipt counts that wait as one not-applicable lap. LOAD credits the
// counted lap as it does for a local ACK, so a linked edit's A check carries
// no missing reindex_wait reason. A primary edit given the same wire shapes,
// including the same checkout claims, keeps the strict check: the credit
// follows the run's own admitted linked view, never a claim on the wire.
func TestEditProbeLinkedFullACKCountedNoWaitAtTheReceiptPoll(t *testing.T) {
	for _, cell := range []string{"linked", "primary"} {
		t.Run(cell, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := editProbeCorpus()
			c.Label = "fixture"
			p := &privateRun{Root: t.TempDir()}
			labels := []string{"fixture", "linked-1"}
			for i, label := range labels {
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
			full, err := makeEditProbeTrafficPlan(c, 42, 100, len(labels))
			if err != nil {
				t.Fatal(err)
			}
			editor := map[string]int{"primary": 0, "linked": 1}[cell]
			var selected editProbeStep
			for _, step := range full.Steps {
				if step.Editor == editor {
					selected = step
					break
				}
			}
			target := p.Repos[editor]
			disk := editProbeDiskStub(t, c, target.Path)
			const checkout = "expected-linked-checkout"
			ack := readLinkedFullACKFixture(t, "smoke-linked-ack.json")
			ack["mutation_id"] = selected.MutationID
			ack["freshness"].(map[string]any)["checkout_id"] = checkout
			receipt, _ := ack["mutation_receipt"].(string)
			// The fixed receipt bound to this ACK's own mutation, commit and bytes.
			status := readLinkedFullACKFixture(t, "pr-linked-receipt.json")
			for _, field := range []string{"mutation_id", "commit_seq", "content_sha256"} {
				status[field] = ack[field]
			}
			status["receipt"], status["checkout_id"] = receipt, checkout
			p.Socket = startStub(t, func(frame stubFrame) (any, *rpcError) {
				if frame.Kind == "search_symbols" {
					return map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "repo_prefix": "fixture", "checkout_id": checkout}}, nil
				}
				if frame.Method != "tools/call" {
					return stubContent(map[string]any{}), nil
				}
				args, _ := frame.Params["arguments"].(map[string]any)
				if frame.Params["name"] == "edit" && args["operation"] == "file" {
					data, err := os.ReadFile(target.File)
					if err != nil {
						t.Error(err)
						return nil, &rpcError{Code: -32000, Message: "read failed"}
					}
					match, _ := args["match"].(string)
					replacement, _ := args["replacement"].(string)
					if match == "" || strings.Count(string(data), match) != 1 {
						return nil, &rpcError{Code: -32000, Message: "exact match missing"}
					}
					if err := os.WriteFile(target.File, []byte(strings.Replace(string(data), match, replacement, 1)), 0o600); err != nil {
						t.Error(err)
						return nil, &rpcError{Code: -32000, Message: "write failed"}
					}
					return stubContent(ack), nil
				}
				if frame.Params["name"] == "change" && args["operation"] == "receipt" {
					return stubContent(status), nil
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
			run, probers, err := prepareEditProbe(ctx, runOptions{Seed: 42, EditCount: 100, Editors: len(labels), Worktrees: 1, Deadline: 2 * time.Second}, p, nil, clients, phase)
			clients = append(clients, probers...)
			if run != nil {
				t.Cleanup(func() { _ = run.close() })
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
			record := state.record
			got := record.Accounting
			// Collection ends on the published receipt. Its typed B refusal is
			// terminal failed coverage, the record's only error.
			if !record.GraphTerminal || record.Outcome != "diagnostic_failure" || !slices.Equal(record.Errors, []string{"A/P/B accounting unavailable: " + strings.Join(got.Reasons, "; ")}) {
				t.Fatalf("published edit did not end on its receipt's accounting alone: %+v", record)
			}
			strict := checkEditProbeAccountingForTool(status, receipt, "edit.file")
			if strict.State != "terminal_unavailable_background" || !slices.Contains(strict.Reasons, "missing applicable A phase reindex_wait") {
				t.Fatalf("strict reading of the fixed receipt changed: %+v", strict)
			}
			if cell == "primary" {
				if record.CheckoutID != "" || !reflect.DeepEqual(got, strict) {
					t.Fatalf("a primary edit's accounting = %+v, want the strict reading %+v", got, strict)
				}
				return
			}
			if record.CheckoutID != checkout {
				t.Fatalf("linked record lost its admitted checkout: %+v", record)
			}
			want := slices.DeleteFunc(slices.Clone(strict.Reasons), func(reason string) bool { return reason == "missing applicable A phase reindex_wait" })
			if got.State != "terminal_unavailable_background" || got.A == nil || !reflect.DeepEqual(got.A, strict.A) || !slices.Equal(got.Reasons, want) {
				t.Fatalf("linked accounting = %+v, want the strict reading less its missing reindex_wait reason %v", got, want)
			}
			for _, reason := range got.Reasons {
				if strings.HasPrefix(reason, "missing applicable A phase") {
					t.Fatalf("linked full ACK kept an A reason: %v", got.Reasons)
				}
			}
		})
	}
}

// The counted no-wait credit belongs only to the run's own linked full ACK
// that returned before its publication, and only when the receipt's producer
// attested the counted lap. The credit removes exactly the missing
// reindex_wait reason; every other cell keeps the strict reading unchanged.
func TestEditProbeLinkedFullACKCountedNoWaitIsOwnedAndExact(t *testing.T) {
	const missing = "missing applicable A phase reindex_wait"
	type pair struct{ ack, receipt string }
	pr, smoke := pair{"pr-linked-ack.json", "pr-linked-receipt.json"}, pair{"smoke-linked-ack.json", "smoke-linked-receipt.json"}
	primary := pair{"smoke-primary-ack.json", "smoke-primary-receipt.json"}
	other, none := "another-checkout", ""
	for _, cell := range []struct {
		name              string
		pair              pair
		checkout          *string // the run's admitted view; nil takes the ACK's own
		tool              string
		edit              func(ack, status map[string]any)
		counted, credited bool
	}{
		{name: "own pending linked ACK", pair: pr, counted: true, credited: true},
		{name: "linked ACK before its receipt counted the lap", pair: smoke, counted: true},
		{name: "primary full ACK that waited", pair: primary},
		{name: "primary route given a linked ACK", pair: pr, checkout: &none},
		{name: "primary route with empty checkout claims", pair: pr, checkout: &none, edit: func(ack, status map[string]any) {
			ack["freshness"].(map[string]any)["checkout_id"], status["checkout_id"] = "", ""
		}},
		{name: "another admitted checkout", pair: pr, checkout: &other},
		{name: "ACK view names another checkout", pair: pr, edit: func(ack, _ map[string]any) { ack["freshness"].(map[string]any)["checkout_id"] = other }},
		{name: "ACK without its view", pair: pr, edit: func(ack, _ map[string]any) { delete(ack, "freshness") }},
		{name: "receipt names another checkout", pair: pr, edit: func(_, status map[string]any) { status["checkout_id"] = other }},
		{name: "full ACK that waited", pair: pr, edit: func(ack, _ map[string]any) { delete(ack, "reindex_pending"); ack["graph_status"] = "fresh" }},
		{name: "ACK reports no pending publication", pair: pr, edit: func(ack, _ map[string]any) { ack["reindex_pending"] = false }},
		{name: "receipt reports another ACK policy", pair: pr, edit: func(_, status map[string]any) { status["ack_mode"] = "local" }},
		{name: "local ACK on a linked route", pair: pr, edit: func(ack, status map[string]any) { ack["ack_mode"], status["ack_mode"] = "local", "local" }, counted: true, credited: true},
		{name: "refused", pair: pr, edit: func(ack, _ map[string]any) { ack["status"] = "refused" }},
		{name: "replayed", pair: pr, edit: func(ack, _ map[string]any) { ack["replayed"] = true }},
		{name: "not committed", pair: pr, edit: func(ack, _ map[string]any) { ack["disk_status"] = "written" }},
		{name: "foreign receipt", pair: pr, edit: func(_, status map[string]any) { status["receipt"] = "commit-other" }},
		{name: "foreign mutation", pair: pr, edit: func(_, status map[string]any) { status["mutation_id"] = "load-edit-other" }},
		{name: "foreign commit", pair: pr, edit: func(_, status map[string]any) { status["commit_seq"] = json.Number("9") }},
		{name: "foreign content", pair: pr, edit: func(_, status map[string]any) { status["content_sha256"] = "content-other" }},
		{name: "symbol edit", pair: pr, tool: "edit.symbol"},
	} {
		t.Run(cell.name, func(t *testing.T) {
			ack, status := readLinkedFullACKFixture(t, cell.pair.ack), readLinkedFullACKFixture(t, cell.pair.receipt)
			view, _ := ack["freshness"].(map[string]any)
			checkout, _ := view["checkout_id"].(string)
			if cell.checkout != nil {
				checkout = *cell.checkout
			}
			if cell.edit != nil {
				cell.edit(ack, status)
			}
			tool := cell.tool
			if tool == "" {
				tool = "edit.file"
			}
			mutationID, _ := ack["mutation_id"].(string)
			receipt, _ := ack["mutation_receipt"].(string)
			counted := editProbeCountedNoWait(ack, status, tool, mutationID, checkout)
			if counted != cell.counted {
				t.Fatalf("counted no-wait = %v, want %v", counted, cell.counted)
			}
			runtime := &editProbeRuntime{}
			strict := runtime.accountingForStatus(t.Context(), status, receipt, tool, false)
			got := runtime.accountingForStatus(t.Context(), status, receipt, tool, counted)
			want := strict
			if cell.credited {
				if !slices.Contains(strict.Reasons, missing) {
					t.Fatalf("strict reading has no missing reindex_wait reason to credit: %+v", strict)
				}
				want.Reasons = slices.DeleteFunc(slices.Clone(strict.Reasons), func(reason string) bool { return reason == missing })
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("accounting = %+v, want %+v", got, want)
			}
			if cell.name == "own pending linked ACK" && (got.State != "terminal_unavailable_background" || got.A == nil ||
				slices.ContainsFunc(got.Reasons, func(reason string) bool { return strings.HasPrefix(reason, "missing applicable A phase") })) {
				t.Fatalf("own linked full ACK is not terminal with a complete A check: %+v", got)
			}
		})
	}
}
