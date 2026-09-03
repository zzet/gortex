package hooks

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/localizationauth"
)

func TestLocalizationReceiptSurvivesStrippedClaudeWire(t *testing.T) {
	for _, tt := range []struct {
		name        string
		tool        string
		enforceable bool
		wireError   bool
	}{
		{name: "direct advisory", tool: gortexMCPToolPrefix + "explore"},
		{name: "plugin enforceable", tool: gortexPluginMCPToolPrefix + "search", enforceable: true},
		{name: "authenticated terminal error", tool: gortexMCPToolPrefix + "read", enforceable: true, wireError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configureLocalizationTerminalTestHome(t)
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			original := map[string]any{
				"operation": "localize",
				"target":    map[string]any{"file": "literal.go"},
			}
			token := captureTerminalAuthToken(t, identity, tt.tool, "tool-use", original)
			if _, exists := original[localizationauth.ArgumentKey]; exists {
				t.Fatal("PreToolUse mutated the decoded source input")
			}

			finalResponse := "FILES:\n#1 literal.go\n\nSYMBOLS:\n#1 literal.go::Exact\n\nEVIDENCE:\n#1 literal.go:7 — exact bytes\n"
			if !localizationauth.Publish(token, localizationauth.Receipt{
				FinalResponse:   finalResponse,
				ContractVersion: 2,
				Enforceable:     tt.enforceable,
			}) {
				t.Fatal("server receipt publish failed")
			}
			response := map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "host-visible response with metadata stripped"}},
			}
			if tt.wireError {
				response["isError"] = true
			}
			post := localizationPostToolPayload(t, tt.tool, "tool-use", identity, response)
			output := captureHookStdout(t, func() { runPostToolUse(post) })

			var decoded HookOutput
			if err := json.Unmarshal([]byte(output), &decoded); err != nil {
				t.Fatalf("PostToolUse output is not valid JSON: %v\n%s", err, output)
			}
			if decoded.Decision != "" || decoded.SystemMessage != "" || decoded.HookSpecificOutput == nil {
				t.Fatalf("incompatible PostToolUse envelope: %#v", decoded)
			}
			gotContext := decoded.HookSpecificOutput.AdditionalContext
			wantContext := localizationTerminalContext + "\n\n" + finalResponse
			if gotContext != wantContext {
				t.Fatalf("additionalContext changed final_response bytes\n got: %q\nwant: %q", gotContext, wantContext)
			}
			for _, required := range []string{
				"Localization for this task is complete",
				"completion.final_response",
				"do not call another tool",
			} {
				if !strings.Contains(gotContext, required) {
					t.Fatalf("additionalContext %q does not contain %q", gotContext, required)
				}
			}
			if got := hasLocalizationTerminal(identity); got != tt.enforceable {
				t.Fatalf("hard marker = %v, want %v", got, tt.enforceable)
			}
		})
	}
}

func TestLocalizationReceiptMarkerStrengthControlsPreToolUse(t *testing.T) {
	for _, tt := range []struct {
		name        string
		enforceable bool
	}{
		{name: "advisory"},
		{name: "enforceable", enforceable: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configureLocalizationTerminalTestHome(t)
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			tool := gortexMCPToolPrefix + "explore"
			token := captureTerminalAuthToken(t, identity, tool, "terminal-tool", map[string]any{"operation": "localize"})
			if !localizationauth.Publish(token, localizationauth.Receipt{
				FinalResponse:   "FILES:\n#1 storage.go\n\nSYMBOLS:\n#1 storage.go::Load\n\nEVIDENCE:\n#1 storage.go:7 — Load\n",
				ContractVersion: localizationTerminalContractV2,
				Enforceable:     tt.enforceable,
			}) {
				t.Fatal("server receipt publish failed")
			}
			post := localizationPostToolPayload(t, tool, "terminal-tool", identity, strippedToolResponse())
			if output := captureHookStdout(t, func() { runPostToolUse(post) }); output == "" {
				t.Fatal("authenticated answer_ready did not emit terminal context")
			}

			marker, marked := localizationTerminalMarkerFor(identity)
			if !marked {
				t.Fatal("authenticated answer_ready did not persist a marker")
			}
			if marker.Advisory != !tt.enforceable {
				t.Fatalf("marker advisory = %v, want %v", marker.Advisory, !tt.enforceable)
			}
			if got := hasLocalizationTerminal(identity); got != tt.enforceable {
				t.Fatalf("hard marker = %v, want %v", got, tt.enforceable)
			}

			checks := []struct {
				name       string
				tool       string
				input      map[string]any
				navigation bool
				// redirected marks a host tool whose access-policy deny would
				// prescribe a Gortex graph call — a call the marker above then
				// refuses. The marker answers those itself.
				redirected bool
			}{
				{name: "direct navigation", tool: gortexMCPToolPrefix + "read", input: map[string]any{"operation": "source"}, navigation: true},
				{name: "plugin navigation", tool: gortexPluginMCPToolPrefix + "search", input: map[string]any{"operation": "symbols"}, navigation: true},
				{name: "host read", tool: "Read", input: map[string]any{"file_path": "README.md"}, redirected: true},
				{name: "host grep", tool: "Grep", input: map[string]any{"pattern": "load"}, redirected: true},
				{name: "host glob", tool: "Glob", input: map[string]any{"pattern": "**/*.go"}, redirected: true},
				{name: "host web search", tool: "WebSearch", input: map[string]any{"query": "storage"}},
				{name: "direct non-navigation Gortex", tool: gortexMCPToolPrefix + "workspace", input: map[string]any{"operation": "info"}},
				{name: "plugin non-navigation Gortex", tool: gortexPluginMCPToolPrefix + "change", input: map[string]any{"operation": "impact"}},
			}
			for _, check := range checks {
				t.Run(check.name, func(t *testing.T) {
					pre := preToolPayload(t, check.tool, "next-tool", identity, check.input)
					encoded := captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeEnrich) })
					var output HookOutput
					if encoded != "" {
						if err := json.Unmarshal([]byte(encoded), &output); err != nil {
							t.Fatalf("decode PreToolUse output %q: %v", encoded, err)
						}
					}
					wantDenied := tt.enforceable || check.navigation || check.redirected
					if !wantDenied {
						if output.HookSpecificOutput != nil && output.HookSpecificOutput.PermissionDecision == "deny" {
							t.Fatalf("advisory marker denied pass-through tool: %#v", output.HookSpecificOutput)
						}
						return
					}
					if output.HookSpecificOutput == nil || output.HookSpecificOutput.PermissionDecision != "deny" {
						t.Fatalf("terminal marker did not deny tool: %#v", output)
					}
					wantReason := localizationAdvisoryDenyReason
					if tt.enforceable {
						wantReason = localizationTerminalDenyReason
					}
					got := output.HookSpecificOutput.PermissionDecisionReason
					if !strings.HasPrefix(got, wantReason) {
						t.Fatalf("terminal deny reason = %q, want prefix %q", got, wantReason)
					}
					// The refusal hands back the answer, so the caller has
					// something to act on instead of another tool to try.
					if answer := strings.TrimSpace(marker.FinalResponse); answer != "" &&
						!strings.Contains(got, answer) {
						t.Fatalf("terminal deny reason %q does not carry the retained answer %q", got, answer)
					}
				})
			}
		})
	}
}

func TestLocalizationTerminalMarkerRetainsAuthenticatedEvidenceIDs(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
	primary := []string{"repo/a.go::A", "repo/b.go::B"}
	evidence := []string{"repo/a.go::A", "repo/b.go::B", "repo/c.go::C"}
	if !markLocalizationTerminalReceipt(identity, localizationauth.Receipt{
		FinalResponse: "answer", PrimaryIDs: primary, EvidenceIDs: evidence,
		ContractVersion: localizationTerminalContractV2, Enforceable: true,
	}) {
		t.Fatal("marker was not written")
	}
	marker, ok := localizationTerminalMarkerFor(identity)
	if !ok {
		t.Fatal("marker was not readable")
	}
	if !slices.Equal(marker.PrimaryIDs, primary) || !slices.Equal(marker.EvidenceIDs, evidence) {
		t.Fatalf("marker evidence authority = primary %#v evidence %#v", marker.PrimaryIDs, marker.EvidenceIDs)
	}
}

func TestLocalizationTerminalMarkerStrengthIsMonotonic(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "monotonic-strength", "prompt", t.TempDir())
	hardTool := gortexMCPToolPrefix + "read"
	advisoryTool := gortexPluginMCPToolPrefix + "search"
	hardToken := captureTerminalAuthToken(t, identity, hardTool, "hard-tool", map[string]any{"operation": "source"})
	advisoryToken := captureTerminalAuthToken(t, identity, advisoryTool, "advisory-tool", map[string]any{"operation": "symbols"})
	publishTestTerminalReceipt(t, hardToken, true)
	publishTestTerminalReceipt(t, advisoryToken, false)

	hardPost := localizationPostToolPayload(t, hardTool, "hard-tool", identity, strippedToolResponse())
	if _, observed := observeLocalizationTerminal(hardPost); !observed {
		t.Fatal("hard receipt was not observed")
	}
	advisoryPost := localizationPostToolPayload(t, advisoryTool, "advisory-tool", identity, strippedToolResponse())
	if _, observed := observeLocalizationTerminal(advisoryPost); !observed {
		t.Fatal("delayed advisory receipt was not observed")
	}

	marker, marked := localizationTerminalMarkerFor(identity)
	if !marked || marker.Advisory || !hasLocalizationTerminal(identity) {
		t.Fatalf("advisory receipt downgraded hard terminal marker: marker=%#v marked=%v", marker, marked)
	}
	raw, err := os.ReadFile(localizationTerminalMarkerPath(identity))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"advisory"`) {
		t.Fatalf("legacy-compatible hard marker unexpectedly encoded advisory field: %s", raw)
	}
	for _, tool := range []string{"Read", "WebSearch"} {
		encoded := captureHookStdout(t, func() {
			runPreToolUse(preToolPayload(t, tool, "", identity, map[string]any{"file_path": "README.md"}), 0, ModeEnrich)
		})
		var output HookOutput
		if err := json.Unmarshal([]byte(encoded), &output); err != nil || output.HookSpecificOutput == nil {
			t.Fatalf("invalid hard terminal deny for %s: %v\n%s", tool, err, encoded)
		}
		if output.HookSpecificOutput.PermissionDecision != "deny" ||
			!strings.HasPrefix(output.HookSpecificOutput.PermissionDecisionReason, localizationTerminalDenyReason) {
			t.Fatalf("hard marker did not remain enforceable for %s: %#v", tool, output.HookSpecificOutput)
		}
	}
}

func TestLocalizationAdvisoryMarkerRotatesAndDelayedPostCannotPoisonNewTurn(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	cwd := t.TempDir()
	oldTurn := beginTestLocalizationTurn(t, "advisory-rotation", "old-prompt", cwd)
	tool := gortexMCPToolPrefix + "explore"
	loadedToken := captureTerminalAuthToken(t, oldTurn, tool, "loaded-old-tool", map[string]any{"operation": "localize"})
	pendingToken := captureTerminalAuthToken(t, oldTurn, tool, "pending-old-tool", map[string]any{"operation": "localize"})
	publishTestTerminalReceipt(t, loadedToken, false)
	publishTestTerminalReceipt(t, pendingToken, false)

	loadedPost := localizationPostToolPayload(t, tool, "loaded-old-tool", oldTurn, strippedToolResponse())
	terminal, observed := observeLocalizationTerminal(loadedPost)
	if !observed {
		t.Fatal("loaded old-turn advisory receipt was not observed")
	}
	reset := mustJSON(t, map[string]any{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      oldTurn.SessionID,
		"prompt_id":       "new-prompt",
		"cwd":             oldTurn.CWD,
	})
	_ = clearLocalizationTerminalFromHook(reset)
	newTurn := currentTestLocalizationTurn(t, oldTurn.SessionID, "new-prompt", "", oldTurn.CWD)

	emitted := true
	if output := captureHookStdout(t, func() { emitted = emitLocalizationTerminalContext(terminal) }); output != "" || emitted {
		t.Fatalf("loaded delayed PostToolUse emitted stale old-turn context: emitted=%v output=%q", emitted, output)
	}
	pendingPost := localizationPostToolPayload(t, tool, "pending-old-tool", oldTurn, strippedToolResponse())
	if _, observed := observeLocalizationTerminal(pendingPost); observed {
		t.Fatal("pending pre-rotation advisory receipt armed the next turn")
	}
	if !markLocalizationTerminalReceipt(oldTurn, localizationauth.Receipt{ContractVersion: localizationTerminalContractV2}) {
		t.Fatal("simulate delayed advisory marker write")
	}
	if _, marked := localizationTerminalMarkerFor(newTurn); marked {
		t.Fatal("old-turn advisory marker poisoned the new turn")
	}
	encoded := captureHookStdout(t, func() {
		runPreToolUse(preToolPayload(t, gortexMCPToolPrefix+"search", "new-tool", newTurn, map[string]any{"operation": "symbols"}), 0, ModeEnrich)
	})
	var output HookOutput
	if encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &output); err != nil {
			t.Fatalf("decode new-turn PreToolUse output %q: %v", encoded, err)
		}
	}
	if output.HookSpecificOutput != nil && output.HookSpecificOutput.PermissionDecision == "deny" {
		t.Fatalf("new turn inherited delayed advisory deny: %#v", output.HookSpecificOutput)
	}
}

func TestLocalizationAuthPreservesPreToolUsePolicyBranches(t *testing.T) {
	for _, tt := range []struct {
		name           string
		mode           Mode
		permissionMode string
		wantDecision   string
		wantConsulted  bool
	}{
		{name: "permissive auto approve", mode: ModeDeny, permissionMode: "auto", wantDecision: "allow"},
		{name: "accept edits auto approve", mode: ModeDeny, permissionMode: "acceptEdits", wantDecision: "allow"},
		{name: "default preserves prompt", mode: ModeDeny, permissionMode: "default", wantDecision: "ask"},
		{name: "plan preserves prompt", mode: ModeDeny, permissionMode: "plan", wantDecision: "ask"},
		{name: "consult unlock marker", mode: ModeConsultUnlock, wantDecision: "ask", wantConsulted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configureLocalizationTerminalTestHome(t)
			problemStatement := "Title: Neutral storage failure\n\nDiskStorage.load returns an empty value."
			identity, ok := beginLocalizationTurnWithProblem(t.Name(), "prompt", "", t.TempDir(), problemStatement)
			if !ok {
				t.Fatal("beginLocalizationTurnWithProblem failed")
			}
			input := map[string]any{
				"operation": "localize",
				"task":      "literal symptom",
				"options":   map[string]any{"new_user_task": true, "keep": "value"},
			}
			pre := mustJSON(t, map[string]any{
				"hook_event_name": "PreToolUse",
				"tool_name":       gortexMCPToolPrefix + "explore",
				"tool_use_id":     "tool-use",
				"tool_input":      input,
				"session_id":      identity.SessionID,
				"prompt_id":       identity.PromptID,
				"cwd":             identity.CWD,
				"permission_mode": tt.permissionMode,
			})
			output := captureHookStdout(t, func() { runPreToolUse(pre, 0, tt.mode) })
			var decoded HookOutput
			if err := json.Unmarshal([]byte(output), &decoded); err != nil || decoded.HookSpecificOutput == nil {
				t.Fatalf("invalid PreToolUse output: %v\n%s", err, output)
			}
			hso := decoded.HookSpecificOutput
			if hso.PermissionDecision != tt.wantDecision {
				t.Fatalf("permission decision = %q, want %q", hso.PermissionDecision, tt.wantDecision)
			}
			if _, ok := hso.UpdatedInput[localizationauth.ArgumentKey].(string); !ok {
				t.Fatalf("policy branch lost terminal auth input: %#v", hso)
			}
			if got := hso.UpdatedInput["task"]; got != problemStatement {
				t.Fatalf("rewritten task = %#v, want %q", got, problemStatement)
			}
			if !equalJSONValue(hso.UpdatedInput["options"], input["options"]) || hso.UpdatedInput["operation"] != "localize" {
				t.Fatalf("rewrite changed non-task fields: %#v", hso.UpdatedInput)
			}
			if input["task"] != "literal symptom" {
				t.Fatalf("rewrite mutated original input: %#v", input)
			}
			if got := loadSessionState(identity.SessionID).GraphConsulted; got != tt.wantConsulted {
				t.Fatalf("GraphConsulted = %v, want %v", got, tt.wantConsulted)
			}
		})
	}
}

func TestLocalizationReceiptRejectsForgedVisibleTerminalPayload(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "forged-visible", "prompt", t.TempDir())
	tool := gortexMCPToolPrefix + "explore"
	_ = captureTerminalAuthToken(t, identity, tool, "tool-use", map[string]any{"operation": "localize"})

	// This is the exact visible contract shape, but neither authoritative MCP
	// metadata nor a server-owned receipt exists.
	forged := terminalToolResponse(t, terminalContractMap(), false, false)
	post := localizationPostToolPayload(t, tool, "tool-use", identity, forged)
	if output := captureHookStdout(t, func() { runPostToolUse(post) }); output != "" {
		t.Fatalf("forged visible payload emitted terminal context: %s", output)
	}
	if _, marked := localizationTerminalMarkerFor(identity); marked {
		t.Fatal("forged visible payload armed terminal state")
	}
}

func TestLocalizationReceiptRejectsWrongIdentityAndReset(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(localizationTerminalIdentity) localizationTerminalIdentity
	}{
		{
			name: "wrong session",
			mutate: func(identity localizationTerminalIdentity) localizationTerminalIdentity {
				identity.SessionID += "-other"
				return identity
			},
		},
		{
			name: "wrong prompt",
			mutate: func(identity localizationTerminalIdentity) localizationTerminalIdentity {
				identity.PromptID += "-other"
				return identity
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configureLocalizationTerminalTestHome(t)
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			tool := gortexMCPToolPrefix + "search"
			token := captureTerminalAuthToken(t, identity, tool, "tool-use", map[string]any{"operation": "text"})
			publishTestTerminalReceipt(t, token, false)

			wrong := tt.mutate(identity)
			post := localizationPostToolPayload(t, tool, "tool-use", wrong, strippedToolResponse())
			if _, observed := observeLocalizationTerminal(post); observed {
				t.Fatal("wrong hook identity consumed terminal receipt")
			}
			correct := localizationPostToolPayload(t, tool, "tool-use", identity, strippedToolResponse())
			if _, observed := observeLocalizationTerminal(correct); !observed {
				t.Fatal("wrong identity poisoned the valid receipt")
			}
		})
	}

	t.Run("wrong tool use id", func(t *testing.T) {
		configureLocalizationTerminalTestHome(t)
		identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
		tool := gortexMCPToolPrefix + "read"
		token := captureTerminalAuthToken(t, identity, tool, "tool-use", map[string]any{"operation": "source"})
		publishTestTerminalReceipt(t, token, false)
		wrong := localizationPostToolPayload(t, tool, "tool-use-other", identity, strippedToolResponse())
		if _, observed := observeLocalizationTerminal(wrong); observed {
			t.Fatal("wrong tool_use_id consumed terminal receipt")
		}
		correct := localizationPostToolPayload(t, tool, "tool-use", identity, strippedToolResponse())
		if _, observed := observeLocalizationTerminal(correct); !observed {
			t.Fatal("wrong tool_use_id poisoned the valid receipt")
		}
	})

	t.Run("prompt reset", func(t *testing.T) {
		configureLocalizationTerminalTestHome(t)
		identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
		tool := gortexMCPToolPrefix + "analyze"
		token := captureTerminalAuthToken(t, identity, tool, "tool-use", map[string]any{"kind": "contracts"})
		publishTestTerminalReceipt(t, token, true)
		reset := mustJSON(t, map[string]any{
			"hook_event_name": "UserPromptSubmit",
			"session_id":      identity.SessionID,
			"prompt_id":       "next-prompt",
			"cwd":             identity.CWD,
		})
		_ = clearLocalizationTerminalFromHook(reset)
		old := localizationPostToolPayload(t, tool, "tool-use", identity, strippedToolResponse())
		if _, observed := observeLocalizationTerminal(old); observed {
			t.Fatal("pre-reset receipt armed the next turn")
		}
	})
}

func TestLocalizationReceiptConcurrentPostHasSingleWinner(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "concurrent-receipt", "prompt", t.TempDir())
	tool := gortexMCPToolPrefix + "relations"
	token := captureTerminalAuthToken(t, identity, tool, "tool-use", map[string]any{"operation": "usages"})
	publishTestTerminalReceipt(t, token, false)
	post := localizationPostToolPayload(t, tool, "tool-use", identity, strippedToolResponse())

	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, observed := observeLocalizationTerminal(post); observed {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("authenticated PostToolUse winners = %d, want 1", got)
	}
}

func TestLocalizationProblemRewriteDirectAndPluginIsIdempotent(t *testing.T) {
	for _, tt := range []struct {
		name string
		tool string
	}{
		{name: "direct", tool: gortexMCPToolPrefix + "explore"},
		{name: "plugin", tool: gortexPluginMCPToolPrefix + "explore"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configureLocalizationTerminalTestHome(t)
			cwd := t.TempDir()
			problemStatement := "Title: Neutral storage failure\n\nStorage.load returns an empty value at internal/storage/load.go:42."
			prompt := "runner-only metadata\n--- ISSUE --------------------------------\n\n" + problemStatement +
				"\n\n------------------------------------------------\nFILES / SYMBOLS / EVIDENCE only"
			_ = clearLocalizationTerminalFromHook(mustJSON(t, map[string]any{
				"hook_event_name": "UserPromptSubmit",
				"session_id":      "rewrite-" + tt.name,
				"prompt_id":       "prompt",
				"cwd":             cwd,
				"prompt":          prompt,
			}))
			state, ok := currentLocalizationTurnState("rewrite-"+tt.name, "prompt", "", cwd)
			if !ok {
				t.Fatal("framed prompt did not initialize turn state")
			}
			if state.ProblemStatement != problemStatement {
				t.Fatalf("stored problem = %q, want %q", state.ProblemStatement, problemStatement)
			}

			base, ok := localizationTerminalBaseFor(state.Identity.SessionID, "", cwd)
			if !ok {
				t.Fatal("localizationTerminalBaseFor failed")
			}
			statePath := localizationTurnPath(base)
			info, err := os.Stat(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("turn state mode = %o, want 600", got)
			}
			rawState, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(rawState), "runner-only metadata") || strings.Contains(string(rawState), "FILES / SYMBOLS / EVIDENCE only") {
				t.Fatalf("turn state retained prompt material outside the framed problem: %s", rawState)
			}

			input := map[string]any{
				"operation": "localize",
				"task":      "model paraphrase",
				"options":   map[string]any{"new_user_task": true, "keep": "value"},
				"output":    map[string]any{"format": "gcx"},
				"custom":    "preserved",
			}
			for _, toolUseID := range []string{"tool-use-1", "tool-use-2"} {
				output := captureHookStdout(t, func() {
					runPreToolUse(preToolPayload(t, tt.tool, toolUseID, state.Identity, input), 0, ModeDeny)
				})
				var decoded HookOutput
				if err := json.Unmarshal([]byte(output), &decoded); err != nil || decoded.HookSpecificOutput == nil {
					t.Fatalf("invalid PreToolUse output: %v\n%s", err, output)
				}
				if got := decoded.HookSpecificOutput.PermissionDecision; got != "ask" {
					t.Fatalf("rewrite permission decision = %q, want ask", got)
				}
				updated := decoded.HookSpecificOutput.UpdatedInput
				if updated["task"] != problemStatement {
					t.Fatalf("rewritten task = %#v, want %q", updated["task"], problemStatement)
				}
				if _, ok := updated[localizationauth.ArgumentKey].(string); !ok {
					t.Fatalf("rewrite lost localization auth: %#v", updated)
				}
				for _, key := range []string{"operation", "options", "output", "custom"} {
					if !equalJSONValue(updated[key], input[key]) {
						t.Fatalf("rewrite changed %q: got %#v want %#v", key, updated[key], input[key])
					}
				}
				if input["task"] != "model paraphrase" {
					t.Fatalf("rewrite mutated original input: %#v", input)
				}
			}

			post := mustJSON(t, map[string]any{
				"hook_event_name": "PostToolUse",
				"tool_name":       tt.tool,
				"tool_use_id":     "tool-use-1",
				"tool_input":      input,
				"session_id":      state.Identity.SessionID,
				"prompt_id":       state.Identity.PromptID,
				"cwd":             state.Identity.CWD,
				"tool_response":   strippedToolResponse(),
			})
			if output := captureHookStdout(t, func() { runPostToolUse(post) }); output != "" {
				t.Fatalf("non-terminal localization PostToolUse emitted %q", output)
			}
			cleared, ok := currentLocalizationTurnState("rewrite-"+tt.name, "prompt", "", cwd)
			if !ok || cleared.ProblemStatement != "" {
				t.Fatalf("matching PostToolUse did not clear problem statement: %#v, ok=%v", cleared, ok)
			}
			rawState, err = os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(rawState), problemStatement) {
				t.Fatalf("turn state retained the executed private problem: %s", rawState)
			}

			afterOutput := captureHookStdout(t, func() {
				runPreToolUse(preToolPayload(t, tt.tool, "tool-use-after", state.Identity, input), 0, ModeDeny)
			})
			var afterDecoded HookOutput
			if err := json.Unmarshal([]byte(afterOutput), &afterDecoded); err != nil || afterDecoded.HookSpecificOutput == nil {
				t.Fatalf("invalid post-clear PreToolUse output: %v\n%s", err, afterOutput)
			}
			if got := afterDecoded.HookSpecificOutput.UpdatedInput["task"]; got != "model paraphrase" {
				t.Fatalf("cleared problem was restored again: task=%#v", got)
			}

			if !markLocalizationTerminal(state.Identity, localizationTerminalContractV2) {
				t.Fatal("markLocalizationTerminal failed")
			}
			terminalOutput := captureHookStdout(t, func() {
				runPreToolUse(preToolPayload(t, tt.tool, "tool-use-terminal", state.Identity, input), 0, ModeDeny)
			})
			var terminalDecoded HookOutput
			if err := json.Unmarshal([]byte(terminalOutput), &terminalDecoded); err != nil || terminalDecoded.HookSpecificOutput == nil {
				t.Fatalf("invalid terminal PreToolUse output: %v\n%s", err, terminalOutput)
			}
			if terminalDecoded.HookSpecificOutput.PermissionDecision != "deny" || terminalDecoded.HookSpecificOutput.UpdatedInput != nil {
				t.Fatalf("terminal deny must precede task rewrite: %#v", terminalDecoded.HookSpecificOutput)
			}
		})
	}
}

func TestLocalizationProblemStateRotatesClearsAndIsolatesSubagents(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	cwd := t.TempDir()
	sessionID := "problem-lifecycle"
	problemStatement := "Title: Neutral incident\n\nCloudStorage.load returns stale data."
	_ = clearLocalizationTerminalFromHook(mustJSON(t, map[string]any{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      sessionID,
		"prompt_id":       "prompt-1",
		"cwd":             cwd,
		"prompt":          "--- INCIDENT ---\n" + problemStatement + "\n----------------",
	}))
	first, ok := currentLocalizationTurnState(sessionID, "prompt-1", "", cwd)
	if !ok || first.ProblemStatement != problemStatement {
		t.Fatalf("initial problem state = %#v, ok=%v", first, ok)
	}

	runSubagentStart(subagentLifecyclePayload(t, "SubagentStart", sessionID, "agent", cwd))
	subagent, ok := currentLocalizationTurnState(sessionID, "", "agent", cwd)
	if !ok {
		t.Fatal("subagent turn state missing")
	}
	if subagent.ProblemStatement != "" {
		t.Fatalf("parent problem leaked into subagent state: %#v", subagent)
	}

	_ = clearLocalizationTerminalFromHook(mustJSON(t, map[string]any{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      sessionID,
		"prompt_id":       "prompt-2",
		"cwd":             cwd,
		"prompt":          "unframed private follow-up",
	}))
	second, ok := currentLocalizationTurnState(sessionID, "prompt-2", "", cwd)
	if !ok {
		t.Fatal("new prompt did not rotate turn state")
	}
	if second.ProblemStatement != "" {
		t.Fatalf("unframed prompt used a raw fallback: %#v", second)
	}
	if second.Identity.TurnToken == first.Identity.TurnToken {
		t.Fatal("new prompt reused prior turn token")
	}
	base, _ := localizationTerminalBaseFor(sessionID, "", cwd)
	rawState, err := os.ReadFile(localizationTurnPath(base))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawState), "unframed private follow-up") || strings.Contains(string(rawState), problemStatement) {
		t.Fatalf("rotated state retained prompt content: %s", rawState)
	}

	_ = clearLocalizationTerminalFromHook(mustJSON(t, map[string]any{
		"hook_event_name": "SessionStart",
		"session_id":      sessionID,
		"cwd":             cwd,
	}))
	if _, ok := currentLocalizationTurnState(sessionID, "", "", cwd); ok {
		t.Fatal("SessionStart did not clear problem turn state")
	}
}

func TestStopClearsLocalizationProblemStatement(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	cwd := t.TempDir()
	problemStatement := "Title: Private storage incident\n\nSecretStorage.load returned tenant-specific data."
	identity, ok := beginLocalizationTurnWithProblem("problem-stop", "prompt", "", cwd, problemStatement)
	if !ok {
		t.Fatal("beginLocalizationTurnWithProblem failed")
	}

	runPostTask(mustJSON(t, map[string]any{
		"hook_event_name":  "Stop",
		"session_id":       identity.SessionID,
		"prompt_id":        identity.PromptID,
		"cwd":              identity.CWD,
		"stop_hook_active": true,
	}), 0)

	state, ok := currentLocalizationTurnState(identity.SessionID, identity.PromptID, "", identity.CWD)
	if !ok || state.ProblemStatement != "" {
		t.Fatalf("Stop did not clear problem statement: %#v, ok=%v", state, ok)
	}
	base, _ := localizationTerminalBaseFor(identity.SessionID, "", identity.CWD)
	rawState, err := os.ReadFile(localizationTurnPath(base))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawState), problemStatement) {
		t.Fatalf("Stop retained private problem bytes: %s", rawState)
	}
}

func TestLocalizationProblemTaskRewriteFailsOpen(t *testing.T) {
	validInput := func() HookInput {
		return HookInput{
			ToolName: gortexMCPToolPrefix + "explore",
			ToolInput: map[string]any{
				"operation": "localize",
				"task":      "model wording",
				"options":   map[string]any{"new_user_task": true},
			},
		}
	}
	problemStatement := "Title: Neutral issue\n\nStorage.load returns no value."
	tests := []struct {
		name    string
		problem string
		alter   func(*HookInput)
	}{
		{name: "no extracted problem", problem: ""},
		{name: "wrong tool", problem: problemStatement, alter: func(input *HookInput) { input.ToolName = gortexMCPToolPrefix + "read" }},
		{name: "wrong operation", problem: problemStatement, alter: func(input *HookInput) { input.ToolInput["operation"] = "task" }},
		{name: "new user task false", problem: problemStatement, alter: func(input *HookInput) { input.ToolInput["options"] = map[string]any{"new_user_task": false} }},
		{name: "new user task wrong type", problem: problemStatement, alter: func(input *HookInput) { input.ToolInput["options"] = map[string]any{"new_user_task": "true"} }},
		{name: "missing options", problem: problemStatement, alter: func(input *HookInput) { delete(input.ToolInput, "options") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := validInput()
			if tt.alter != nil {
				tt.alter(&input)
			}
			if updated := localizationPreToolUpdatedInput(input, "", tt.problem); updated != nil {
				t.Fatalf("ineligible rewrite did not fail open: %#v", updated)
			}
			updated := localizationPreToolUpdatedInput(input, "opaque-auth", tt.problem)
			if updated["task"] != "model wording" || input.ToolInput["task"] != "model wording" {
				t.Fatalf("ineligible rewrite changed task: updated=%#v original=%#v", updated, input.ToolInput)
			}
			if updated[localizationauth.ArgumentKey] != "opaque-auth" {
				t.Fatalf("auth injection lost on fail-open path: %#v", updated)
			}
		})
	}
}

func TestLocalizationProblemTaskRewriteRequiresClearlyLossyModelTask(t *testing.T) {
	problemStatement := "abcdefgh"
	tests := []struct {
		name    string
		task    string
		rewrite bool
	}{
		{name: "empty", task: "", rewrite: true},
		{name: "below half", task: "abc", rewrite: true},
		{name: "trimmed below half", task: "  abc\n", rewrite: true},
		{name: "exact half", task: "abcd"},
		{name: "trimmed exact half", task: " \tabcd\n"},
		{name: "above half", task: "abcde"},
	}
	for _, tool := range []string{gortexMCPToolPrefix + "explore", gortexPluginMCPToolPrefix + "explore"} {
		for _, tt := range tests {
			t.Run(tool+"/"+tt.name, func(t *testing.T) {
				input := HookInput{
					ToolName: tool,
					ToolInput: map[string]any{
						"operation": "localize",
						"task":      tt.task,
						"options":   map[string]any{"new_user_task": true},
					},
				}
				wantTask := tt.task
				if tt.rewrite {
					wantTask = problemStatement
				}
				for range 2 {
					updated := localizationPreToolUpdatedInput(input, "opaque-auth", problemStatement)
					if updated["task"] != wantTask || updated[localizationauth.ArgumentKey] != "opaque-auth" {
						t.Fatalf("updated input = %#v, want task %q with auth", updated, wantTask)
					}
					if input.ToolInput["task"] != tt.task {
						t.Fatalf("rewrite mutated original model task: %#v", input.ToolInput)
					}
				}
				if got := localizationProblemTaskRewrite(input, problemStatement); got != tt.rewrite {
					t.Fatalf("rewrite decision = %v, want %v", got, tt.rewrite)
				}
			})
		}
	}
}

func TestLocalizationProblemTaskRewriteConcurrent(t *testing.T) {
	input := HookInput{
		ToolName: gortexMCPToolPrefix + "explore",
		ToolInput: map[string]any{
			"operation": "localize",
			"task":      "model wording",
			"options":   map[string]any{"new_user_task": true, "keep": true},
		},
	}
	problemStatement := "Title: Concurrent issue\n\nStorage.load returns no value."
	failed := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				updated := localizationPreToolUpdatedInput(input, "opaque-auth", problemStatement)
				if updated["task"] != problemStatement || updated[localizationauth.ArgumentKey] != "opaque-auth" || input.ToolInput["task"] != "model wording" {
					failed <- struct{}{}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(failed)
	if len(failed) != 0 {
		t.Fatal("concurrent task rewrite was not deterministic")
	}
}

func TestSessionStartNamesMountedExploreAndPreservesCompleteTask(t *testing.T) {
	briefing := rulePreamble()
	for _, required := range []string{
		"`mcp__gortex__explore` (never a bare `explore`)",
		"localize task may be concise",
		"faithfully preserve the issue title",
		"every user-supplied technical identifier, path, literal, error, symptom, and stated hypothesis",
		"never invent a causal hypothesis",
		"clearly framed problem section may be restored exactly at execution",
		"only for a clearly lossy model task",
		"evidence can reflect details beyond a concise tool argument",
		"For `needs_recovery`, make one accepted, bounded Gortex MCP `search` or `read` call",
		"preserves the recovery allowance",
		"the rejected request does not count as the accepted recovery",
		"Do not call host Read, Grep, Glob, or Bash",
		"intentionally not executed and replays the same retained terminal payload",
		"not stale or canned output or an integration failure",
	} {
		if !strings.Contains(briefing, required) {
			t.Fatalf("SessionStart rule is missing %q:\n%s", required, briefing)
		}
	}
	if strings.Contains(briefing, "call `explore(operation") {
		t.Fatalf("SessionStart still suggests a bare explore call:\n%s", briefing)
	}
}

func captureTerminalAuthToken(
	t *testing.T,
	identity localizationTerminalIdentity,
	tool, toolUseID string,
	input map[string]any,
) string {
	t.Helper()
	pre := preToolPayload(t, tool, toolUseID, identity, input)
	output := captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeDeny) })
	var decoded HookOutput
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("PreToolUse auth output is not valid JSON: %v\n%s", err, output)
	}
	if decoded.Decision != "" || decoded.SystemMessage != "" || decoded.HookSpecificOutput == nil {
		t.Fatalf("incompatible PreToolUse auth envelope: %#v", decoded)
	}
	hso := decoded.HookSpecificOutput
	if hso.HookEventName != "PreToolUse" || hso.PermissionDecision != "ask" || hso.AdditionalContext != "" {
		t.Fatalf("auth injection changed hook policy: %#v", hso)
	}
	raw, ok := hso.UpdatedInput[localizationauth.ArgumentKey]
	if !ok {
		t.Fatalf("PreToolUse did not inject %s: %#v", localizationauth.ArgumentKey, hso.UpdatedInput)
	}
	token, ok := raw.(string)
	if !ok || token == "" {
		t.Fatalf("invalid auth token %#v", raw)
	}
	for key, want := range input {
		if got := hso.UpdatedInput[key]; !equalJSONValue(got, want) {
			t.Fatalf("UpdatedInput[%q] = %#v, want %#v", key, got, want)
		}
	}
	return token
}

func publishTestTerminalReceipt(t *testing.T, token string, enforceable bool) {
	t.Helper()
	if !localizationauth.Publish(token, localizationauth.Receipt{
		FinalResponse:   "FILES:\n#1 exact.go\n\nSYMBOLS:\n#1 exact.go::Call\n\nEVIDENCE:\n#1 exact.go:1 — exact.go::Call",
		ContractVersion: 2,
		Enforceable:     enforceable,
	}) {
		t.Fatal("Publish failed")
	}
}

func strippedToolResponse() map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "stripped response"}},
	}
}

func equalJSONValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
