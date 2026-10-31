package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func editProbeEditorSaveCorpus(t *testing.T, count int) Corpus {
	t.Helper()
	c := editProbeSymbolCorpus(t, "go")
	c.EditKind, c.EditTool, c.EditSender = "rename", "edit.file", editProbeEditorSave
	for i := range count {
		c.EditInitialContent += strings.ReplaceAll(c.EditTemplate, "{{marker}}", editProbePlaceholder(42, i))
	}
	return c
}

// editProbeDiskStub answers every default oracle, readback and the restore
// batch from the current bytes of the Go files on disk, as a daemon that has
// indexed them would. A name no longer declared answers the exact not-found
// refusal; caller subgraphs keep the stable callee's own node.
func editProbeDiskStub(t *testing.T, c Corpus, dir string) func(stubFrame) (any, *rpcError) {
	declared := regexp.MustCompile(`func (GortexEditProbe\w+)\(`)
	callee := c.Symbol[strings.LastIndex(c.Symbol, "::")+2:]
	return func(frame stubFrame) (any, *rpcError) {
		if frame.Method != "tools/call" {
			return stubContent(map[string]any{}), nil
		}
		args, _ := frame.Params["arguments"].(map[string]any)
		if frame.Params["name"] == "edit" && args["operation"] == "batch" {
			for _, raw := range args["changes"].([]any) {
				change := raw.(map[string]any)
				path := filepath.Join(dir, strings.TrimPrefix(change["path"].(string), "fixture/"))
				var err error
				if change["op"] == "delete_file" {
					err = os.Remove(path)
				} else {
					err = os.WriteFile(path, []byte(change["new_string"].(string)), 0o600)
				}
				if err != nil {
					t.Error(err)
				}
			}
			return stubContent(map[string]any{"committed": true}), nil
		}
		owner := map[string]string{}
		byFile := map[string][]any{}
		graph, edges := []any{map[string]any{"id": "fixture/" + c.Symbol}}, []any{}
		paths, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Error(err)
			}
			file := filepath.Base(path)
			for _, match := range declared.FindAllStringSubmatch(string(data), -1) {
				id := "fixture/" + file + "::" + match[1]
				owner[match[1]] = file
				byFile[file] = append(byFile[file], map[string]any{"id": id})
				graph = append(graph, map[string]any{"id": id})
				edges = append(edges, map[string]any{"from": id, "to": "fixture/" + c.Symbol, "kind": "calls"})
			}
		}
		target, _ := args["target"].(map[string]any)
		symbol, _ := target["symbol"].(string)
		file, _ := target["file"].(string)
		symbol, file = strings.TrimPrefix(symbol, "fixture/"), strings.TrimPrefix(file, "fixture/")
		_, name, _ := strings.Cut(symbol, "::")
		query, _ := args["query"].(string)
		rows := []any{}
		switch frame.Params["name"].(string) + "." + args["operation"].(string) {
		case "read.file":
			data, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Error(err)
			}
			return stubContent(map[string]any{"path": "fixture/" + file, "content": string(data)}), nil
		case "read.source":
			if owner[name] == "" || symbol != owner[name]+"::"+name {
				return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "symbol not found: fixture/" + symbol}}}, nil
			}
			return stubContent(map[string]any{"id": "fixture/" + symbol, "name": name, "file_path": "fixture/" + owner[name], "source": "func " + name + "() int { return " + callee + "() }"}), nil
		case "read.summary":
			return stubContent(map[string]any{"nodes": byFile[file]}), nil
		case "read.editing_context":
			return stubContent(map[string]any{"defines": byFile[file]}), nil
		case "search.symbols":
			if owner[query] != "" {
				rows = append(rows, map[string]any{"id": "fixture/" + owner[query] + "::" + query})
			}
			return stubContent(map[string]any{"results": rows, "total": len(rows), "truncated": false, "query_class": "symbol"}), nil
		case "search.text":
			if owner[query] != "" {
				rows = append(rows, map[string]any{"path": "fixture/" + owner[query], "line": 3, "text": "func " + query + "() int { return " + callee + "() }"})
			}
			return stubContent(map[string]any{"matches": rows, "count": len(rows), "query": query, "truncated": false}), nil
		case "relations.callers", "trace.call_chain":
			return stubContent(map[string]any{"nodes": graph, "edges": edges, "total_nodes": len(graph), "total_edges": len(edges), "truncated": false}), nil
		}
		t.Errorf("unexpected tool call %v", frame.Params)
		return stubContent(map[string]any{}), nil
	}
}

// The runtime holds prepared states for the first n planned ordinals; each
// has its own prober session beside the one fixed editor.
func newEditProbeNativeRun(t *testing.T, ctx context.Context, c Corpus, plan editProbePlan, dir string, n int) *editProbeRuntime {
	t.Helper()
	socket := startStub(t, editProbeDiskStub(t, c, dir))
	open := func() *socketClient {
		client, err := openMCP(ctx, socket, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	original, err := os.ReadFile(filepath.Join(dir, c.File))
	if err != nil {
		t.Fatal(err)
	}
	phase := &Phase{Metrics: map[string]Metric{}, EditProbe: newEditProbeReport(plan, c, true)}
	run := &editProbeRuntime{options: runOptions{Deadline: 3 * time.Second, Seed: 42}, p: &privateRun{Repos: []privateRepo{{Prefix: "fixture", Path: dir, Corpus: c}}}, phase: phase, plan: plan, editors: []*socketClient{open()}, original: original, origin: time.Now(), requireAccounting: true}
	for _, step := range plan.Steps[:n] {
		sink := newEditProbeHeldSink(t)
		sink.unblock()
		state := startHeldEditProbeSpool(t, sink)
		state.step, state.record, state.client = step, newEditProbeRecord(c, step), open()
		state.record.Outcome, state.record.Errors = "not_started", nil // as prepared
		if step.BeforeSymbol != "" {
			state.absenceBefore = map[string]bool{step.BeforeSymbol: true}
		}
		run.states = append(run.states, state)
	}
	return run
}

func awaitEditProbeFirstProbe(t *testing.T, s *editProbeState) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		started := !s.firstProbeWrite.IsZero()
		s.mu.Unlock()
		if started {
			return
		}
	}
	t.Fatal("no probe wire start")
}

func checkEditProbeNativeRecord(t *testing.T, record editProbeRecord, sender string) {
	t.Helper()
	if record.Outcome != "complete" || len(record.Errors) != 0 || record.Readback.Value == nil {
		t.Fatalf("native record did not complete: %+v", record)
	}
	if record.Ack.Value != nil || record.Ack.Reason != "native write has no MCP acknowledgement" || record.Receipt != "" || record.MutationID != "" || record.Accounting.State != "not_applicable_native_write" || record.Derived.Value != nil || record.GraphTerminal {
		t.Fatalf("native record invented an ACK, receipt or accounting: %+v", record)
	}
	for name, lap := range record.PhasesMS {
		if lap.Value != nil {
			t.Fatal("native record invented a phase", name)
		}
	}
	native := record.NativeWrite
	if native == nil || native.Sender != sender || native.StartNS == nil || native.EndNS == nil || *native.EndNS < *native.StartNS || native.Window.Value == nil || native.Before == nil || native.Written == nil {
		t.Fatalf("native write window missing: %+v", native)
	}
	for _, back := range record.Readbacks {
		if back.ObservationOnly != (back.Kind != "file") || back.Kind == "file" && !back.Correct {
			t.Fatalf("only the file readback gates a native write: %+v", record.Readbacks)
		}
	}
}

func TestEditProbeShellWriteFollowsOwnToolACKInPlace(t *testing.T) {
	c := editProbeCorpus()
	c.EditSender = editProbeShellAfterTool
	plan, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := makeEditProbePlan(editProbeCorpus(), 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Sender != editProbeShellAfterTool || plan.Tool != "edit.file" || !slices.Contains(plan.NotSelected, "other native senders run as their own cells") {
		t.Fatalf("shell plan identity: %+v", plan)
	}
	for i, step := range plan.Steps {
		want := reference.Steps[i]
		if step.DueNS != want.DueNS || step.ActivateNS != want.ActivateNS || step.Marker != want.Marker {
			t.Fatal("native sender changed the offered schedule")
		}
		if i%2 == 0 {
			if step.Sender != "" || step.After != nil || step.RequestHash != want.RequestHash || step.MutationID != want.MutationID {
				t.Fatalf("tool ordinal changed: %+v", step)
			}
			continue
		}
		if step.Sender != "shell_write" || step.After == nil || *step.After != i-1 || step.AfterEvent != "ack" || step.MutationID != "" || step.Request.Tool != "native" || step.Request.Arguments["path"] != c.File || step.Request.Arguments["match"] != want.Request.Arguments["match"] || step.Request.Arguments["replacement"] != want.Request.Arguments["replacement"] || step.Request.Arguments["options"] != nil || step.Request.Arguments["output"] != nil {
			t.Fatalf("shell ordinal does not follow its tool edit: %+v", step)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, c.File)
	original := "package sample\n\nfunc Example() int { return 1 }\n\n" + c.EditAnchor + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newEditProbeNativeRun(t, ctx, c, plan, dir, 2)
	tool, shell := run.states[0], run.states[1]
	run.activate(ctx, plan.Steps[1])
	done := make(chan struct{})
	go func() {
		run.doEdit(ctx, plan.Steps[1])
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Fatal("shell write did not wait for the preceding tool edit's own ACK")
	}
	tool.mu.Lock()
	tool.acknowledged = time.Now()
	tool.mu.Unlock()
	tool.signalAck()
	<-done
	record := shell.record
	checkEditProbeNativeRecord(t, record, "shell_write")
	native := record.NativeWrite
	if native.After == nil || *native.After != 0 || native.DependencyReadyNS == nil || *native.StartNS < *native.DependencyReadyNS || native.Replaced || native.Written.Size <= native.Before.Size {
		t.Fatalf("shell write was not an in-place write after the ACK: %+v", native)
	}
	if native.Before.Inode != nil && (native.Written.Inode == nil || *native.Before.Inode != *native.Written.Inode) {
		t.Fatal("shell write replaced the file")
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "func "+plan.Steps[1].Marker+"()") {
		t.Fatal("shell write missing from disk")
	}
}

func TestEditProbeEditorSaveKeepsSizeAndReplacesAtomically(t *testing.T) {
	c := editProbeEditorSaveCorpus(t, 100)
	plan, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := makeEditProbePlan(editProbeCorpus(), 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range plan.Steps {
		placeholder := editProbePlaceholder(42, i)
		match, _ := step.Request.Arguments["match"].(string)
		replacement, _ := step.Request.Arguments["replacement"].(string)
		if step.DueNS != reference.Steps[i].DueNS || step.Sender != "editor_save" || step.After != nil || step.MutationID != "" || step.BeforeSymbol != c.File+"::"+placeholder || len(placeholder) != len(step.Marker) || step.Request.Tool != "native" || len(match) != len(replacement) || !strings.Contains(match, placeholder) || !strings.Contains(replacement, step.Marker) {
			t.Fatalf("editor save step is not a same-size placeholder rename: %+v", step)
		}
	}

	c = editProbeEditorSaveCorpus(t, 1)
	plan, err = makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[0]
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, c.File)
	if err := os.WriteFile(path, []byte(c.EditInitialContent), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newEditProbeNativeRun(t, ctx, c, plan, dir, 1)
	run.activate(ctx, step)
	awaitEditProbeFirstProbe(t, run.states[0])
	run.doEdit(ctx, step)
	record := run.states[0].record
	checkEditProbeNativeRecord(t, record, "editor_save")
	native := record.NativeWrite
	if !native.Replaced || native.Before.Size != native.Written.Size || len(record.Readbacks) != 3 || record.Readbacks[2].Kind != "old_source_absent" {
		t.Fatalf("editor save did not replace same-size bytes: %+v %+v", native, record.Readbacks)
	}
	if native.Before.Inode != nil && (native.Written.Inode == nil || *native.Before.Inode == *native.Written.Inode) {
		t.Fatal("editor save wrote in place instead of renaming")
	}
	if temps, _ := filepath.Glob(filepath.Join(dir, ".*.gortex-save.tmp")); len(temps) != 0 {
		t.Fatal("editor save left its temporary file", temps)
	}
	// The terminal native record admits the timed restore batch.
	if err := run.finish(); err != nil {
		t.Fatal(err)
	}
	if err := run.restore(ctx); err != nil || run.phase.EditProbe.RestoreMS.Value == nil {
		t.Fatalf("restore after native save: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != c.EditInitialContent {
		t.Fatal("restore did not return original bytes")
	}
}

func TestEditProbeNativeDependencyFailureIsExplicit(t *testing.T) {
	c := editProbeCorpus()
	c.EditSender = editProbeShellAfterTool
	plan, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, c.File)
	original := "package sample\n\nfunc Example() int { return 1 }\n\n" + c.EditAnchor + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newEditProbeNativeRun(t, ctx, c, plan, dir, 2)
	run.activate(ctx, plan.Steps[1])
	run.states[0].signalAck() // the tool edit ended without its own ACK
	run.doEdit(ctx, plan.Steps[1])
	record := run.states[1].record
	if record.Outcome != "diagnostic_failure" || !slices.Contains(record.Errors, "native write dependency failed: ordinal 0 has no own acknowledgement") || record.NativeWrite == nil || record.NativeWrite.StartNS != nil || record.NativeWrite.Window.Value != nil || record.Ack.Value != nil {
		t.Fatalf("dependency failure was not explicit: %+v", record)
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Fatal("native write ran without its dependency")
	}
}

func TestEditProbeNativeSenderRefusesUnownedPathsAndWrongFixtures(t *testing.T) {
	shell := editProbeCorpus()
	shell.EditSender = editProbeShellAfterTool
	save := editProbeEditorSaveCorpus(t, 100)
	for name, build := range map[string]func() (editProbePlan, error){
		"shell_rename": func() (editProbePlan, error) {
			c := editProbeRenameCorpus(t, "go", 100)
			c.EditTool, c.EditSender = "edit.file", editProbeShellAfterTool
			return makeEditProbePlan(c, 42, 100)
		},
		"shell_write_tool": func() (editProbePlan, error) {
			c := editProbeWriteCorpus(t, "go")
			c.EditSender = editProbeShellAfterTool
			return makeEditProbePlan(c, 42, 100)
		},
		"shell_one_edit":    func() (editProbePlan, error) { return makeEditProbePlan(shell, 42, 1) },
		"shell_two_editors": func() (editProbePlan, error) { return makeEditProbeTrafficPlan(shell, 42, 100, 2) },
		"save_addition": func() (editProbePlan, error) {
			c := editProbeCorpus()
			c.EditSender = editProbeEditorSave
			return makeEditProbePlan(c, 42, 100)
		},
		"save_two_editors": func() (editProbePlan, error) { return makeEditProbeTrafficPlan(save, 42, 100, 2) },
		"save_without_placeholders": func() (editProbePlan, error) {
			c := editProbeRenameCorpus(t, "go", 100)
			c.EditTool, c.EditSender = "edit.file", editProbeEditorSave
			return makeEditProbePlan(c, 42, 100)
		},
		"unknown_sender": func() (editProbePlan, error) {
			c := editProbeCorpus()
			c.EditSender = "watch"
			return makeEditProbePlan(c, 42, 100)
		},
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s: wrong native fixture planned", name)
		}
	}

	c := editProbeEditorSaveCorpus(t, 1)
	plan, err := makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[0]
	step.Request.Arguments = map[string]any{"path": "other.go", "match": step.Request.Arguments["match"], "replacement": step.Request.Arguments["replacement"]}
	plan.Steps[0] = step
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	for _, name := range []string{c.File, "other.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(c.EditInitialContent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := newEditProbeNativeRun(t, ctx, c, plan, dir, 1)
	run.activate(ctx, step)
	run.doEdit(ctx, step)
	if record := run.states[0].record; record.Outcome != "diagnostic_failure" || !slices.Contains(record.Errors, "native sender writes only the registered owned fixture file") {
		t.Fatalf("unowned native path accepted: %+v", record)
	}
	for _, name := range []string{c.File, "other.go"} {
		if data, _ := os.ReadFile(filepath.Join(dir, name)); string(data) != c.EditInitialContent {
			t.Fatal("refused native write changed", name)
		}
	}
}
