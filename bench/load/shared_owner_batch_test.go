package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This opt-in fixture runs one real owned SQLite daemon over two fresh files.
// It checks the actual transport/domain/membership boundary, not latency or a
// corpus/campaign acceptance population. No retained wire can satisfy it.
func TestSharedOwnerNativeBatchUsesActualJournalMembers(t *testing.T) {
	binary := os.Getenv("GORTEX_SHARED_OWNER_NATIVE_BINARY")
	if binary == "" {
		t.Skip("requires an explicitly pinned native shared-owner producer")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("native fixture requires an absolute pinned executable")
	}
	evidence := os.Getenv("GORTEX_SHARED_OWNER_BATCH_EVIDENCE")
	if !filepath.IsAbs(evidence) {
		t.Fatal("native fixture requires a fresh absolute evidence directory")
	}
	if err := os.Mkdir(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := func(name string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(evidence, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Use the operator's external, short TMPDIR for the actual socket too.
	socketDir, err := os.MkdirTemp(os.TempDir(), "shared-socket-")
	if err != nil {
		t.Fatal(err)
	}
	p := &privateRun{Root: root, Binary: binary, SocketDir: socketDir, Socket: filepath.Join(socketDir, "d.sock"), Store: filepath.Join(root, "store.db"), Config: filepath.Join(root, "config.yaml"), editACKMode: "local"}
	ownedPID := 0
	t.Cleanup(func() {
		stop, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		_, forced, stopErr := p.stop(stop)
		if forced || stopErr != nil {
			t.Errorf("native fixture normal stop: forced=%t error=%v", forced, stopErr)
		}
		closure, err := json.Marshal(map[string]any{"owned_daemon_pid": ownedPID, "forced_daemon_stop": forced, "normal_stop_error": stopErr != nil, "runtime_root": root, "socket": p.Socket})
		if err != nil {
			t.Error(err)
		} else {
			keep("owned-closure.json", closure)
		}
		for _, name := range []string{"daemon-1.stdout.log", "daemon-1.stderr.log"} {
			if raw, readErr := os.ReadFile(filepath.Join(root, name)); readErr == nil {
				keep(name, raw)
			}
		}
		if _, err := os.Lstat(p.Socket); !os.IsNotExist(err) {
			t.Errorf("owned socket remains after normal stop: %v", err)
		}
		if err := os.RemoveAll(socketDir); err != nil {
			t.Error(err)
		}
	})
	for _, dir := range []string{"home", "cache", "config", "data", "runtime", "tmp", "repos", "hooks"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gitConfig := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(p.Config, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p.Env = privateEnv(root, p.Socket, gitConfig)
	repo := filepath.Join(root, "repos", "fixture")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"go.mod": "module sharedfixture\n\ngo 1.27\n",
		"a.go":   "package sharedfixture\n\nfunc Moved() int { return 1 }\n",
		"b.go":   "package sharedfixture\n\nfunc Keeper() int { return 2 }\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.git(ctx, repo, "init", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.git(ctx, repo, "add", "go.mod", "a.go", "b.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.git(ctx, repo, "-c", "user.name=SharedFixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "initial owned fixture"); err != nil {
		t.Fatal(err)
	}
	p.Repos = []privateRepo{{Path: repo, File: filepath.Join(repo, "a.go"), Corpus: Corpus{Label: "fixture", File: "a.go"}}}
	configured, err := p.writeBootRepos(false)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	ownedPID = p.process.Process.Pid
	gate, _, err := p.awaitReady(ctx, started, configured)
	if err != nil {
		t.Fatal(err)
	}
	keep("readiness.json", gate["readiness"])
	var ready readiness
	if err := json.Unmarshal(gate["readiness"], &ready); err != nil {
		t.Fatal(err)
	}
	for _, tracked := range ready.TrackedRepos {
		if filepath.Clean(tracked.Path) == repo {
			p.Repos[0].Prefix = tracked.Prefix
		}
	}
	if p.Repos[0].Prefix == "" {
		t.Fatal("actual ready primary has no public prefix")
	}
	client, err := openMCP(ctx, p.Socket, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	prober, err := openMCP(ctx, p.Socket, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prober.Close() })
	_, probeIdentity, err := prober.callWithIdentity(ctx, "tools/call", map[string]any{"name": "read", "arguments": map[string]any{"operation": "file", "target": map[string]any{"file": "a.go"}, "output": map[string]any{"format": "json", "max_bytes": 0}}})
	if err != nil {
		t.Fatal(err)
	}
	p.Repos[0].Corpus.EditKind, p.Repos[0].Corpus.EditTool = "append_leaf", "edit.batch"
	prefix := p.Repos[0].Prefix
	before := []string{"package sharedfixture\n\nfunc Moved() int { return 1 }\n", "package sharedfixture\n\nfunc Keeper() int { return 2 }\n"}
	after := []string{before[0] + "func BatchAddedA() int { return 3 }\n", before[1] + "func BatchAddedB() int { return 4 }\n"}
	changes := []any{}
	for i, name := range []string{"a.go", "b.go"} {
		changes = append(changes, map[string]any{"op": "edit_file", "path": filepath.Join(repo, name), "old_string": before[i], "new_string": after[i]})
	}
	transaction := "actual-two-file-journal-batch"
	raw, identity, err := client.callWithIdentity(ctx, "tools/call", map[string]any{"name": "edit", "arguments": map[string]any{"operation": "batch", "changes": changes, "options": map[string]any{"transaction_id": transaction}, "output": map[string]any{"format": "json", "max_bytes": 0}}})
	keep("batch-ack.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	body, err := decodeEditProbeResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	offered := request{Tool: "edit", Operation: "batch", Arguments: map[string]any{"changes": changes, "options": map[string]any{"transaction_id": transaction}}}
	intentID, intents, err := editProbeBatchSetIntent(offered, repo, prefix)
	if err != nil || intentID != transaction {
		t.Fatalf("actual offered frontier: %v", err)
	}
	refs, err := decodeEditProbeBatchSetAcknowledgement(body, transaction, intents, strconv.FormatInt(identity.ID, 10))
	if err != nil {
		t.Fatalf("real complete two-file journal batch cannot enter report decoder: %v", err)
	}
	sink, err := os.Create(filepath.Join(evidence, "continuation-raw.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	step := editProbeStep{Request: offered, Marker: "BatchAddedA"}
	state := &editProbeState{step: step, client: prober, sink: sink, record: editProbeRecord{FirstCorrect: map[string]Metric{}}, firstProbeWrite: probeIdentity.WriteStarted, editWrite: identity.WriteStarted}
	state.startRaw()
	defer state.finishRaw()
	runtime := &editProbeRuntime{p: p, phase: &Phase{EditProbe: &editProbeReport{Role: "candidate"}}, editors: []*socketClient{client}, plan: editProbePlan{Tool: "edit.batch", Mode: "json", File: "a.go"}, origin: identity.Started, options: runOptions{Deadline: 5 * time.Second}, requireAccounting: true}
	runtime.finishEditAfterACK(ctx, ctx, state, step, identity.Started, raw, identity, nil)
	if state.record.Outcome != "diagnostic_failure" || state.record.Ack.Value == nil || state.record.BatchSet == nil || state.record.BatchAccounting == nil || state.record.BatchAccounting.State != "observed_batch_shared_owner_windows_checks_pass" || !state.record.GraphTerminal {
		t.Fatalf("actual post-ACK continuation did not dispatch batch set/report: %+v", state.record)
	}
	if state.record.Accounting.State != "terminal_unavailable_shared_owner" || !strings.Contains(strings.Join(state.record.Accounting.Reasons, ";"), "reindex_wait is unobserved with counts 0/0/0") {
		t.Fatalf("missing real A applicability was waived: %+v", state.record.Accounting)
	}
	if len(state.record.Readbacks) != 2 || !state.record.Readbacks[0].Correct || !state.record.Readbacks[1].Correct {
		t.Fatal("actual editing session own-ACK immediate readbacks were not retained")
	}
	rows, ok := body["file_observations"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatal("actual batch lacks two retained member references")
	}
	statuses := map[string]map[string]any{}
	for i, value := range rows {
		row, ok := value.(map[string]any)
		if !ok {
			t.Fatal("malformed actual file reference")
		}
		receipt, ok := row["receipt"].(string)
		if !ok || receipt == "" {
			t.Fatal("actual file reference has no receipt")
		}
		for {
			raw, err := client.call(ctx, "tools/call", map[string]any{"name": "change", "arguments": map[string]any{"operation": "receipt", "options": map[string]any{"receipt": receipt}, "output": map[string]any{"format": "json", "max_bytes": 0}}})
			keep([]string{"batch-member-a.json", "batch-member-b.json"}[i], raw)
			if err != nil {
				t.Fatal(err)
			}
			status, err := decodeEditProbeResult(raw)
			if err != nil {
				t.Fatal(err)
			}
			availability, _ := status["shared_owner_accounting_v1_availability"].(map[string]any)
			if availability["state"] == "frozen" {
				if status["receipt_kind"] != "batch_file_observation" || status["transaction_id"] != transaction || status["acknowledgement_state"] != "not_applicable_shared_transaction_rpc" {
					t.Fatal("actual batch member lost journal/ACK domain")
				}
				statuses[receipt] = status
				break
			}
			if availability["state"] == "unavailable" || ctx.Err() != nil {
				t.Fatalf("actual batch owner unavailable: %v / %v", availability, ctx.Err())
			}
			if err := waitContext(ctx, 25*time.Millisecond); err != nil {
				t.Fatal(err)
			}
		}
	}
	ackRaw, err := client.call(ctx, "tools/call", map[string]any{"name": "change", "arguments": map[string]any{"operation": "receipt", "options": map[string]any{"receipt": refs.Acknowledgement}, "output": map[string]any{"format": "json", "max_bytes": 0}}})
	keep("batch-shared-ack-status.json", ackRaw)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := decodeEditProbeResult(ackRaw)
	if err != nil {
		t.Fatal(err)
	}
	accounting, batch := runtime.accountingForBatchSetStatuses(ctx, statuses, ack, refs)
	if accounting.State != "terminal_unavailable_shared_owner" || accounting.A == nil || accounting.P != nil || accounting.B != nil || accounting.Shared != nil || batch.State != "observed_batch_shared_owner_windows_checks_pass" || len(batch.Files) != 2 {
		t.Fatalf("actual batch report unavailable or invented per-file windows: %+v %+v", accounting, batch)
	}
	for _, file := range batch.Files {
		if file.Shared == nil || file.AcknowledgementState != "not_applicable_shared_transaction_rpc" {
			t.Fatal("actual member lost exact owner or ACKNA")
		}
	}
	encoded, err := json.MarshalIndent(editProbeRecord{Accounting: accounting, BatchSet: &refs, BatchAccounting: batch}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	keep("normalized-batch-accounting.json", encoded)
	for _, fault := range []string{"missing_member", "foreign_path", "replaced_ticket", "missing_membership", "wrong_hash", "foreign_process", "unknown_version", "ordinary_domain", "wrong_rpc"} {
		t.Run(fault, func(t *testing.T) {
			rows := map[string]map[string]any{}
			for key, row := range statuses {
				rows[key] = cloneSharedOwnerModel(t, row)
			}
			ackCopy := cloneSharedOwnerModel(t, ack)
			row := rows[refs.Files[1].Observation]
			proof := row["shared_owner_accounting_v1"].(map[string]any)
			switch fault {
			case "missing_member":
				delete(rows, refs.Files[1].Observation)
			case "foreign_path":
				row["path"] = "foreign/a.go"
			case "replaced_ticket":
				row["publication_generation"] = json.Number("99999")
			case "missing_membership":
				delete(proof, "membership")
			case "wrong_hash":
				row["after_sha256"] = "foreign"
			case "foreign_process":
				proof["definition"].(map[string]any)["owner"].(map[string]any)["process"].(map[string]any)["pid"] = json.Number("1")
			case "unknown_version":
				row["shared_owner_accounting_v2"] = map[string]any{}
			case "ordinary_domain":
				row["disk_status"] = "committed"
			case "wrong_rpc":
				ackCopy["acknowledgement"].(map[string]any)["rpc_id"] = "foreign"
			}
			fresh := &editProbeRuntime{p: p, phase: &Phase{EditProbe: &editProbeReport{Role: "candidate"}}}
			got, batch := fresh.accountingForBatchSetStatuses(ctx, rows, ackCopy, refs)
			if got.State == "observed_shared_owner_windows_checks_pass" || batch.State == "observed_batch_shared_owner_windows_checks_pass" || len(got.Reasons) == 0 || got.P != nil || got.B != nil {
				t.Fatalf("%s acquired false batch evidence: %+v %+v", fault, got, batch)
			}
			if fault != "wrong_rpc" && got.A == nil {
				t.Fatal("missing batch membership erased independently valid transaction A")
			}
		})
	}
	for _, fault := range []string{"missing_file", "duplicate_observation", "foreign_after", "foreign_result", "missing_fingerprint", "unsupported_operation"} {
		t.Run(fault, func(t *testing.T) {
			bad := cloneSharedOwnerModel(t, body)
			switch fault {
			case "missing_file":
				bad["files"] = bad["files"].([]any)[:1]
			case "duplicate_observation":
				bad["file_observations"].([]any)[1].(map[string]any)["receipt"] = refs.Files[0].Observation
			case "foreign_after":
				bad["files"].([]any)[1].(map[string]any)["after_sha256"] = refs.Files[0].AfterSHA256
			case "foreign_result":
				bad["results"].([]any)[1].(map[string]any)["path"] = "foreign/b.go"
			case "missing_fingerprint":
				delete(bad, "fingerprint")
			case "unsupported_operation":
				bad["results"].([]any)[1].(map[string]any)["op"] = "delete_file"
			}
			decoded, err := decodeEditProbeBatchSetAcknowledgement(bad, transaction, intents, refs.WireID)
			if fault == "duplicate_observation" {
				if err != nil || decoded.validateReferences() == nil {
					t.Fatal("missing timing must preserve committed source ACK but remain unavailable")
				}
			} else if err == nil {
				t.Fatal("invalid complete journal frontier was accepted")
			}
		})
	}
	report := runtime.finishSharedOwners()
	if report == nil || report.DistinctOwners != 1 || report.CompleteOwnerWindows != 1 || report.Execution.Count != 1 || report.Background.Count != 1 {
		t.Fatal("real two-file batch did not retain exactly one complete report owner")
	}
	normalized, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	keep("normalized-batch-owner-report.json", normalized)
	for i, name := range []string{"a.go", "b.go"} {
		data, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil || string(data) != after[i] {
			t.Fatalf("actual batch bytes %s: %v", name, err)
		}
	}
	t.Log("actual two-file journal batch report component only; no benchmark/latency/coverage closure")
}

// The reporting slice retains all offered after-images, not all original
// baselines. Until those are owned, refusal must precede EVERY restore write.
func TestBatchSetRestoreRefusesBeforeAnyPartialWrite(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("mutated-"+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var writes atomic.Int64
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Params["name"] == "edit" {
			writes.Add(1)
		}
		return stubContent(map[string]any{"status": "committed"}), nil
	})
	client, err := openMCP(t.Context(), socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	run := &editProbeRuntime{p: &privateRun{Repos: []privateRepo{{Path: root, Prefix: "fixture"}}}, editors: []*socketClient{client}, plan: editProbePlan{Tool: "edit.batch", File: "a.go"}, original: []byte("before-a"), phase: &Phase{EditProbe: &editProbeReport{Records: []editProbeRecord{{BatchSet: &editProbeBatchSetReferences{Files: []editProbeBatchReferences{{Path: filepath.Join(root, "a.go")}, {Path: filepath.Join(root, "b.go")}}}, GraphTerminal: true}}}}}
	if err := run.restore(t.Context()); err == nil || !strings.Contains(err.Error(), "complete original baselines") {
		t.Fatalf("partial restore was not refused: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("unsupported multi-file restore issued a mutation")
	}
	for _, name := range []string{"a.go", "b.go"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(data) != "mutated-"+name {
			t.Fatalf("refusal changed %s: %v", name, err)
		}
	}
}
