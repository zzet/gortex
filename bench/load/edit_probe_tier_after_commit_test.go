package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tierAfterCommitRecordWire returns a marshalled record's tier_after_commit_ms
// exactly as a report reader sees it, or "" when the key is absent.
func tierAfterCommitRecordWire(t *testing.T, record []byte) string {
	t.Helper()
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(record, &wire); err != nil {
		t.Fatal(err)
	}
	return string(wire["tier_after_commit_ms"])
}

// editProbeFileWrite applies an edit.file request to the fixture on disk, as
// the daemon would before its ACK.
func editProbeFileWrite(path string, args map[string]any) *rpcError {
	data, err := os.ReadFile(path)
	if err != nil {
		return &rpcError{Code: -32000, Message: err.Error()}
	}
	match, _ := args["match"].(string)
	replacement, _ := args["replacement"].(string)
	if match == "" || strings.Count(string(data), match) != 1 {
		return &rpcError{Code: -32000, Message: "exact match missing"}
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), match, replacement, 1)), 0o600); err != nil {
		return &rpcError{Code: -32000, Message: err.Error()}
	}
	return nil
}

// Each record carries the daemon's own per-tier publication latency from the
// last receipt status it observed: null for a tier not yet published, absent
// when the daemon reports none. The client times stay as they were.
func TestEditProbeRecordCopiesTierAfterCommitFromFinalReceipt(t *testing.T) {
	const pending = `{"file_graph":1.5,"relations":null,"derived":null}`
	const published = `{"file_graph":1.5,"relations":42.25,"derived":97.5}`
	for _, tc := range []struct {
		name string
		// first is on the first receipt status, final on every later one;
		// "" leaves the field out of that status.
		first, final string
		terminal     bool
		outcome      string
		want         string // "" means the record carries no tier_after_commit_ms
		err          string
	}{
		{name: "published", first: pending, final: published, terminal: true, outcome: "complete", want: published},
		{name: "unpublished_at_deadline", first: pending, final: pending, outcome: "product_deadline", want: pending},
		{name: "receipt_without_figure", terminal: true, outcome: "complete"},
		{name: "malformed", first: pending, final: `{"file_graph":"soon","relations":2,"derived":3}`, terminal: true, outcome: "diagnostic_failure", err: "last decoded receipt status: tier_after_commit_ms file_graph"},
		// Only the last status counts: an earlier malformed figure that a
		// later valid one replaces is not an error, and a later status
		// without the key clears an earlier figure.
		{name: "malformed_then_valid", first: `{"file_graph":"soon","relations":2,"derived":3}`, final: published, terminal: true, outcome: "complete", want: published},
		{name: "later_status_without_figure", first: pending, terminal: true, outcome: "complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := editProbeCorpus()
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			step := plan.Steps[0]
			dir := t.TempDir()
			path := filepath.Join(dir, c.File)
			if err := os.WriteFile(path, []byte("package sample\nfunc Example() int { return 1 }\n"+c.EditAnchor+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			disk := editProbeDiskReplies(c, dir, func(err error) { t.Errorf("fixture read: %v", err) })
			var polls atomic.Int64
			run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(frame stubFrame) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				if frame.Method == "tools/call" && frame.Params["name"] == "edit" && args["operation"] == "file" {
					if failure := editProbeFileWrite(path, args); failure != nil {
						return nil, failure
					}
					return stubContent(map[string]any{"mutation_receipt": "receipt-a"}), nil
				}
				if frame.Method == "tools/call" && frame.Params["name"] == "change" && args["operation"] == "receipt" {
					status := map[string]any{"receipt": "receipt-a", "mutation_id": step.MutationID, "graph_status": "pending", "graph_status_terminal": false}
					tier := tc.first
					if polls.Add(1) > 1 {
						tier = tc.final
						if tc.terminal {
							status["graph_status"], status["graph_status_terminal"] = "fresh", true
						}
					}
					if tier != "" {
						status["tier_after_commit_ms"] = json.RawMessage(tier)
					}
					return stubContent(status), nil
				}
				return disk(frame)
			})
			run.requireAccounting = false
			if !tc.terminal {
				// Wide enough for the edit, its readbacks and at least two
				// receipt polls on a loaded race runner.
				run.options.Deadline = 2 * time.Second
			}
			run.activate(ctx, step)
			awaitEditProbeFirstProbe(t, run.states[0])
			run.doEdit(ctx, step)
			if polls.Load() < 2 {
				t.Fatalf("the fixture needs a later receipt status than the first: %d", polls.Load())
			}
			_ = run.finish() // reports a failure for every record that did not complete
			if len(run.phase.EditProbe.Records) != 1 {
				t.Fatalf("finished records = %d, want 1", len(run.phase.EditProbe.Records))
			}
			record := run.phase.EditProbe.Records[0]
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if got := tierAfterCommitRecordWire(t, data); got != tc.want {
				t.Errorf("tier_after_commit_ms = %s, want %s", got, tc.want)
			}
			if record.Outcome != tc.outcome {
				t.Fatalf("outcome = %s, want %s: %v", record.Outcome, tc.outcome, record.Errors)
			}
			// The error names the last status the loop decoded, which is not
			// the final poll when that poll failed to decode.
			invalid, worded := 0, 0
			for _, message := range record.Errors {
				if strings.Contains(message, "tier_after_commit_ms") {
					invalid++
				}
				if tc.err != "" && strings.Contains(message, tc.err) {
					worded++
				}
			}
			if tc.err != "" && (invalid != 1 || worded != 1) || tc.err == "" && invalid != 0 {
				t.Errorf("tier_after_commit_ms errors = %d (%d matching %q) in %v", invalid, worded, tc.err, record.Errors)
			}
			// The client relation time keeps its own definition: the slowest
			// relation oracle's first-correct time from edit send.
			client := editProbeGroupTime(record.FirstCorrect, "relations")
			if (client.Value == nil) != (record.Relations.Value == nil) || client.Value != nil && *client.Value != *record.Relations.Value {
				t.Errorf("t_relations_ms = %+v, want the client group time %+v", record.Relations, client)
			}
			if tc.outcome == "complete" && record.Relations.Value == nil {
				t.Error("a complete record lost its client relation time")
			}
		})
	}
}

// The six-repository co-change cell sends every edit through the same receipt
// loop, one editor per repository. The co-change setup only runs before the
// edits, so each record keeps its own route's figure from its own receipt.
func TestEditProbeSixRepositoryCoChangeRecordsTierAfterCommitPerRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	o, err := parseRun([]string{
		"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(),
		"--scenario", editProbeScenario, "--edit-repos", "6", "--edit-agents", "6",
		"--agents", "0", "--worktrees", "0", "--edit-count", "100", "--seed", "42",
		"--edit-wait-cochange", "--edit-probe-role", "baseline",
	})
	if err != nil || !o.EditWaitCoChange {
		t.Fatalf("six-repository co-change cell refused: %v", err)
	}
	o.Deadline = 2 * time.Second
	p := &privateRun{Root: t.TempDir()}
	corpora := editProbeRoutingCorpora()
	replies := make(map[string]func(stubFrame) (any, *rpcError), len(corpora))
	for i, c := range corpora {
		root := filepath.Join(p.Root, c.Label)
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf("package sample\n// route-%d\nfunc Example%d() int { return %d }\n%s\n", i, i, i, c.EditAnchor)
		if err := os.WriteFile(filepath.Join(root, c.File), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		p.Repos = append(p.Repos, privateRepo{Path: root, File: filepath.Join(root, c.File), Prefix: "fixture", Corpus: c})
		replies[root] = editProbeDiskReplies(c, root, func(err error) { t.Errorf("route %d fixture read: %v", i, err) })
	}
	// Relations publish on the second status; derived is still unpublished
	// when the edit completes, so it stays null on the final receipt.
	pending := func(route int) string {
		return fmt.Sprintf(`{"file_graph":%d.5,"relations":null,"derived":null}`, route+1)
	}
	published := func(route int) string {
		return fmt.Sprintf(`{"file_graph":%d.5,"relations":%d.25,"derived":null}`, route+1, 10*(route+1))
	}
	var mu sync.Mutex
	mutations, polls := map[string]string{}, map[string]int{}
	p.Socket = startStub(t, func(frame stubFrame) (any, *rpcError) {
		route := -1
		for i, repo := range p.Repos {
			if frame.Cwd == repo.Path {
				route = i
			}
		}
		if route < 0 {
			return nil, &rpcError{Code: -32000, Message: "unknown checkout"}
		}
		args, _ := frame.Params["arguments"].(map[string]any)
		options, _ := args["options"].(map[string]any)
		switch {
		case frame.Method == "tools/call" && frame.Params["name"] == "edit" && args["operation"] == "file":
			if failure := editProbeFileWrite(p.Repos[route].File, args); failure != nil {
				return nil, failure
			}
			mutation, _ := options["mutation_id"].(string)
			receipt := fmt.Sprintf("route-%d-%s", route, mutation)
			mu.Lock()
			mutations[frame.Cwd+"|"+receipt] = mutation
			mu.Unlock()
			return stubContent(map[string]any{"mutation_receipt": receipt}), nil
		case frame.Method == "tools/call" && frame.Params["name"] == "change" && args["operation"] == "receipt":
			receipt, _ := options["receipt"].(string)
			key := frame.Cwd + "|" + receipt
			mu.Lock()
			mutation := mutations[key]
			polls[key]++
			first := polls[key] == 1
			mu.Unlock()
			if mutation == "" {
				return nil, &rpcError{Code: -32000, Message: "receipt belongs to another checkout"}
			}
			status := map[string]any{"receipt": receipt, "mutation_id": mutation, "graph_status": "pending", "graph_status_terminal": false, "tier_after_commit_ms": json.RawMessage(pending(route))}
			if !first {
				status["graph_status"], status["graph_status_terminal"], status["tier_after_commit_ms"] = "fresh", true, json.RawMessage(published(route))
			}
			return stubContent(status), nil
		}
		return replies[frame.Cwd](frame)
	})
	phase := &Phase{Metrics: map[string]Metric{}, Gate: map[string]json.RawMessage{}}
	var clients []*socketClient
	t.Cleanup(func() { closeClients(clients) })
	for _, repo := range p.Repos {
		client, err := openMCP(ctx, p.Socket, repo.Path)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}
	run, probers, err := prepareEditProbe(ctx, o, p, nil, clients, phase)
	clients = append(clients, probers...)
	if run != nil {
		t.Cleanup(func() { _ = run.close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	if !run.plan.sixRepoTopology() {
		t.Fatal("the fixture did not plan the six-repository topology")
	}
	run.origin = time.Now()
	ordinals := make(map[int]int, len(corpora)) // ordinal -> route
	for route := range corpora {
		var step editProbeStep
		found := false
		for _, candidate := range run.plan.Steps {
			if candidate.Editor == route {
				step, found = candidate, true
				break
			}
		}
		if !found {
			t.Fatalf("route %d has no offered edit", route)
		}
		ordinals[step.Ordinal] = route
		state := run.states[step.Ordinal]
		run.activate(ctx, step)
		awaitEditProbeFirstProbe(t, state)
		run.doEdit(ctx, step)
		state.mu.Lock()
		outcome, errs, label := state.record.Outcome, state.record.Errors, state.record.Route
		state.mu.Unlock()
		if outcome != "complete" || label != corpora[route].Label {
			t.Fatalf("route %d edit did not complete on its own route: outcome=%s route=%s errors=%v", route, outcome, label, errs)
		}
	}
	_ = run.finish() // the other offered edits stay censored; only the records matter here
	for _, record := range phase.EditProbe.Records {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		got := tierAfterCommitRecordWire(t, data)
		route, sent := ordinals[record.Ordinal]
		switch {
		case sent && got != published(route):
			t.Errorf("route %d record tier_after_commit_ms = %s, want its own final receipt %s", route, got, published(route))
		case !sent && got != "":
			t.Errorf("unsent edit %d invented tier_after_commit_ms %s", record.Ordinal, got)
		}
	}
	summary := phase.EditProbe.Summary
	if summary == nil {
		t.Fatal("finished report has no summary")
	}
	daemon, client := summary.Measures["tier_after_commit_ms.relations"], summary.Measures["t_relations_ms"]
	if daemon.Count != len(corpora) || daemon.Censored != 100-len(corpora) || daemon.Max.Value != nil {
		t.Errorf("daemon relation latency beside the client time = %+v", daemon)
	}
	if client.Count != len(corpora) || client.Censored != 100-len(corpora) {
		t.Errorf("client relation latency changed: %+v", client)
	}
	if derived := summary.Measures["tier_after_commit_ms.derived"]; derived.Count != 0 || derived.Censored != 100 || derived.NotApplicable != 0 {
		t.Errorf("unpublished derived tier = %+v, want every record censored", derived)
	}
}

// The summary reports the daemon tier latencies beside the client times. A
// tier null on its record's final receipt was not published yet, so it is
// censored even when the edit completed; a record with no figure at all is
// censored only if it did not complete.
func TestEditProbeSummaryReportsTierAfterCommitBesideClientTimes(t *testing.T) {
	decode := func(wire string) editProbeRecord {
		t.Helper()
		var record editProbeRecord
		if err := json.Unmarshal([]byte(wire), &record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	records := []editProbeRecord{
		decode(`{"outcome":"complete","t_relations_ms":{"value":30,"unit":"ms"},"tier_after_commit_ms":{"file_graph":1,"relations":12,"derived":40}}`),
		decode(`{"outcome":"complete","t_relations_ms":{"value":31,"unit":"ms"},"tier_after_commit_ms":{"file_graph":1,"relations":14,"derived":null}}`),
		decode(`{"outcome":"product_deadline","tier_after_commit_ms":{"file_graph":2,"relations":null,"derived":null}}`),
		decode(`{"outcome":"complete","t_relations_ms":{"value":32,"unit":"ms"}}`),
		decode(`{"outcome":"phase_censored"}`),
	}
	summary := summarizeEditProbe(records, 42)
	for name, want := range map[string][3]int{
		"tier_after_commit_ms.file_graph": {3, 1, 1},
		"tier_after_commit_ms.relations":  {2, 2, 1},
		"tier_after_commit_ms.derived":    {1, 3, 1},
		"t_relations_ms":                  {3, 2, 0},
	} {
		got, present := summary.Measures[name]
		if !present || [3]int{got.Count, got.Censored, got.NotApplicable} != want {
			t.Errorf("%s = %+v (present %v), want count/censored/not_applicable %v", name, got, present, want)
		}
	}
	// Four ranked daemon records (12, 14 and two censored); the record
	// without a figure is not ranked.
	if relations := summary.Measures["tier_after_commit_ms.relations"]; relations.P50.Value == nil || *relations.P50.Value != 14 {
		t.Errorf("daemon relation p50 = %+v, want 14", relations.P50)
	}
	// Five ranked client records, two of them censored: the nearest-rank
	// p50 is the third value, untouched by the daemon figures.
	if client := summary.Measures["t_relations_ms"]; client.P50.Value == nil || *client.P50.Value != 32 {
		t.Errorf("client relation p50 = %+v, want 32", client.P50)
	}
	// A daemon without the figure adds no daemon measures at all.
	without := summarizeEditProbe(records[3:], 42)
	for name := range without.Measures {
		if strings.HasPrefix(name, "tier_after_commit_ms.") {
			t.Errorf("records without the figure produced %s", name)
		}
	}
}
