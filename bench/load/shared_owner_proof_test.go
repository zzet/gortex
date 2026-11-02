package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
)

func sharedOwnerProofModel(t *testing.T) (*privateRun, *editProbeSharedBinding, map[string]any) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	statePath := filepath.Join(root, "cache", "gortex", "daemon.state.json")
	for _, dir := range []string{repo, filepath.Dir(statePath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestSharedOwnerBindingChildProcess$", "-test.timeout=20s")
	configureOwnedProcess(cmd)
	cmd.Env = append(privateEnv(root, filepath.Join(root, "unused.sock"), filepath.Join(root, "gitconfig")), "GORTEX_SHARED_OWNER_BINDING_CHILD=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var exitErr error
	go func() { exitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = input.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned model child did not stop")
			_ = killOwnedProcess(cmd)
			<-done
		}
		if exitErr != nil {
			t.Errorf("owned model child: %v", exitErr)
		}
	})
	native, err := platform.CaptureProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	birth, err := native.Token()
	if err != nil {
		t.Fatal(err)
	}
	p := &privateRun{Root: root, Binary: exe, Store: filepath.Join(root, "owned.db"), process: cmd, done: done, startCount: 1, Repos: []privateRepo{{Path: repo, Prefix: "primary"}}}
	state, err := json.Marshal(daemon.RuntimeState{PID: cmd.Process.Pid, ProcessBirthToken: birth, BackendPath: p.Store, StartupPhase: daemon.StartupServing})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	binding, err := captureEditProbeSharedBinding(t.Context(), p, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	member := func(name string, ordinal int) map[string]any {
		return map[string]any{"receipt": "commit-" + name, "publication_key": "publication-" + name, "path": filepath.Join(repo, name+".go"), "ticket": ordinal, "physical_commit_sequence": ordinal + 6, "content_sha256": strings.Repeat("a", 64), "checkout_id": "", "checkout_incarnation": ""}
	}
	owner := map[string]any{"process": map[string]any{"pid": cmd.Process.Pid, "birth_token": birth}, "server_instance": "model-server", "recorder_instance": "model-recorder", "checkout": map[string]any{"kind": "watcher_primary", "root": repo, "id": "", "incarnation": "", "repo_prefix": "primary"}, "anchor_publication_key": "publication-a", "anchor_ticket": 1, "retry": 0, "invocation_anchor_key": "publication-a", "invocation_anchor_ticket": 1, "arm_frontier": 2}
	background, _, _ := sharedBackgroundFixture()
	background["execution_ref"].(map[string]any)["owner"] = owner
	first, second := member("a", 1), member("b", 2)
	definition := map[string]any{"scope": "shared_mutation_set_executor", "owner": owner, "frontier_count": 2, "members": []any{first, second}, "execution": sharedExecutionFixture(), "background": background, "background_unavailable_reason": nil}
	witness := map[string]any{"owner": owner, "member": first, "applied_generation": 1, "same_status_projection": true, "live_server_ledger_member": true, "live_recorder_member": true, "live_anchor": true, "full_membership_retained": true, "original_invocation_verified": true, "publication_terminal_success": true}
	status := map[string]any{"found": true, "receipt": "commit-a", "path": "a.go", "disk_status": "committed", "graph_status": "fresh", "graph_status_terminal": true, "reindex_generation": 1, "applied_generation": 1, "commit_seq": 7, "content_sha256": strings.Repeat("a", 64), "publication_phases": map[string]any{"key": "publication-a", "ticket": 1, "source": "mcp_edit", "clock": "monotonic", "terminal": true}, "phase_timing": map[string]any{"patch_wall_ms": nil}, "shared_owner_accounting_v1_availability": map[string]any{"state": "frozen", "reason": nil}, "shared_owner_accounting_v1": map[string]any{"reference": map[string]any{"owner": owner, "member": first}, "definition": definition, "membership": witness}}
	return p, binding, cloneSharedOwnerModel(t, status)
}

func cloneSharedOwnerModel(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// These are typed-wire models over a real owned child binding. They do not
// claim that this child ran a Server or generated the model's witness flags.
// The producer's real ledger/C1 tests and an owned transport proof are separate.
func TestSharedOwnerProofRequiresFullSameStatusMembershipAndDomain(t *testing.T) {
	p, binding, original := sharedOwnerProofModel(t)
	got, err := checkEditProbeSharedProof(original, "commit-a", binding, p, "candidate")
	if err != nil || got.ownerKey == "" || len(got.definition) == 0 || got.execution == nil || got.background == nil || got.execution.WallNS != 11_000_000 || got.background.WallNS != 15_000_000 {
		t.Fatalf("complete typed model rejected: %+v %v", got, err)
	}
	proof := func(v map[string]any) map[string]any { return v["shared_owner_accounting_v1"].(map[string]any) }
	definition := func(v map[string]any) map[string]any { return proof(v)["definition"].(map[string]any) }
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"unknown version", func(v map[string]any) { v["shared_owner_accounting_v2"] = map[string]any{} }},
		{"truncated status", func(v map[string]any) { v["truncated"] = true }},
		{"budget-clipped status", func(v map[string]any) { v["_truncated_by_budget"] = true }},
		{"untyped truncation claim", func(v map[string]any) { v["truncated"] = "false" }},
		{"error status", func(v map[string]any) { v["error"] = map[string]any{"message": "failed"} }},
		{"missing definition", func(v map[string]any) { delete(proof(v), "definition") }},
		{"not frozen", func(v map[string]any) {
			v["shared_owner_accounting_v1_availability"].(map[string]any)["state"] = "pending"
		}},
		{"scope mismatch", func(v map[string]any) { definition(v)["scope"] = "point" }},
		{"foreign process", func(v map[string]any) {
			definition(v)["owner"].(map[string]any)["process"].(map[string]any)["pid"] = json.Number("1")
		}},
		{"foreign birth", func(v map[string]any) {
			definition(v)["owner"].(map[string]any)["process"].(map[string]any)["birth_token"] = "foreign"
		}},
		{"missing constructor name", func(v map[string]any) { definition(v)["owner"].(map[string]any)["server_instance"] = "" }},
		{"wrong root", func(v map[string]any) {
			definition(v)["owner"].(map[string]any)["checkout"].(map[string]any)["root"] = p.Root
		}},
		{"wrong prefix", func(v map[string]any) {
			definition(v)["owner"].(map[string]any)["checkout"].(map[string]any)["repo_prefix"] = "peer"
		}},
		{"retry cannot borrow first attempt", func(v map[string]any) { definition(v)["owner"].(map[string]any)["retry"] = json.Number("1") }},
		{"count cannot certify subset", func(v map[string]any) { definition(v)["frontier_count"] = json.Number("3") }},
		{"duplicate original member", func(v map[string]any) { rows := definition(v)["members"].([]any); rows[1] = rows[0] }},
		{"missing member from full frontier", func(v map[string]any) { definition(v)["members"] = definition(v)["members"].([]any)[:1] }},
		{"foreign reference owner", func(v map[string]any) { proof(v)["reference"].(map[string]any)["owner"] = map[string]any{} }},
		{"foreign membership owner", func(v map[string]any) { proof(v)["membership"].(map[string]any)["owner"] = map[string]any{} }},
		{"evicted peer", func(v map[string]any) { proof(v)["membership"].(map[string]any)["full_membership_retained"] = false }},
		{"noncurrent projection", func(v map[string]any) { proof(v)["membership"].(map[string]any)["same_status_projection"] = false }},
		{"rebound ledger", func(v map[string]any) { proof(v)["membership"].(map[string]any)["live_server_ledger_member"] = false }},
		{"changed physical sequence", func(v map[string]any) { v["commit_seq"] = json.Number("8") }},
		{"changed physical content", func(v map[string]any) { v["content_sha256"] = strings.Repeat("b", 64) }},
		{"wrong public path", func(v map[string]any) { v["path"] = "b.go" }},
		{"foreign checkout", func(v map[string]any) { v["checkout_id"] = "linked" }},
		{"nonphysical observation", func(v map[string]any) { v["receipt_kind"] = "batch_file_observation" }},
		{"coalesced serving ticket", func(v map[string]any) { v["applied_generation"] = json.Number("2") }},
		{"different publication alias", func(v map[string]any) { v["publication_phases"].(map[string]any)["key"] = "later-key" }},
		{"legacy physical numbers mixed in", func(v map[string]any) { v["phase_timing"].(map[string]any)["patch_wall_ms"] = json.Number("11") }},
		{"legacy background mixed in", func(v map[string]any) {
			v["requested_background"] = map[string]any{"background_wall_ms": json.Number("15")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := cloneSharedOwnerModel(t, original)
			tc.change(value)
			if got, err := checkEditProbeSharedProof(value, "commit-a", binding, p, "candidate"); err == nil {
				t.Fatalf("invalid typed owner/domain received window credit: %+v", got)
			}
		})
	}
	partial := cloneSharedOwnerModel(t, original)
	definition(partial)["background"] = nil
	definition(partial)["background_unavailable_reason"] = "overlapping_or_invalid_arm"
	got, err = checkEditProbeSharedProof(partial, "commit-a", binding, p, "candidate")
	if err == nil || got.execution == nil || got.background != nil || got.ownerKey == "" {
		t.Fatalf("E-only frozen definition must remain explicit blocked B: %+v %v", got, err)
	}
	if _, err := checkEditProbeSharedProof(original, "commit-a", binding, p, "baseline"); err == nil {
		t.Fatal("a raw candidate-looking proof crossed the actual report arm")
	}
}
