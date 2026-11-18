package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func editProbeRelocationRefusalCorpus(t *testing.T, language string) Corpus {
	t.Helper()
	c := editProbeSymbolCorpus(t, language)
	ext := filepath.Ext(c.File)
	c.EditTool, c.EditKind, c.EditExpect = "refactor.move", "relocate", "refusal"
	c.EditDestination = "destination" + ext
	c.EditDestinationContent = map[string]string{".ts": "export function DestinationAnchor(): number { return 7; }\n", ".py": "def DestinationAnchor():\n    return 7\n"}[ext]
	for i := range 100 {
		c.EditInitialContent += strings.ReplaceAll(c.EditTemplate, "{{marker}}", fmt.Sprintf("GortexEditProbe%016xN%03dBefore", uint64(42), i))
	}
	return c
}

func TestEditProbeRelocationRefusalCellRequiresExactProducerRefusal(t *testing.T) {
	for _, language := range []string{"typescript", "python"} {
		c := editProbeRelocationRefusalCorpus(t, language)
		plan, err := makeEditProbePlan(c, 42, 100)
		if err != nil {
			t.Fatal(language, err)
		}
		for _, step := range plan.Steps {
			if !step.ExpectedRefusal || !step.Relocated || step.Request.name() != "refactor.move" || editProbeOracles(c, step) != nil {
				t.Fatalf("%s refusal step: %+v", language, step)
			}
		}
		if len(editProbeOperations(plan, c)) != len(plan.Steps) {
			t.Fatal("a refusal cell offered probes")
		}
		unmarked := c
		unmarked.EditExpect = ""
		if _, err := makeEditProbePlan(unmarked, 42, 100); err == nil || !strings.HasPrefix(err.Error(), "unsupported language") {
			t.Fatalf("%s relocation without the explicit refusal cell: %v", language, err)
		}
		misspelled := c
		misspelled.EditExpect = "refused"
		if _, err := makeEditProbePlan(misspelled, 42, 100); err == nil {
			t.Fatal("unknown edit_expect admitted")
		}
		other := editProbeSymbolCorpus(t, language)
		other.EditExpect = "refusal"
		if _, err := makeEditProbePlan(other, 42, 100); err == nil || !strings.Contains(err.Error(), "applies only to TypeScript/Python relocation") {
			t.Fatalf("edit_expect admitted for %s: %v", other.EditTool, err)
		}
	}
	golang := moveProbeCorpus(42)
	golang.EditExpect = "refusal"
	if _, err := makeEditProbePlan(golang, 42, 100); err == nil || !strings.Contains(err.Error(), "Go relocation is supported") {
		t.Fatalf("Go relocation offered as a refusal: %v", err)
	}
}

func TestEditProbeRefusalCellKeepsBytesAndSkipsRestore(t *testing.T) {
	for _, outcome := range []string{"exact", "other_path", "extra_content", "acknowledged", "bytes_changed"} {
		t.Run(outcome, func(t *testing.T) {
			c := editProbeRelocationRefusalCorpus(t, "typescript")
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			step := plan.Steps[0]
			dir := t.TempDir()
			for name, content := range map[string]string{c.File: c.EditInitialContent, c.EditDestination: c.EditDestinationContent} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := strings.TrimSpace(strings.ReplaceAll(c.EditTemplate, "{{marker}}", step.BeforeSymbol[strings.LastIndex(step.BeforeSymbol, "::")+2:]))
			refusal := fmt.Sprintf("unsupported language: move_symbol only supports Go files (got %q)", "fixture/"+c.File)
			var mu sync.Mutex
			var batches int
			socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				switch frame.Params["name"] {
				case "refactor":
					switch outcome {
					case "other_path":
						return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": strings.Replace(refusal, "fixture/", "other/", 1)}}}, nil
					case "extra_content":
						return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": refusal}, map[string]any{"type": "text", "text": "retry with move_file"}}}, nil
					case "acknowledged":
						return stubContent(map[string]any{"mutation_receipt": "moved"}), nil
					case "bytes_changed":
						if err := os.WriteFile(filepath.Join(dir, c.EditDestination), []byte(c.EditDestinationContent+before+"\n"), 0o600); err != nil {
							t.Error(err)
						}
					}
					return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": refusal}}}, nil
				case "edit":
					mu.Lock()
					batches++
					mu.Unlock()
					return stubContent(map[string]any{}), nil
				}
				if args["operation"] == "file" {
					return stubContent(map[string]any{"path": "fixture/" + c.File, "content": c.EditInitialContent}), nil
				}
				return stubContent(map[string]any{"id": "fixture/" + step.BeforeSymbol, "file_path": "fixture/" + c.File, "name": step.BeforeSymbol[strings.LastIndex(step.BeforeSymbol, "::")+2:], "source": before}), nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			editor, err := openMCP(ctx, socket, "")
			if err != nil {
				t.Fatal(err)
			}
			defer editor.Close()
			sink := newEditProbeHeldSink(t)
			sink.unblock()
			state := startHeldEditProbeSpool(t, sink)
			state.step, state.record = step, newEditProbeRecord(c, step)
			state.record.Outcome, state.record.Errors = "not_started", nil // as prepared
			phase := &Phase{Metrics: map[string]Metric{}, EditProbe: newEditProbeReport(plan, c, true)}
			run := &editProbeRuntime{options: runOptions{Deadline: 2 * time.Second, Seed: 42}, p: &privateRun{Repos: []privateRepo{{Prefix: "fixture", Path: dir, Corpus: c}}}, phase: phase, plan: plan, editors: []*socketClient{editor}, states: []*editProbeState{state}, original: []byte(c.EditInitialContent), moveOriginal: []byte(c.EditDestinationContent), origin: time.Now(), requireAccounting: true}
			run.activate(ctx, step)
			run.doEdit(ctx, step)
			record := state.record
			if outcome == "exact" {
				if record.Outcome != "complete" || len(record.Errors) != 0 || record.Accounting.State != "not_applicable_refused" || record.Receipt != "" || record.Ack.Value != nil || record.Readback.Value == nil || len(record.Readbacks) != 3 || record.Readbacks[0].Refusal != refusal {
					t.Fatalf("exact refusal cell: %+v", record)
				}
				for name, lap := range record.PhasesMS {
					if lap.Value != nil {
						t.Fatal("refusal invented a phase", name)
					}
				}
			} else if record.Outcome != "diagnostic_failure" || len(record.Errors) == 0 {
				t.Fatalf("%s passed as a refusal: %+v", outcome, record)
			}
			// The post-window step proves the bytes, sends no restore batch and
			// reports no restore time.
			err = run.restore(ctx)
			if (err != nil) != (outcome == "bytes_changed") || phase.EditProbe.RestoreMS.Value != nil || phase.EditProbe.RestoreMS.Reason != "refusal cell made no change" {
				t.Fatalf("refusal restore: %v %+v", err, phase.EditProbe.RestoreMS)
			}
			mu.Lock()
			defer mu.Unlock()
			if batches != 0 {
				t.Fatal("refusal cell sent an edit batch")
			}
		})
	}
}
