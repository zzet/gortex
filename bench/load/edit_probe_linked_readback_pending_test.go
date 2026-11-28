package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const linkedReadbackCheckout = "expected-linked-checkout"

// The labelled base fallback a linked checkout's cwd-bound graph read gets
// while its route republishes: the requested view is the checkout's own, the
// answer came from the base graph and says it is not exact.
func linkedReadbackPendingRider() map[string]any {
	return map[string]any{"requested_view": "worktree:" + linkedReadbackCheckout, "actual_view": "base", "exact": false,
		"fallback_reason": "view_building", "checkout_id": linkedReadbackCheckout, "graph_id": "graph-1"}
}

func linkedReadbackExactRider() map[string]any {
	view := "worktree:" + linkedReadbackCheckout
	return map[string]any{"requested_view": view, "actual_view": view, "exact": true, "checkout_id": linkedReadbackCheckout, "graph_id": "graph-1"}
}

// A tool reply with the freshness rider in the result envelope's _meta, where
// the product places it on answers and tool errors alike.
func withLinkedReadbackRider(reply any, rider map[string]any) any {
	envelope, _ := reply.(map[string]any)
	out := make(map[string]any, len(envelope)+1)
	for key, value := range envelope {
		out[key] = value
	}
	if rider != nil {
		out["_meta"] = map[string]any{"freshness": rider}
	}
	return out
}

func linkedReadbackNotFound(symbol string, rider map[string]any) any {
	return withLinkedReadbackRider(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "symbol not found: " + symbol}}}, rider)
}

// One edit on a two-checkout routed run, through the ordinary doEdit path.
type linkedReadbackCell struct {
	primary bool
	// The editor session's read.source replies answered before the view
	// publishes; -1 never publishes.
	fallbacks int
	rider     func(map[string]any) // edits the pending rider
	ack       func(map[string]any) // edits the linked ACK
	// Overrides for the editor session's replies; n counts that kind's calls.
	source   func(n int, frame stubFrame, disk func(stubFrame) (any, *rpcError)) (any, *rpcError)
	file     func(n int, frame stubFrame, disk func(stubFrame) (any, *rpcError)) (any, *rpcError)
	deadline time.Duration
	// The candidate role requires A/P/B accounting; the default baseline role
	// isolates the readback contract from it.
	candidate bool
	// The ACK and receipt wire fixtures (testdata/linked-full-ack), by
	// default the linked smoke ACK and the fixed linked receipt; receipt
	// edits the receipt after it is bound to the ACK's identity.
	ackFixture, statusFixture string
	receipt                   func(status map[string]any)
}

type linkedReadbackResult struct {
	record editProbeRecord
	json   map[string]any // the record as the report encodes it
	run    *editProbeRuntime
	reads  map[string]int // the editor session's calls per readback kind
}

func runLinkedReadbackCell(t *testing.T, cell linkedReadbackCell) linkedReadbackResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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
	editor := 1
	if cell.primary {
		editor = 0
	}
	var selected editProbeStep
	for _, step := range full.Steps {
		if step.Editor == editor {
			selected = step
			break
		}
	}
	target := p.Repos[editor]
	disk := editProbeDiskStub(t, c, target.Path)
	ackFixture, statusFixture := cell.ackFixture, cell.statusFixture
	if ackFixture == "" {
		ackFixture = "smoke-linked-ack.json"
	}
	if statusFixture == "" {
		statusFixture = "pr-linked-receipt.json"
	}
	ack := readLinkedFullACKFixture(t, ackFixture)
	ack["mutation_id"] = selected.MutationID
	if view, ok := ack["freshness"].(map[string]any); ok && !cell.primary {
		view["checkout_id"] = linkedReadbackCheckout
	}
	if cell.ack != nil {
		cell.ack(ack)
	}
	receipt, _ := ack["mutation_receipt"].(string)
	status := readLinkedFullACKFixture(t, statusFixture)
	for _, field := range []string{"mutation_id", "commit_seq", "content_sha256"} {
		status[field] = ack[field]
	}
	status["receipt"] = receipt
	if !cell.primary {
		status["checkout_id"] = linkedReadbackCheckout
	}
	if cell.receipt != nil {
		cell.receipt(status)
	}
	rider := linkedReadbackPendingRider()
	if cell.rider != nil {
		cell.rider(rider)
	}
	var mu sync.Mutex
	editConnection, reads := 0, map[string]int{}
	published := func() bool { return cell.fallbacks >= 0 && reads["source"] > cell.fallbacks }
	p.Socket = startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Kind == "search_symbols" {
			return map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "repo_prefix": "fixture", "checkout_id": linkedReadbackCheckout}}, nil
		}
		if frame.Method != "tools/call" {
			return stubContent(map[string]any{}), nil
		}
		args, _ := frame.Params["arguments"].(map[string]any)
		name := fmt.Sprint(frame.Params["name"]) + "." + fmt.Sprint(args["operation"])
		if name == "edit.file" {
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
			mu.Lock()
			editConnection = frame.Connection
			mu.Unlock()
			return stubContent(ack), nil
		}
		if name == "change.receipt" {
			return stubContent(status), nil
		}
		mu.Lock()
		own := editConnection != 0 && frame.Connection == editConnection
		kind := strings.TrimPrefix(name, "read.")
		n := 0
		if own && (name == "read.source" || name == "read.file") {
			reads[kind]++
			n = reads[kind]
		}
		pending := !published()
		mu.Unlock()
		target, _ := args["target"].(map[string]any)
		symbol, _ := target["symbol"].(string)
		switch {
		case own && name == "read.source" && cell.source != nil:
			return cell.source(n, frame, disk)
		case own && name == "read.file" && cell.file != nil:
			return cell.file(n, frame, disk)
		case own && name == "read.source" && pending:
			// The editor's own reads see the base until the view publishes.
			return linkedReadbackNotFound(symbol, rider), nil
		case name == "read.source":
			// Probes read the published view, so first-correct times never
			// depend on how often the editor re-reads.
			reply, fault := disk(frame)
			return withLinkedReadbackRider(reply, linkedReadbackExactRider()), fault
		}
		return disk(frame)
	})
	gate, err := json.Marshal(map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "repo_prefix": "fixture", "checkout_id": linkedReadbackCheckout}})
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
	deadline := cell.deadline
	if deadline == 0 {
		deadline = 3 * time.Second
	}
	run, probers, err := prepareEditProbe(ctx, runOptions{Seed: 42, EditCount: 100, Editors: len(labels), Worktrees: 1, Deadline: deadline, editProbeBaseline: !cell.candidate}, p, nil, clients, phase)
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
	state.stopProbes()
	state.mu.Lock()
	record := state.record
	state.mu.Unlock()
	if want := map[bool]string{true: "", false: linkedReadbackCheckout}[cell.primary]; record.CheckoutID != want {
		t.Fatalf("record checkout = %q, want %q", record.CheckoutID, want)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]any
	if err := json.Unmarshal(data, &encoded); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return linkedReadbackResult{record: record, json: encoded, run: run, reads: maps.Clone(reads)}
}

// The encoded readback rows of one kind, in order.
func linkedReadbackRows(t *testing.T, encoded map[string]any, kind string) []map[string]any {
	t.Helper()
	rows, _ := encoded["readbacks"].([]any)
	var out []map[string]any
	for _, row := range rows {
		back, _ := row.(map[string]any)
		if back["kind"] == kind {
			out = append(out, back)
		}
	}
	return out
}

func linkedReadbackNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case map[string]any:
		return linkedReadbackNumber(v["value"])
	}
	return 0, false
}

// The smoke's linked edit: a reindex_pending ACK, a correct disk read, and a
// source read the product answers from the labelled base fallback until the
// checkout republishes. That read is pending, never incorrect: LOAD re-reads
// on the same session until the answer is exact and measures the first exact
// answer.
func TestEditProbeLinkedImmediateReadbackFallbackIsPendingNotIncorrect(t *testing.T) {
	const fallbacks = 3
	got := runLinkedReadbackCell(t, linkedReadbackCell{fallbacks: fallbacks})
	record := got.record
	if record.Outcome != "complete" || len(record.Errors) != 0 {
		t.Fatalf("a pending linked readback failed its record: outcome %s errors %v", record.Outcome, record.Errors)
	}
	if got.reads["source"] != fallbacks+1 {
		t.Fatalf("editor session source reads = %d, want %d pending and one exact", got.reads["source"], fallbacks)
	}
	rows := linkedReadbackRows(t, got.json, "source")
	if len(rows) != 2 {
		t.Fatalf("source readback rows = %v, want the pending immediate row and the exact row", rows)
	}
	first, exact := rows[0], rows[1]
	if first["correct"] != false || first["pending"] != true || first["pending_reason"] != "view_building" {
		t.Fatalf("immediate source row = %v, want pending view_building and not correct", first)
	}
	if exact["correct"] != true || exact["pending"] == true {
		t.Fatalf("exact source row = %v, want correct and not pending", exact)
	}
	disposition, _ := first["disposition"].(map[string]any)
	if disposition["kind"] != "untyped_tool_error" || disposition["actual_view"] != "base" || disposition["exact"] != false || disposition["fallback_reason"] != "view_building" {
		t.Fatalf("pending row disposition lost its rider: %v", disposition)
	}
	for _, row := range linkedReadbackRows(t, got.json, "file") {
		if row["pending"] == true || row["correct"] != true {
			t.Fatalf("a disk readback was pending or wrong: %v", row)
		}
	}
	census, _ := got.json["readback_pending"].(map[string]any)
	source, _ := census["source"].(map[string]any)
	firstExact, measured := linkedReadbackNumber(source["first_exact_ms"])
	immediate, _ := linkedReadbackNumber(first["latency_ms"])
	if !measured || source["pending_reason"] != "view_building" || source["first_exact_correct"] != true || source["rereads"] != float64(fallbacks) || firstExact < immediate {
		t.Fatalf("readback_pending.source = %v, want first_exact_ms measured and >= the immediate latency %v", source, immediate)
	}
	if _, ok := linkedReadbackNumber(source["first_pending_ms"]); !ok {
		t.Fatalf("readback_pending.source has no first_pending_ms: %v", source)
	}
	if record.Readback.Value == nil || *record.Readback.Value != firstExact {
		t.Fatalf("t_readback_ms = %+v, want the first exact reply's finish %v", record.Readback, firstExact)
	}
}

// An answer the product claims exact for the requested view is scored by its
// oracle, before and after a pending window alike.
func TestEditProbeLinkedReadbackWrongExactAnswerStillFails(t *testing.T) {
	notFound := func(n int, frame stubFrame, _ func(stubFrame) (any, *rpcError)) (any, *rpcError) {
		args, _ := frame.Params["arguments"].(map[string]any)
		target, _ := args["target"].(map[string]any)
		symbol, _ := target["symbol"].(string)
		return linkedReadbackNotFound(symbol, linkedReadbackExactRider()), nil
	}
	t.Run("immediate", func(t *testing.T) {
		got := runLinkedReadbackCell(t, linkedReadbackCell{fallbacks: 0, source: notFound})
		if got.record.Outcome != "diagnostic_failure" || !slices.Equal(got.record.Errors, []string{"immediate source readback incorrect"}) {
			t.Fatalf("a wrong exact answer = %s %v, want the immediate incorrect error alone", got.record.Outcome, got.record.Errors)
		}
		for _, row := range linkedReadbackRows(t, got.json, "source") {
			if row["pending"] == true {
				t.Fatalf("a wrong exact answer was pending: %v", row)
			}
		}
	})
	t.Run("after the view published", func(t *testing.T) {
		pendingThenWrong := func(n int, frame stubFrame, disk func(stubFrame) (any, *rpcError)) (any, *rpcError) {
			if n <= 2 {
				args, _ := frame.Params["arguments"].(map[string]any)
				target, _ := args["target"].(map[string]any)
				symbol, _ := target["symbol"].(string)
				return linkedReadbackNotFound(symbol, linkedReadbackPendingRider()), nil
			}
			return notFound(n, frame, disk)
		}
		got := runLinkedReadbackCell(t, linkedReadbackCell{fallbacks: 2, source: pendingThenWrong})
		if got.record.Outcome != "diagnostic_failure" || len(got.record.Errors) == 0 || !strings.Contains(strings.Join(got.record.Errors, "; "), "source readback incorrect") {
			t.Fatalf("a wrong exact answer after a pending window = %s %v, want a readback failure", got.record.Outcome, got.record.Errors)
		}
		if got.record.Readback.Value != nil {
			t.Fatalf("t_readback_ms measured a wrong answer: %+v", got.record.Readback)
		}
	})
}

// A view that never publishes within the record deadline leaves the readback
// outstanding: the record is a product deadline, never complete, and the
// pending reply never supplies t_readback_ms.
func TestEditProbeLinkedReadbackPendingNeverExactIsProductDeadline(t *testing.T) {
	got := runLinkedReadbackCell(t, linkedReadbackCell{fallbacks: -1, deadline: 750 * time.Millisecond})
	record := got.record
	if record.Outcome != "product_deadline" || record.DeadlineReason != "immediate_readbacks_outstanding" {
		t.Fatalf("a never-exact pending readback = %s (%s) %v, want product_deadline immediate_readbacks_outstanding", record.Outcome, record.DeadlineReason, record.Errors)
	}
	if record.Readback.Value != nil {
		t.Fatalf("t_readback_ms taken from a pending reply: %+v", record.Readback)
	}
	if got.reads["source"] < 2 {
		t.Fatalf("pending readback was not re-read: %d source reads", got.reads["source"])
	}
}

// The primary never serves its own route from a fallback; a primary edit
// whose ACK and rider name an empty checkout, with every other pending
// condition met, stays incorrect.
func TestEditProbePrimaryReadbackFallbackIsIncorrect(t *testing.T) {
	got := runLinkedReadbackCell(t, linkedReadbackCell{primary: true, fallbacks: 3,
		ack:   func(ack map[string]any) { ack["freshness"].(map[string]any)["checkout_id"] = "" },
		rider: func(rider map[string]any) { rider["requested_view"], rider["checkout_id"] = "worktree:", "" }})
	if got.record.Outcome != "diagnostic_failure" || !slices.Equal(got.record.Errors, []string{"immediate source readback incorrect"}) {
		t.Fatalf("primary fallback readback = %s %v, want the immediate incorrect error alone", got.record.Outcome, got.record.Errors)
	}
	if got.reads["source"] != 1 {
		t.Fatalf("a primary fallback was re-read %d times", got.reads["source"]-1)
	}
}

// Only the run's own linked view, substituted for a typed pending reason after
// an ACK that documents its own pending publication, earns a pending readback.
func TestEditProbeReadbackPendingRequiresOwnViewAndTypedReason(t *testing.T) {
	for _, cell := range []struct {
		name  string
		rider func(map[string]any)
		ack   func(map[string]any)
	}{
		{name: "another checkout's view", rider: func(r map[string]any) { r["requested_view"] = "worktree:another-checkout" }},
		{name: "route moved", rider: func(r map[string]any) { r["fallback_reason"] = "route_moved" }},
		{name: "no reason", rider: func(r map[string]any) { delete(r, "fallback_reason") }},
		{name: "exact absent", rider: func(r map[string]any) { delete(r, "exact") }},
		{name: "own view served inexact", rider: func(r map[string]any) { r["actual_view"] = r["requested_view"] }},
		{name: "ACK reports no pending publication", ack: func(a map[string]any) { a["reindex_pending"] = false }},
		{name: "ACK graph already fresh", ack: func(a map[string]any) { a["graph_status"] = "fresh" }},
		{name: "replayed ACK", ack: func(a map[string]any) { a["replayed"] = true }},
		{name: "ACK names another checkout", ack: func(a map[string]any) { a["freshness"].(map[string]any)["checkout_id"] = "another-checkout" }},
	} {
		t.Run(cell.name, func(t *testing.T) {
			got := runLinkedReadbackCell(t, linkedReadbackCell{fallbacks: 3, rider: cell.rider, ack: cell.ack})
			if got.record.Outcome != "diagnostic_failure" || !slices.Contains(got.record.Errors, "immediate source readback incorrect") {
				t.Fatalf("readback = %s %v, want incorrect", got.record.Outcome, got.record.Errors)
			}
			for _, row := range linkedReadbackRows(t, got.json, "source") {
				if row["pending"] == true {
					t.Fatalf("readback was pending: %v", row)
				}
			}
			if _, present := got.json["readback_pending"]; present {
				t.Fatalf("pending census present: %v", got.json["readback_pending"])
			}
		})
	}
}

// The disk read must be correct at once, whatever rider it carries.
func TestEditProbeFileReadbackNeverPending(t *testing.T) {
	stale := func(n int, frame stubFrame, _ func(stubFrame) (any, *rpcError)) (any, *rpcError) {
		reply := stubContent(map[string]any{"path": "fixture/sample.go", "content": "package sample\n"})
		return withLinkedReadbackRider(reply, linkedReadbackPendingRider()), nil
	}
	got := runLinkedReadbackCell(t, linkedReadbackCell{fallbacks: 0, file: stale})
	if got.record.Outcome != "diagnostic_failure" || !slices.Equal(got.record.Errors, []string{"immediate file readback incorrect"}) {
		t.Fatalf("a wrong file readback = %s %v, want the immediate file incorrect error alone", got.record.Outcome, got.record.Errors)
	}
	if got.reads["file"] != 1 {
		t.Fatalf("a file readback was re-read %d times", got.reads["file"]-1)
	}
	for _, row := range linkedReadbackRows(t, got.json, "file") {
		if row["pending"] == true {
			t.Fatalf("a file readback was pending: %v", row)
		}
	}
}

// The rider rides the result envelope's _meta on tool errors too; the
// disposition reports it beside the untyped error.
func TestEditProbeReadbackDispositionCarriesRiderOnToolErrors(t *testing.T) {
	encode := func(t *testing.T, value any) map[string]any {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	reply, err := json.Marshal(linkedReadbackNotFound("fixture/sample.go::Marker", linkedReadbackPendingRider()))
	if err != nil {
		t.Fatal(err)
	}
	got := encode(t, editProbeReadbackDispositionOf("source", reply, nil, &rpcError{Code: -32000, Message: "symbol not found"}, nil))
	if got["kind"] != "untyped_tool_error" || got["requested_view"] != "worktree:"+linkedReadbackCheckout || got["actual_view"] != "base" || got["exact"] != false || got["fallback_reason"] != "view_building" {
		t.Fatalf("tool-error disposition = %v, want the rider's view, exactness and fallback reason", got)
	}

	// A decoded answer keeps its freshness and adds the rider's file scope.
	answer := map[string]any{"_meta": map[string]any{"freshness": map[string]any{"requested_view": "auto", "actual_view": "worktree:x", "exact": true, "freshness_scope": "file", "graph_freshness": "pending"}},
		"content": []any{map[string]any{"type": "text", "text": `{"content":"package sample\n"}`}}}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	got = encode(t, editProbeReadbackDispositionOf("file", raw, map[string]any{"content": "package sample\n"}, nil, nil))
	if got["kind"] != "answer" || got["exact"] != true || got["freshness_scope"] != "file" || got["actual_view"] != "worktree:x" {
		t.Fatalf("answer disposition = %v, want the rider beside the answer", got)
	}

	// No rider: no rider fields, never a zero exactness.
	plain, err := json.Marshal(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "boom"}}})
	if err != nil {
		t.Fatal(err)
	}
	got = encode(t, editProbeReadbackDispositionOf("source", plain, nil, &rpcError{Code: -32000, Message: "boom"}, nil))
	for _, field := range []string{"requested_view", "actual_view", "exact", "fallback_reason", "freshness_scope"} {
		if _, present := got[field]; present {
			t.Fatalf("a reply without a rider reported %s: %v", field, got)
		}
	}
}

// The summary's pending census counts pending kinds and their re-reads and
// ranks the first exact answer; a pending-then-exact record is not censored.
func TestSummarizeEditProbeReadbackPendingCensus(t *testing.T) {
	var records []editProbeRecord
	for i, exact := range []float64{180, 220, 260} {
		row := fmt.Sprintf(`{"ordinal":%d,"outcome":"complete","t_readback_ms":{"value":%v,"unit":"ms"},
			"readbacks":[{"kind":"file","latency_ms":{"value":3,"unit":"ms"},"correct":true,"error":""},
				{"kind":"source","latency_ms":{"value":4,"unit":"ms"},"correct":false,"error":"symbol not found","pending":true,"pending_reason":"view_building"},
				{"kind":"source","latency_ms":{"value":5,"unit":"ms"},"correct":true,"error":"","reread":true}],
			"readback_pending":{"source":{"pending_reason":"view_building","first_pending_ms":{"value":30,"unit":"ms"},"rereads":%d,"first_exact_ms":{"value":%v,"unit":"ms"},"first_exact_correct":true}}}`, i, exact, i+2, exact)
		var record editProbeRecord
		if err := json.Unmarshal([]byte(row), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	// A primary record answered exactly at once has no census entry.
	var primary editProbeRecord
	if err := json.Unmarshal([]byte(`{"ordinal":3,"outcome":"complete","t_readback_ms":{"value":40,"unit":"ms"},
		"readbacks":[{"kind":"file","latency_ms":{"value":3,"unit":"ms"},"correct":true,"error":""},{"kind":"source","latency_ms":{"value":6,"unit":"ms"},"correct":true,"error":""}]}`), &primary); err != nil {
		t.Fatal(err)
	}
	records = append(records, primary)
	data, err := json.Marshal(summarizeEditProbe(records, 42))
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	measures, _ := summary["measures"].(map[string]any)
	readback, _ := measures["t_readback_ms"].(map[string]any)
	if readback["count"] != float64(4) || readback["censored"] != float64(0) {
		t.Fatalf("t_readback_ms = %v, want four measured and none censored", readback)
	}
	if source, _ := measures["readback_ms.source"].(map[string]any); source["count"] != float64(4) {
		t.Fatalf("readback_ms.source = %v, want one immediate attempt per record", source)
	}
	census, _ := summary["readback_pending"].(map[string]any)
	source, _ := census["source"].(map[string]any)
	firstExact, _ := source["first_exact_ms"].(map[string]any)
	max, _ := linkedReadbackNumber(firstExact["max"])
	if source["count"] != float64(3) || source["rereads"] != float64(2+3+4) || firstExact["count"] != float64(3) || firstExact["censored"] != float64(0) || max != 260 {
		t.Fatalf("readback_pending.source = %v, want three pending records, nine re-reads and first_exact_ms ranked", source)
	}
}

func linkedReadbackNotFoundFor(frame stubFrame, rider map[string]any) (any, *rpcError) {
	args, _ := frame.Params["arguments"].(map[string]any)
	target, _ := args["target"].(map[string]any)
	symbol, _ := target["symbol"].(string)
	return linkedReadbackNotFound(symbol, rider), nil
}

// TestEditProbeLinkedCorrectReplyUnderPendingRiderIsNotPending: a reply the oracle accepted, even under a pending rider, is correct
// at once: not pending, not re-read, no census.
func TestEditProbeLinkedCorrectReplyUnderPendingRiderIsNotPending(t *testing.T) {
	correctInexact := func(n int, frame stubFrame, disk func(stubFrame) (any, *rpcError)) (any, *rpcError) {
		reply, fault := disk(frame)
		return withLinkedReadbackRider(reply, linkedReadbackPendingRider()), fault
	}
	got := runLinkedReadbackCell(t, linkedReadbackCell{fallbacks: 0, source: correctInexact})
	if got.record.Outcome != "complete" || len(got.record.Errors) != 0 {
		t.Fatalf("correct inexact reply = %s %v", got.record.Outcome, got.record.Errors)
	}
	rows := linkedReadbackRows(t, got.json, "source")
	if got.reads["source"] != 1 || len(rows) != 1 || rows[0]["correct"] != true || rows[0]["pending"] == true {
		t.Fatalf("correct inexact reply was pending or re-read: reads=%d rows=%v", got.reads["source"], rows)
	}
	if _, present := got.json["readback_pending"]; present {
		t.Fatalf("pending census present for a correct reply: %v", got.json["readback_pending"])
	}
}

// TestEditProbeLinkedRereadLeavingThePendingContractFails: a re-read whose rider leaves the pending contract is a record error,
// not another pending reply.
func TestEditProbeLinkedRereadLeavingThePendingContractFails(t *testing.T) {
	for _, cell := range []struct {
		name  string
		rider func() map[string]any
	}{
		{name: "route moved", rider: func() map[string]any {
			r := linkedReadbackPendingRider()
			r["fallback_reason"] = "route_moved"
			return r
		}},
		{name: "another view", rider: func() map[string]any {
			r := linkedReadbackPendingRider()
			r["requested_view"] = "worktree:other"
			return r
		}},
		{name: "no rider", rider: func() map[string]any { return nil }},
	} {
		t.Run(cell.name, func(t *testing.T) {
			departing := func(n int, frame stubFrame, _ func(stubFrame) (any, *rpcError)) (any, *rpcError) {
				if n <= 2 {
					return linkedReadbackNotFoundFor(frame, linkedReadbackPendingRider())
				}
				return linkedReadbackNotFoundFor(frame, cell.rider())
			}
			got := runLinkedReadbackCell(t, linkedReadbackCell{fallbacks: -1, source: departing, deadline: time.Second})
			joined := strings.Join(got.record.Errors, "; ")
			if got.record.Outcome != "diagnostic_failure" || !strings.Contains(joined, "source readback left the pending contract") {
				t.Fatalf("departure = %s %v, want diagnostic_failure with the departure error", got.record.Outcome, got.record.Errors)
			}
			if got.reads["source"] != 3 {
				t.Fatalf("source reads = %d, want the re-read to stop at the departure", got.reads["source"])
			}
		})
	}
}

// TestEditProbeLinkedUncommittedACKEarnsNoPendingReadback: an ACK that is applied but not disk-committed documents no pending
// publication; the fallback stays incorrect and is not re-read.
func TestEditProbeLinkedUncommittedACKEarnsNoPendingReadback(t *testing.T) {
	got := runLinkedReadbackCell(t, linkedReadbackCell{fallbacks: 3, ack: func(a map[string]any) { a["disk_status"] = "staged" }})
	if got.record.Outcome == "complete" || got.reads["source"] != 1 {
		t.Fatalf("uncommitted ACK: outcome %s, source reads %d, errors %v", got.record.Outcome, got.reads["source"], got.record.Errors)
	}
	for _, row := range linkedReadbackRows(t, got.json, "source") {
		if row["pending"] == true {
			t.Fatalf("readback was pending after an uncommitted ACK: %v", row)
		}
	}
}

// TestEditProbeLinkedExactRereadRowIsMarkedReread: the exact re-read row is marked reread, so readback_ms.<kind> keeps
// one immediate attempt per record.
func TestEditProbeLinkedExactRereadRowIsMarkedReread(t *testing.T) {
	got := runLinkedReadbackCell(t, linkedReadbackCell{fallbacks: 2})
	rows := linkedReadbackRows(t, got.json, "source")
	if len(rows) != 2 || rows[1]["reread"] != true || rows[0]["reread"] == true {
		t.Fatalf("source rows = %v, want the exact row marked reread and the immediate row not", rows)
	}
	data, err := json.Marshal(summarizeEditProbe([]editProbeRecord{got.record}, 42))
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	measures, _ := summary["measures"].(map[string]any)
	source, _ := measures["readback_ms.source"].(map[string]any)
	if source["count"] != float64(1) {
		t.Fatalf("readback_ms.source = %v, want one immediate attempt", source)
	}
}

// A re-read that fails below the answer level names why, not only that the
// rider is missing.
func TestEditProbeRiderDepartureNamesTheFailedReply(t *testing.T) {
	back := editProbeReadback{Disposition: &editProbeReadbackDisposition{Kind: editProbeReadbackTransportError}, Error: "connection reset"}
	if got := editProbeRiderDeparture(back); got != "no freshness rider; disposition transport_error: connection reset" {
		t.Fatalf("departure = %q", got)
	}
}
