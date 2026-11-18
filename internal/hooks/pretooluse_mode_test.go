package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRunPreToolUse_EnrichModeDowngradesDeny verifies that in ModeEnrich
// the same input that produces a deny in ModeDeny is downgraded to an
// additionalContext payload — the agent's call is never blocked, but
// the deny rationale still surfaces so the agent learns the graph
// alternative.
func TestRunPreToolUse_EnrichModeDowngradesDeny(t *testing.T) {
	withEditBlocking(t, true)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/repo/handler.go"}}`)

	t.Run("deny mode → permission decision = deny", func(t *testing.T) {
		out := captureStdout(t, func() { runPreToolUse(payload, port, ModeDeny) })
		if out == "" {
			t.Fatal("expected JSON output")
		}
		var dec HookOutput
		if err := json.Unmarshal([]byte(out), &dec); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		if dec.HookSpecificOutput == nil {
			t.Fatal("missing hookSpecificOutput")
		}
		if dec.HookSpecificOutput.PermissionDecision != "deny" {
			t.Errorf("expected deny, got: %q", dec.HookSpecificOutput.PermissionDecision)
		}
		if dec.HookSpecificOutput.AdditionalContext != "" {
			t.Errorf("deny mode must NOT also emit additionalContext, got: %q", dec.HookSpecificOutput.AdditionalContext)
		}
	})

	t.Run("enrich mode → additionalContext, no deny", func(t *testing.T) {
		out := captureStdout(t, func() { runPreToolUse(payload, port, ModeEnrich) })
		if out == "" {
			t.Fatal("expected JSON output")
		}
		var dec HookOutput
		if err := json.Unmarshal([]byte(out), &dec); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		if dec.HookSpecificOutput == nil {
			t.Fatal("missing hookSpecificOutput")
		}
		if dec.HookSpecificOutput.PermissionDecision == "deny" {
			t.Errorf("enrich mode must NEVER deny; got: %+v", dec.HookSpecificOutput)
		}
		if dec.HookSpecificOutput.AdditionalContext == "" {
			t.Error("enrich mode should surface the deny rationale as additionalContext")
		}
		// The downgraded message should still reference the public graph
		// mutation alternative so the agent learns
		// without being blocked.
		if !strings.Contains(dec.HookSpecificOutput.AdditionalContext, "choose `edit` operation `symbol`, `file`, or `batch`") {
			t.Errorf("downgraded context should retain graph alternative hints, got:\n%s",
				dec.HookSpecificOutput.AdditionalContext)
		}
	})
}

// TestRunPreToolUse_EnrichModePreservesSoftContext makes sure non-deny
// soft guidance (the "PREFER graph tools" tip on unindexed source) is
// unchanged in ModeEnrich — only deny responses are downgraded.
func TestRunPreToolUse_EnrichModePreservesSoftContext(t *testing.T) {
	port := fakeIndexedBridge(t, map[string]bool{}) // nothing indexed → soft path

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"/repo/unindexed.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeEnrich) })
	if out == "" {
		t.Fatal("expected JSON output for soft context")
	}
	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if dec.HookSpecificOutput == nil || dec.HookSpecificOutput.AdditionalContext == "" {
		t.Fatal("expected soft additionalContext to survive ModeEnrich")
	}
	if dec.HookSpecificOutput.PermissionDecision == "deny" {
		t.Errorf("soft path must not become a deny — got: %q",
			dec.HookSpecificOutput.PermissionDecision)
	}
}

// TestParseMode covers every supported input plus the fallback for
// unknown / empty values.
func TestParseMode(t *testing.T) {
	cases := map[string]Mode{
		"":                 ModeDeny,
		"deny":             ModeDeny,
		"DENY":             ModeDeny,
		"  deny  ":         ModeDeny,
		"enrich":           ModeEnrich,
		"ENRICH":           ModeEnrich,
		"consult-unlock":   ModeConsultUnlock,
		"Consult-Unlock":   ModeConsultUnlock,
		"  consult-unlock": ModeConsultUnlock,
		"nudge":            ModeAdaptiveNudge,
		"NUDGE":            ModeAdaptiveNudge,
		"adaptive-nudge":   ModeAdaptiveNudge,
		"unknown":          ModeDeny,
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			if got := ParseMode(input); got != want {
				t.Errorf("ParseMode(%q) = %v, want %v", input, got, want)
			}
		})
	}
}

func TestModeString(t *testing.T) {
	if ModeDeny.String() != "deny" {
		t.Errorf("ModeDeny.String() = %q, want \"deny\"", ModeDeny.String())
	}
	if ModeEnrich.String() != "enrich" {
		t.Errorf("ModeEnrich.String() = %q, want \"enrich\"", ModeEnrich.String())
	}
	if ModeConsultUnlock.String() != "consult-unlock" {
		t.Errorf("ModeConsultUnlock.String() = %q, want \"consult-unlock\"", ModeConsultUnlock.String())
	}
	if ModeAdaptiveNudge.String() != "nudge" {
		t.Errorf("ModeAdaptiveNudge.String() = %q, want \"nudge\"", ModeAdaptiveNudge.String())
	}
}

// decodeHookOutput unmarshals captured stdout into a HookOutput,
// failing the test on malformed JSON.
func decodeHookOutput(t *testing.T, out string) HookOutput {
	t.Helper()
	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	return dec
}

// TestRunPreToolUse_ConsultUnlock_DeniesBeforeConsult verifies that
// under ModeConsultUnlock a fallback Read of indexed source is hard-
// denied while the session has not yet queried the Gortex graph, and
// the deny reason explains how to unlock.
func TestRunPreToolUse_ConsultUnlock_DeniesBeforeConsult(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"cu-1","tool_input":{"file_path":"/repo/handler.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeConsultUnlock) })
	if out == "" {
		t.Fatal("expected JSON output before consult")
	}
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil {
		t.Fatal("missing hookSpecificOutput")
	}
	if dec.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected hard deny before consult, got %q", dec.HookSpecificOutput.PermissionDecision)
	}
	if !strings.Contains(dec.HookSpecificOutput.PermissionDecisionReason, "mcp__gortex__") {
		t.Errorf("deny reason should tell the agent to query the graph, got:\n%s",
			dec.HookSpecificOutput.PermissionDecisionReason)
	}
}

// TestRunPreToolUse_ConsultUnlock_AllowsAfterConsult verifies that once
// a mcp__gortex__* call has recorded the per-session marker, the same
// fallback Read is downgraded from a deny to additionalContext.
func TestRunPreToolUse_ConsultUnlock_AllowsAfterConsult(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	// Step 1: the agent queries the Gortex graph. The hook records the
	// marker and emits nothing (no-op pass-through).
	consult := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__search_symbols","session_id":"cu-2","tool_input":{"query":"Foo"}}`)
	consultOut := captureStdout(t, func() { runPreToolUse(consult, port, ModeConsultUnlock) })
	if consultOut != "" {
		t.Errorf("gortex MCP call should be a silent no-op, got: %q", consultOut)
	}
	if !loadSessionState("cu-2").GraphConsulted {
		t.Fatal("expected GraphConsulted marker to be set after a mcp__gortex__* call")
	}

	// Step 2: the previously-denied fallback Read is now downgraded.
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"cu-2","tool_input":{"file_path":"/repo/handler.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeConsultUnlock) })
	if out == "" {
		t.Fatal("expected JSON output after consult")
	}
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil {
		t.Fatal("missing hookSpecificOutput")
	}
	if dec.HookSpecificOutput.PermissionDecision == "deny" {
		t.Errorf("after consult the deny must be downgraded, got: %+v", dec.HookSpecificOutput)
	}
	if dec.HookSpecificOutput.AdditionalContext == "" {
		t.Error("downgraded result should still surface the graph guidance as additionalContext")
	}
}

// TestRunPreToolUse_ConsultUnlock_MarkerIsPerSession ensures one
// session consulting the graph does not unlock another session.
func TestRunPreToolUse_ConsultUnlock_MarkerIsPerSession(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	consult := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__get_symbol","session_id":"cu-A","tool_input":{}}`)
	_ = captureStdout(t, func() { runPreToolUse(consult, port, ModeConsultUnlock) })

	// A different session has NOT consulted — its fallback Read is
	// still hard-denied.
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"cu-B","tool_input":{"file_path":"/repo/handler.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeConsultUnlock) })
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil || dec.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("session B never consulted; expected hard deny, got: %+v", dec.HookSpecificOutput)
	}
}

// TestRunPreToolUse_ConsultUnlock_SoftPathUnchanged confirms that an
// unindexed file (soft-guidance, not a deny) is unaffected by the
// consult-unlock posture regardless of marker state.
func TestRunPreToolUse_ConsultUnlock_SoftPathUnchanged(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{}) // nothing indexed

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"cu-soft","tool_input":{"file_path":"/repo/unindexed.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeConsultUnlock) })
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil || dec.HookSpecificOutput.AdditionalContext == "" {
		t.Fatal("expected soft additionalContext to survive ModeConsultUnlock")
	}
	if dec.HookSpecificOutput.PermissionDecision == "deny" {
		t.Errorf("soft path must not become a deny, got %q", dec.HookSpecificOutput.PermissionDecision)
	}
}

// nudgeReadDecision feeds one non-symbolic Read of indexed source
// through ModeAdaptiveNudge and returns the decoded HookOutput. An
// empty out string means the call was allowed with no payload.
func nudgeReadDecision(t *testing.T, port int, sessionID string) (string, HookOutput) {
	t.Helper()
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"` +
		sessionID + `","tool_input":{"file_path":"/repo/handler.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeAdaptiveNudge) })
	if out == "" {
		return "", HookOutput{}
	}
	return out, decodeHookOutput(t, out)
}

// TestRunPreToolUse_AdaptiveNudge_FiresOncePerBurst is the core spec:
// the first nudgeThreshold-1 non-symbolic calls are allowed, the Nth is
// soft-denied, and the (N+1)th is allowed again because the streak was
// reset by the nudge.
func TestRunPreToolUse_AdaptiveNudge_FiresOncePerBurst(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	// Calls 1..nudgeThreshold-1: allowed (no deny).
	for i := 1; i < nudgeThreshold; i++ {
		_, dec := nudgeReadDecision(t, port, "nudge-1")
		if dec.HookSpecificOutput != nil && dec.HookSpecificOutput.PermissionDecision == "deny" {
			t.Fatalf("call %d should be allowed (below threshold), got deny", i)
		}
	}

	// Call N (== nudgeThreshold): soft-denied exactly once.
	out, dec := nudgeReadDecision(t, port, "nudge-1")
	if out == "" || dec.HookSpecificOutput == nil {
		t.Fatalf("call %d should emit a soft-deny payload", nudgeThreshold)
	}
	if dec.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("call %d should be soft-denied, got %q", nudgeThreshold,
			dec.HookSpecificOutput.PermissionDecision)
	}
	reason := dec.HookSpecificOutput.PermissionDecisionReason
	if !strings.Contains(reason, "fires once") {
		t.Errorf("nudge reason should note it fires once, got:\n%s", reason)
	}
	if !strings.Contains(reason, "`search`") || !strings.Contains(reason, "`relations`") {
		t.Errorf("nudge reason should point at Gortex graph tools, got:\n%s", reason)
	}

	// Call N+1: allowed — the nudge reset the streak.
	_, dec = nudgeReadDecision(t, port, "nudge-1")
	if dec.HookSpecificOutput != nil && dec.HookSpecificOutput.PermissionDecision == "deny" {
		t.Errorf("call %d should proceed (streak reset by the nudge), got deny", nudgeThreshold+1)
	}

	// The streak counter is back to zero on disk.
	if got := loadSessionState("nudge-1").NonSymbolicStreak; got != 1 {
		// After the reset (call N) the allowed call N+1 increments the
		// streak back to 1.
		t.Errorf("expected streak=1 after one post-nudge call, got %d", got)
	}
}

// TestRunPreToolUse_AdaptiveNudge_GortexCallResetsStreak verifies a
// mcp__gortex__* call clears the non-symbolic streak so a partial burst
// never escalates to a nudge.
func TestRunPreToolUse_AdaptiveNudge_GortexCallResetsStreak(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	// Build a streak just shy of the threshold.
	for i := 1; i < nudgeThreshold; i++ {
		nudgeReadDecision(t, port, "nudge-2")
	}
	if got := loadSessionState("nudge-2").NonSymbolicStreak; got != nudgeThreshold-1 {
		t.Fatalf("expected streak=%d before reset, got %d", nudgeThreshold-1, got)
	}

	// A Gortex MCP call resets the streak.
	gortexCall := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__search_symbols","session_id":"nudge-2","tool_input":{"query":"Foo"}}`)
	_ = captureStdout(t, func() { runPreToolUse(gortexCall, port, ModeAdaptiveNudge) })
	if got := loadSessionState("nudge-2").NonSymbolicStreak; got != 0 {
		t.Fatalf("gortex MCP call should reset streak to 0, got %d", got)
	}

	// The next non-symbolic call is allowed — the burst was broken.
	_, dec := nudgeReadDecision(t, port, "nudge-2")
	if dec.HookSpecificOutput != nil && dec.HookSpecificOutput.PermissionDecision == "deny" {
		t.Errorf("post-reset call should be allowed, got deny")
	}
}

// TestRunPreToolUse_AdaptiveNudge_SymbolicCallResetsStreak verifies that
// any non-denying call (here, a Read of an unindexed file) also resets
// the streak — only denying fallback calls extend it.
func TestRunPreToolUse_AdaptiveNudge_SymbolicCallResetsStreak(t *testing.T) {
	withSessionDir(t)
	// handler.go is indexed (denying); unindexed.go is not (soft).
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	for i := 1; i < nudgeThreshold; i++ {
		nudgeReadDecision(t, port, "nudge-3")
	}

	// A Read of an unindexed file produces a non-denying result.
	soft := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"nudge-3","tool_input":{"file_path":"/repo/unindexed.go"}}`)
	_ = captureStdout(t, func() { runPreToolUse(soft, port, ModeAdaptiveNudge) })
	if got := loadSessionState("nudge-3").NonSymbolicStreak; got != 0 {
		t.Errorf("a non-denying call should reset the streak, got %d", got)
	}
}

// TestRunPreToolUse_AdaptiveNudge_LogsNudged checks the once-per-burst
// soft-deny is recorded in the telemetry log as DecisionNudged.
func TestRunPreToolUse_AdaptiveNudge_LogsNudged(t *testing.T) {
	withSessionDir(t)
	logPath := redirectTelemetry(t)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	for i := 1; i <= nudgeThreshold; i++ {
		nudgeReadDecision(t, port, "nudge-4")
	}

	recs := readDecisions(t, logPath)
	var nudged int
	for _, r := range recs {
		if r.Decision == DecisionNudged {
			nudged++
			if r.Tool != "Read" {
				t.Errorf("nudged record should carry the tool name, got %q", r.Tool)
			}
		}
	}
	if nudged != 1 {
		t.Errorf("expected exactly one nudged telemetry record, got %d (all: %+v)", nudged, recs)
	}
}

// TestRunPreToolUse_AutoApprove_GortexToolUnderPermissiveMode verifies
// that a Gortex MCP tool call under a permissive permission mode
// ("acceptEdits") is auto-approved with an explicit allow decision.
func TestRunPreToolUse_AutoApprove_GortexToolUnderPermissiveMode(t *testing.T) {
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__search_symbols","permission_mode":"acceptEdits","tool_input":{"query":"Foo"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	if out == "" {
		t.Fatal("expected an allow payload for a gortex tool under acceptEdits")
	}
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil {
		t.Fatal("missing hookSpecificOutput")
	}
	if dec.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("expected permissionDecision=allow, got %q", dec.HookSpecificOutput.PermissionDecision)
	}
	if dec.HookSpecificOutput.PermissionDecisionReason == "" {
		t.Error("allow decision should carry a reason")
	}
}

// TestRunPreToolUse_AutoApprove_BypassPermissionsApproved verifies
// "bypassPermissions" is on the allowlist — the most permissive host
// mode must not make Gortex's own tools prompt (#841).
func TestRunPreToolUse_AutoApprove_BypassPermissionsApproved(t *testing.T) {
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__search_symbols","permission_mode":"bypassPermissions","tool_input":{"query":"Foo"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	if out == "" {
		t.Fatal("expected an allow payload for a gortex tool under bypassPermissions")
	}
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil || dec.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("expected permissionDecision=allow, got: %+v", dec.HookSpecificOutput)
	}
}

// TestRunPreToolUse_AutoApprove_NonGortexToolNotApproved verifies a
// non-Gortex tool under a permissive mode is not auto-approved — the
// branch is gated on the mcp__gortex__ prefix.
func TestRunPreToolUse_AutoApprove_NonGortexToolNotApproved(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{}) // unindexed → soft path

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","permission_mode":"acceptEdits","tool_input":{"file_path":"/repo/unindexed.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeDeny) })
	if out != "" {
		dec := decodeHookOutput(t, out)
		if dec.HookSpecificOutput != nil && dec.HookSpecificOutput.PermissionDecision == "allow" {
			t.Errorf("a non-gortex tool must never be auto-approved, got: %+v", dec.HookSpecificOutput)
		}
	}
}

// TestRunPreToolUse_AutoApprove_FiresInAnyMode confirms the branch is
// independent of the Mode enum — it auto-approves a Gortex tool under
// acceptEdits even when the posture is, e.g., ModeConsultUnlock.
func TestRunPreToolUse_AutoApprove_FiresInAnyMode(t *testing.T) {
	withSessionDir(t)
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__get_symbol","permission_mode":"auto","session_id":"aa-1","tool_input":{}}`)
	for _, mode := range []Mode{ModeDeny, ModeEnrich, ModeConsultUnlock, ModeAdaptiveNudge} {
		out := captureStdout(t, func() { runPreToolUse(payload, 0, mode) })
		if out == "" {
			t.Fatalf("mode %s: expected an allow payload", mode)
		}
		dec := decodeHookOutput(t, out)
		if dec.HookSpecificOutput == nil || dec.HookSpecificOutput.PermissionDecision != "allow" {
			t.Errorf("mode %s: expected auto-approve allow, got: %+v", mode, dec.HookSpecificOutput)
		}
	}
}

// TestRunPreToolUse_AutoApprove_RecordsConsultMarker verifies an
// auto-approved Gortex call under ModeConsultUnlock still records the
// consult marker — otherwise a permissive permission mode would keep
// fallback reads locked for the whole session.
func TestRunPreToolUse_AutoApprove_RecordsConsultMarker(t *testing.T) {
	withSessionDir(t)
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__get_symbol","permission_mode":"acceptEdits","session_id":"aa-consult","tool_input":{}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, 0, ModeConsultUnlock) })
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil || dec.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("expected auto-approve allow, got: %+v", dec.HookSpecificOutput)
	}
	if !loadSessionState("aa-consult").GraphConsulted {
		t.Error("auto-approved gortex call must record the consult-unlock marker")
	}
}

// TestIsPermissivePermissionMode exercises the allowlist directly:
// only "acceptEdits", "auto", and "bypassPermissions" are permissive;
// everything else, including unknown future modes, is not.
func TestIsPermissivePermissionMode(t *testing.T) {
	cases := map[string]bool{
		"acceptEdits":       true,
		"auto":              true,
		"  acceptEdits  ":   true, // trimmed
		"bypassPermissions": true,
		"default":           false,
		"plan":              false,
		"":                  false,
		"AcceptEdits":       false, // case-sensitive — exact match only
		"someFutureMode":    false,
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			if got := isPermissivePermissionMode(input); got != want {
				t.Errorf("isPermissivePermissionMode(%q) = %v, want %v", input, got, want)
			}
		})
	}
}
