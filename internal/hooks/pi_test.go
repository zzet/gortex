package hooks

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/localizationauth"
)

func TestHandlePi_SessionStartReturnsOrientation(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "1.0.0", Ready: true,
			TrackedRepos: []daemon.TrackedRepoStatus{{Name: "repo", Path: "/tmp/repo", Workspace: "repo", Nodes: 10}},
			Workspaces:   []daemon.WorkspaceSummary{{Slug: "repo"}},
		}, nil
	})
	d := handlePi([]byte(`{"event":"session_start","cwd":"/tmp/repo"}`), 0, ModeDeny)
	if d.Orientation == "" {
		t.Fatal("expected a non-empty orientation on session_start")
	}
	if d.Block {
		t.Error("session_start must never block")
	}
}

func TestHandlePi_ToolCallBlocksTrackedGreedyGlob(t *testing.T) {
	stubTrackedScope(t, true)

	d := handlePi([]byte(`{"event":"tool_call","tool_name":"Glob","tool_input":{"pattern":"**/*.go"},"cwd":"/tmp/repo"}`), 0, ModeDeny)
	if !d.Block {
		t.Fatalf("expected block on greedy source glob, got %+v", d)
	}
	if d.Reason == "" {
		t.Error("a block must carry a reason")
	}
}

func TestHandlePi_EnrichModeDowngradesBlockToContext(t *testing.T) {
	stubTrackedScope(t, true)

	d := handlePi([]byte(`{"event":"tool_call","tool_name":"Glob","tool_input":{"pattern":"**/*.go"},"cwd":"/tmp/repo"}`), 0, ModeEnrich)
	if d.Block {
		t.Errorf("enrich mode must not block, got block=true")
	}
	if d.AdditionalContext == "" {
		t.Error("enrich mode should surface the guidance as additional_context")
	}
}

func TestHandlePi_GortexToolNeverBlocks(t *testing.T) {
	stubTrackedScope(t, true)

	// Even a name that would otherwise classify as a fallback is allowed
	// when flagged as a Gortex graph tool.
	d := handlePi([]byte(`{"event":"tool_call","tool_name":"Glob","tool_input":{"pattern":"**/*.go"},"is_gortex_tool":true,"cwd":"/tmp/repo"}`), 0, ModeDeny)
	if d.Block {
		t.Errorf("a Gortex graph tool call must never be blocked, got %+v", d)
	}
}

func TestHandlePi_UnknownEventIsNoOp(t *testing.T) {
	d := handlePi([]byte(`{"event":"turn_end"}`), 0, ModeDeny)
	if d.Block || d.Reason != "" || d.AdditionalContext != "" || d.Orientation != "" {
		t.Errorf("unknown event must be a no-op, got %+v", d)
	}
}

func TestHandlePi_GarbledInputIsNoOp(t *testing.T) {
	d := handlePi([]byte(`{not json`), 0, ModeDeny)
	if d.Block || d.Orientation != "" {
		t.Errorf("garbled input must be a no-op, got %+v", d)
	}
}

func TestHandlePi_StopBriefsOnlyTheSessionsOwnEdits(t *testing.T) {
	withSessionDir(t)
	withTrackedRepos(t, daemon.TrackedRepoStatus{Prefix: "gortex", Path: repoFixtureRoot})
	srv := newFakeServer(map[string]string{
		"detect_changes":        incidentChangeSet(),
		"explain_change_impact": `{"risk":"LOW","summary":"1 symbol","total_affected":2}`,
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	// Pi gives paths relative to its cwd, here a subdirectory of the repo.
	cwd := filepath.Join(repoFixtureRoot, "internal")
	call := mustJSON(t, map[string]any{
		"event": "tool_call", "tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join("hooks", "posttask.go")},
		"cwd": cwd, "session_id": "pi-a", "record_only": true,
	})
	if d := handlePi(call, port, ModeDeny); d != (BridgeDecision{}) {
		t.Fatalf("a record-only call must decide nothing, got %+v", d)
	}

	d := handlePi(mustJSON(t, map[string]any{"event": "stop", "cwd": cwd, "session_id": "pi-a"}), port, ModeDeny)
	if d.Block {
		t.Fatalf("diagnostics must not block, got %+v", d)
	}
	for _, want := range []string{"Post-Task Diagnostics", "Your edits this session:** 1 symbol(s) across 1 file(s)"} {
		if !strings.Contains(d.AdditionalContext, want) {
			t.Errorf("briefing missing %q\n---\n%s", want, d.AdditionalContext)
		}
	}
}

func TestHandlePi_StopAfterItsOwnContinuationIsSilent(t *testing.T) {
	withSessionDir(t)
	srv := newFakeServer(map[string]string{"detect_changes": incidentChangeSet()})
	defer srv.Close()

	d := handlePi([]byte(`{"event":"stop","session_id":"pi-b","stop_hook_active":true}`), portFromURL(t, srv.URL), ModeDeny)
	if d != (BridgeDecision{}) {
		t.Errorf("a rerun stop must be silent, got %+v", d)
	}
}

func TestHandlePi_StopBlocksOnAnUnbackedClaim(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	// Pi has no prompt id, so its turn is keyed by session and cwd alone.
	identity := beginTestLocalizationTurn(t, t.Name(), "", t.TempDir())
	primary := []string{"repo/a.go::Writer.write"}
	if !markLocalizationTerminalReceipt(identity, localizationauth.Receipt{
		FinalResponse: "answer", PrimaryIDs: primary, EvidenceIDs: primary,
		ContractVersion: localizationTerminalContractV2, Enforceable: true,
	}) {
		t.Fatal("terminal marker was not written")
	}

	stop := mustJSON(t, map[string]any{
		"event": "stop", "session_id": identity.SessionID, "cwd": identity.CWD,
		"last_assistant_message": "SYMBOLS:\n- flush",
	})
	d := handlePi(stop, 0, ModeDeny)
	if !d.Block || !strings.Contains(d.Reason, "claim_check") {
		t.Fatalf("expected a claim-check block, got %+v", d)
	}
}

func TestHandlePi_GortexEditIsCreditedToTheSession(t *testing.T) {
	withSessionDir(t)
	stubTrackedScope(t, true)
	target := filepath.Join(repoFixtureRoot, "a.go")

	d := handlePi(mustJSON(t, map[string]any{
		"event": "tool_call", "tool_name": "edit", "tool_input": map[string]any{"file_path": target},
		"is_gortex_tool": true, "session_id": "pi-c", "cwd": repoFixtureRoot,
	}), 0, ModeDeny)
	if d.Block {
		t.Fatalf("a Gortex edit must never be blocked, got %+v", d)
	}
	if got := loadSessionState("pi-c").WrittenPaths; !slices.Equal(got, []string{target}) {
		t.Errorf("WrittenPaths = %v, want the Gortex edit's target", got)
	}
}

func TestHandlePi_RecordOnlyNeverBlocks(t *testing.T) {
	withSessionDir(t)
	stubTrackedScope(t, true)

	d := handlePi([]byte(`{"event":"tool_call","tool_name":"Glob","tool_input":{"pattern":"**/*.go"},"cwd":"/tmp/repo","record_only":true}`), 0, ModeDeny)
	if d != (BridgeDecision{}) {
		t.Errorf("record-only must decide nothing, got %+v", d)
	}
}

func TestHandlePi_StopReachesTheDaemonFromTheSessionCWD(t *testing.T) {
	withSessionDir(t)
	cwd := filepath.Join(repoFixtureRoot, "internal")
	var dialed []string
	old := callServerToolDaemonFn
	callServerToolDaemonFn = func(dir, name string, _ map[string]any) string {
		dialed = append(dialed, dir+" "+name)
		return ""
	}
	t.Cleanup(func() { callServerToolDaemonFn = old })

	handlePi(mustJSON(t, map[string]any{"event": "stop", "cwd": cwd, "session_id": "pi-e"}), 0, ModeDeny)
	if !slices.Equal(dialed, []string{cwd + " detect_changes"}) {
		t.Errorf("daemon calls = %v, want detect_changes from the session cwd", dialed)
	}
}
