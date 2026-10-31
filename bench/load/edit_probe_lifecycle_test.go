package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func editProbeCreateDeleteCorpus(t *testing.T) Corpus {
	t.Helper()
	c := editProbeWriteCorpus(t, "go")
	c.EditTool, c.EditSender = "edit.file", editProbeCreateDelete
	return c
}

func TestEditProbeCreateDeletePairsUseOwnedNewFilesAndExactAbsence(t *testing.T) {
	c := editProbeCreateDeleteCorpus(t)
	plan, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := makeEditProbePlan(editProbeCorpus(), 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range plan.Steps {
		creator := plan.Steps[i-i%2]
		file := "gortex_edit_probe_" + creator.Marker + ".go"
		if step.DueNS != reference.Steps[i].DueNS || step.File != file || step.Request.Tool != "native" || step.Request.Arguments["path"] != file || step.MutationID != "" {
			t.Fatalf("lifecycle step does not own its new file: %+v", step)
		}
		names := map[string]bool{}
		for _, oracle := range editProbeOracles(c, step) {
			names[oracle.name] = true
		}
		if i%2 == 0 {
			if step.Sender != "create_file" || step.After != nil || step.Deleted || step.BeforeSymbol != "" || step.Request.Arguments["content"] != "package sample\n"+strings.ReplaceAll(c.EditTemplate, "{{marker}}", step.Marker) || len(names) != 7 {
				t.Fatalf("create step: %+v %v", step, names)
			}
			continue
		}
		if step.Sender != "delete_file" || step.After == nil || *step.After != i-1 || step.AfterEvent != "first_correct" || !step.Deleted || step.BeforeSymbol != file+"::"+creator.Marker || len(names) != 4 || !names["source"] || !names["search_visible"] || !names["text"] || !names["callers"] {
			t.Fatalf("delete step must prove exact absence of its own pair's file: %+v %v", step, names)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, c.File), []byte(c.EditInitialContent), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newEditProbeNativeRun(t, ctx, c, plan, dir, 2)
	created := filepath.Join(dir, plan.Steps[0].File)
	for i, step := range plan.Steps[:2] {
		run.activate(ctx, step)
		awaitEditProbeFirstProbe(t, run.states[i])
		run.doEdit(ctx, step)
		record := run.states[i].record
		if record.Outcome != "complete" || len(record.Errors) != 0 || record.Ack.Value != nil || record.Accounting.State != "not_applicable_native_write" || record.NativeWrite == nil || record.NativeWrite.Window.Value == nil {
			t.Fatalf("%s did not complete: %+v", step.Sender, record)
		}
		if _, err := os.Stat(created); (err == nil) != (i == 0) {
			t.Fatalf("%s left the created file in the wrong state: %v", step.Sender, err)
		}
	}
	create, remove := run.states[0].record, run.states[1].record
	if create.NativeWrite.Before != nil || create.NativeWrite.Written == nil || create.Readback.Value == nil {
		t.Fatalf("create window: %+v", create)
	}
	if remove.NativeWrite.Before == nil || remove.NativeWrite.Written != nil || remove.Readback.Value != nil || remove.Readback.Reason != "a deleted file has no readback to gate" || len(remove.Readbacks) != 1 || remove.Readbacks[0].ExpectedOutcome != "symbol_absent" || !remove.Readbacks[0].ObservationOnly || remove.ExpectedRefusals == 0 {
		t.Fatalf("delete was not proved by exact absence: %+v", remove)
	}
	if err := run.restore(ctx); err != nil || run.phase.EditProbe.RestoreMS.Value != nil || run.phase.EditProbe.RestoreMS.Reason != "every created file was deleted by its own pair" {
		t.Fatalf("restore after complete pairs: %v %+v", err, run.phase.EditProbe.RestoreMS)
	}
}

func TestEditProbeCreatedFilesRestoredByOneTimedBatch(t *testing.T) {
	c := editProbeCreateDeleteCorpus(t)
	plan, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, c.File), []byte(c.EditInitialContent), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newEditProbeNativeRun(t, ctx, c, plan, dir, 2)
	run.activate(ctx, plan.Steps[0])
	awaitEditProbeFirstProbe(t, run.states[0])
	run.doEdit(ctx, plan.Steps[0])
	// A create whose probes did not all become first-correct never admits
	// its delete; the created file stays for the restore batch.
	run.states[0].mu.Lock()
	run.states[0].record.Outcome = "product_deadline"
	run.states[0].mu.Unlock()
	run.activate(ctx, plan.Steps[1])
	run.doEdit(ctx, plan.Steps[1])
	remove := run.states[1].record
	created := filepath.Join(dir, plan.Steps[0].File)
	if remove.Outcome != "diagnostic_failure" || !slices.Contains(remove.Errors, "native write dependency failed: ordinal 0 did not complete every first-correct probe") || remove.NativeWrite.StartNS != nil {
		t.Fatalf("delete ran without its create: %+v", remove)
	}
	if _, err := os.Stat(created); err != nil {
		t.Fatal("delete removed a file whose create was unproved")
	}
	if err := run.restore(ctx); err != nil || run.phase.EditProbe.RestoreMS.Value == nil {
		t.Fatalf("restore did not remove the created file in one timed batch: %v", err)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatal("created file remains after restore")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, c.File)); string(data) != c.EditInitialContent {
		t.Fatal("restore changed the fixture")
	}

	for name, mutate := range map[string]func(*Corpus){
		"typescript": func(c *Corpus) {
			*c = editProbeWriteCorpus(t, "typescript")
			c.EditTool, c.EditSender = "edit.file", editProbeCreateDelete
		},
		"nested_fixture": func(c *Corpus) { c.File, c.Symbol = "pkg/sample.go", "pkg/sample.go::Example" },
		"compact":        func(c *Corpus) { c.EditOutput = "compact" },
		"no_fixture":     func(c *Corpus) { c.EditInitialContent = "" },
	} {
		bad := editProbeCreateDeleteCorpus(t)
		mutate(&bad)
		if _, err := makeEditProbePlan(bad, 42, 100); err == nil {
			t.Errorf("%s: create/delete planned", name)
		} else if name == "typescript" && !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("TypeScript create/delete is a gap, not inapplicable: %v", err)
		}
	}
	if _, err := makeEditProbePlan(c, 42, 1); err == nil {
		t.Error("create/delete one-edit diagnostic planned")
	}
}

func TestEditProbeHotFilePairsShareDueAndFixedEditors(t *testing.T) {
	single, err := makeEditProbePlan(editProbeCorpus(), 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []Corpus{editProbeCorpus(), editProbeWriteCorpus(t, "go")} {
		c.EditSender = editProbeHotFile
		plan, err := makeEditProbeTrafficPlan(c, 42, 100, 2)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Editors != 2 || plan.Sender != editProbeHotFile || plan.SessionModel != editProbeFixedEditorModel || len(plan.Steps) != 100 {
			t.Fatalf("hot-file plan identity: %+v", plan)
		}
		for k := range 50 {
			a, b := plan.Steps[2*k], plan.Steps[2*k+1]
			if a.DueNS != b.DueNS || a.DueNS != single.Steps[k].DueNS || a.Editor != 0 || b.Editor != 1 || a.Sender != "" || b.Sender != "" || a.MutationID == "" || b.MutationID == "" || a.Marker == b.Marker {
				t.Fatalf("hot-file pair %d is not two MCP edits at one due time: %+v %+v", k, a, b)
			}
		}
	}
	hot := editProbeCorpus()
	hot.EditSender = editProbeHotFile
	symbol := editProbeSymbolCorpus(t, "go")
	symbol.EditSender = editProbeHotFile
	rename := editProbeRenameCorpus(t, "go", 100)
	rename.EditTool, rename.EditSender = "edit.file", editProbeHotFile
	for name, build := range map[string]func() (editProbePlan, error){
		"one_editor":    func() (editProbePlan, error) { return makeEditProbePlan(hot, 42, 100) },
		"three_editors": func() (editProbePlan, error) { return makeEditProbeTrafficPlan(hot, 42, 100, 3) },
		"symbol_tool":   func() (editProbePlan, error) { return makeEditProbeTrafficPlan(symbol, 42, 100, 2) },
		"rename":        func() (editProbePlan, error) { return makeEditProbeTrafficPlan(rename, 42, 100, 2) },
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s: hot file planned", name)
		}
	}
}
