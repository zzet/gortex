package main

import (
 "context"
 "os"
 "sync"
 "testing"
 "time"
)

func completeMovePublicRecord() editProbeRecord {
 record := editProbeRecord{FirstCorrect: make(map[string]Metric)}
 for _, name := range []string{"source", "summary", "editing_context", "search_visible", "text", "callers", "call_chain", "old_source_absent"} { record.FirstCorrect[name] = measured(1, "ms") }
 for _, name := range []string{"file", "source", "old_source_absent", "source_file_after_move"} { record.Readbacks = append(record.Readbacks, editProbeReadback{Kind: name, Correct: true}) }
 return record
}

func TestMoveCollectorQueriesOriginalFilesAndKeepsASeparate(t *testing.T) {
 refs, statuses := moveAccountingFixture(t)
 var mu sync.Mutex
 var queried []string
 socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
  args, _ := frame.Params["arguments"].(map[string]any)
  options, _ := args["options"].(map[string]any)
  receipt, _ := options["receipt"].(string)
  mu.Lock(); queried = append(queried, receipt); mu.Unlock()
  for _, file := range refs.Files { if file.Receipt == receipt { return stubContent(statuses[file.Role]), nil } }
  return stubContent(map[string]any{"found": false}), nil
 })
 ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
 defer cancel()
 client, err := openMCP(ctx, socket, "")
 if err != nil { t.Fatal(err) }
 defer client.Close()
 sink, err := os.CreateTemp(t.TempDir(), "move-evidence-*.jsonl")
 if err != nil { t.Fatal(err) }
 defer sink.Close()
 state := &editProbeState{client: client, sink: sink, record: completeMovePublicRecord()}
 state.startRaw()
 defer state.finishRaw()
 run := &editProbeRuntime{options: runOptions{Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Prefix: "fixture"}}}, phase: &Phase{Metrics: map[string]Metric{}}, origin: time.Now(), requireAccounting: true}
 run.collectMoveEvidence(ctx, state, refs, 17)
 if ctx.Err() != nil || !state.record.GraphTerminal || state.record.MoveAccounting == nil || state.record.MoveAccounting.State != "observed_move_windows_checks_pass" || state.record.Receipt != "" || state.record.Accounting.A != nil {
  t.Fatalf("standalone collector borrowed physical timing or failed: %+v %v", state.record, ctx.Err())
 }
 mu.Lock(); defer mu.Unlock()
 if len(queried) != 2 || queried[0] != refs.Files[0].Receipt || queried[1] != refs.Files[1].Receipt { t.Fatal(queried) }
}

func TestMoveCollectorCannotBorrowForeignOrPendingRefusal(t *testing.T) {
 refs, statuses := moveAccountingFixture(t)
 file := refs.Files[0]
 row := statuses[file.Role]
 row["direct_reconcile_state"] = "pending_observer"
 if moveOwnedRefusal(row, refs, file) { t.Fatal("pending callback treated as terminal") }
 row["direct_reconcile_state"] = "unavailable_competing_commit"
 if !moveOwnedRefusal(row, refs, file) { t.Fatal("owned immutable refusal not retained") }
 row["operation_id"] = "move-999"
 if moveOwnedRefusal(row, refs, file) { t.Fatal("foreign refusal ended collection") }
}

func TestMoveBaselinePublicCompletionNeverInventsReceiptOrMeasuredPhases(t *testing.T) {
 refs, _ := moveAccountingFixture(t)
 refs.Files, refs.ReferenceError = nil, "standalone move observation references unavailable"
 state := &editProbeState{record: completeMovePublicRecord()}
 run := &editProbeRuntime{}
 run.collectMoveEvidence(t.Context(), state, refs, 17)
 if state.record.GraphTerminal || state.record.GraphTerminalBasis != "baseline_complete_public_move_facts_no_receipt_proof" || state.record.MoveAccounting == nil || state.record.MoveAccounting.State != "unavailable" || state.record.MoveAccounting.A != nil { t.Fatal(state.record) }
 state.record.Readbacks[0].Correct = false
 if movePublicFactsComplete(state.record) { t.Fatal("incorrect public readback qualified baseline") }
}
