package main

import (
 "context"
 "errors"
 "path/filepath"
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

func TestMoveRestoreUsesOnePublicBatchForBothOwnedFiles(t *testing.T) {
 root := t.TempDir()
 source, target := filepath.Join(root, "source.go"), filepath.Join(root, "target.go")
 sourceNow, targetNow := "package fixture\nfunc Survives() {}\n", "package fixture\nfunc Moved() {}\n"
 sourceBefore, targetBefore := "package fixture\nfunc Moved() {}\nfunc Survives() {}\n", "package fixture\n"
 if err := os.WriteFile(source, []byte(sourceNow), 0600); err != nil { t.Fatal(err) }
 if err := os.WriteFile(target, []byte(targetNow), 0600); err != nil { t.Fatal(err) }
 var mu sync.Mutex
 var received []any
 var handlerErr error
 socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
  args, _ := frame.Params["arguments"].(map[string]any)
  changes, _ := args["changes"].([]any)
  mu.Lock()
  defer mu.Unlock()
  received = changes
  for _, raw := range changes {
   change, _ := raw.(map[string]any)
   path, _ := change["path"].(string)
   before, _ := change["old_string"].(string)
   after, _ := change["new_string"].(string)
   var absolute, want string
   switch path {
   case "fixture/source.go": absolute, want = source, sourceNow
   case "fixture/target.go": absolute, want = target, targetNow
   default: handlerErr = errors.New("unowned restore path"); continue
   }
   if change["op"] != "edit_file" || before != want { handlerErr = errors.New("restore lost exact current bytes"); continue }
   if err := os.WriteFile(absolute, []byte(after), 0600); err != nil { handlerErr = err }
  }
  return stubContent(map[string]any{"status": "committed"}), nil
 })
 ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
 defer cancel()
 client, err := openMCP(ctx, socket, "")
 if err != nil { t.Fatal(err) }
 defer client.Close()
 refs, _ := moveAccountingFixture(t)
 run := &editProbeRuntime{options: runOptions{Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Path: root, Prefix: "fixture", Corpus: Corpus{EditDestination: "target.go"}}}}, editor: client, plan: editProbePlan{Tool: "refactor.move", File: "source.go"}, original: []byte(sourceBefore), moveOriginal: []byte(targetBefore), requireAccounting: true, phase: &Phase{EditProbe: &editProbeReport{Records: []editProbeRecord{{Move: &refs}}}}}
 if err := run.restore(ctx); err == nil { t.Fatal("unknown publication allowed cleanup mutation") }
 mu.Lock(); called := len(received); mu.Unlock()
 if called != 0 { t.Fatal("pending outcome reached public restore") }
 run.phase.EditProbe.Records[0].GraphTerminal = true
 if err := run.restore(ctx); err != nil { t.Fatal(err) }
 mu.Lock(); defer mu.Unlock()
 if handlerErr != nil || len(received) != 2 { t.Fatal(received, handlerErr) }
 actualSource, err := os.ReadFile(source)
 if err != nil { t.Fatal(err) }
 actualTarget, err := os.ReadFile(target)
 if err != nil { t.Fatal(err) }
 if string(actualSource) != sourceBefore || string(actualTarget) != targetBefore { t.Fatal("two-file restore lost bytes") }
}
