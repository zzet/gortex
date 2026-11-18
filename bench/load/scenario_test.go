package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublicEditAcknowledgementAndIndexedVisibility(t *testing.T) {
	var mu sync.Mutex
	var marker string
	editCalls := 0
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		mu.Lock()
		defer mu.Unlock()
		args, _ := frame.Params["arguments"].(map[string]any)
		if frame.Params["name"] == "edit" {
			editCalls++
			if args["operation"] != "file" || args["match"] != "package sample\n" {
				return nil, &rpcError{Code: -32602, Message: "invalid edit"}
			}
			marker = "GortexLoadMarkerA1S2"
			if !strings.Contains(args["replacement"].(string), marker) {
				return nil, &rpcError{Code: -32602, Message: "missing marker"}
			}
			return stubContent(map[string]any{"mutation_id": "receipt", "status": "applied"}), nil
		}
		return stubContent(map[string]any{"results": []any{map[string]any{"name": marker, "id": "sample/sample.go::" + marker}}}), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	c := sampleCorpus()
	c.EditAnchor = "package sample\n"
	c.EditTemplate = "\nfunc {{marker}}() {}\n"
	observation, rows := editAndObserve(ctx, runOptions{Deadline: time.Second}, client, privateRepo{Corpus: c, Prefix: "sample"}, 1, 2)
	if observation.Outcome != "visible" || observation.VisibilityMS.Value == nil || len(observation.Acknowledgement) == 0 || len(rows) != 2 || rows[0].Tool != "edit.file" || rows[1].Role != "probe" {
		t.Fatalf("edit/readback: %+v %+v", observation, rows)
	}
	mu.Lock()
	defer mu.Unlock()
	if editCalls != 1 {
		t.Fatalf("public edit calls=%d", editCalls)
	}
}

func TestVisibilityRequiresActualIndexedIdentity(t *testing.T) {
	for _, data := range []string{`{"query":"Marker"}`, `{"source":"func Marker() {}"}`, `{"name":"Other"}`, `{"name":"Marker"}`, `{"id":"ext::Marker","name":"Marker"}`} {
		if containsIdentity(json.RawMessage(data), "Marker") {
			t.Fatalf("non-indexed identity passed: %s", data)
		}
	}
	if !containsIdentity(json.RawMessage(`{"results":[{"id":"sample/a.go::Marker","name":"Marker"}]}`), "Marker") {
		t.Fatal("indexed result rejected")
	}
}

func TestRunOptionsFloorAndExplicitSaturation(t *testing.T) {
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir()}
	options, err := parseRun(base)
	if err != nil || options.Profile != "agent" || options.SweepRepetitions != 15 || options.SweepBudget != 10*time.Minute {
		t.Fatalf("defaults: %+v %v", options, err)
	}
	if _, err := parseRun(append(base, "--profile", "saturation")); err != nil {
		t.Fatal(err)
	}
	if _, err := parseRun(append(base, "--host", "floor", "--scenario", "read")); err != nil {
		t.Fatalf("floor should count active readers only: %v", err)
	}
	for _, extra := range [][]string{{"--host", "floor", "--scenario", "edit"}, {"--host", "floor", "--scenario", "many"}, {"--host", "floor", "--scenario", "worktrees"}, {"--host", "reference", "--keep-state"}, {"--scenario", "unknown"}} {
		if _, err := parseRun(append(base, extra...)); err == nil {
			t.Fatalf("invalid options accepted: %v", extra)
		}
	}
}

func TestStateCleanupPreservesEvidence(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"store.db", "store.db-wal", "store.db-shm", "daemon.log", "read.cpu.pprof", "config.yaml"} {
		if err := os.WriteFile(root+string(os.PathSeparator)+name, []byte("evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := privateRun{Root: root}
	if err := p.removeState(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"store.db", "store.db-wal", "store.db-shm"} {
		if _, err := os.Stat(root + string(os.PathSeparator) + name); !os.IsNotExist(err) {
			t.Fatalf("state retained %s: %v", name, err)
		}
	}
	for _, name := range []string{"daemon.log", "read.cpu.pprof", "config.yaml"} {
		if _, err := os.Stat(root + string(os.PathSeparator) + name); err != nil {
			t.Fatalf("evidence removed %s: %v", name, err)
		}
	}
}

func TestEditorOnlyAndMixedCountPolicies(t *testing.T) {
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir()}
	for _, scenario := range []string{"edit", "many-edit"} {
		for _, count := range []string{"1", "4", "8"} {
			options, err := parseRun(append(base, "--scenario", scenario, "--agents", "0", "--edit-agents", count))
			if err != nil || options.Agents != 0 || options.Editors < 1 {
				t.Fatalf("editor-only %s/%s: %+v %v", scenario, count, options, err)
			}
		}
	}
	for _, extra := range [][]string{
		{"--scenario", "edit", "--agents", "0", "--edit-agents", "0"},
		{"--scenario", "edit", "--agents", "4", "--edit-agents", "0"},
		{"--scenario", "many-edit", "--agents", "0", "--edit-agents", "0"},
		{"--scenario", "read", "--agents", "0"},
		{"--scenario", "many", "--agents", "0"},
		{"--scenario", "worktrees", "--agents", "0"},
		{"--scenario", "sweep", "--agents", "0"},
		{"--scenario", "cold", "--agents", "0"},
		{"--scenario", "restart", "--agents", "0"},
		{"--scenario", "all", "--agents", "0"},
		{"--scenario", "all", "--edit-agents", "0"},
		{"--agents", "-1"}, {"--edit-agents", "-1"},
		{"--host", "floor", "--scenario", "edit", "--agents", "0", "--edit-agents", "8"},
		{"--host", "floor", "--scenario", "edit", "--agents", "1", "--edit-agents", "4"},
	} {
		if _, err := parseRun(append(base, extra...)); err == nil {
			t.Fatalf("invalid population accepted: %v", extra)
		}
	}
	for _, extra := range [][]string{
		{"--scenario", "read", "--edit-agents", "0"},
		{"--scenario", "edit", "--agents", "4", "--edit-agents", "4"},
		{"--host", "floor", "--scenario", "edit", "--agents", "0", "--edit-agents", "4"},
		{"--host", "floor", "--scenario", "edit", "--agents", "2", "--edit-agents", "2"},
	} {
		if _, err := parseRun(append(base, extra...)); err != nil {
			t.Fatalf("valid population rejected: %v: %v", extra, err)
		}
	}
}

func TestEditorOnlyStandaloneAndPairedExecuteExactPopulations(t *testing.T) {
	for _, paired := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "paired"}[paired], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			options := runOptions{Profile: "agent", Agents: 0, Editors: 2, Duration: 9 * time.Second, Deadline: time.Second}
			labels := []string{"standalone"}
			if paired {
				labels = []string{"tag", "main"}
			}
			var mu sync.Mutex
			edited, read := make(map[string]bool), make(map[string]bool)
			states := make([]*pairedState, len(labels))
			for i, label := range labels {
				socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
					args, _ := frame.Params["arguments"].(map[string]any)
					key := label + ":" + strconv.Itoa(frame.Connection)
					mu.Lock()
					defer mu.Unlock()
					if frame.Params["name"] == "edit" {
						edited[key] = true
						return stubContent(map[string]any{"status": "applied"}), nil
					}
					if query, ok := args["query"].(string); ok && strings.HasPrefix(query, "GortexLoadMarker") {
						return stubContent(map[string]any{"results": []any{map[string]any{"id": "sample/sample.go::" + query, "name": query}}}), nil
					}
					if !edited[key] {
						t.Errorf("unexpected pure reader in editor-only population: %s", key)
					}
					read[key] = true
					if len(read) == len(labels)*options.Editors {
						cancel()
					}
					return stubContent(map[string]any{"ok": true}), nil
				})
				corpus := sampleCorpus()
				corpus.EditAnchor, corpus.EditTemplate = "package sample\n", "\nfunc {{marker}}() {}\n"
				state := &pairedState{p: &privateRun{Repos: []privateRepo{{Corpus: corpus, Prefix: "sample"}}}, phase: &Phase{Name: "edit"}}
				for range options.Editors {
					client, err := openMCP(ctx, socket, "")
					if err != nil {
						t.Fatal(err)
					}
					defer client.Close()
					state.clients = append(state.clients, client)
				}
				states[i] = state
			}
			if paired {
				coordinator, err := newPairedCoordinator(ctx, options, labels, "")
				if err != nil {
					t.Fatal(err)
				}
				defer coordinator.cancel()
				coordinator.states = states
				_ = coordinator.agents(ctx)
			} else {
				_ = runAgents(ctx, options, states[0].p, states[0].clients, true, states[0].phase)
			}
			mu.Lock()
			defer mu.Unlock()
			if ctx.Err() != context.Canceled || len(edited) != len(labels)*options.Editors || len(read) != len(edited) {
				t.Fatalf("not every combined agent edited then read: edits=%v reads=%v cause=%v", edited, read, ctx.Err())
			}
		})
	}
}
