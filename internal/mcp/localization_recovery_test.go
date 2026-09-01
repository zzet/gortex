package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestWeakReadAllowsBoundedSearchRecoveriesThenTerminates(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "weak_read_search_recovery")
	terminal := server.localizationFor(ctx)
	preferred := "repo/search.go::findCandidate"
	terminal.armRefinementForTask("find candidate resolution", preferred, []string{preferred}, nil)

	readSpec, ok := server.facades.operation("read", "source")
	if !ok {
		t.Fatal("read.source facade operation is missing")
	}
	searchSpec, ok := server.facades.operation("search", "text")
	if !ok {
		t.Fatal("search.text facade operation is missing")
	}
	readCalls := 0
	searchCalls := 0
	server.facades.capture(mcpgo.NewTool(readSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		readCalls++
		return mcpgo.NewToolResultText(`{"source":"func findCandidate() {}"}`), nil
	})
	server.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		searchCalls++
		return mcpgo.NewToolResultText(`{"matches":[{"symbol":"repo/search.go::resolveCandidate"}]}`), nil
	})

	readResult, err := server.handleFacade(ctx, "read", localizationRecoveryRequest("read", "source", map[string]any{
		"target": map[string]any{"symbol": preferred},
	}))
	if err != nil || readResult == nil || readResult.IsError || readCalls != 1 {
		t.Fatalf("weak preferred read = (%#v, %v), calls=%d", readResult, err, readCalls)
	}
	requireLocalizationResultStateEqual(t, terminal, readResult, localizationStateNeedsRecovery, false, 1)

	// The stub search returns a page with no graph-backed identity, so it
	// corroborates nothing. One further allowance follows; the second spends it.
	searchResult, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "resolveCandidate",
	}))
	if err != nil || searchResult == nil || searchResult.IsError || searchCalls != 1 {
		t.Fatalf("bounded recovery search = (%#v, %v), calls=%d", searchResult, err, searchCalls)
	}
	requireLocalizationResultStateEqual(t, terminal, searchResult, localizationStateNeedsRecovery, false, 1)

	secondResult, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "findCandidate",
	}))
	if err != nil || secondResult == nil || secondResult.IsError || searchCalls != 2 {
		t.Fatalf("second bounded recovery search = (%#v, %v), calls=%d", secondResult, err, searchCalls)
	}
	requireLocalizationResultStateEqual(t, terminal, secondResult, localizationStateAnswerReady, true, 0)

	extra, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "another anchor",
	}))
	if err != nil {
		t.Fatalf("post-recovery call returned transport error: %v", err)
	}
	requireLocalizationUnconfirmedReplay(t, extra)
	if searchCalls != 2 {
		t.Fatalf("post-recovery search reached handler: calls=%d", searchCalls)
	}
}

func TestRecoveryRejectsUnrelatedAnchorWithoutConsumingAllowance(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "unrelated_recovery_anchor")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationRecoveryCompletion(), "--multiline with --replace duplicates printer output")

	searchSpec, ok := server.facades.operation("search", "text")
	if !ok {
		t.Fatal("search.text facade operation is missing")
	}
	calls := 0
	server.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return mcpgo.NewToolResultText(`{"matches":[{"text":"replace output"}]}`), nil
	})

	result, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "fn sink_matched",
	}))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("unrelated recovery = (%#v, %v), want corrective tool error", result, err)
	}
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("corrective result content = %#v, want one text block", result.Content)
	}
	var corrective struct {
		ErrorCode ErrorCode `json:"error_code"`
		Retriable bool      `json:"retriable"`
		Data      struct {
			Contract localizationTerminalContract `json:"contract"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &corrective); err != nil {
		t.Fatalf("decode corrective error %q: %v", text, err)
	}
	if corrective.ErrorCode != ErrCodeLocalizationComplete || !corrective.Retriable {
		t.Fatalf("corrective error = %#v", corrective)
	}
	if corrective.Data.Contract.Terminal ||
		corrective.Data.Contract.Completion.State != localizationStateNeedsRecovery ||
		corrective.Data.Contract.Completion.AllowedToolCalls != 1 {
		t.Fatalf("corrective contract = %#v", corrective.Data.Contract)
	}
	terminal.mu.Lock()
	stored := terminal.completionLocked()
	terminal.mu.Unlock()
	if stored.State != localizationStateNeedsRecovery || stored.AllowedToolCalls != 1 {
		t.Fatalf("stored recovery after rejected anchor = %#v", stored)
	}
	if calls != 0 {
		t.Fatalf("unrelated recovery reached handler: calls=%d", calls)
	}

	retry, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "--replace",
	}))
	if err != nil || retry == nil || retry.IsError || calls != 1 {
		t.Fatalf("corrected recovery = (%#v, %v), calls=%d", retry, err, calls)
	}
	// Accepted, so the allowance the rejection preserved was spent here. The
	// stub page corroborates nothing, so one further allowance remains.
	requireLocalizationResultStateEqual(t, terminal, retry, localizationStateNeedsRecovery, false, 1)

	last, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "--multiline",
	}))
	if err != nil || last == nil || last.IsError || calls != 2 {
		t.Fatalf("final recovery = (%#v, %v), calls=%d", last, err, calls)
	}
	requireLocalizationResultStateEqual(t, terminal, last, localizationStateAnswerReady, true, 0)
}

func TestRecoveryAcceptsTaskAlignedIdentifierAndCompactLiteralAnchors(t *testing.T) {
	tests := []struct {
		name  string
		task  string
		query string
	}{
		{name: "identifier segment", task: "find candidate resolution", query: "resolveCandidate"},
		{name: "flag", task: "--multiline with --replace duplicates output", query: "--replace"},
		{name: "compact literal", task: `register the locale code "ku"`, query: "ku"},
		{name: "specific VCS path", task: "confirm the completed VCS default exclusion change", query: `".jj/"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !localizationRecoveryQueryAligned(tt.task, tt.query) {
				t.Fatalf("query %q should align with task %q", tt.query, tt.task)
			}
		})
	}
	if localizationRecoveryQueryAligned("--multiline with --replace duplicates output", "fn sink_matched") {
		t.Fatal("generic adjacent declaration unexpectedly aligned with task")
	}
}

func TestRecoveryRejectsDigestOnlyTermFromRG2095Incident(t *testing.T) {
	terminal := newLocalizationTerminalState()
	task := "--multiline with --replace causes duplicate output when a match spans multiple lines"
	digest := &localizationEvidenceDigest{Evidence: []localizationDigestRow{{
		ID:       "ripgrep/crates/printer/src/standard.rs::StandardSink.replacer",
		Name:     "replacer",
		QualName: "StandardSink.replacer",
		File:     "crates/printer/src/standard.rs",
	}}}
	completion := newLocalizationRecoveryCompletion()
	completion.digest = digest
	terminal.armForTask(completion, task)

	blocked, reserved := terminal.authorize("search", "text", map[string]any{"query": "fn sink_matched"})
	if blocked == nil {
		t.Fatal("digest-only generic sink term re-admitted the RG2095 recovery failure")
	}
	if reserved {
		t.Fatal("rejected recovery unexpectedly reserved the one-shot allowance")
	}
}

func TestRecoveryFailureRestoresOnceThenReleasesAdvisory(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "weak_recovery_failure")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationRecoveryCompletion(), "find candidate resolution")

	searchSpec, ok := server.facades.operation("search", "symbols")
	if !ok {
		t.Fatal("search.symbols facade operation is missing")
	}
	calls := 0
	server.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return nil, errors.New("recovery backend unavailable")
	})
	request := localizationRecoveryRequest("search", "symbols", map[string]any{"query": "resolveCandidate"})

	first, err := server.handleFacade(ctx, "search", request)
	if err != nil || first == nil || !first.IsError || calls != 1 {
		t.Fatalf("first failed recovery = (%#v, %v), calls=%d", first, err, calls)
	}
	requireLocalizationResultStateEqual(t, terminal, first, localizationStateNeedsRecovery, false, 1)

	second, err := server.handleFacade(ctx, "search", request)
	if err != nil || second == nil || !second.IsError || calls != 2 {
		t.Fatalf("second failed recovery = (%#v, %v), calls=%d", second, err, calls)
	}
	requireLocalizationResultStateEqual(t, terminal, second, localizationStateLocalized, false, 0)

	third, err := server.handleFacade(ctx, "search", request)
	if err == nil || third != nil {
		t.Fatalf("released navigation did not expose the ordinary handler error: (%#v, %v)", third, err)
	}
	if calls != 3 {
		t.Fatalf("advisory release kept localization interception active: calls=%d", calls)
	}
}

func TestEnforceableAnswerReadyLocksBeforeHandler(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "strong_answer_ready_lock")
	completion := newLocalizationCompletion(true, "")
	completion.Enforceable = true
	server.localizationFor(ctx).armForTask(completion, "find candidate resolution")

	searchSpec, ok := server.facades.operation("search", "text")
	if !ok {
		t.Fatal("search.text facade operation is missing")
	}
	calls := 0
	server.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return mcpgo.NewToolResultText("unexpected"), nil
	})

	result, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "resolveCandidate",
	}))
	if err != nil {
		t.Fatalf("strong terminal search returned transport error: %v", err)
	}
	requireLocalizationTerminalReplay(t, result, "search", "text")
	if calls != 0 {
		t.Fatalf("enforceable answer_ready reached handler: calls=%d", calls)
	}
}

func TestUnsupportedRecoveryAttemptReleasesAdvisoryBeforeSchemaDispatch(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "unsupported_recovery")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationRecoveryCompletion(), "find candidate resolution")

	result, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "not_an_operation", map[string]any{
		"query": "resolveCandidate",
	}))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("unsupported recovery = (%#v, %v), want advisory tool error", result, err)
	}
	requireLocalizationResultStateEqual(t, terminal, result, localizationStateLocalized, false, 0)
}

func TestSchemaInvalidAllowedRecoveryReleasesAdvisoryBeforeHandler(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "schema_invalid_recovery")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationRecoveryCompletion(), "find candidate resolution")

	searchSpec, ok := server.facades.operation("search", "text")
	if !ok {
		t.Fatal("search.text facade operation is missing")
	}
	calls := 0
	server.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return mcpgo.NewToolResultText("unexpected"), nil
	})
	invalid, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query":   "resolveCandidate",
		"options": "not-an-object",
	}))
	if err != nil || invalid == nil || !invalid.IsError {
		t.Fatalf("schema-invalid recovery = (%#v, %v), want advisory tool error", invalid, err)
	}
	requireLocalizationResultStateEqual(t, terminal, invalid, localizationStateLocalized, false, 0)
	if calls != 0 {
		t.Fatalf("schema-invalid recovery reached handler: calls=%d", calls)
	}

	valid, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "resolveCandidate",
	}))
	if err != nil || valid == nil || valid.IsError {
		t.Fatalf("released navigation did not reach ordinary handler: (%#v, %v)", valid, err)
	}
	if calls != 1 {
		t.Fatalf("advisory release kept schema-invalid recovery interception active: calls=%d", calls)
	}
}

func TestStaleInvalidRecoveryTicketCannotConsumeNewTaskState(t *testing.T) {
	state := &localizationTerminalState{}
	state.armForTask(newLocalizationRecoveryCompletion(), "old anchor task")
	blocked, oldGeneration := state.interceptAnswerReady("search", "text", map[string]any{"query": "old anchor"})
	if blocked != nil || oldGeneration == 0 {
		t.Fatalf("old invalid-recovery preflight = (%#v, %d)", blocked, oldGeneration)
	}

	state.armForTask(newLocalizationRecoveryCompletion(), "new anchor task")
	if completion, consumed := state.consumeInvalidRecovery("search", "text", oldGeneration); consumed {
		t.Fatalf("stale invalid request consumed new task: %#v", completion)
	}
	state.mu.Lock()
	stored := state.completionLocked()
	state.mu.Unlock()
	if stored.State != localizationStateNeedsRecovery || stored.AllowedToolCalls != 1 {
		t.Fatalf("new task completion after stale invalid request = %#v", stored)
	}

	blocked, newGeneration := state.interceptAnswerReady("search", "text", map[string]any{"query": "new anchor"})
	if blocked != nil || newGeneration == 0 || newGeneration == oldGeneration {
		t.Fatalf("new invalid-recovery preflight = (%#v, %d), old generation=%d", blocked, newGeneration, oldGeneration)
	}
	completion, consumed := state.consumeInvalidRecovery("search", "text", newGeneration)
	if !consumed || completion.State != localizationStateLocalized || completion.Enforceable {
		t.Fatalf("current invalid request did not release advisory state: (%#v, %v)", completion, consumed)
	}
}

func TestStaleRecoveryCannotConsumeNewTaskState(t *testing.T) {
	state := &localizationTerminalState{}
	state.armForTask(newLocalizationRecoveryCompletion(), "old anchor task")
	blocked, token := state.authorizeWithToken("search", "text", map[string]any{"query": "old anchor"})
	if blocked != nil || token == 0 {
		t.Fatalf("old recovery reservation = (%#v, %d)", blocked, token)
	}

	state.reset()
	strong := newLocalizationCompletion(true, "")
	strong.Enforceable = true
	state.armForTask(strong, "new task")
	stale := state.finishReservedReadToken(token, true)
	if stale.State != localizationStateInactive {
		t.Fatalf("stale finisher completion = %#v, want inactive", stale)
	}
	if blocked, reserved := state.authorize("search", "text", map[string]any{"query": "new anchor"}); reserved {
		t.Fatal("new strong task reserved a recovery call")
	} else {
		requireLocalizationTerminalReplay(t, blocked, "search", "text")
	}
}

func TestRecoveryEvidenceRejectsLongSymptomSinkEcho(t *testing.T) {
	task := "Storage writes stall during commit"
	row := localizationDigestRow{
		ID:        "repo/storage/report.go::Reporter.ReportStorageFailureAsPending",
		Name:      "ReportStorageFailureAsPending",
		QualName:  "Reporter.ReportStorageFailureAsPending",
		Kind:      "method",
		File:      "repo/storage/report.go",
		Signature: "storage writes stall during commit",
	}
	if localizationRecoveryEvidenceAlignedWithLead(task, localizationTaskLead(task), "storage", "search.text", []localizationDigestRow{row}) {
		t.Fatalf("long symptom sink became confident through body echoes: %#v", row)
	}
}

func TestRecoveryEvidenceAcceptsTwoSegmentImplementation(t *testing.T) {
	task := "Storage writes stall during commit"
	row := localizationDigestRow{
		ID:       "repo/storage/flush.go::Storage.Flush",
		Name:     "StorageFlush",
		QualName: "Storage.Flush",
		Kind:     "method",
		File:     "repo/storage/flush.go",
	}
	if !localizationRecoveryEvidenceAlignedWithLead(task, localizationTaskLead(task), "", "read.source", []localizationDigestRow{row}) {
		t.Fatalf("two-segment implementation did not satisfy proportional lead confidence: %#v", row)
	}
}

func TestRecoveryEvidenceAcceptsLongTechnicalIdentifierWithShortLeadTerms(t *testing.T) {
	task := "JWT auth failures block requests"
	row := localizationDigestRow{
		ID:        "repo/auth/policy.go::Policy.ApplyJWTAuthPolicy",
		Name:      "ApplyJWTAuthPolicy",
		QualName:  "Policy.ApplyJWTAuthPolicy",
		Kind:      "method",
		File:      "repo/auth/policy.go",
		Signature: "jwt auth request failure",
	}
	if !localizationRecoveryEvidenceAlignedWithLead(task, localizationTaskLead(task), "", "read.source", []localizationDigestRow{row}) {
		t.Fatalf("short technical lead terms did not establish long identifier coverage: %#v", row)
	}
}

func TestRecoveryEvidenceRejectsGenericCallableWithSignatureOnlyOverlap(t *testing.T) {
	task := "Storage commits stall during flush"
	row := localizationDigestRow{
		ID:        "repo/storage/handler.go::Handler.Handle",
		Name:      "Handle",
		QualName:  "Handler.Handle",
		Kind:      "method",
		File:      "repo/storage/handler.go",
		Signature: "func Handle() storage commits stall during flush",
	}
	if localizationRecoveryEvidenceAlignedWithLead(task, localizationTaskLead(task), "storage", "search.text", []localizationDigestRow{row}) {
		t.Fatalf("generic callable borrowed confidence from its signature: %#v", row)
	}
}

func TestRecoveryWeakResultReAllowsThenTerminatesUnconfirmedForEveryOperation(t *testing.T) {
	longSink := localizationDigestRow{
		ID:        "fixture/storage/report.go::Reporter.ReportStorageFailureAsPending",
		Name:      "ReportStorageFailureAsPending",
		QualName:  "Reporter.ReportStorageFailureAsPending",
		Kind:      "method",
		File:      "fixture/storage/report.go",
		Signature: "storage writes stall during commit",
	}
	tests := []struct {
		name      string
		facade    string
		operation string
		arguments map[string]any
	}{
		{name: "text search", facade: "search", operation: "text", arguments: map[string]any{"query": "storage"}},
		{name: "symbol search", facade: "search", operation: "symbols", arguments: map[string]any{"query": "storage"}},
		{name: "source read", facade: "read", operation: "source", arguments: map[string]any{"target": map[string]any{"symbol": longSink.ID}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newLocalizationTerminalState()
			state.armForTask(newLocalizationRecoveryCompletion(), "Storage writes stall during commit")

			blocked, token := state.authorizeWithToken(test.facade, test.operation, test.arguments)
			if blocked != nil || token == 0 {
				t.Fatalf("recovery authorization = (%#v, %d)", blocked, token)
			}
			completion := state.finishReservedReadTokenWithDigest(token, true, []localizationDigestRow{longSink}, true)
			if completion.State != localizationStateNeedsRecovery || completion.AllowedToolCalls != 1 || completion.Enforceable {
				t.Fatalf("weak accepted recovery did not keep one further allowance: %#v", completion)
			}

			blocked, token = state.authorizeWithToken(test.facade, test.operation, test.arguments)
			if blocked != nil || token == 0 {
				t.Fatalf("second recovery authorization = (%#v, %d)", blocked, token)
			}
			completion = state.finishReservedReadTokenWithDigest(token, true, []localizationDigestRow{longSink}, true)
			if completion.State != localizationStateAnswerReady || completion.Enforceable {
				t.Fatalf("spent recovery allowance did not terminate: %#v", completion)
			}
			if !strings.HasPrefix(completion.FinalResponse, localizationProvisionalHeading) {
				t.Fatalf("uncorroborated terminal page claimed proof: %q", completion.FinalResponse)
			}
		})
	}
}

func TestPlannedRecoveryDerivesOneProductionCallableFamily(t *testing.T) {
	current := []localizationDigestRow{{
		ID:   "repo/stream.go::stream_multi_line_handler",
		Name: "stream_multi_line_handler",
		Kind: "function",
		File: "repo/stream.go",
	}}
	retained := &localizationEvidenceDigest{Evidence: []localizationDigestRow{
		{
			ID:   "repo/codec.go::Codec.transform_with_capture",
			Name: "transform_with_capture",
			Kind: "method",
			File: "repo/codec.go",
		},
		{
			ID:   "repo/codec.go::Codec.transform_with_capture_at",
			Kind: "method",
			File: "repo/codec.go",
		},
		{
			ID:   "repo/tests/codec_test.go::transform_via_fixture",
			Name: "transform_via_fixture",
			Kind: "function",
			File: "repo/tests/codec_test.go",
		},
	}}
	operation, anchor, ok := localizationPlannedRecoveryForWeakRefinement(
		"combining --multi-line with --transform duplicates output",
		current,
		retained,
	)
	if !ok || operation != "search.symbols" || anchor != "transform_with" {
		t.Fatalf("planned recovery = (%q, %q, %v), want search.symbols transform_with", operation, anchor, ok)
	}
}

func TestPlannedRecoveryRequiresUniqueComplementaryFamily(t *testing.T) {
	current := []localizationDigestRow{{
		ID: "repo/stream.go::multi_line_handler", Name: "multi_line_handler", Kind: "function", File: "repo/stream.go",
	}}
	tests := []struct {
		name     string
		task     string
		current  []localizationDigestRow
		retained []localizationDigestRow
	}{
		{
			name:    "ambiguous families",
			task:    "combining --multi-line with --transform duplicates output",
			current: current,
			retained: []localizationDigestRow{
				{ID: "repo/codec.go::transform_with_capture", Name: "transform_with_capture", Kind: "function", File: "repo/codec.go"},
				{ID: "repo/codec.go::transform_via_buffer", Name: "transform_via_buffer", Kind: "function", File: "repo/codec.go"},
			},
		},
		{
			name:     "current read covers no explicit concept",
			task:     "combining --multi-line with --transform duplicates output",
			current:  []localizationDigestRow{{ID: "repo/sink.go::output_sink", Name: "output_sink", Kind: "function", File: "repo/sink.go"}},
			retained: []localizationDigestRow{{ID: "repo/codec.go::transform_with_capture", Name: "transform_with_capture", Kind: "function", File: "repo/codec.go"}},
		},
		{
			name:     "only one explicit concept",
			task:     "--transform duplicates output",
			current:  current,
			retained: []localizationDigestRow{{ID: "repo/codec.go::transform_with_capture", Name: "transform_with_capture", Kind: "function", File: "repo/codec.go"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if operation, anchor, ok := localizationPlannedRecoveryForWeakRefinement(
				tt.task,
				tt.current,
				&localizationEvidenceDigest{Evidence: tt.retained},
			); ok {
				t.Fatalf("unexpected planned recovery = (%q, %q)", operation, anchor)
			}
		})
	}
}

func TestPlannedRecoveryReplaceFamilyRegression(t *testing.T) {
	current := []localizationDigestRow{{
		ID:   "crates/searcher/src/sink.rs::sink_slow_multi_line_only_matching",
		Name: "sink_slow_multi_line_only_matching",
		Kind: "function",
		File: "crates/searcher/src/sink.rs",
	}}
	retained := &localizationEvidenceDigest{Evidence: []localizationDigestRow{
		current[0],
		{
			ID:       "crates/searcher/src/glue.rs::Matcher.replace_with_captures",
			Name:     "replace_with_captures",
			QualName: "Matcher.replace_with_captures",
			Kind:     "method",
			File:     "crates/searcher/src/glue.rs",
		},
		{
			ID:       "crates/searcher/src/glue.rs::Matcher.replace_with_captures_at",
			Name:     "replace_with_captures_at",
			QualName: "Matcher.replace_with_captures_at",
			Kind:     "method",
			File:     "crates/searcher/src/glue.rs",
		},
	}}
	operation, anchor, ok := localizationPlannedRecoveryForWeakRefinement(
		"--multiline with --replace duplicates output while --only-matching spans lines",
		current,
		retained,
	)
	if !ok || operation != "search.symbols" || anchor != "replace_with" {
		t.Fatalf("planned recovery = (%q, %q, %v), want search.symbols replace_with", operation, anchor, ok)
	}
}

func TestPlannedRecoveryIsExactRetriableAndWeakResultReAllowsRecovery(t *testing.T) {
	task := "Printed records are duplicated unexpectedly\ncombining --multiline with --replace while --only-matching spans lines"
	preferred := "crates/searcher/src/sink.rs::sink_slow_multi_line_only_matching"
	current := []localizationDigestRow{{
		ID: preferred, Name: "sink_slow_multi_line_only_matching", Kind: "function", File: "crates/searcher/src/sink.rs",
	}}
	retained := &localizationEvidenceDigest{Evidence: []localizationDigestRow{
		current[0],
		{ID: "crates/searcher/src/glue.rs::Matcher.replace_with_captures", Name: "replace_with_captures", Kind: "method", File: "crates/searcher/src/glue.rs"},
		{ID: "crates/searcher/src/glue.rs::Matcher.replace_with_captures_at", Name: "replace_with_captures_at", Kind: "method", File: "crates/searcher/src/glue.rs"},
	}}
	state := newLocalizationTerminalState()
	state.armRefinementForTask(task, preferred, []string{preferred}, retained)
	blocked, token := state.authorizeWithToken("read", "source", map[string]any{"target": map[string]any{"symbol": preferred}})
	if blocked != nil || token == 0 {
		t.Fatalf("refinement authorization = (%#v, %d)", blocked, token)
	}
	completion := state.finishReservedReadTokenWithDigest(token, true, current, true)
	if completion.State != localizationStateNeedsRecovery || completion.RequiredAction != `search.symbols("replace_with")` {
		t.Fatalf("planned completion = %#v", completion)
	}
	if len(completion.AllowedOperations) != 1 || completion.AllowedOperations[0] != "search.symbols" {
		t.Fatalf("planned allowed operations = %#v", completion.AllowedOperations)
	}
	wantInstruction := `Call Gortex MCP search(operation:"symbols", query:"replace_with"); then respond from the accepted result and follow its completion.`
	if completion.Instruction != wantInstruction {
		t.Fatalf("planned instruction = %q, want %q", completion.Instruction, wantInstruction)
	}
	encoded, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("marshal planned completion: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("decode planned completion: %v", err)
	}
	if _, exists := wire["recoveryOperation"]; exists {
		t.Fatalf("session-only recovery operation leaked onto wire: %s", encoded)
	}
	if _, exists := wire["recoveryAnchor"]; exists {
		t.Fatalf("session-only recovery anchor leaked onto wire: %s", encoded)
	}

	// A search on some other anchor is genuinely off-plan. read.file is not a
	// counter-example any more: naming a file releases the contract by design,
	// so the planned plan is checked with a call that has no such meaning.
	wrong, generation := state.interceptAnswerReady("search", "symbols", map[string]any{"query": "SomeOtherAnchor"})
	if wrong == nil || !wrong.IsError || generation != 0 {
		t.Fatalf("wrong planned navigation = (%#v, %d), want retriable block", wrong, generation)
	}
	text, ok := singleTextContent(wrong)
	if !ok {
		t.Fatalf("wrong planned navigation content = %#v", wrong.Content)
	}
	var mismatch struct {
		Retriable bool `json:"retriable"`
	}
	if err := json.Unmarshal([]byte(text), &mismatch); err != nil || !mismatch.Retriable {
		t.Fatalf("planned mismatch = (%#v, %v)", mismatch, err)
	}
	state.mu.Lock()
	stored := state.completionLocked()
	state.mu.Unlock()
	if stored.RequiredAction != completion.RequiredAction || stored.AllowedToolCalls != 1 {
		t.Fatalf("planned allowance changed after wrong navigation: %#v", stored)
	}

	wrong, token = state.authorizeWithToken("search", "symbols", map[string]any{"query": "replace"})
	if wrong == nil || token != 0 {
		t.Fatalf("wrong planned anchor = (%#v, %d)", wrong, token)
	}
	blocked, token = state.authorizeWithToken("search", "symbols", map[string]any{"query": "replace_with"})
	if blocked != nil || token == 0 {
		t.Fatalf("exact planned recovery authorization = (%#v, %d)", blocked, token)
	}
	// The planned anchor was derived from evidence the page already retained, so
	// a hit on it corroborates and terminalizes. A weak page that agrees with
	// nothing keeps the second allowance instead.
	weak := []localizationDigestRow{{
		ID:   "crates/other/src/unrelated.rs::Unrelated.run",
		Name: "run", Kind: "method", File: "crates/other/src/unrelated.rs",
	}}
	completion = state.finishReservedReadTokenWithDigest(token, true, weak, true)
	if completion.State != localizationStateNeedsRecovery || completion.RequiredAction != "recover_once" ||
		completion.AllowedToolCalls != 1 || completion.Enforceable {
		t.Fatalf("weak planned result did not keep one further allowance: %#v", completion)
	}
}

func TestPlannedRecoveryEmptyResultReAllowsThenTerminatesUnconfirmed(t *testing.T) {
	state := newLocalizationTerminalState()
	state.armForTask(newLocalizationPlannedRecoveryCompletion("search.symbols", "transform_with"), "--multi-line with --transform duplicates output")
	blocked, token := state.authorizeWithToken("search", "symbols", map[string]any{"query": "transform_with"})
	if blocked != nil || token == 0 {
		t.Fatalf("planned recovery authorization = (%#v, %d)", blocked, token)
	}
	completion := state.finishReservedReadTokenWithDigest(token, true, nil, true)
	// An exact plan that returns nothing has disproved the plan, not the task:
	// the next allowance is unplanned so the caller can pick its own anchor.
	if completion.State != localizationStateNeedsRecovery || completion.RequiredAction != "recover_once" ||
		completion.AllowedToolCalls != 1 || completion.Enforceable {
		t.Fatalf("empty planned result did not keep one further allowance: %#v", completion)
	}

	blocked, token = state.authorizeWithToken("search", "symbols", map[string]any{"query": "--transform"})
	if blocked != nil || token == 0 {
		t.Fatalf("second recovery authorization = (%#v, %d)", blocked, token)
	}
	completion = state.finishReservedReadTokenWithDigest(token, true, nil, true)
	if completion.State != localizationStateAnswerReady || completion.Enforceable {
		t.Fatalf("spent recovery allowance did not terminate: %#v", completion)
	}
}

// A recovery page whose identities say nothing about the request can still be
// the right location — when it contributes a declaration the page did not have
// in a file the page already located. A row the page already ranked contributes
// nothing: re-returning it is the recovery agreeing with itself.
func TestRecoveryTerminalizesOnlyWhenItContributesAlignedNovelEvidence(t *testing.T) {
	ranked := captureTestRow("fixture/storage/flush.go::Storage.Flush", "fixture/storage/flush.go")
	retained := mergeLocalizationEvidenceDigest([]localizationDigestRow{ranked}, nil)
	tests := []struct {
		name  string
		row   localizationDigestRow
		state string
	}{
		{
			name: "novel declaration in a retained file",
			row: localizationDigestRow{
				ID: "fixture/storage/flush.go::Storage.helper", Name: "helper",
				Kind: "method", File: "fixture/storage/flush.go",
			},
			state: localizationStateAnswerReady,
		},
		{
			name:  "row the page already ranked",
			row:   ranked,
			state: localizationStateNeedsRecovery,
		},
		{
			name: "unrelated file",
			row: localizationDigestRow{
				ID: "fixture/other/pool.go::Pool.helper", Name: "helper",
				Kind: "method", File: "fixture/other/pool.go",
			},
			state: localizationStateNeedsRecovery,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newLocalizationTerminalState()
			completion := newLocalizationRecoveryCompletion()
			completion.digest = retained
			state.armForTask(completion, "Storage writes stall during commit")

			blocked, token := state.authorizeWithToken("search", "symbols", map[string]any{"query": "storage"})
			if blocked != nil || token == 0 {
				t.Fatalf("recovery authorization = (%#v, %d)", blocked, token)
			}
			result := state.finishReservedReadTokenWithDigest(token, true, []localizationDigestRow{test.row}, true)
			if result.State != test.state {
				t.Fatalf("recovery completion = %#v, want state %q", result, test.state)
			}
			if test.state == localizationStateAnswerReady &&
				!strings.HasPrefix(result.FinalResponse, localizationAnswerHeading) {
				t.Fatalf("corroborated terminal did not emit the proven page: %q", result.FinalResponse)
			}
		})
	}
}

// The page ranks several declarations from one file. A recovery that returns
// those same siblings has narrowed nothing, and terminalizing on them stops a
// session on a candidate the ranking had already offered and the caller had
// already passed over.
func TestRecoveryReturningOnlyRankedSiblingsDoesNotTerminalize(t *testing.T) {
	siblings := []localizationDigestRow{
		captureTestRow("fixture/storage/flush.go::Storage.Flush", "fixture/storage/flush.go"),
		captureTestRow("fixture/storage/flush.go::Storage.Sync", "fixture/storage/flush.go"),
	}
	baseline := mergeLocalizationEvidenceDigest(siblings, nil)
	newcomer := localizationDigestRow{
		ID: "fixture/storage/flush.go::Storage.drain", Name: "drain",
		Kind: "method", File: "fixture/storage/flush.go",
	}
	tests := []struct {
		name  string
		rows  []localizationDigestRow
		state string
	}{
		{name: "ranked siblings only", rows: siblings, state: localizationStateNeedsRecovery},
		{name: "one ranked sibling", rows: siblings[1:], state: localizationStateNeedsRecovery},
		{
			name:  "ranked siblings plus a novel declaration",
			rows:  append(append([]localizationDigestRow(nil), siblings...), newcomer),
			state: localizationStateAnswerReady,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newLocalizationTerminalState()
			completion := newLocalizationRecoveryCompletion()
			completion.digest = baseline
			state.armForTask(completion, "Storage writes stall during commit")

			blocked, token := state.authorizeWithToken("search", "symbols", map[string]any{"query": "storage"})
			if blocked != nil || token == 0 {
				t.Fatalf("recovery authorization = (%#v, %d)", blocked, token)
			}
			result := state.finishReservedReadTokenWithDigest(token, true, test.rows, true)
			if result.State != test.state {
				t.Fatalf("recovery completion = %#v, want state %q", result, test.state)
			}
		})
	}
}

// Rows an uncorroborated recovery surfaced are kept for the caller, but they
// must never become the evidence the next recovery corroborates against.
func TestASecondRecoveryCannotCorroborateItselfWithTheFirstResult(t *testing.T) {
	retained := mergeLocalizationEvidenceDigest([]localizationDigestRow{
		captureTestRow("fixture/storage/flush.go::Storage.Flush", "fixture/storage/flush.go"),
	}, nil)
	state := newLocalizationTerminalState()
	completion := newLocalizationRecoveryCompletion()
	completion.digest = retained
	state.armForTask(completion, "Storage writes stall during commit")

	unrelated := []localizationDigestRow{{
		ID: "fixture/other/pool.go::Pool.helper", Name: "helper",
		Kind: "method", File: "fixture/other/pool.go",
	}}
	for attempt, want := range []string{localizationStateNeedsRecovery, localizationStateAnswerReady} {
		blocked, token := state.authorizeWithToken("search", "symbols", map[string]any{"query": "storage"})
		if blocked != nil || token == 0 {
			t.Fatalf("recovery %d authorization = (%#v, %d)", attempt+1, blocked, token)
		}
		result := state.finishReservedReadTokenWithDigest(token, true, unrelated, true)
		if result.State != want {
			t.Fatalf("recovery %d completion = %#v, want state %q", attempt+1, result, want)
		}
		if want == localizationStateAnswerReady {
			if result.Enforceable || !strings.HasPrefix(result.FinalResponse, localizationProvisionalHeading) {
				t.Fatalf("self-corroborated recovery claimed proof: %#v", result)
			}
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !localizationDigestHasRow(state.digest, unrelated[0].ID) {
		t.Fatalf("uncorroborated recovery discarded what it surfaced: %#v", state.digest)
	}
}

// The two limits are different axes: a handler that never ran proves nothing
// about the evidence, so it must not cost a corroboration allowance.
func TestFailedRecoveryHandlerDoesNotSpendACorroborationAllowance(t *testing.T) {
	state := newLocalizationTerminalState()
	state.armForTask(newLocalizationRecoveryCompletion(), "Storage writes stall during commit")

	blocked, token := state.authorizeWithToken("search", "symbols", map[string]any{"query": "storage"})
	if blocked != nil || token == 0 {
		t.Fatalf("recovery authorization = (%#v, %d)", blocked, token)
	}
	if failed := state.finishReservedReadTokenWithDigest(token, false, nil, false); failed.State != localizationStateNeedsRecovery {
		t.Fatalf("failed recovery handler = %#v, want restored allowance", failed)
	}
	state.mu.Lock()
	remaining := state.recoveryAllowancesRemaining
	state.mu.Unlock()
	if remaining != localizationRecoveryAllowanceCap {
		t.Fatalf("failed handler spent %d corroboration allowances", localizationRecoveryAllowanceCap-remaining)
	}
}

func localizationDigestHasRow(digest *localizationEvidenceDigest, id string) bool {
	if digest == nil {
		return false
	}
	for _, row := range digest.Evidence {
		if row.ID == id {
			return true
		}
	}
	return false
}

func localizationRecoveryRequest(facade, operation string, arguments map[string]any) mcpgo.CallToolRequest {
	arguments["operation"] = operation
	return mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: facade, Arguments: arguments}}
}

func requireLocalizationResultStateEqual(
	t *testing.T,
	state *localizationTerminalState,
	result *mcpgo.CallToolResult,
	wantState string,
	wantTerminal bool,
	wantAllowed int,
) {
	t.Helper()
	if result == nil {
		t.Fatal("localization result is nil")
	}
	payload, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %T, want map", result.StructuredContent)
	}
	wire := decodeLocalizationCompletion(t, payload["completion"])
	terminal, ok := payload["terminal"].(bool)
	if !ok || terminal != wantTerminal {
		t.Fatalf("structured terminal = %#v, want %v", payload["terminal"], wantTerminal)
	}
	if wire.State != wantState || wire.AllowedToolCalls != wantAllowed {
		t.Fatalf("wire completion = %#v, want state=%q allowed=%d", wire, wantState, wantAllowed)
	}
	if wantState == localizationStateNeedsRecovery {
		if wire.RequiredAction != "recover_once" || len(wire.AllowedOperations) != len(localizationRecoveryOperations) {
			t.Fatalf("recovery completion is not directional/machine-readable: %#v", wire)
		}
		wantInstruction := newLocalizationRecoveryCompletion().Instruction
		if wire.Instruction != wantInstruction {
			t.Fatalf("recovery instruction = %q, want %q", wire.Instruction, wantInstruction)
		}
		// The instruction sends the caller to Gortex rather than to host tools, so
		// it must also name the move that works when the candidates are wrong.
		for _, required := range []string{
			`search(operation:"text" or "symbols"`,
			`read(operation:"file", target:{file:"<path>"})`,
			"Do not call host Read, Grep, Glob, Bash",
		} {
			if !strings.Contains(wire.Instruction, required) {
				t.Fatalf("recovery instruction is missing %q: %q", required, wire.Instruction)
			}
		}
	}

	if result.Meta == nil || result.Meta.AdditionalFields == nil {
		t.Fatal("localization host metadata is missing")
	}
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok {
		t.Fatalf("localization host metadata = %T, want localizationHostEnvelope", result.Meta.AdditionalFields[localizationHostMetaKey])
	}
	state.mu.Lock()
	stored := state.completionLocked()
	state.mu.Unlock()
	requireLocalizationCompletionJSONEqual(t, wire, host.Contract.Completion, "wire/meta")
	if wantState == localizationStateLocalized {
		if stored.State != localizationStateInactive {
			t.Fatalf("localized advisory did not release session state: %#v", stored)
		}
	} else {
		requireLocalizationCompletionJSONEqual(t, wire, stored, "wire/state")
	}
	if host.Contract.Terminal != wantTerminal {
		t.Fatalf("host terminal = %v, want %v", host.Contract.Terminal, wantTerminal)
	}
}

func decodeLocalizationCompletion(t *testing.T, value any) localizationCompletion {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	var completion localizationCompletion
	if err := json.Unmarshal(encoded, &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	return completion
}

func requireLocalizationCompletionJSONEqual(t *testing.T, left, right localizationCompletion, label string) {
	t.Helper()
	leftJSON, err := json.Marshal(left)
	if err != nil {
		t.Fatalf("marshal left %s completion: %v", label, err)
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		t.Fatalf("marshal right %s completion: %v", label, err)
	}
	if string(leftJSON) != string(rightJSON) {
		t.Fatalf("%s completion mismatch:\nwire=%s\nother=%s", label, leftJSON, rightJSON)
	}
}
