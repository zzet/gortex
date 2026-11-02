package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in fixture runs one real owned SQLite daemon over two fresh files.
// It checks the actual transport/domain/membership boundary, not latency or a
// corpus/campaign acceptance population. No retained wire can satisfy it.
func TestSharedOwnerNativeMoveUsesActualServerMembership(t *testing.T) {
	binary := os.Getenv("GORTEX_SHARED_OWNER_NATIVE_BINARY")
	if binary == "" {
		t.Skip("requires an explicitly pinned native shared-owner producer")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("native fixture requires an absolute pinned executable")
	}
	evidence := os.Getenv("GORTEX_SHARED_OWNER_TRANSPORT_EVIDENCE")
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
	prefix := p.Repos[0].Prefix
	sourceID, targetID := prefix+"/a.go::Moved", prefix+"/b.go::Moved"
	raw, _, err := client.callWithIdentity(ctx, "tools/call", map[string]any{"name": "refactor", "arguments": map[string]any{"operation": "move", "target": map[string]any{"symbol": sourceID}, "destination": "b.go", "format": "json"}})
	keep("move-ack.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	body, err := decodeEditProbeResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]editProbeMovePath{}
	for role, file := range map[string]string{"source": "a.go", "target": "b.go"} {
		paths[role] = editProbeMovePath{Relative: file, Qualified: prefix + "/" + file, Absolute: filepath.Join(repo, file)}
	}
	refs, err := decodeEditProbeMoveAcknowledgement(body, sourceID, targetID, paths)
	if err != nil || refs.ReferenceError != "" || len(refs.Files) != 2 {
		t.Fatalf("actual two-file move acknowledgement: %+v %v", refs, err)
	}
	binding, err := captureEditProbeSharedBinding(ctx, p, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	var report editProbeSharedOwnerReport
	var references []*editProbeSharedReference
	for _, file := range refs.Files {
		for {
			raw, err := client.call(ctx, "tools/call", map[string]any{"name": "change", "arguments": map[string]any{"operation": "receipt", "options": map[string]any{"receipt": file.Receipt}}})
			keep("status-"+file.Role+".json", raw)
			if err != nil {
				t.Fatal(err)
			}
			row, err := decodeEditProbeResult(raw)
			if err != nil {
				t.Fatal(err)
			}
			child, _ := row["physical_commit"].(map[string]any)
			availability, _ := child["shared_owner_accounting_v1_availability"].(map[string]any)
			if availability["state"] == "frozen" {
				if row["physical_commit_state"] != "observed" || row["physical_commit_shared_acknowledgement"] != true || row["physical_commit_additive_with_acknowledgement"] != false {
					t.Fatal("actual physical child lost its shared/nonadditive parent linkage")
				}
				receipt, _ := child["receipt"].(string)
				proof, proofErr := checkEditProbeSharedProof(child, receipt, binding, p, "candidate")
				ref, err := report.observe(proof, proofErr)
				if err != nil {
					t.Fatalf("actual live Server/member proof rejected: %v", err)
				}
				references = append(references, ref)
				break
			}
			if availability["state"] == "unavailable" || ctx.Err() != nil {
				t.Fatalf("actual shared owner evidence unavailable: %v (%v)", availability, ctx.Err())
			}
			if err := waitContext(ctx, 25*time.Millisecond); err != nil {
				t.Fatal(err)
			}
		}
	}
	report.summarize()
	if len(references) != 2 || references[0].DefinitionKey != references[1].DefinitionKey || references[0].Member == references[1].Member || report.DistinctOwners != 1 || report.CompleteOwnerWindows != 1 || report.Execution.Count != 1 || report.Background.Count != 1 {
		t.Fatal("two actual physical files did not retain distinct membership in one measured owner")
	}
	for file, contains := range map[string]bool{"a.go": false, "b.go": true} {
		raw, err := os.ReadFile(filepath.Join(repo, file))
		if err != nil || strings.Contains(string(raw), "func Moved()") != contains {
			t.Fatalf("actual committed move bytes for %s: %v", file, err)
		}
	}
	normalized, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	keep("normalized-owner-windows.json", normalized)
	t.Log("actual owned SQLite/public-MCP two-file membership passed; no latency/campaign or per-file P/B acceptance claimed")
}
