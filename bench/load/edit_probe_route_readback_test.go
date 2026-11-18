package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two selected ordinals are a unit diagnostic, never a hundred-edit campaign.
// The stub performs actual owned writes and binds each receipt to its cwd.
func TestEditProbeRoutedReadbacksAndRestoreKeepEachCheckoutOriginal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	p := &privateRun{Root: t.TempDir()}
	originals := make([]string, 2)
	steps := make([]editProbeStep, 2)
	replies := make(map[string]func(stubFrame) (any, *rpcError))
	for i := range 2 {
		c := editProbeCorpus()
		c.Label = fmt.Sprintf("checkout-%d", i)
		callee := fmt.Sprintf("Example%d", i)
		c.Symbol = c.File + "::" + callee
		c.EditAnchor = "func Caller() int { return " + callee + "() }"
		c.EditTemplate = "\nfunc {{marker}}() int { return " + callee + "() }\n"
		root := filepath.Join(p.Root, c.Label)
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		originals[i] = fmt.Sprintf("package sample\n// sentinel-%d\nfunc %s() int { return %d }\n%s\n", i, callee, i, c.EditAnchor)
		if err := os.WriteFile(filepath.Join(root, c.File), []byte(originals[i]), 0600); err != nil {
			t.Fatal(err)
		}
		p.Repos = append(p.Repos, privateRepo{Path: root, File: filepath.Join(root, c.File), Prefix: "fixture", Corpus: c})
		plan, err := makeEditProbeTrafficPlan(c, 42, 100, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range plan.Steps {
			if step.Editor == i {
				steps[i] = step
				break
			}
		}
		replies[root] = editProbeDiskStub(t, c, root)
	}
	var mu sync.Mutex
	receipts := make(map[string]string)
	p.Socket = startStub(t, func(frame stubFrame) (any, *rpcError) {
		repo := -1
		for i, r := range p.Repos {
			if frame.Cwd == r.Path {
				repo = i
				break
			}
		}
		if repo < 0 {
			return nil, &rpcError{Code: -32000, Message: "unknown checkout"}
		}
		args, _ := frame.Params["arguments"].(map[string]any)
		options, _ := args["options"].(map[string]any)
		if frame.Params["name"] == "edit" && args["operation"] == "file" {
			target, _ := args["target"].(map[string]any)
			if target["file"] != "fixture/"+p.Repos[repo].Corpus.File {
				return nil, &rpcError{Code: -32000, Message: "foreign file"}
			}
			data, err := os.ReadFile(p.Repos[repo].File)
			if err != nil {
				t.Error(err)
				return nil, &rpcError{Code: -32000, Message: "read failed"}
			}
			match, _ := args["match"].(string)
			replacement, _ := args["replacement"].(string)
			if match == "" || strings.Count(string(data), match) != 1 {
				return nil, &rpcError{Code: -32000, Message: "exact match missing"}
			}
			if err := os.WriteFile(p.Repos[repo].File, []byte(strings.Replace(string(data), match, replacement, 1)), 0600); err != nil {
				t.Error(err)
				return nil, &rpcError{Code: -32000, Message: "write failed"}
			}
			mutation, _ := options["mutation_id"].(string)
			receipt := fmt.Sprintf("checkout-%d-%s", repo, mutation)
			mu.Lock()
			receipts[frame.Cwd+"|"+receipt] = mutation
			mu.Unlock()
			return stubContent(map[string]any{"mutation_receipt": receipt}), nil
		}
		if frame.Params["name"] == "change" && args["operation"] == "receipt" {
			receipt, _ := options["receipt"].(string)
			mu.Lock()
			mutation := receipts[frame.Cwd+"|"+receipt]
			mu.Unlock()
			if mutation == "" {
				return nil, &rpcError{Code: -32000, Message: "receipt belongs to another checkout"}
			}
			return stubContent(map[string]any{"graph_status_terminal": true, "graph_status": "fresh", "mutation_id": mutation}), nil
		}
		return replies[frame.Cwd](frame)
	})
	phase := &Phase{Metrics: map[string]Metric{}}
	var clients []*socketClient
	t.Cleanup(func() { closeClients(clients) })
	for _, repo := range p.Repos {
		client, err := openMCP(ctx, p.Socket, repo.Path)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}
	run, probers, err := prepareEditProbe(ctx, runOptions{Seed: 42, EditCount: 100, Editors: 2, Deadline: time.Second, editProbeBaseline: true}, p, nil, clients, phase)
	clients = append(clients, probers...)
	if run != nil {
		t.Cleanup(func() { _ = run.close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	run.origin = time.Now()
	selected := make([]*editProbeState, 2)
	for i, step := range steps {
		state := run.states[step.Ordinal]
		// Corpus-specific requests are independently pinned by the planner
		// test. This diagnostic selects one real ordinal per checkout.
		state.step, state.record = step, newEditProbeRecord(p.Repos[i].Corpus, step)
		state.record.Errors = nil
		selected[i] = state
	}
	phase.EditProbe.Records = nil
	for i, state := range selected {
		raw, _, err := run.perform(ctx, state, run.editorFor(state.step), state.step.Request, "edit", "workload", 0)
		if err != nil {
			t.Fatal(err)
		}
		body, err := decodeEditProbeResult(raw)
		if err != nil {
			t.Fatal(err)
		}
		receipt, _ := body["mutation_receipt"].(string)
		status := request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": receipt}}}
		raw, _, err = run.perform(ctx, state, state.client, status, "status", "probe", 1)
		if err != nil {
			t.Fatal(err)
		}
		body, err = decodeEditProbeResult(raw)
		if err != nil || body["mutation_id"] != state.step.MutationID || body["graph_status_terminal"] != true {
			t.Fatalf("own receipt missing: %v %v", body, err)
		}
		state.record.Receipt, state.record.GraphTerminal = receipt, true
		_, correct := run.immediateReadbacks(ctx, state, state.step, []string{"file", "source"}, nil)
		if !correct {
			t.Errorf("checkout%d same-session readbacks used another fixture/callee: %+v", i, state.record.Readbacks)
		}
		foreign := selected[1-i].client
		if _, _, err := run.perform(ctx, state, foreign, status, "status", "probe", 2); err == nil {
			t.Error("foreign checkout session borrowed a receipt")
		}
		phase.EditProbe.Records = append(phase.EditProbe.Records, state.record)
	}
	foreignReceipt := request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": selected[1].record.Receipt}}}
	if _, _, err := run.perform(ctx, selected[0], selected[0].client, foreignReceipt, "status", "probe", 3); err == nil {
		t.Fatal("same-prefix foreign receipt borrowed another checkout's mutation")
	}
	// A completed sibling cannot justify rewriting an unknown/pending route.
	beforeRestore := make([][]byte, len(p.Repos))
	for i, repo := range p.Repos {
		beforeRestore[i], err = os.ReadFile(repo.File)
		if err != nil {
			t.Fatal(err)
		}
	}
	phase.EditProbe.Records[1].GraphTerminal = false
	if err := run.restore(ctx); err == nil {
		t.Fatal("pending route permitted unsafe cleanup")
	}
	for i, repo := range p.Repos {
		actual, err := os.ReadFile(repo.File)
		if err != nil || string(actual) != string(beforeRestore[i]) {
			t.Fatalf("pending cleanup changed checkout%d: %v", i, err)
		}
	}
	phase.EditProbe.Records[1].GraphTerminal = true
	if err := run.restore(ctx); err != nil {
		t.Fatal(err)
	}
	var restores []struct {
		Route  string `json:"route"`
		Editor int    `json:"editor"`
	}
	if err := json.Unmarshal(phase.EditProbe.Restore, &restores); err != nil || len(restores) != 2 {
		t.Fatalf("restore evidence lost a checkout: %s %v", phase.EditProbe.Restore, err)
	}
	for i, row := range restores {
		if row.Route != p.Repos[i].Corpus.Label || row.Editor != i {
			t.Fatalf("restore borrowed another checkout's owner: %+v", row)
		}
	}
	for i, repo := range p.Repos {
		actual, err := os.ReadFile(repo.File)
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != originals[i] {
			t.Errorf("checkout%d original not restored through its own view: %q", i, actual)
		}
	}
}
