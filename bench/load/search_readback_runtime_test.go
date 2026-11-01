package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// One real owned edit is a bounded protocol control, never a thousand-probe
// performance sample. The first search response can be wrong even if a later
// poll would be correct; only that first editor-session response counts here.
func TestSearchReadbackAcceptanceKeepsFirstSameSessionExactLineWitness(t *testing.T) {
	for _, fault := range []string{"none", "wrong_line", "wrong_path", "missing_line", "refusal"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			c := editProbeRenameCorpus(t, "go", 1)
			c.EditTool = "edit.file"
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			plan.SearchReadbackAcceptance = true
			step := plan.Steps[0]
			old := step.Marker + "Before"
			line := strings.Count(strings.Split(c.EditInitialContent, "func "+old)[0], "\n") + 1
			// The controlled unit plan pins the same pre-dispatch expectation
			// as the real 1000-edit planner, without offering a campaign.
			encoded, err := json.Marshal(step)
			if err != nil {
				t.Fatal(err)
			}
			var offered map[string]any
			if err := json.Unmarshal(encoded, &offered); err != nil {
				t.Fatal(err)
			}
			offered["expected_search_line"] = line
			encoded, err = json.Marshal(offered)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &step); err != nil {
				t.Fatal(err)
			}
			plan.Steps[0] = step
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.File), []byte(c.EditInitialContent), 0o600); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var editorCalls []string
			searches := 0
			fallback := editProbeDiskStub(t, c, dir)
			run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(frame stubFrame) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				name, _ := frame.Params["name"].(string)
				operation, _ := args["operation"].(string)
				if frame.Method != "tools/call" {
					return stubContent(map[string]any{}), nil
				}
				if frame.Connection == 1 {
					mu.Lock()
					editorCalls = append(editorCalls, name+"."+operation)
					mu.Unlock()
				}
				if name == "edit" {
					data, err := os.ReadFile(filepath.Join(dir, c.File))
					if err != nil {
						t.Error(err)
					}
					match, _ := args["match"].(string)
					replacement, _ := args["replacement"].(string)
					if strings.Count(string(data), match) != 1 {
						t.Error("edit did not own one original declaration")
					}
					if err := os.WriteFile(filepath.Join(dir, c.File), []byte(strings.Replace(string(data), match, replacement, 1)), 0o600); err != nil {
						t.Error(err)
					}
					return stubContent(map[string]any{"mutation_receipt": "owned-rename"}), nil
				}
				if name == "change" {
					return stubContent(map[string]any{"graph_status_terminal": true, "graph_status": "fresh", "mutation_id": step.MutationID}), nil
				}
				if name == "search" {
					mu.Lock()
					searches++
					mu.Unlock()
					query, _ := args["query"].(string)
					if query == old {
						if operation == "symbols" {
							return stubContent(map[string]any{"results": []any{}, "total": 0, "truncated": false}), nil
						}
						return stubContent(map[string]any{"matches": []any{}, "count": 0, "truncated": false}), nil
					}
					if fault == "refusal" {
						return nil, &rpcError{Code: -32000, Message: "view pending"}
					}
					path, observedLine := "fixture/"+c.File, line
					if fault == "wrong_path" {
						path = "fixture/foreign.go"
					}
					if fault == "wrong_line" {
						observedLine++
					}
					if operation == "symbols" {
						row := map[string]any{"id": path + "::" + step.Marker, "file_path": path, "start_line": observedLine}
						if fault == "missing_line" {
							delete(row, "start_line")
						}
						return stubContent(map[string]any{"results": []any{row}, "total": 1, "truncated": false}), nil
					}
					row := map[string]any{"path": path, "line": observedLine, "text": "func " + step.Marker + "() int { return Example() }"}
					if fault == "missing_line" {
						delete(row, "line")
					}
					return stubContent(map[string]any{"matches": []any{row}, "count": 1, "truncated": false}), nil
				}
				return fallback(frame)
			})
			run.options.SearchReadbackAcceptance = true
			run.requireAccounting = false
			state := run.states[0]
			zero := int64(0)
			state.record.ActivatedNS = &zero
			run.doEdit(ctx, step)
			state.finishRaw()
			mu.Lock()
			calls := append([]string(nil), editorCalls...)
			searchCount := searches
			mu.Unlock()
			if len(calls) < 3 || calls[0] != "edit.file" || calls[1] != "search.symbols" || calls[2] != "search.text" {
				t.Fatalf("searches were not the first same-editor post-ACK calls: %v", calls)
			}
			if searchCount < 2 {
				t.Fatal("actual immediate searches missing")
			}
			immediate := map[string]editProbeReadback{}
			for _, back := range state.record.Readbacks {
				immediate[back.Kind] = back
			}
			for _, kind := range []string{"search_visible", "text"} {
				back, found := immediate[kind]
				if !found || back.Correct != (fault == "none") || back.Latency.Value == nil {
					t.Fatalf("first exact search witness lost/refused/wrong line credited: %s %+v", kind, back)
				}
				data, err := json.Marshal(back)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				if fields["expected_line"] != float64(line) {
					t.Fatalf("exact pre-dispatch line expectation absent: %s", data)
				}
			}
		})
	}
}
