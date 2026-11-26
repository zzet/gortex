package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Exact product texts from a linked-checkout smoke run.
const (
	refusalRetryCheckout   = "expected-linked-checkout"
	refusalRetryNotRouted  = `view_building: checkout "` + refusalRetryCheckout + `" is not fully routed yet`
	refusalRetryRouteMoved = "view_read_only: checkout mutation was not admitted; no files were changed: indexer: checkout mutation view is stale: the selected route moved: checkout route changed; read the current exact view and retry. Native fallback status was not established for this checkout. Wait for a fresh view and retry the Gortex edit, or reconnect the MCP session."
)

// A linked checkout that is still routing, or whose route moved under an
// edit, refuses the edit before admission and asks for a retry. LOAD re-sends
// the edit within the record's own deadline and counts each re-send on the
// record by refusal code, instead of failing the edit on first sight. When the
// time left cannot cover another attempt as slow as the refused one, the
// record ends on the product's refusal as a product deadline named by the
// refusal code. Other refusals, other wordings and wire codes, and a tool
// deadline that may have left work running, are never re-sent; a re-send
// after any other retryable refusal is counted under its wire error code,
// and only the edit call's own re-sends are counted. A re-sent edit whose
// acknowledgement is a replay of an earlier attempt is flagged: that attempt
// committed although its reply said no files were changed.
func TestEditProbeRetriesTransientCheckoutRefusalsWithinDeadline(t *testing.T) {
	const checkout = refusalRetryCheckout
	notRouted, routeMoved := refusalRetryNotRouted, refusalRetryRouteMoved
	readOnly := "view_read_only: a pinned commit view only supports reads"
	abandoned := `tool "edit" exceeded its 55.071287166s deadline and was abandoned so the session stays responsive. The work may still complete in the background, so treat any side effect as unknown.`
	// The same admission refusal for a checkout whose disk changed: the
	// product asks for a fresh view, which a re-send does not wait for.
	diskMoved := "view_read_only: checkout mutation was not admitted; no files were changed: indexer: checkout mutation view is stale: checkout disk changed; wait for a fresh view and retry. Native fallback status was not established for this checkout. Wait for a fresh view and retry the Gortex edit, or reconnect the MCP session."
	otherBuilding := `view_building: checkout "` + checkout + `" has no published commit generation`
	// Two content items; the reply joins them on a newline.
	twoItems := routeMoved + "\nNative read/edit is allowed for this checkout until a fresh view is ready."
	otherWire := &rpcError{Code: -32603, Message: notRouted}
	// The daemon's JSON-RPC shape: the code is only in the data.
	loading := &rpcError{Code: -32000, Message: "repository loading: fixture (starting, 0%)", Data: map[string]any{"error_code": "repo_loading", "retryable": true}}
	const deadline = 2 * time.Second
	for _, cell := range []struct {
		name    string
		refuse  []any // edit replies before the edit applies, tool error text or JSON-RPC error; the last repeats when apply is false
		apply   bool
		calls   int // edit requests the product saw; 0 means at least two
		retries map[string]int
		outcome string
		reason  string        // deadline_reason when the record ends on a typed refusal
		hold    time.Duration // each edit reply is due this long after the previous one was due
		status  *rpcError     // the first receipt status read is refused with this, then re-sent
		replay  bool          // the first refusal changed files; the re-send is answered as a replay
	}{
		{name: "building_then_moved_then_applied", refuse: []any{notRouted, routeMoved}, apply: true, calls: 3, retries: map[string]int{"view_building": 1, "view_read_only": 1}, outcome: "complete"},
		{name: "instant_building_until_the_deadline", refuse: []any{notRouted}, outcome: "product_deadline", reason: "view_building"},
		// The first re-send fits in the 55% left; after the second refusal
		// about 10% is left, less than another 45% wait.
		{name: "building_held_45_percent_of_the_deadline", refuse: []any{notRouted}, calls: 2, retries: map[string]int{"view_building": 1}, outcome: "product_deadline", reason: "view_building", hold: deadline * 45 / 100},
		{name: "other_read_only_is_terminal", refuse: []any{readOnly}, calls: 1, outcome: "edit_error"},
		{name: "abandoned_tool_deadline_is_terminal", refuse: []any{abandoned}, calls: 1, outcome: "edit_error"},
		{name: "disk_moved_is_terminal", refuse: []any{diskMoved}, calls: 1, outcome: "edit_error"},
		{name: "other_building_wording_is_terminal", refuse: []any{otherBuilding}, calls: 1, outcome: "edit_error"},
		{name: "multi_content_refusal_is_terminal", refuse: []any{twoItems}, calls: 1, outcome: "edit_error"},
		{name: "refusal_text_under_another_wire_code_is_terminal", refuse: []any{otherWire}, calls: 1, outcome: "edit_error"},
		{name: "repo_loading_counted_by_its_error_code", refuse: []any{loading}, apply: true, calls: 2, retries: map[string]int{"repo_loading": 1}, outcome: "complete", status: loading},
		{name: "replayed_resend_is_flagged", refuse: []any{routeMoved}, apply: true, calls: 2, retries: map[string]int{"view_read_only": 1}, outcome: "complete", replay: true},
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
			var first time.Time
			statusRefused := false
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
					if first.IsZero() {
						first = time.Now()
					}
					due := first.Add(time.Duration(call+1) * cell.hold)
					mu.Unlock()
					time.Sleep(time.Until(due))
					apply := func() *rpcError {
						data, err := os.ReadFile(linked.File)
						if err != nil {
							t.Error(err)
							return &rpcError{Code: -32000, Message: "read failed"}
						}
						match, _ := args["match"].(string)
						replacement, _ := args["replacement"].(string)
						if match == "" || strings.Count(string(data), match) != 1 {
							return &rpcError{Code: -32000, Message: "exact match missing"}
						}
						if err := os.WriteFile(linked.File, []byte(strings.Replace(string(data), match, replacement, 1)), 0o600); err != nil {
							t.Error(err)
							return &rpcError{Code: -32000, Message: "write failed"}
						}
						options, _ := args["options"].(map[string]any)
						mu.Lock()
						mutation, _ = options["mutation_id"].(string)
						mu.Unlock()
						return nil
					}
					if cell.replay && call > 0 {
						// The ledger answers the same mutation id from the attempt that committed.
						return stubContent(map[string]any{"mutation_receipt": "linked-receipt", "replayed": true}), nil
					}
					if call < len(cell.refuse) || !cell.apply {
						if cell.replay {
							// Against the product's contract, this refusal changed files.
							if fault := apply(); fault != nil {
								return nil, fault
							}
						}
						refusal := cell.refuse[min(call, len(cell.refuse)-1)]
						if wire, ok := refusal.(*rpcError); ok {
							return nil, wire
						}
						var content []any
						for _, text := range strings.Split(refusal.(string), "\n") {
							content = append(content, map[string]any{"type": "text", "text": text})
						}
						return map[string]any{"isError": true, "content": content}, nil
					}
					if fault := apply(); fault != nil {
						return nil, fault
					}
					return stubContent(map[string]any{"mutation_receipt": "linked-receipt"}), nil
				}
				if frame.Params["name"] == "change" && args["operation"] == "receipt" {
					mu.Lock()
					defer mu.Unlock()
					if cell.status != nil && !statusRefused {
						statusRefused = true
						return nil, cell.status
					}
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
				Retries  map[string]int `json:"edit_refusal_retries"`
				Replayed bool           `json:"resent_edit_replayed"`
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
			if fields.Replayed != cell.replay {
				t.Fatalf("resent_edit_replayed %v, want %v: %s", fields.Replayed, cell.replay, wire)
			}
			if record.DeadlineReason != cell.reason {
				t.Fatalf("deadline reason %q, want %q", record.DeadlineReason, cell.reason)
			}
			if cell.reason == "" {
				return
			}
			// The record and the phase both end on the product's refusal, not
			// on a request still in flight at the deadline.
			refusal := "RPC -32000: " + cell.refuse[len(cell.refuse)-1].(string)
			if !slices.Equal(record.Errors, []string{refusal}) {
				t.Fatalf("record errors %q, want only the typed refusal", record.Errors)
			}
			run.mu.Lock()
			var final Attempt
			for _, attempt := range phase.Attempts {
				if attempt.Role == "workload" && attempt.Tool == step.Request.name() {
					final = attempt
				}
			}
			run.mu.Unlock()
			if final.Outcome != "error" || final.Error != refusal {
				t.Fatalf("last edit attempt %q %q, want the typed refusal as an error", final.Outcome, final.Error)
			}
		})
	}
}

// The product waits for a checkout's route before it refuses, up to about
// 57 s of its tool deadline, so one typed checkout refusal can take most of a
// 120 s record. A re-send is made only when the time left exceeds the refused
// attempt's own time plus the backoff; otherwise the call ends on the
// product's refusal, a known "no files were changed", instead of a request
// still in flight at the deadline. The smoke's timings, on a virtual clock.
func TestEditProbeCheckoutRefusalResendNeedsRoomForAnother(t *testing.T) {
	type reply struct {
		after time.Duration
		text  string // "" applies the edit
	}
	for _, cell := range []struct {
		name     string
		timeout  time.Duration
		replies  []reply // the last repeats
		outcomes []string
	}{
		// 63 s left after the first refusal, 6 s after the second.
		{"view_building_57s_of_120s", 120 * time.Second, []reply{{57 * time.Second, refusalRetryNotRouted}}, []string{"retryable", "error"}},
		// Busy refusals had used the rest; 17.5 s left after the refusal.
		{"view_building_57s_with_17_5s_left", 74500 * time.Millisecond, []reply{{57 * time.Second, refusalRetryNotRouted}}, []string{"error"}},
		// 82 s left, then 44 s, then 6 s.
		{"moved_route_38s_of_120s", 120 * time.Second, []reply{{38 * time.Second, refusalRetryRouteMoved}}, []string{"retryable", "retryable", "error"}},
		{"route_ready_on_the_resend", 120 * time.Second, []reply{{57 * time.Second, refusalRetryNotRouted}, {time.Second, ""}}, []string{"retryable", "success"}},
	} {
		t.Run(cell.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				caller := &scriptedEditCaller{}
				caller.reply = func(ctx context.Context, call int) (json.RawMessage, error) {
					r := cell.replies[min(call, len(cell.replies)-1)]
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(r.after):
					}
					if r.text == "" {
						return json.Marshal(stubContent(map[string]any{"status": "applied"}))
					}
					return json.Marshal(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": r.text}}})
				}
				req := request{Tool: "edit", Operation: "file", Arguments: map[string]any{"operation": "file"}}
				_, attempts, err := callEditProbeTool(context.Background(), caller, req, req.Arguments, "workload", 0, 0, 0, cell.timeout, editProbeRetrySleep(42, 0, 0))
				var outcomes []string
				for _, attempt := range attempts {
					outcomes = append(outcomes, attempt.Outcome)
				}
				if !slices.Equal(outcomes, cell.outcomes) || caller.calls != len(cell.outcomes) {
					t.Fatalf("attempts %v after %d requests, want %v: %v", outcomes, caller.calls, cell.outcomes, err)
				}
				final := cell.replies[min(len(cell.outcomes)-1, len(cell.replies)-1)].text
				if final == "" {
					if err != nil {
						t.Fatalf("applied edit returned %v", err)
					}
					return
				}
				var remote *rpcError
				if !errors.As(err, &remote) || remote.Message != final || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("call did not end on the product's refusal: %v", err)
				}
				if last := attempts[len(attempts)-1]; last.LogicalMS >= float64(cell.timeout/time.Millisecond) {
					t.Fatalf("refusal answered at %.0f ms, not before the %v deadline", last.LogicalMS, cell.timeout)
				}
			})
		})
	}
}

// scriptedEditCaller answers each tools/call with reply, by request ordinal.
type scriptedEditCaller struct {
	calls int
	reply func(context.Context, int) (json.RawMessage, error)
}

func (c *scriptedEditCaller) call(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	call := c.calls
	c.calls++
	return c.reply(ctx, call)
}
