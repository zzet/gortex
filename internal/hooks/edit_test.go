package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

// withEditBlocking flips GORTEX_HOOK_BLOCK_EDIT for one test, and
// restores the previous value (including unset) on cleanup. Avoids
// leaking state between tests run in parallel via t.Cleanup.
func withEditBlocking(t *testing.T, on bool) {
	t.Helper()
	t.Setenv(editBlockingEnvVar, map[bool]string{true: "1", false: ""}[on])
}

// fakeIndexedBridge stubs the daemon file-indexed probe so the named
// files are treated as indexed (one symbol each) for the duration of the
// test, restoring the real probe on cleanup.
// Returns a dummy port (0) so the legacy `port := fakeIndexedBridge(...)`
// call sites still compile; the value is unused now that the indexed check
// routes through the stubbed fileIndexedFn seam rather than an HTTP port.
func fakeIndexedBridge(t *testing.T, indexedPaths map[string]bool) int {
	t.Helper()
	prev := fileIndexedFn
	t.Cleanup(func() { fileIndexedFn = prev })
	fileIndexedFn = func(_, filePath string) (bool, int) {
		if indexedPaths[filePath] {
			return true, 1
		}
		return false, 0
	}
	return 0
}

func TestEnrichEdit_Disabled_AdvisesWithoutBlocking(t *testing.T) {
	// The env gate decides how hard the redirect lands, not whether the agent
	// hears about it. A write door that stays silent is the whole defect.
	withEditBlocking(t, false)
	fakeIndexedBridge(t, map[string]bool{"/repo/foo.go": true})

	result := enrichEdit(map[string]any{"file_path": "/repo/foo.go"}, "")
	if result.deny {
		t.Fatalf("disabled ⇒ must not block; got deny=%v", result.deny)
	}
	if !strings.Contains(result.context, "/repo/foo.go") || !strings.Contains(result.context, "`edit`") {
		t.Errorf("disabled ⇒ advisory naming the graph path; got %q", result.context)
	}
	if strings.Contains(result.context, "BLOCKED") {
		t.Error("advisory context must not claim the edit was blocked")
	}
}

func TestEnrichEdit_NonSource_PassThrough(t *testing.T) {
	withEditBlocking(t, true)
	fakeIndexedBridge(t, map[string]bool{"/repo/README.md": true})

	result := enrichEdit(map[string]any{"file_path": "/repo/README.md"}, "")
	if result.deny || result.context != "" {
		t.Errorf("non-source ⇒ pass through; got deny=%v ctx=%q", result.deny, result.context)
	}
}

func TestEnrichEdit_NotIndexed_PassThrough(t *testing.T) {
	withEditBlocking(t, true)
	fakeIndexedBridge(t, map[string]bool{"/repo/other.go": true})

	result := enrichEdit(map[string]any{"file_path": "/repo/foo.go"}, "")
	if result.deny || result.context != "" {
		t.Errorf("unindexed source ⇒ pass through; got deny=%v ctx=%q", result.deny, result.context)
	}
}

func TestEnrichEdit_IndexedSource_Denies(t *testing.T) {
	withEditBlocking(t, true)
	fakeIndexedBridge(t, map[string]bool{"/repo/foo.go": true})

	result := enrichEdit(map[string]any{"file_path": "/repo/foo.go"}, "")
	if !result.deny {
		t.Fatal("expected deny for Edit on indexed source")
	}
	for _, want := range []string{"BLOCKED", "choose `edit` operation `symbol`, `file`, or `batch`", `refactor(operation:"rename")`} {
		if !strings.Contains(result.reason, want) {
			t.Errorf("reason missing %q:\n%s", want, result.reason)
		}
	}
}

func TestEnrichWrite_IndexedSource_Denies(t *testing.T) {
	withEditBlocking(t, true)
	fakeIndexedBridge(t, map[string]bool{"/repo/server.go": true})

	result := enrichWrite(map[string]any{"file_path": "/repo/server.go"}, "")
	if !result.deny {
		t.Fatal("expected deny for Write on indexed source")
	}
	if !strings.Contains(result.reason, `edit(operation:"write")`) {
		t.Errorf("reason should mention edit(operation=write), got:\n%s", result.reason)
	}
}

func TestEnrichWrite_Disabled_AdvisesWithoutBlocking(t *testing.T) {
	withEditBlocking(t, false)
	fakeIndexedBridge(t, map[string]bool{"/repo/server.go": true})

	result := enrichWrite(map[string]any{"file_path": "/repo/server.go"}, "")
	if result.deny {
		t.Fatalf("disabled ⇒ must not block; got deny=%v", result.deny)
	}
	if !strings.Contains(result.context, `edit(operation:"write")`) {
		t.Errorf("disabled ⇒ advisory naming the graph path; got %q", result.context)
	}
}

func TestEnrichWrite_NewFile_PassThrough(t *testing.T) {
	withEditBlocking(t, true)
	fakeIndexedBridge(t, map[string]bool{}) // nothing indexed

	result := enrichWrite(map[string]any{"file_path": "/repo/new.go"}, "")
	if result.deny || result.context != "" {
		t.Errorf("new file ⇒ pass through; got deny=%v ctx=%q", result.deny, result.context)
	}
}

func TestWriteRedirect_BlockingKeepsTextForPosturesThatDowngrade(t *testing.T) {
	// A posture that softens a deny reads `context`. Leaving it empty when
	// blocking made turning the gate ON quieter than leaving it off.
	withEditBlocking(t, true)
	fakeIndexedBridge(t, map[string]bool{"/repo/foo.go": true})

	blocked := enrichEdit(map[string]any{"file_path": "/repo/foo.go"}, "")
	if !blocked.deny {
		t.Fatal("gate on ⇒ deny")
	}
	if !strings.Contains(blocked.context, "/repo/foo.go") {
		t.Fatalf("blocking redirect must still carry advisory text, got %q", blocked.context)
	}
	if strings.Contains(blocked.context, "BLOCKED") {
		t.Error("the downgrade text must not claim the call was blocked")
	}

	input := HookInput{ToolName: "Edit", ToolInput: map[string]any{"file_path": "/repo/foo.go"}}
	softened := applyMode(input, false, ModeEnrich, blocked)
	if softened.deny {
		t.Error("ModeEnrich must never deny")
	}
	if !strings.Contains(softened.context, "/repo/foo.go") || strings.Contains(softened.context, "BLOCKED") {
		t.Errorf("ModeEnrich should advise without claiming a block, got %q", softened.context)
	}
}

func TestWriteRedirect_NudgePostureStillAdvisesBelowThreshold(t *testing.T) {
	withSessionDir(t)
	withEditBlocking(t, true)
	fakeIndexedBridge(t, map[string]bool{"/repo/foo.go": true})

	input := HookInput{
		ToolName:  "Edit",
		ToolInput: map[string]any{"file_path": "/repo/foo.go"},
		SessionID: "write-nudge",
	}
	result := applyMode(input, false, ModeAdaptiveNudge, enrichEdit(input.ToolInput, ""))
	if result.deny {
		t.Fatal("the first call of a burst is allowed through")
	}
	if !strings.Contains(result.context, "/repo/foo.go") {
		t.Errorf("an allowed write must still be told about the graph path, got %q", result.context)
	}
}

func TestEnrichEdit_DispatchedFromEnrich(t *testing.T) {
	withEditBlocking(t, true)
	fakeIndexedBridge(t, map[string]bool{"/repo/x.go": true})

	input := HookInput{ToolName: "Edit", ToolInput: map[string]any{"file_path": "/repo/x.go"}}
	result := enrich(input, 0)
	if !result.deny {
		t.Errorf("dispatcher must route Edit to enrichEdit; got deny=%v", result.deny)
	}
}

func TestEditBlockingEnabled_Variants(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"no", false},
		{"off", false},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"on", true},
		{"anything-else", true},
	}
	for _, c := range cases {
		t.Setenv(editBlockingEnvVar, c.val)
		got := editBlockingEnabled()
		if got != c.want {
			t.Errorf("editBlockingEnabled(%q) = %v, want %v", c.val, got, c.want)
		}
	}
}

// TestPreToolUse_RecordsWriteTargets covers the three placement hazards for
// the write-target capture: it must survive the auto-approve early return
// under a permissive permission mode (the primary Gortex write door), it must
// fire for tools outside preToolUsePolicyTools, and it must stay silent for
// reads.
func TestPreToolUse_RecordsWriteTargets(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    []string
	}{
		{
			name:    "host Edit",
			payload: `{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Edit","tool_input":{"file_path":"/repo/a.go"}}`,
			want:    []string{"/repo/a.go"},
		},
		{
			// MultiEdit is not in preToolUsePolicyTools, so capture has to
			// happen above that gate or this records nothing.
			name:    "MultiEdit above the policy gate",
			payload: `{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"MultiEdit","tool_input":{"file_path":"/repo/multi.go"}}`,
			want:    []string{"/repo/multi.go"},
		},
		{
			// Under acceptEdits every Gortex MCP call short-circuits at the
			// auto-approve branch; capture has to happen above that too.
			name:    "gortex edit under acceptEdits",
			payload: `{"hook_event_name":"PreToolUse","session_id":"s","permission_mode":"acceptEdits","tool_name":"mcp__gortex__edit","tool_input":{"target":{"file":"internal/b.go"}}}`,
			want:    []string{"internal/b.go"},
		},
		{
			name:    "Read records nothing",
			payload: `{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Read","tool_input":{"file_path":"/repo/a.go"}}`,
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withSessionDir(t)
			captureStdout(t, func() { runPreToolUse([]byte(tc.payload), 0, ModeDeny) })

			got := loadSessionState("s").WrittenPaths
			if len(got) != len(tc.want) {
				t.Fatalf("WrittenPaths = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("WrittenPaths[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestEnrichEdit_ReturnsValidJSONWhenWrappedByDispatcher(t *testing.T) {
	// Sanity: runPreToolUse must produce well-formed deny JSON when
	// the underlying enrichEdit denies. Catches future regressions
	// where a struct tag drift breaks the wire format.
	withEditBlocking(t, true)
	fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/repo/handler.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	if out == "" {
		t.Fatal("expected JSON output for deny path")
	}
	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if dec.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected permissionDecision=deny, got %q", dec.HookSpecificOutput.PermissionDecision)
	}
}
