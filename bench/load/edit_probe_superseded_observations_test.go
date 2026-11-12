package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/bench/receiptproof"
)

func TestEditProbeSupersededFinishKeepsPreACKObservations(t *testing.T) {
	for _, mode := range []string{"proven", "stale_after_ACK", "immediate_read_error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			corpus := editProbeCorpus()
			plan, err := makeEditProbePlan(corpus, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			plan.GateModes = true
			step := plan.Steps[0]
			root := t.TempDir()
			file := filepath.Join(root, corpus.File)
			if err := os.WriteFile(file, []byte("package sample\nfunc Example() int { return 1 }\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			disk := editProbeDiskStub(t, corpus, root)
			var failReadback atomic.Bool
			run := newEditProbeStubRun(t, ctx, corpus, plan, root, 1, func(frame stubFrame) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				if failReadback.Load() && frame.Params["name"] == "read" && args["operation"] == "file" {
					return nil, &rpcError{Code: -32000, Message: "readback failed"}
				}
				return disk(frame)
			})
			run.p.Root = root
			state := run.states[0]
			record, proof := supersededRecordFixture(t)
			record.Ordinal, record.Editor = step.Ordinal, step.Editor
			record.Readbacks = nil
			for name := range record.FirstCorrect {
				record.FirstCorrect[name] = unavailable("ms", "not observed yet")
			}
			state.record = record
			state.started = time.Now()
			oracles := editProbeOracles(corpus, step)
			observe := func(oracle editProbeOracle) {
				t.Helper()
				raw, identity, callErr := run.perform(ctx, state, state.client, oracle.call, "oracle:"+oracle.name, "probe", 1)
				if err := run.observeProbe(state, oracle, identity.WriteStarted, raw, identity, callErr, false); err != nil {
					t.Fatal(err)
				}
			}
			observe(oracles[0]) // The new symbol does not exist before its edit.
			wrong, err := json.Marshal(stubContent(map[string]any{"freshness": "fresh", "edges": []any{}}))
			if err != nil {
				t.Fatal(err)
			}
			identity := wireCallIdentity{WriteStarted: time.Now(), Finished: time.Now()}
			if err := run.observeProbe(state, oracles[5], identity.WriteStarted, wrong, identity, nil, false); err != nil {
				t.Fatal(err)
			}
			if state.record.ProbeErrors != 1 || state.record.PrecommitFreshMismatches != 1 || state.record.StaleClaimedFresh {
				t.Fatalf("pre-ACK observations lost or made stale: %+v", state.record)
			}
			state.acknowledged = time.Now()
			content := "package sample\nfunc Example() int { return 1 }\nfunc " + step.Marker + "() int { return Example() }\n"
			if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, oracle := range oracles {
				observe(oracle)
				if state.record.FirstCorrect[oracle.name].Value == nil {
					t.Fatalf("actual final oracle missing: %s", oracle.name)
				}
			}
			if len(oracles) != 7 {
				t.Fatalf("first-correct denominator=%d", len(oracles))
			}
			failReadback.Store(mode == "immediate_read_error")
			_, correct := run.immediateReadbacks(ctx, state, step, []string{"source", "file", "summary", "search_visible", "text"}, nil)
			if correct == failReadback.Load() || len(state.record.Readbacks) != 5 {
				t.Fatalf("actual readbacks correct=%v count=%d", correct, len(state.record.Readbacks))
			}
			if mode == "stale_after_ACK" {
				identity = wireCallIdentity{WriteStarted: time.Now(), Finished: time.Now()}
				if err := run.observeProbe(state, oracles[5], identity.WriteStarted, wrong, identity, nil, false); err != nil {
					t.Fatal(err)
				}
				if !state.record.StaleClaimedFresh {
					t.Fatal("actual post-ACK false-fresh observation was not retained")
				}
			}
			status := supersededStatusFixture()
			raw, err := json.Marshal(stubContent(status))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeEditProbeResult(raw)
			if err != nil {
				t.Fatal(err)
			}
			state.record.Accounting = checkEditProbeAccounting(decoded, record.Receipt)
			state.record.Errors[0] = "A/P/B accounting unavailable: " + strings.Join(state.record.Accounting.Reasons, "; ")
			state.record.supersededStatus = supersededStatus(decoded, raw, 34)
			state.record.Outcome = "diagnostic_failure"
			if state.record.supersededStatus == nil || len(state.record.Errors) != 1 && mode == "proven" {
				t.Fatal("exact status or sole accounting error missing")
			}
			proof.Guard.Repo = "fixture"
			guard, err := json.Marshal(proof.Guard)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "daemon-1.stderr.log"), append(guard, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			run.phase.EditProbe.PhysicalWriteScope = &editProbePhysicalWriteScope{Kind: "shared_worktree_file", File: corpus.File, Editors: 4}
			err = run.finish()
			got := run.phase.EditProbe.Records[0]
			if got.ProbeErrors != 1 || got.PrecommitFreshMismatches != 1 {
				t.Fatal("finish erased observation counts")
			}
			if mode == "proven" {
				if err != nil || got.Outcome != receiptproof.SupersededOutcome || got.Superseded == nil {
					t.Fatalf("proven receipt rejected because of retained pre-ACK observations: %v", err)
				}
				if got.Accounting.P != nil || got.Accounting.B != nil || got.Superseded.AccountingError != record.Errors[0] || len(got.Accounting.Reasons) == 0 {
					t.Fatal("unavailable timing/refusal was erased")
				}
			} else if err == nil || got.Superseded != nil || got.Outcome == receiptproof.SupersededOutcome {
				t.Fatal("actual final-read failure was classified")
			}
			if mode == "immediate_read_error" && (got.Readbacks[1].Correct || got.Readbacks[1].Error == "" || !strings.Contains(strings.Join(got.Errors, ";"), "immediate file readback incorrect")) {
				t.Fatal("actual failed immediate readback lost its error")
			}
		})
	}
}
