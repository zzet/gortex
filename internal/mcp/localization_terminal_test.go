package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

func TestHandleFacadeRejectsLocalizationBypassesWithoutClearingState(t *testing.T) {
	registry := newFacadeRegistry()
	calls := 0
	registry.capture(mcpgo.NewTool("explore"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return mcpgo.NewToolResultText("unexpected"), nil
	})
	server := &Server{facades: registry, localization: &localizationTerminalState{}}
	ctx := WithSessionID(context.Background(), "localize-validation")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationCompletion(true, ""), "Locate Foo")

	requests := []struct {
		name string
		args map[string]any
	}{
		{name: "task top-level override", args: map[string]any{"operation": "task", "task": "Locate Bar", "localize": true}},
		{name: "task nested override", args: map[string]any{"operation": "task", "task": "Locate Bar", "options": map[string]any{"localize": true}}},
		{name: "empty localize", args: map[string]any{"operation": "localize", "task": ""}},
		{name: "nested localize task", args: map[string]any{"operation": "localize", "options": map[string]any{"task": "Locate Bar"}}},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Name = "explore"
			req.Params.Arguments = request.args
			result, err := server.handleFacade(ctx, "explore", req)
			if err != nil {
				t.Fatalf("handleFacade() transport error = %v", err)
			}
			operation, _ := request.args["operation"].(string)
			requireLocalizationTerminalReplay(t, result, "explore", normalizeFacadeOperation(operation))
			if blocked := terminal.block("search", "symbols", nil); blocked == nil {
				t.Fatal("invalid localization request cleared terminal state")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("legacy explore calls = %d, want 0", calls)
	}
}

func TestHandleFacadeReplaysMalformedBoundaryAfterAnswerReady(t *testing.T) {
	registry := newFacadeRegistry()
	calls := 0
	registry.capture(mcpgo.NewTool("explore"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return mcpgo.NewToolResultText("unexpected"), nil
	})
	server := &Server{facades: registry, localization: &localizationTerminalState{}}
	ctx := WithSessionID(context.Background(), "malformed-new-task-boundary")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationCompletion(true, ""), "Locate Foo")

	tests := []struct {
		name       string
		facade     string
		operation  string
		args       map[string]any
		want       string
		wantReplay bool
	}{
		{
			name: "options not object", operation: "localize", wantReplay: true,
			args: map[string]any{"operation": "localize", "task": "Locate Bar", "options": "true"},
		},
		{
			name: "boundary not boolean", operation: "localize", wantReplay: true,
			args: map[string]any{"operation": "localize", "task": "Locate Bar", "options": map[string]any{"new_user_task": "true"}},
		},
		{
			name: "boundary on unsupported explore operation", operation: "outline", wantReplay: true,
			args: map[string]any{"operation": "outline", "task": "Locate Bar", "options": map[string]any{"new_user_task": true}},
		},
		{
			name: "boundary on another facade", facade: "search", operation: "symbols", wantReplay: true,
			args: map[string]any{"operation": "symbols", "query": "Run", "options": map[string]any{"new_user_task": true}},
		},
		{
			name: "valid boundary without task", operation: "localize",
			args: map[string]any{"operation": "localize", "task": "", "options": map[string]any{"new_user_task": true}},
			want: "explore.localize requires task",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facade := test.facade
			if facade == "" {
				facade = "explore"
			}
			req := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: facade, Arguments: test.args}}
			result, err := server.handleFacade(ctx, facade, req)
			if test.wantReplay {
				if err != nil {
					t.Fatalf("malformed post-terminal boundary returned transport error: %v", err)
				}
				requireLocalizationTerminalReplay(t, result, facade, test.operation)
				return
			}
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("valid boundary with invalid request = (%#v, %v), want validation error", result, err)
			}
			text, _ := singleTextContent(result)
			if !strings.Contains(text, test.want) {
				t.Fatalf("validation text = %q, want %q", text, test.want)
			}
			if blocked := terminal.block("search", "symbols", nil); blocked == nil {
				t.Fatal("invalid new-task request cleared the prior terminal contract")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("malformed boundary dispatched %d legacy call(s)", calls)
	}
}

func TestCompleteEmptyLocalizationReplacesPriorContract(t *testing.T) {
	terminal := &localizationTerminalState{}
	server := &Server{localization: terminal}
	terminal.armForTask(newLocalizationCompletion(true, ""), "Locate A")

	result := server.completeEmptyLocalization(context.Background(), "Locate B", exploreDefaultBudgetTokens)
	if result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("empty localization result = %v, want one successful compact envelope", result)
	}
	content, ok := result.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("empty localization content type = %T, want TextContent", result.Content[0])
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(content.Text), &envelope); err != nil {
		t.Fatalf("decode empty localization envelope: %v", err)
	}
	for _, field := range []string{"files", "symbols", "evidence"} {
		items, ok := envelope[field].([]any)
		if !ok || len(items) != 0 {
			t.Fatalf("%s = %#v, want empty array", field, envelope[field])
		}
	}
	completion, ok := envelope["completion"].(map[string]any)
	if !ok {
		t.Fatalf("completion = %#v, want object", envelope["completion"])
	}
	if state, _ := completion["state"].(string); state != localizationStateAnswerReady {
		t.Fatalf("empty completion state = %q, want advisory answer_ready", state)
	}
	if action, _ := completion["required_action"].(string); action != "respond" {
		t.Fatalf("empty completion action = %q, want respond", action)
	}
	terminal.mu.Lock()
	state, exact := terminal.state, terminal.exactSymbol
	terminal.mu.Unlock()
	if state != localizationStateAnswerReady || exact != "" {
		t.Fatalf("empty localization armed terminal state=%q exact=%q", state, exact)
	}
	if blocked := terminal.block("search", "symbols", map[string]any{"query": "better anchor"}); blocked == nil {
		t.Fatal("zero-candidate advisory completion did not stop repeated localization")
	}
}

func TestLocalizationFacadeIsExplicit(t *testing.T) {
	registry := newFacadeRegistry()
	localize, ok := registry.operation("explore", "localize")
	if !ok {
		t.Fatal("explore(localize) is not registered")
	}
	if localize.Legacy != "explore" || localize.Fixed["localize"] != true {
		t.Fatalf("unexpected localize mapping: %#v", localize)
	}
	task, ok := registry.operation("explore", "task")
	if !ok {
		t.Fatal("explore(task) is not registered")
	}
	if _, terminal := task.Fixed["localize"]; terminal {
		t.Fatalf("ordinary explore(task) must remain non-terminal: %#v", task.Fixed)
	}
}

func TestLocalizationCompletionEnvelope(t *testing.T) {
	completion := newLocalizationCompletion(true, "")
	result := newLocalizationExploreResult(completion, []exploreTarget{{node: &graph.Node{
		ID: "repo/pkg/file.go::Run", Name: "Run", Kind: graph.KindFunction,
		FilePath: "pkg/file.go", StartLine: 12,
		QualName: "resolver.Run",
		Meta: map[string]any{
			"signature": "func Run()", "search_qual_name": "pkg.Run",
			"search_signature": "func pkg.Run()",
		},
	}, source: "func Run() {}"}}, 1600)
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("expected one text result: %#v", result)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode completion envelope: %v\n%s", err, text)
	}
	if envelope.Completion.State != localizationStateNeedsRecovery || envelope.Completion.RequiredAction != "recover_once" || envelope.Completion.AllowedToolCalls != 1 {
		t.Fatalf("unexpected completion: %#v", envelope.Completion)
	}
	if len(envelope.Files) != 1 || len(envelope.Symbols) != 1 || envelope.Symbols[0] != "repo/pkg/file.go::Run" || len(envelope.Evidence) != 1 {
		t.Fatalf("missing localization payload: %#v", envelope)
	}
	if envelope.Evidence[0].QualName != "pkg.Run" || envelope.Evidence[0].Signature != "func pkg.Run()" {
		t.Fatalf("normalized retrieval metadata not used: %#v", envelope.Evidence[0])
	}
	if strings.Contains(text, "RANKED LOCALIZATION") || strings.Contains(text, "## Likely targets") || len(text) > 2000 {
		t.Fatalf("localize envelope duplicated the legacy rendering or exceeded its compact budget (%d bytes): %s", len(text), text)
	}
}

func TestLocalizationTerminalStateInterceptsOnlyNavigation(t *testing.T) {
	state := newLocalizationTerminalState()
	state.arm(newLocalizationCompletion(true, ""))
	for _, facade := range []string{"explore", "search", "read", "relations", "trace", "analyze"} {
		blocked := state.block(facade, "anything", nil)
		requireLocalizationTerminalReplay(t, blocked, facade, "anything")
	}
	for _, facade := range []string{"change", "edit", "refactor", "workspace", "session", "recall", "remember", "capabilities"} {
		if blocked := state.block(facade, "anything", nil); blocked != nil {
			t.Fatalf("%s must remain dispatchable after answer_ready: %#v", facade, blocked)
		}
	}
}

func TestLocalizationNeedsExactlyOneRead(t *testing.T) {
	state := newLocalizationTerminalState()
	state.arm(newLocalizationCompletion(false, "repo/pkg/file.go::Run"))
	wrong := map[string]any{"target": map[string]any{"symbol": "repo/pkg/file.go::Other"}}
	if state.block("read", "source", wrong) == nil {
		t.Fatal("a different symbol must not consume the exact-read allowance")
	}
	exact := map[string]any{"target": map[string]any{"symbol": "repo/pkg/file.go::Run"}}
	if blocked := state.block("read", "source", exact); blocked != nil {
		t.Fatalf("the named exact read should be allowed: %#v", blocked)
	}
	if state.block("read", "source", exact) == nil {
		t.Fatal("the exact-read allowance must be consumed once")
	}
}

func TestLocalizationRefinementAllowsExactlyOneCandidateRead(t *testing.T) {
	state := newLocalizationTerminalState()
	candidate := "repo/pkg/file.go::Resolver.Run"
	other := "repo/pkg/other.go::Other"
	state.armRefinementRoutesForTask(
		"locate resolver behavior",
		candidate,
		[]string{candidate, other},
		map[string]localizationRefinementRoute{
			candidate: {enforceable: true},
			other:     {enforceable: true},
		},
		nil,
	)

	wrong := map[string]any{"target": map[string]any{"symbol": "repo/pkg/wrong.go::Wrong"}}
	if blocked, reserved := state.authorize("read", "source", wrong); blocked == nil || reserved {
		t.Fatalf("unreturned refinement target was admitted: blocked=%#v reserved=%v", blocked, reserved)
	}
	if blocked, reserved := state.authorize("search", "text", map[string]any{"query": "Run"}); blocked == nil || reserved {
		t.Fatalf("broad refinement search was admitted: blocked=%#v reserved=%v", blocked, reserved)
	}
	if blocked, reserved := state.authorize("change", "impact", nil); blocked != nil || reserved {
		t.Fatalf("non-navigation tool was blocked: blocked=%#v reserved=%v", blocked, reserved)
	}

	alternate := map[string]any{"target": map[string]any{"symbol": other}}
	if blocked, reserved := state.authorize("read", "source", alternate); blocked != nil || !reserved {
		t.Fatalf("alternate returned candidate was not reserved: blocked=%#v reserved=%v", blocked, reserved)
	}
	if blocked, reserved := state.authorize("read", "source", alternate); blocked == nil || reserved {
		t.Fatalf("concurrent refinement read was admitted: blocked=%#v reserved=%v", blocked, reserved)
	}
	state.finishReservedRead(false)
	read := map[string]any{"target": map[string]any{"symbol": candidate}}
	if blocked, reserved := state.authorize("read", "source", read); blocked != nil || !reserved {
		t.Fatalf("failed refinement did not restore allowance: blocked=%#v reserved=%v", blocked, reserved)
	}
	state.finishReservedRead(true)
	if blocked, reserved := state.authorize("read", "source", read); reserved {
		t.Fatal("second successful refinement read reserved a handler")
	} else {
		requireLocalizationTerminalReplay(t, blocked, "read", "source")
	}
}

func TestHandleFacadeRefinementReadReturnsAnswerReadyCompletion(t *testing.T) {
	registry := newFacadeRegistry()
	candidate := "repo/pkg/file.go::Resolver.Run"
	registry.capture(mcpgo.NewTool("get_symbol_source", mcpgo.WithString("id", mcpgo.Required())), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText(`{"id":"repo/pkg/file.go::Resolver.Run","source":"func (r Resolver) Run() {}"}`), nil
	})
	analyzeCalls := 0
	registry.capture(mcpgo.NewTool("why"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		analyzeCalls++
		return mcpgo.NewToolResultText(`{"result":"unexpected"}`), nil
	})
	server := &Server{facades: registry, localization: newLocalizationTerminalState(), sessions: newSessionMap()}
	ctx := WithSessionID(context.Background(), "refinement-read-completion")
	server.localizationFor(ctx).armRefinementRoutesForTask(
		"locate resolver behavior",
		candidate,
		[]string{candidate},
		map[string]localizationRefinementRoute{candidate: {enforceable: true}},
		testEvidenceDigest(),
	)

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "read"
	req.Params.Arguments = map[string]any{
		"operation": "source",
		"target":    map[string]any{"symbol": candidate},
	}
	result, err := server.handleFacade(ctx, "read", req)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("refinement read failed: result=%#v err=%v", result, err)
	}
	body, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("refinement read returned no text: %#v", result)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("refinement response is not one JSON object: %v: %q", err, body)
	}
	if payload["source"] != "func (r Resolver) Run() {}" {
		t.Fatalf("refinement response lost source payload: %#v", payload)
	}
	completion, ok := payload["completion"].(map[string]any)
	if !ok || completion["state"] != localizationStateAnswerReady || completion["required_action"] != "respond" || completion["allowed_tool_calls"] != float64(0) {
		t.Fatalf("refinement response omitted answer-ready completion: %#v", payload["completion"])
	}
	if result.Meta == nil || result.Meta.AdditionalFields == nil {
		t.Fatal("first terminal read omitted host-only fallback metadata")
	}
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || host.Evidence == nil || len(host.Evidence.Evidence) == 0 || host.Evidence.Evidence[0].File != "repo/storage/disk.go" {
		t.Fatalf("first terminal read fallback envelope = %#v", result.Meta.AdditionalFields[localizationHostMetaKey])
	}
	if blocked := server.localizationFor(ctx).block("explore", "localize", nil); blocked == nil {
		t.Fatal("successful refinement read did not commit terminal state")
	}

	analyzeReq := mcpgo.CallToolRequest{}
	analyzeReq.Params.Name = "analyze"
	analyzeReq.Params.Arguments = map[string]any{
		"kind":   "why",
		"target": map[string]any{"symbol": candidate},
	}
	blockedAnalyze, err := server.handleFacade(ctx, "analyze", analyzeReq)
	if err != nil {
		t.Fatalf("terminal analyze returned transport error: %v", err)
	}
	requireLocalizationTerminalReplay(t, blockedAnalyze, "analyze", "why")
	if analyzeCalls != 0 {
		t.Fatalf("terminal analyze reached its legacy handler %d time(s)", analyzeCalls)
	}
}

func TestLocalizationTerminalEvidenceIsPerSession(t *testing.T) {
	server := &Server{
		localization: newLocalizationTerminalState(),
		sessions:     newSessionMap(),
	}
	ctxA := WithSessionID(context.Background(), "a")
	ctxB := WithSessionID(context.Background(), "b")
	completionA := newLocalizationCompletion(true, "")
	completionA.digest = testEvidenceDigest()
	completionB := newLocalizationCompletion(true, "")
	completionB.digest = &localizationEvidenceDigest{
		Files:   []string{"repo/queue/memory.go"},
		Symbols: []string{"repo/queue/memory.go::MemoryQueue.Push"},
		Evidence: []localizationDigestRow{{
			Rank: 1, ID: "repo/queue/memory.go::MemoryQueue.Push", Name: "Push",
			File: "repo/queue/memory.go", Line: 21,
		}},
	}
	server.localizationFor(ctxA).arm(completionA)
	server.localizationFor(ctxB).arm(completionB)

	replayA := server.localizationFor(ctxA).block("search", "symbols", nil)
	replayB := server.localizationFor(ctxB).block("read", "source", nil)
	requireLocalizationTerminalReplay(t, replayA, "search", "symbols")
	requireLocalizationTerminalReplay(t, replayB, "read", "source")
	wireA, _ := json.Marshal(replayA)
	wireB, _ := json.Marshal(replayB)
	if !strings.Contains(string(wireA), "repo/storage/disk.go") || strings.Contains(string(wireA), "repo/queue/memory.go") {
		t.Fatalf("session A replay crossed evidence boundaries: %s", wireA)
	}
	if !strings.Contains(string(wireB), "repo/queue/memory.go") || strings.Contains(string(wireB), "repo/storage/disk.go") {
		t.Fatalf("session B replay crossed evidence boundaries: %s", wireB)
	}
	if blocked := server.localizationFor(context.Background()).block("search", "symbols", nil); blocked != nil {
		t.Fatalf("embedded default inherited daemon session state: %#v", blocked)
	}
}

func TestHandleFacadeTaskCannotEscapeTerminalState(t *testing.T) {
	registry := newFacadeRegistry()
	called := false
	registry.capture(mcpgo.NewTool("explore"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		called = true
		return mcpgo.NewToolResultText("unexpected"), nil
	})
	server := &Server{
		facades:      registry,
		localization: newLocalizationTerminalState(),
		sessions:     newSessionMap(),
	}
	ctx := WithSessionID(context.Background(), "diagnosis")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationCompletion(true, ""), "locate the failing writer")
	for _, task := range []string{
		"diagnose the failing writer",
		"find where that writer failure originates",
	} {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "explore"
		req.Params.Arguments = map[string]any{"operation": "task", "task": task}
		result, err := server.handleFacade(ctx, "explore", req)
		if err != nil {
			t.Fatalf("ordinary task escaped terminal state: result=%#v err=%v", result, err)
		}
		requireLocalizationTerminalReplay(t, result, "explore", "task")
	}
	if called {
		t.Fatal("ordinary explore(task) dispatched after localization completed")
	}
	if blocked := terminal.block("search", "symbols", nil); blocked == nil {
		t.Fatal("ordinary explore(task) cleared terminal state")
	}
}

func TestHandleFacadeTaskRunsWhenTerminalInactive(t *testing.T) {
	registry := newFacadeRegistry()
	called := false
	registry.capture(mcpgo.NewTool("explore"), func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		called = true
		if req.GetBool("localize", false) {
			t.Fatal("explore(task) must not inherit the localize fixed argument")
		}
		return mcpgo.NewToolResultText("ordinary diagnostic neighborhood"), nil
	})
	server := &Server{
		facades:      registry,
		localization: newLocalizationTerminalState(),
		sessions:     newSessionMap(),
	}
	ctx := WithSessionID(context.Background(), "inactive-diagnosis")
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "explore"
	req.Params.Arguments = map[string]any{"operation": "task", "task": "diagnose the failure"}
	result, err := server.handleFacade(ctx, "explore", req)
	if err != nil || result == nil || result.IsError || !called {
		t.Fatalf("inactive ordinary task did not dispatch: result=%#v err=%v called=%v", result, err, called)
	}
	if blocked := server.localizationFor(ctx).block("search", "symbols", nil); blocked != nil {
		t.Fatalf("ordinary task unexpectedly armed terminal state: %#v", blocked)
	}
}

func TestHandleFacadeExplicitNewUserTaskCrossesTerminalBoundary(t *testing.T) {
	registry := newFacadeRegistry()
	calls := 0
	registry.capture(mcpgo.NewTool("explore", mcpgo.WithString("task", mcpgo.Required())), func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		if req.GetString("task", "") != "Diagnose Bar" {
			t.Fatalf("legacy task = %q, want Diagnose Bar", req.GetString("task", ""))
		}
		return mcpgo.NewToolResultText("diagnostic neighborhood"), nil
	})
	server := &Server{facades: registry, localization: newLocalizationTerminalState(), sessions: newSessionMap()}
	ctx := WithSessionID(context.Background(), "explicit-task-boundary")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationCompletion(true, ""), "Locate Foo")

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "explore"
	req.Params.Arguments = map[string]any{
		"operation": "task",
		"task":      "Diagnose Bar",
		"options":   map[string]any{"new_user_task": true},
	}
	result, err := server.handleFacade(ctx, "explore", req)
	if err != nil || result == nil || result.IsError || calls != 1 {
		t.Fatalf("explicit task boundary = (%#v, %v), calls=%d", result, err, calls)
	}
	if text, _ := singleTextContent(result); text != "diagnostic neighborhood" {
		t.Fatalf("explicit task boundary text = %q", text)
	}
	if blocked := terminal.block("search", "symbols", nil); blocked != nil {
		t.Fatalf("successful diagnostic task retained prior terminal state: %#v", blocked)
	}
	terminal.mu.Lock()
	fingerprint := terminal.taskFingerprint
	terminal.mu.Unlock()
	if fingerprint != "Diagnose Bar" {
		t.Fatalf("successful diagnostic task committed fingerprint %q", fingerprint)
	}
}

func TestHandleFacadeExactReadCommitsOnlyOnSuccess(t *testing.T) {
	registry := newFacadeRegistry()
	calls := 0
	registry.capture(mcpgo.NewTool("get_symbol_source", mcpgo.WithString("id", mcpgo.Required())), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		if calls == 1 {
			return mcpgo.NewToolResultError("transient read failure"), nil
		}
		return mcpgo.NewToolResultText("func Run() {}"), nil
	})
	server := &Server{facades: registry, localization: newLocalizationTerminalState(), sessions: newSessionMap()}
	ctx := WithSessionID(context.Background(), "exact-read")
	exactCompletion := newLocalizationCompletion(false, "repo/pkg/file.go::Run")
	exactCompletion.enforceableOnAnswerReady = true
	server.localizationFor(ctx).armForTask(exactCompletion, "locate Run")
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "read"
	req.Params.Arguments = map[string]any{
		"operation": "source",
		"target":    map[string]any{"symbol": "repo/pkg/file.go::Run"},
	}
	first, err := server.handleFacade(ctx, "read", req)
	if err != nil || first == nil || !first.IsError {
		t.Fatalf("expected transient read failure: result=%#v err=%v", first, err)
	}
	second, err := server.handleFacade(ctx, "read", req)
	if err != nil || second == nil || second.IsError {
		t.Fatalf("retry should retain and consume the allowance on success: result=%#v err=%v", second, err)
	}
	body, ok := singleTextContent(second)
	if !ok || !strings.Contains(body, `"state":"needs_recovery"`) ||
		!strings.Contains(body, `"required_action":"recover_once"`) || !strings.Contains(body, `"terminal":false`) {
		t.Fatalf("successful unproven exact-read retry omitted recovery completion: %q", body)
	}
	third, err := server.handleFacade(ctx, "read", req)
	if err != nil || third == nil || third.IsError || calls != 3 {
		t.Fatalf("bounded exact-read recovery = result=%#v err=%v calls=%d", third, err, calls)
	}
	fourth, err := server.handleFacade(ctx, "read", req)
	if err != nil || calls != 3 {
		t.Fatalf("post-recovery exact read = result=%#v err=%v calls=%d", fourth, err, calls)
	}
	requireLocalizationTerminalReplay(t, fourth, "read", "source")
}

func TestHandleFacadeExhaustedCorrectionFailureCarriesTerminalCompletion(t *testing.T) {
	registry := newFacadeRegistry()
	preferred := "repo/pkg/find.go::Find"
	alternate := "repo/pkg/resolve.go::Resolve"
	alternateCalls := 0
	registry.capture(mcpgo.NewTool("get_symbol_source", mcpgo.WithString("id", mcpgo.Required())), func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if req.GetString("id", "") == preferred {
			return mcpgo.NewToolResultText("func Find() {}"), nil
		}
		alternateCalls++
		if alternateCalls == 1 {
			return mcpgo.NewToolResultError("transient correction failure"), nil
		}
		return nil, errors.New("persistent correction failure")
	})
	server := &Server{facades: registry, localization: newLocalizationTerminalState(), sessions: newSessionMap()}
	ctx := WithSessionID(context.Background(), "exhausted-correction")
	server.localizationFor(ctx).armRefinementRoutesForTask(
		"locate resolver behavior",
		preferred,
		[]string{preferred, alternate},
		map[string]localizationRefinementRoute{
			preferred: {},
			alternate: {enforceable: true},
		},
		testEvidenceDigest(),
	)

	read := func(symbol string) (*mcpgo.CallToolResult, error) {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "read"
		req.Params.Arguments = map[string]any{
			"operation": "source",
			"target":    map[string]any{"symbol": symbol},
		}
		return server.handleFacade(ctx, "read", req)
	}
	first, err := read(preferred)
	if err != nil || first == nil || first.IsError {
		t.Fatalf("preferred read = (%#v, %v), want success", first, err)
	}
	firstBody, _ := singleTextContent(first)
	if !strings.Contains(firstBody, `"state":"needs_exact_read"`) ||
		!strings.Contains(firstBody, `"required_action":"read_exact"`) ||
		!strings.Contains(firstBody, alternate) {
		t.Fatalf("preferred read omitted correction contract: %q", firstBody)
	}

	second, err := read(alternate)
	if err != nil || second == nil || !second.IsError {
		t.Fatalf("first correction failure = (%#v, %v), want tool error", second, err)
	}
	third, err := read(alternate)
	if err != nil || third == nil || !third.IsError {
		t.Fatalf("exhausted correction failure = (%#v, %v), want observable tool error", third, err)
	}
	thirdBody, _ := singleTextContent(third)
	if !strings.Contains(thirdBody, "persistent correction failure") ||
		!strings.Contains(thirdBody, `"state":"answer_ready"`) ||
		!strings.Contains(thirdBody, `"required_action":"respond"`) ||
		!strings.Contains(thirdBody, `"terminal":true`) {
		t.Fatalf("exhausted failure omitted terminal completion: %q", thirdBody)
	}
	requireFacadeIdentity(t, third, facadeOperationSpec{Facade: "read", Operation: "source", Legacy: "get_symbol_source"})
	if blocked, err := read(alternate); err != nil {
		t.Fatalf("post-terminal read returned transport error: %v", err)
	} else {
		requireLocalizationTerminalReplay(t, blocked, "read", "source")
	}
}

func TestDecorateExhaustedLocalizationReadFailureKeepsErrorAndFacadeIdentity(t *testing.T) {
	spec := facadeOperationSpec{Facade: "read", Operation: "source", Legacy: "get_symbol_source"}
	completion := newLocalizationCompletion(true, "")

	t.Run("transport error overrides success-shaped result", func(t *testing.T) {
		result, err := decorateExhaustedLocalizationReadFailure(
			mcpgo.NewToolResultText("stale success payload"),
			errors.New("transport read failure"),
			completion,
			spec,
		)
		if err != nil || result == nil || !result.IsError {
			t.Fatalf("decorated transport failure = (%#v, %v), want tool error", result, err)
		}
		body, _ := singleTextContent(result)
		if !strings.Contains(body, "transport read failure") || strings.Contains(body, "stale success payload") ||
			!strings.Contains(body, `"state":"answer_ready"`) {
			t.Fatalf("transport failure payload = %q", body)
		}
		requireFacadeIdentity(t, result, spec)
	})

	t.Run("existing tool error metadata is preserved", func(t *testing.T) {
		original := mcpgo.NewToolResultError("tool read failure")
		original.Meta = mcpgo.NewMetaFromMap(map[string]any{"handler": "kept"})
		result, err := decorateExhaustedLocalizationReadFailure(original, nil, completion, spec)
		if err != nil || result == nil || !result.IsError {
			t.Fatalf("decorated tool failure = (%#v, %v), want tool error", result, err)
		}
		if result.Meta.AdditionalFields["handler"] != "kept" {
			t.Fatalf("existing handler metadata lost: %#v", result.Meta)
		}
		requireFacadeIdentity(t, result, spec)
	})
}

func requireFacadeIdentity(t *testing.T, result *mcpgo.CallToolResult, spec facadeOperationSpec) {
	t.Helper()
	if result == nil || result.Meta == nil || result.Meta.AdditionalFields == nil {
		t.Fatalf("facade result has no metadata: %#v", result)
	}
	identity, ok := result.Meta.AdditionalFields["gortex_facade"].(map[string]any)
	if !ok || identity["surface_version"] != FacadeSurfaceVersion ||
		identity["facade"] != spec.Facade || identity["operation"] != spec.Operation ||
		identity["canonical_tool"] != spec.Legacy {
		t.Fatalf("facade identity = %#v, want %#v", identity, spec)
	}
}

func TestHandleFacadeFailedDifferentLocalizePreservesTerminalState(t *testing.T) {
	registry := newFacadeRegistry()
	calls := 0
	registry.capture(mcpgo.NewTool("explore"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return mcpgo.NewToolResultError("localization failed"), nil
	})
	server := &Server{facades: registry, localization: &localizationTerminalState{}}
	ctx := WithSessionID(context.Background(), "failed-different-localize")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationCompletion(true, ""), "Locate Foo")

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "explore"
	req.Params.Arguments = map[string]any{
		"operation": "localize", "task": "Locate Bar",
		"options": map[string]any{"new_user_task": true},
	}
	result, err := server.handleFacade(ctx, "explore", req)
	if err != nil || result == nil || !result.IsError || calls != 1 {
		t.Fatalf("failed boundary localize = (%v, %v), calls=%d, want one tool error", result, err, calls)
	}
	if blocked := terminal.block("search", "symbols", nil); blocked == nil {
		t.Fatal("failed boundary localize cleared terminal state")
	}
	terminal.mu.Lock()
	fingerprint := terminal.taskFingerprint
	terminal.mu.Unlock()
	if fingerprint != "Locate Foo" {
		t.Fatalf("failed boundary replaced task fingerprint with %q", fingerprint)
	}
}

func TestHandleFacadeExplicitNewUserTaskCommitsOnSuccess(t *testing.T) {
	registry := newFacadeRegistry()
	var server *Server
	calls := 0
	registry.capture(mcpgo.NewTool("explore", mcpgo.WithString("task", mcpgo.Required())), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		server.localizationFor(ctx).armForTask(newLocalizationCompletion(true, ""), req.GetString("task", ""))
		return mcpgo.NewToolResultText("localized"), nil
	})
	server = &Server{facades: registry, localization: newLocalizationTerminalState(), sessions: newSessionMap()}
	ctx := WithSessionID(context.Background(), "new-user-boundary")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationCompletion(true, ""), "Locate Foo")

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "explore"
	req.Params.Arguments = map[string]any{
		"operation": "localize", "task": "Locate Bar",
		"options": map[string]any{"new_user_task": true},
	}
	result, err := server.handleFacade(ctx, "explore", req)
	if err != nil || result == nil || result.IsError || calls != 1 {
		t.Fatalf("explicit boundary localize = (%v, %v), calls=%d", result, err, calls)
	}
	terminal.mu.Lock()
	fingerprint := terminal.taskFingerprint
	terminal.mu.Unlock()
	if fingerprint != "Locate Bar" {
		t.Fatalf("successful boundary committed fingerprint %q", fingerprint)
	}

	req.Params.Arguments = map[string]any{"operation": "localize", "task": "Locate Baz"}
	blocked, err := server.handleFacade(ctx, "explore", req)
	if err != nil || calls != 1 {
		t.Fatalf("later localize without boundary escaped: result=%#v err=%v calls=%d", blocked, err, calls)
	}
	requireLocalizationTerminalReplay(t, blocked, "explore", "localize")
}

func TestHandleFacadeNewUserTaskPanicRollsBack(t *testing.T) {
	registry := newFacadeRegistry()
	registry.capture(mcpgo.NewTool("explore"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		panic("localization panic")
	})
	server := &Server{facades: registry, localization: newLocalizationTerminalState(), sessions: newSessionMap()}
	ctx := WithSessionID(context.Background(), "new-user-panic")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationCompletion(true, ""), "Locate Foo")
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "explore"
	req.Params.Arguments = map[string]any{
		"operation": "localize", "task": "Locate Bar",
		"options": map[string]any{"new_user_task": true},
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = server.handleFacade(ctx, "explore", req)
	}()
	if recovered == nil {
		t.Fatal("handleFacade did not propagate localization panic")
	}
	terminal.mu.Lock()
	fingerprint := terminal.taskFingerprint
	reservation := terminal.reservation
	terminal.mu.Unlock()
	if fingerprint != "Locate Foo" || reservation != nil {
		t.Fatalf("panic changed prior contract: fingerprint=%q reservation=%#v", fingerprint, reservation)
	}
}

func TestHandleFacadeConcurrentLocalizeAdmitsOnlyOne(t *testing.T) {
	registry := newFacadeRegistry()
	started := make(chan struct{})
	release := make(chan struct{})
	var server *Server
	registry.capture(mcpgo.NewTool("explore", mcpgo.WithString("task", mcpgo.Required())), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		close(started)
		<-release
		server.localizationFor(ctx).armForTask(newLocalizationCompletion(true, ""), req.GetString("task", ""))
		return mcpgo.NewToolResultText("localized"), nil
	})
	server = &Server{facades: registry, localization: newLocalizationTerminalState(), sessions: newSessionMap()}
	ctx := WithSessionID(context.Background(), "concurrent-localize")
	firstReq := mcpgo.CallToolRequest{}
	firstReq.Params.Name = "explore"
	firstReq.Params.Arguments = map[string]any{"operation": "localize", "task": "Locate First"}
	type response struct {
		result *mcpgo.CallToolResult
		err    error
	}
	firstDone := make(chan response, 1)
	go func() {
		result, err := server.handleFacade(ctx, "explore", firstReq)
		firstDone <- response{result: result, err: err}
	}()
	<-started

	secondReq := mcpgo.CallToolRequest{}
	secondReq.Params.Name = "explore"
	secondReq.Params.Arguments = map[string]any{
		"operation": "localize", "task": "Locate Second",
		"options": map[string]any{"new_user_task": true},
	}
	second, err := server.handleFacade(ctx, "explore", secondReq)
	if err != nil || second == nil || !second.IsError {
		t.Fatalf("concurrent localize was not blocked: result=%#v err=%v", second, err)
	}
	close(release)
	first := <-firstDone
	if first.err != nil || first.result == nil || first.result.IsError {
		t.Fatalf("admitted localize failed: result=%#v err=%v", first.result, first.err)
	}
	terminal := server.localizationFor(ctx)
	terminal.mu.Lock()
	fingerprint := terminal.taskFingerprint
	terminal.mu.Unlock()
	if fingerprint != "Locate First" {
		t.Fatalf("blocked concurrent localize won state: fingerprint=%q", fingerprint)
	}
}

func TestLocalizationStaleReservationCannotOverwriteNewerState(t *testing.T) {
	terminal := newLocalizationTerminalState()
	token, blocked := terminal.beginLocalize("Locate Stale", false)
	if blocked != nil || token == 0 {
		t.Fatalf("first reservation = (%d, %#v)", token, blocked)
	}
	terminal.armForTask(newLocalizationCompletion(true, ""), "Locate Stale")
	terminal.reset()
	if terminal.finishLocalize(token, true) {
		t.Fatal("generation-stale reservation committed")
	}

	newToken, blocked := terminal.beginLocalize("Locate Current", false)
	if blocked != nil || newToken == 0 {
		t.Fatalf("new reservation = (%d, %#v)", newToken, blocked)
	}
	terminal.armForTask(newLocalizationCompletion(true, ""), "Locate Current")
	if !terminal.finishLocalize(newToken, true) {
		t.Fatal("current reservation did not commit")
	}
	if terminal.finishLocalize(token, true) {
		t.Fatal("stale finisher matched a newer reservation")
	}
	terminal.mu.Lock()
	fingerprint := terminal.taskFingerprint
	terminal.mu.Unlock()
	if fingerprint != "Locate Current" {
		t.Fatalf("stale finisher overwrote %q", fingerprint)
	}
}

func TestHandleFacadeExactReadPanicRestoresReservation(t *testing.T) {
	registry := newFacadeRegistry()
	calls := 0
	registry.capture(mcpgo.NewTool("get_symbol_source"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		if calls == 1 {
			panic("legacy source panic")
		}
		return mcpgo.NewToolResultText("source"), nil
	})
	server := &Server{facades: registry, localization: &localizationTerminalState{}}
	ctx := WithSessionID(context.Background(), "exact-read-panic")
	terminal := server.localizationFor(ctx)
	const symbol = "repo/internal/file.go::Target"
	exactCompletion := newLocalizationCompletion(false, symbol)
	exactCompletion.enforceableOnAnswerReady = true
	terminal.armForTask(exactCompletion, "Locate Target")

	args := map[string]any{
		"operation": "source",
		"target":    map[string]any{"symbol": symbol},
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "read"
	req.Params.Arguments = args
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = server.handleFacade(ctx, "read", req)
	}()
	if recovered == nil {
		t.Fatal("handleFacade() did not propagate legacy panic")
	}
	result, err := server.handleFacade(ctx, "read", req)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("exact read retry = (%v, %v), want success", result, err)
	}
	third, err := server.handleFacade(ctx, "read", req)
	if err != nil || third == nil || third.IsError {
		t.Fatalf("third exact read recovery = (%v, %v), want success", third, err)
	}
	fourth, err := server.handleFacade(ctx, "read", req)
	if err != nil {
		t.Fatalf("fourth exact read = (%v, %v), want terminal block", fourth, err)
	}
	requireLocalizationTerminalReplay(t, fourth, "read", "source")
	if calls != 3 {
		t.Fatalf("legacy source calls = %d, want 3", calls)
	}
}

func TestHandleFacadeLocalizeBlocksParaphrasesWithoutBoundary(t *testing.T) {
	registry := newFacadeRegistry()
	calls := 0
	registry.capture(mcpgo.NewTool("explore", mcpgo.WithString("task", mcpgo.Required())), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return mcpgo.NewToolResultText("unexpected"), nil
	})
	server := &Server{facades: registry, localization: newLocalizationTerminalState(), sessions: newSessionMap()}
	ctx := WithSessionID(context.Background(), "repeat-localize")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationCompletion(true, ""), "Locate Run Handler")

	for _, task := range []string{
		"  Locate   Run Handler ",
		"Locate run Handler",
		"find where the run handler fails",
	} {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "explore"
		req.Params.Arguments = map[string]any{"operation": "localize", "task": task}
		result, err := server.handleFacade(ctx, "explore", req)
		if err != nil {
			t.Fatalf("localize(%q) bypassed active contract: result=%#v err=%v", task, result, err)
		}
		requireLocalizationTerminalReplay(t, result, "explore", "localize")
	}
	if calls != 0 {
		t.Fatalf("blocked localize calls dispatched %d legacy request(s)", calls)
	}
	terminal.mu.Lock()
	fingerprint := terminal.taskFingerprint
	terminal.mu.Unlock()
	if fingerprint != "Locate Run Handler" {
		t.Fatalf("blocked localize changed task fingerprint to %q", fingerprint)
	}
}

func TestExploreAnswerReadyUsesNormalizedRetrievalMetadata(t *testing.T) {
	node := &graph.Node{
		ID: "pkg/worker.go::run", Name: "run", Kind: graph.KindMethod,
		FilePath: "pkg/worker.go", QualName: "resolver.run",
		Meta: map[string]any{"search_qual_name": "BillingService.Reconcile", "search_signature": "func BillingService.Reconcile(invoice Invoice) error"},
	}
	task := "locate BillingService.Reconcile"
	if !exploreAnswerReady(task, []exploreTarget{{node: node, score: 1}}) {
		t.Fatal("normalized retrieval metadata should make the explicit localization answer-ready")
	}
	delete(node.Meta, "search_qual_name")
	delete(node.Meta, "search_signature")
	if exploreAnswerReady(task, []exploreTarget{{node: node, score: 1}}) {
		t.Fatal("resolver-only metadata must not accidentally satisfy the retrieval-specific query")
	}
}

func TestLocalizationEnvelopeOmitsOversizedSource(t *testing.T) {
	node := &graph.Node{ID: "pkg/huge.go::Huge", Name: "Huge", Kind: graph.KindFunction, FilePath: "pkg/huge.go"}
	result := newLocalizationExploreResult(newLocalizationCompletion(true, ""), []exploreTarget{{node: node, source: strings.Repeat("x", 32_000)}}, 1000)
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("expected compact text result: %#v", result)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode compact envelope: %v", err)
	}
	if len(envelope.Evidence) != 1 || envelope.Evidence[0].Source != "" {
		t.Fatalf("oversized source leaked into compact envelope: %#v", envelope.Evidence)
	}
	if len(text) > 1500 {
		t.Fatalf("compact envelope exceeded size guard: %d bytes", len(text))
	}
}
