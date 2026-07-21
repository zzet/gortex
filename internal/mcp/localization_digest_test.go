package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func testEvidenceDigest() *localizationEvidenceDigest {
	return newLocalizationEvidenceDigest(localizationExploreEnvelope{
		Files:   []string{"repo/storage/disk.go", "repo/storage/cloud.go"},
		Symbols: []string{"repo/storage/disk.go::DiskStorage.Load", "repo/storage/cloud.go::CloudStorage.Load"},
		Evidence: []localizationEvidence{
			{Rank: 1, ID: "repo/storage/disk.go::DiskStorage.Load", Name: "Load", File: "repo/storage/disk.go", Line: 42, Signature: "func (s *DiskStorage) Load(key string) ([]byte, error)"},
			{Rank: 2, ID: "repo/storage/cloud.go::CloudStorage.Load", Name: "Load", File: "repo/storage/cloud.go", Line: 17},
		},
	})
}

func requireLocalizationTerminalReplay(t *testing.T, result *mcpgo.CallToolResult, facade, operation string) localizationTerminalContract {
	t.Helper()
	if result == nil || result.IsError {
		t.Fatalf("terminal result = %#v, want successful evidence replay", result)
	}
	text, ok := singleTextContent(result)
	if !ok || !strings.Contains(text, localizationReplayDirective) || !strings.Contains(text, "FILES:") {
		t.Fatalf("terminal result content = %#v, want one text block", result.Content)
	}
	var raw []byte
	switch structured := result.StructuredContent.(type) {
	case json.RawMessage:
		raw = structured
	default:
		var err error
		raw, err = json.Marshal(structured)
		if err != nil {
			t.Fatalf("encode terminal replay: %v", err)
		}
	}
	var payload localizationReplayWirePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode terminal replay %q: %v", raw, err)
	}
	if payload.FinalResponse == "" || payload.Directive != localizationReplayDirective {
		t.Fatalf("terminal replay = %#v", payload)
	}
	if strings.Contains(string(raw), `"facade"`) || strings.Contains(string(raw), `"operation"`) {
		t.Fatalf("terminal replay depends on attempted route %s/%s: %s", facade, operation, raw)
	}
	if result.Meta == nil || result.Meta.AdditionalFields == nil {
		t.Fatal("terminal replay omitted host metadata")
	}
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || !host.Replay || !host.Contract.Terminal ||
		host.Contract.Completion.State != localizationStateAnswerReady ||
		host.Contract.Completion.ContractVersion != localizationTerminalContractV2 ||
		host.Contract.Completion.AllowedToolCalls != 0 ||
		host.Contract.Completion.FinalResponse != payload.FinalResponse {
		t.Fatalf("terminal replay host envelope = %#v", result.Meta.AdditionalFields[localizationHostMetaKey])
	}
	if host.Evidence != nil {
		encoded, err := json.Marshal(host.Evidence)
		if err != nil || strings.Contains(string(encoded), `"source"`) {
			t.Fatalf("terminal replay leaked source bodies: %s (%v)", encoded, err)
		}
	}
	return host.Contract
}

func requireLocalizationTerminalError(t *testing.T, result *mcpgo.CallToolResult, facade, operation string) localizationTerminalContract {
	t.Helper()
	if result == nil || !result.IsError {
		t.Fatalf("terminal result = %#v, want typed MCP error", result)
	}
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("terminal result content = %#v, want one text block", result.Content)
	}
	var payload struct {
		ErrorCode ErrorCode `json:"error_code"`
		Message   string    `json:"message"`
		Retriable bool      `json:"retriable"`
		Data      struct {
			Contract  localizationTerminalContract `json:"contract"`
			Facade    string                       `json:"facade"`
			Operation string                       `json:"operation"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode terminal error %q: %v", text, err)
	}
	if payload.ErrorCode != ErrCodeLocalizationTerminal || payload.Message == "" || payload.Retriable {
		t.Fatalf("terminal error = %#v", payload)
	}
	if payload.Data.Facade != facade || payload.Data.Operation != operation {
		t.Fatalf("terminal error route = %q/%q, want %q/%q", payload.Data.Facade, payload.Data.Operation, facade, operation)
	}
	contract := payload.Data.Contract
	if !contract.Terminal || contract.Completion.State != localizationStateAnswerReady ||
		contract.Completion.ContractVersion != localizationTerminalContractV2 || contract.Completion.AllowedToolCalls != 0 {
		t.Fatalf("terminal contract = %#v", contract)
	}
	return contract
}

func TestPostTerminalNavigationReturnsIdenticalSuccessfulReplay(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")

	var canonical []byte
	for _, facade := range []string{"explore", "search", "read", "relations", "trace", "analyze"} {
		for repeat := 0; repeat < 3; repeat++ {
			result, reserved := state.authorize(facade, "any_operation", nil)
			if reserved {
				t.Fatalf("%s repeat %d reserved a handler call", facade, repeat)
			}
			contract := requireLocalizationTerminalReplay(t, result, facade, "any_operation")
			if contract.Completion.Enforceable {
				t.Fatalf("%s repeat %d unexpectedly upgraded advisory evidence", facade, repeat)
			}
			wire, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal %s result: %v", facade, err)
			}
			if canonical == nil {
				canonical = wire
			} else if !reflect.DeepEqual(wire, canonical) {
				t.Fatalf("%s repeat %d replay changed\nfirst: %s\nnext:  %s", facade, repeat, canonical, wire)
			}
			if !strings.Contains(string(wire), "repo/storage/disk.go") || strings.Contains(string(wire), `"source"`) {
				t.Fatalf("%s replay omitted retained evidence or leaked source: %s", facade, wire)
			}
		}
	}
	for _, facade := range []string{"change", "edit", "refactor", "workspace", "session", "recall", "remember", "capabilities"} {
		if result, reserved := state.authorize(facade, "any_operation", nil); result != nil || reserved {
			t.Fatalf("%s must remain dispatchable after answer_ready: result=%#v reserved=%v", facade, result, reserved)
		}
	}
}

func TestRepeatLocalizeAgainstTerminalContractReturnsReplay(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")

	token, blocked := state.beginLocalize("find the storage load implementations again", false)
	if token != 0 {
		t.Fatal("repeat localize must not reserve the handler slot")
	}
	requireLocalizationTerminalReplay(t, blocked, "explore", "localize")
}

func TestRefinementPromotionRetainsDigestForReplay(t *testing.T) {
	state := &localizationTerminalState{}
	candidate := "repo/storage/disk.go::DiskStorage.Load"
	state.armRefinementForTask("find the storage load implementations", candidate, []string{candidate}, testEvidenceDigest())

	args := map[string]any{"target": map[string]any{"symbol": candidate}}
	if blocked, reserved := state.authorize("read", "source", args); blocked != nil || !reserved {
		t.Fatalf("permitted refinement read = (%v, %v), want reservation", blocked, reserved)
	}
	state.finishReservedRead(true)

	terminalizing, reserved := state.authorize("search", "symbols", nil)
	if reserved {
		t.Fatal("post-promotion navigation reserved a handler")
	}
	// Advisory evidence gets one bounded recovery opportunity. Rejecting an
	// unsupported attempt is the transition into answer_ready and may remain a
	// typed error; every later navigation call must replay successfully.
	requireLocalizationTerminalError(t, terminalizing, "search", "symbols")
	terminal, reserved := state.authorize("search", "symbols", nil)
	if reserved {
		t.Fatal("post-terminal navigation reserved a handler")
	}
	requireLocalizationTerminalReplay(t, terminal, "search", "symbols")
	encoded, err := json.Marshal(terminal)
	if err != nil || !strings.Contains(string(encoded), "repo/storage/cloud.go") {
		t.Fatalf("promotion replay omitted retained evidence: %s (%v)", encoded, err)
	}
}

func TestRefinementAllowsOneAlternateRankedCandidateRead(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/storage/disk.go::DiskStorage.Load"
	alternate := "repo/storage/cloud.go::CloudStorage.Load"
	state.armRefinementRoutesForTask(
		"find the storage load implementations",
		preferred,
		[]string{preferred, alternate},
		map[string]localizationRefinementRoute{
			preferred: {enforceable: true},
			alternate: {enforceable: true},
		},
		testEvidenceDigest(),
	)

	if completion := state.completionLocked(); !strings.Contains(completion.RequiredAction, "recommended") ||
		!strings.Contains(completion.RequiredAction, "any returned candidate") {
		t.Fatalf("required action does not explain alternate candidate authorization: %q", completion.RequiredAction)
	}
	args := map[string]any{"target": map[string]any{"symbol": alternate}}
	if blocked, reserved := state.authorize("read", "source", args); blocked != nil || !reserved {
		t.Fatalf("alternate ranked candidate read = (%v, %v), want reservation", blocked, reserved)
	}
	state.finishReservedRead(true)
	if result, reserved := state.authorize("read", "source", args); reserved {
		t.Fatal("second read reserved a handler")
	} else {
		requireLocalizationTerminalReplay(t, result, "read", "source")
	}
}

func TestDigestLifecycleAndLegacyFallback(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")
	state.keepOpenForTask("new unrelated work")
	if blocked, _ := state.authorize("search", "symbols", nil); blocked != nil {
		t.Fatalf("inactive state must not block, got %+v", blocked)
	}
	if state.digest != nil {
		t.Fatal("keepOpenForTask must clear the retained digest")
	}

	withoutDigest := &localizationTerminalState{}
	withoutDigest.armForTask(newLocalizationCompletion(true, ""), "task without digest")
	blocked, _ := withoutDigest.authorize("search", "symbols", nil)
	requireLocalizationTerminalReplay(t, blocked, "search", "symbols")
}

func TestDigestByteCapShedsEvidenceTail(t *testing.T) {
	envelope := localizationExploreEnvelope{}
	for i := 0; i < 400; i++ {
		envelope.Evidence = append(envelope.Evidence, localizationEvidence{
			Rank:      i + 1,
			ID:        fmt.Sprintf("repo/big/file.go::Sym%03d", i),
			Name:      strings.Repeat("n", 40),
			File:      "repo/big/file.go",
			Line:      i,
			Signature: strings.Repeat("s", 2000),
		})
	}
	digest := newLocalizationEvidenceDigest(envelope)
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal digest: %v", err)
	}
	if len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("digest = %d bytes, want <= %d", len(encoded), localizationDigestMaxBytes)
	}
	if len(digest.Evidence) == 0 || len(digest.Evidence) >= localizationReplayEvidenceLimit {
		t.Fatalf("byte cap retained %d rows, want a non-empty shed prefix", len(digest.Evidence))
	}
	if !reflect.DeepEqual(digest.Files, []string{"repo/big/file.go"}) {
		t.Fatalf("files were not rebuilt from retained evidence: %#v", digest.Files)
	}
	if len(digest.Symbols) != len(digest.Evidence) {
		t.Fatalf("symbols=%d evidence=%d, want one supported symbol per row", len(digest.Symbols), len(digest.Evidence))
	}
}

func TestDigestByteCapRetainsSingleMandatoryRowAfterSheddingOptionalFields(t *testing.T) {
	envelope := localizationExploreEnvelope{Evidence: []localizationEvidence{{
		Rank:      1,
		ID:        "repo/registry.go::Registry.Configure",
		Name:      strings.Repeat("n", 1000),
		QualName:  strings.Repeat("q", 3000),
		Kind:      "method",
		File:      "repo/registry.go",
		Line:      17,
		Signature: strings.Repeat("s", 8000),
		Callers:   []string{strings.Repeat("caller", 1000)},
		Callees:   []string{strings.Repeat("callee", 1000)},
	}}}

	digest := newLocalizationEvidenceDigest(envelope)
	if len(digest.Evidence) != 1 {
		t.Fatalf("mandatory evidence rows = %d, want 1", len(digest.Evidence))
	}
	row := digest.Evidence[0]
	if row.ID != envelope.Evidence[0].ID || row.File != envelope.Evidence[0].File || row.Line != envelope.Evidence[0].Line {
		t.Fatalf("mandatory row identity changed while shedding: %#v", row)
	}
	if row.Signature != "" || row.QualName != "" {
		t.Fatalf("largest optional fields were retained after the digest fit: %#v", row)
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal digest: %v", err)
	}
	if len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("shed digest = %d bytes, want <= %d", len(encoded), localizationDigestMaxBytes)
	}
}

func TestPostTerminalReadsAreIntercepted(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")

	blocked, reserved := state.authorize("read", "source", map[string]any{
		"target": map[string]any{"symbol": "repo/storage/disk.go::DiskStorage.Load"},
	})
	if reserved {
		t.Fatal("post-terminal read reserved a handler")
	}
	requireLocalizationTerminalReplay(t, blocked, "read", "source")
}

func TestLocalizationDigestKeepsOnlyConcreteBoundedEvidence(t *testing.T) {
	envelope := localizationExploreEnvelope{
		Files:   []string{"pkg/unsupported.go", "pkg/a.go", "pkg/b.go", "pkg/c.go", "pkg/d.go", "pkg/e.go", "pkg/f.go"},
		Symbols: []string{"repo/pkg/unsupported.go::Unsupported", "repo/pkg/a.go::A", "repo/pkg/b.go::B", "repo/pkg/c.go::C", "repo/pkg/d.go::D", "repo/pkg/e.go::E", "repo/pkg/f.go::F"},
		Evidence: []localizationEvidence{
			{Rank: 1, ID: "repo/pkg/a.go::A", Name: "A", Kind: "function", File: "pkg/a.go", Line: 10, Callers: []string{"repo/pkg/caller.go::CallA"}},
			{Rank: 2, ID: "repo/pkg/b.go::B", Name: "B", Kind: "method", File: "pkg/b.go", Line: 20},
			{Rank: 3, ID: "repo/pkg/c.go::C", Name: "C", Kind: "method", File: "pkg/c.go", Line: 30},
			{Rank: 4, ID: "repo/pkg/d.go::D", Name: "D", Kind: "method", File: "pkg/d.go", Line: 40},
			{Rank: 5, ID: "repo/pkg/e.go::E", Name: "E", Kind: "method", File: "pkg/e.go", Line: 50},
			{Rank: 6, ID: "repo/pkg/f.go::F", Name: "F", Kind: "method", File: "pkg/f.go", Line: 60},
			{Rank: 7, ID: "repo/pkg/missing.go::Missing", Name: "Missing"},
		},
	}

	digest := newLocalizationEvidenceDigest(envelope)
	if len(digest.Evidence) != localizationReplayEvidenceLimit {
		t.Fatalf("retained evidence = %d, want %d", len(digest.Evidence), localizationReplayEvidenceLimit)
	}
	wantFiles := []string{"pkg/a.go", "pkg/b.go", "pkg/c.go", "pkg/d.go", "pkg/e.go"}
	wantSymbols := []string{"repo/pkg/a.go::A", "repo/pkg/b.go::B", "repo/pkg/c.go::C", "repo/pkg/d.go::D", "repo/pkg/e.go::E"}
	if !reflect.DeepEqual(digest.Files, wantFiles) {
		t.Fatalf("digest files = %#v, want %#v", digest.Files, wantFiles)
	}
	if !reflect.DeepEqual(digest.Symbols, wantSymbols) {
		t.Fatalf("digest symbols = %#v, want %#v", digest.Symbols, wantSymbols)
	}
	if row := digest.Evidence[0]; row.Name != "A" || row.File != "pkg/a.go" || row.Line != 10 {
		t.Fatalf("required evidence fields were dropped: %#v", row)
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal digest: %v", err)
	}
	encodedText := string(encoded)
	if strings.Contains(encodedText, "final_response") || strings.Contains(encodedText, "unsupported") ||
		strings.Contains(encodedText, `"callers"`) || strings.Contains(encodedText, `"callees"`) {
		t.Fatalf("digest retained unsupported or graph-expansion fields: %s", encoded)
	}
}

func TestLocalizationAnswerReadyResultCarriesBoundedReplayContract(t *testing.T) {
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	result := localizationAnswerReadyResult(completion)
	if result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("terminal result = %#v, want one successful text block", result)
	}
	visible, ok := singleTextContent(result)
	if !ok || !strings.Contains(visible, localizationReplayDirective) || !strings.Contains(visible, "FILES:") ||
		!strings.Contains(visible, "repo/storage/disk.go") || !strings.Contains(visible, "SYMBOLS:") ||
		!strings.Contains(visible, "EVIDENCE:") {
		t.Fatalf("terminal result text = %q", visible)
	}
	if len(visible) > len(localizationReplayDirective)+2+localizationFinalResponseMaxBytes {
		t.Fatalf("visible replay = %d bytes, want bounded final response", len(visible))
	}
	raw, ok := result.StructuredContent.(json.RawMessage)
	if !ok {
		t.Fatalf("structured terminal replay type = %T, want json.RawMessage", result.StructuredContent)
	}
	if !strings.HasPrefix(string(raw), `{"directive":`) {
		t.Fatalf("terminal replay does not lead with its actionable directive: %s", raw)
	}
	var payload localizationReplayWirePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode terminal replay: %v", err)
	}
	if payload.Directive != localizationReplayDirective || payload.FinalResponse == "" {
		t.Fatalf("structured terminal replay = %#v", payload)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 2 {
		t.Fatalf("model-visible replay must contain only actionable fields: %s (%v)", raw, err)
	}
	if result.Meta == nil || result.Meta.AdditionalFields == nil {
		t.Fatal("terminal result omitted host-only metadata")
	}
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || !host.Replay || host.Evidence == nil || len(host.Evidence.Evidence) != 2 ||
		host.FallbackFormat == "" || host.Evidence.Evidence[0].File != "repo/storage/disk.go" {
		t.Fatalf("host-only fallback envelope = %#v", result.Meta.AdditionalFields[localizationHostMetaKey])
	}
	if host.Contract.Terminal != localizationContractFor(completion).Terminal ||
		host.Contract.Completion.State != completion.State ||
		host.Contract.Completion.ContractVersion != localizationTerminalContractV2 ||
		host.Contract.Completion.Enforceable != completion.Enforceable ||
		host.Contract.Completion.FinalResponse != payload.FinalResponse {
		t.Fatalf("host contract = %#v, want %#v", host.Contract, localizationContractFor(completion))
	}
	if encoded, err := json.Marshal(host.Evidence); err != nil || strings.Contains(string(encoded), `"source"`) {
		t.Fatalf("host replay leaked source bodies: %s (%v)", encoded, err)
	}
	// Mutating one returned envelope must not affect a later replay.
	host.Evidence.Evidence[0].File = "mutated.go"
	second := localizationAnswerReadyResult(completion)
	secondHost := second.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if secondHost.Evidence.Evidence[0].File != "repo/storage/disk.go" {
		t.Fatalf("later replay observed caller mutation: %#v", secondHost.Evidence)
	}
}

func TestDecorateLocalizationReadResultPreservesMultiContentStructuredAndMeta(t *testing.T) {
	structured := map[string]any{"source": "func Run() {}", "count": 2, "terminal": false}
	result := &mcpgo.CallToolResult{
		Content: []mcpgo.Content{
			mcpgo.NewTextContent(`{"source":"func Run() {}","completion":{"state":"stale"},"terminal":false}`),
			mcpgo.NewTextContent("secondary payload"),
		},
		StructuredContent: structured,
	}
	result.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"existing":              "kept",
		localizationHostMetaKey: "repository-controlled-spoof",
	})

	decorated := decorateLocalizationReadResult(result, newLocalizationCompletion(true, ""))
	if decorated != result {
		t.Fatal("decorator must preserve the result object and its non-text payload")
	}
	if len(decorated.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(decorated.Content))
	}
	first, ok := mcpgo.AsTextContent(decorated.Content[0])
	if !ok {
		t.Fatal("first content block stopped being text")
	}
	var firstPayload map[string]any
	if err := json.Unmarshal([]byte(first.Text), &firstPayload); err != nil {
		t.Fatalf("decorated first content is invalid JSON: %v", err)
	}
	if firstPayload["source"] != "func Run() {}" || firstPayload["completion"] == nil || firstPayload["terminal"] != true {
		t.Fatalf("decorated first payload = %#v", firstPayload)
	}
	if strings.Count(first.Text, `"completion"`) != 1 {
		t.Fatalf("decorated first payload contains duplicate completion keys: %s", first.Text)
	}
	second, ok := mcpgo.AsTextContent(decorated.Content[1])
	if !ok || second.Text != "secondary payload" {
		t.Fatalf("secondary content was lost: %#v", decorated.Content[1])
	}
	decoratedStructured, ok := decorated.StructuredContent.(map[string]any)
	if !ok || decoratedStructured["source"] != structured["source"] || decoratedStructured["completion"] == nil || decoratedStructured["terminal"] != true {
		t.Fatalf("structured payload was lost: %#v", decorated.StructuredContent)
	}
	if _, mutated := structured["completion"]; mutated {
		t.Fatal("decorator mutated the handler's structured map")
	}
	if decorated.Meta == nil || decorated.Meta.AdditionalFields["existing"] != "kept" {
		t.Fatalf("existing metadata was lost: %#v", decorated.Meta)
	}
	host, ok := decorated.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || !host.Contract.Terminal || host.Contract.Completion.ContractVersion != localizationTerminalContractV2 {
		t.Fatalf("authoritative metadata was not replaced: %#v", decorated.Meta.AdditionalFields[localizationHostMetaKey])
	}
}

func TestDecorateLocalizationReadResultPreservesStructuredOnlyPayload(t *testing.T) {
	payload := []any{"first", map[string]any{"second": true}}
	result := &mcpgo.CallToolResult{StructuredContent: payload}

	decorated := decorateLocalizationReadResult(result, newLocalizationCompletion(true, ""))
	if len(decorated.Content) != 1 {
		t.Fatalf("completion content blocks = %d, want 1", len(decorated.Content))
	}
	completionText, ok := mcpgo.AsTextContent(decorated.Content[0])
	if !ok || !strings.Contains(completionText.Text, `"state":"answer_ready"`) {
		t.Fatalf("structured-only result omitted visible completion: %#v", decorated.Content)
	}
	wrapped, ok := decorated.StructuredContent.(map[string]any)
	if !ok || !reflect.DeepEqual(wrapped["payload"], payload) || wrapped["completion"] == nil {
		t.Fatalf("structured-only payload was lost: %#v", decorated.StructuredContent)
	}
}

func TestDecorateLocalizationReadResultMirrorsContentOnlyPayloadForHosts(t *testing.T) {
	completion := newLocalizationCompletion(true, "")

	plain := mcpgo.NewToolResultText("func Load() {}")
	decoratedPlain := decorateLocalizationReadResult(plain, completion)
	plainStructured, ok := decoratedPlain.StructuredContent.(map[string]any)
	if !ok || plainStructured["text"] != "func Load() {}" || plainStructured["completion"] == nil {
		t.Fatalf("plain content was not mirrored for structured-first hosts: %#v", decoratedPlain.StructuredContent)
	}
	plainText, ok := singleTextContent(decoratedPlain)
	if !ok || !strings.Contains(plainText, "func Load() {}") {
		t.Fatalf("plain content block was lost: %#v", decoratedPlain.Content)
	}

	multi := &mcpgo.CallToolResult{Content: []mcpgo.Content{
		mcpgo.NewTextContent("primary"),
		mcpgo.NewTextContent("secondary"),
	}}
	decoratedMulti := decorateLocalizationReadResult(multi, completion)
	multiStructured, ok := decoratedMulti.StructuredContent.(map[string]any)
	mirrored, mirroredOK := multiStructured["content"].([]mcpgo.Content)
	if !ok || !mirroredOK || len(mirrored) != 2 || multiStructured["completion"] == nil {
		t.Fatalf("multi-content payload was not mirrored: %#v", decoratedMulti.StructuredContent)
	}
	if len(decoratedMulti.Content) != 2 {
		t.Fatalf("multi-content blocks = %d, want 2", len(decoratedMulti.Content))
	}
}

func TestPermittedRefinementReadInvokesHandlerAndPreservesPayload(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "refinement_read_payload")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	if reply := srv.MCPServer().HandleMessage(ctx, initFrame); reply == nil {
		t.Fatal("initialize returned nil")
	}

	readSpec, ok := srv.facades.operation("read", "source")
	if !ok {
		t.Fatal("read.source facade operation is missing")
	}
	searchSpec, ok := srv.facades.operation("search", "symbols")
	if !ok {
		t.Fatal("search.symbols facade operation is missing")
	}

	const symbol = "repo/storage/disk.go::DiskStorage.Load"
	readCalls := 0
	searchCalls := 0
	srv.facades.capture(mcpgo.NewTool(readSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		readCalls++
		result := mcpgo.NewToolResultText(`{"source":"func (s *DiskStorage) Load() {}","language":"go"}`)
		result.Meta = mcpgo.NewMetaFromMap(map[string]any{"handler": "kept"})
		return result, nil
	})
	srv.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		searchCalls++
		return mcpgo.NewToolResultText("unexpected search result"), nil
	})
	srv.localizationFor(ctx).armRefinementRoutesForTask(
		"find the storage load implementation",
		symbol,
		[]string{symbol},
		map[string]localizationRefinementRoute{symbol: {enforceable: true}},
		testEvidenceDigest(),
	)

	readRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name: "read",
		Arguments: map[string]any{
			"operation": "source",
			"target":    map[string]any{"symbol": symbol},
		},
	}}
	result, err := srv.handleFacade(ctx, "read", readRequest)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("permitted refinement read = (%+v, %v)", result, err)
	}
	if readCalls != 1 {
		t.Fatalf("read handler calls = %d, want 1", readCalls)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(result.Content))
	}
	first, ok := mcpgo.AsTextContent(result.Content[0])
	if !ok || !strings.Contains(first.Text, `"source":"func (s *DiskStorage) Load() {}"`) ||
		!strings.Contains(first.Text, `"state":"answer_ready"`) {
		t.Fatalf("decorated first payload = %#v", result.Content[0])
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok || structured["source"] != "func (s *DiskStorage) Load() {}" ||
		structured["language"] != "go" || structured["completion"] == nil {
		t.Fatalf("structured source payload was lost: %#v", result.StructuredContent)
	}
	if result.Meta == nil || result.Meta.AdditionalFields["handler"] != "kept" {
		t.Fatalf("handler metadata was lost: %#v", result.Meta)
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal MCP result: %v", err)
	}
	var roundTrip struct {
		Content           []map[string]any `json:"content"`
		StructuredContent map[string]any   `json:"structuredContent"`
	}
	if err := json.Unmarshal(wire, &roundTrip); err != nil {
		t.Fatalf("unmarshal MCP result: %v", err)
	}
	if len(roundTrip.Content) != 1 || roundTrip.StructuredContent["source"] != "func (s *DiskStorage) Load() {}" ||
		roundTrip.StructuredContent["language"] != "go" || roundTrip.StructuredContent["completion"] == nil {
		t.Fatalf("wire result lost source payload: %s", wire)
	}

	searchRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name:      "search",
		Arguments: map[string]any{"operation": "  SyMbOlS  ", "query": "Load"},
	}}
	terminal, err := srv.handleFacade(ctx, "search", searchRequest)
	if err != nil {
		t.Fatalf("post-refinement navigation error = %v", err)
	}
	if searchCalls != 0 {
		t.Fatalf("search handler calls after answer_ready = %d, want 0", searchCalls)
	}
	requireLocalizationTerminalReplay(t, terminal, "search", "symbols")
}

func TestAnswerReadyNavigationDispatchNeverInvokesLegacyHandler(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "terminal_handler_intercept")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	if reply := srv.MCPServer().HandleMessage(ctx, initFrame); reply == nil {
		t.Fatal("initialize returned nil")
	}

	spec, ok := srv.facades.operation("search", "symbols")
	if !ok {
		t.Fatal("search.symbols facade operation is missing")
	}
	readSpec, ok := srv.facades.operation("read", "source")
	if !ok {
		t.Fatal("read.source facade operation is missing")
	}
	changeSpec, ok := srv.facades.operation("change", "detect")
	if !ok {
		t.Fatal("change.detect facade operation is missing")
	}
	handlerCalls := 0
	changeCalls := 0
	srv.facades.capture(mcpgo.NewTool(spec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		handlerCalls++
		return mcpgo.NewToolResultText(strings.Repeat("expensive", 10_000)), nil
	})
	srv.facades.capture(mcpgo.NewTool(readSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		handlerCalls++
		return mcpgo.NewToolResultText(strings.Repeat("source", 10_000)), nil
	})
	srv.facades.capture(mcpgo.NewTool(changeSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		changeCalls++
		return mcpgo.NewToolResultText(`{"changed":[]}`), nil
	})
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	srv.localizationFor(ctx).armForTask(completion, "find the storage load implementations")

	request := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name: "search",
		Arguments: map[string]any{
			"operation": "  SyMbOlS  ",
			"query":     "Load",
		},
	}}
	for repeat := 0; repeat < 10; repeat++ {
		result, err := srv.handleFacade(ctx, "search", request)
		if err != nil {
			t.Fatalf("repeat %d result = (%+v, %v)", repeat, result, err)
		}
		requireLocalizationTerminalReplay(t, result, "search", "symbols")
	}
	if handlerCalls != 0 {
		t.Fatalf("legacy handler invoked %d times after answer_ready", handlerCalls)
	}
	readRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name: "read",
		Arguments: map[string]any{
			"operation": "source",
			"target":    map[string]any{"symbol": "repo/storage/disk.go::DiskStorage.Load"},
		},
	}}
	readResult, err := srv.handleFacade(ctx, "read", readRequest)
	if err != nil {
		t.Fatalf("post-terminal read = (%+v, %v)", readResult, err)
	}
	requireLocalizationTerminalReplay(t, readResult, "read", "source")
	if handlerCalls != 0 {
		t.Fatalf("read handler invoked %d times after answer_ready", handlerCalls)
	}

	// The pre-validation gate also catches malformed recovery attempts instead
	// of spending a turn on schema errors.
	request.Params.Arguments = map[string]any{"operation": "not_an_operation"}
	result, err := srv.handleFacade(ctx, "search", request)
	if err != nil {
		t.Fatalf("malformed post-terminal request = (%+v, %v)", result, err)
	}
	requireLocalizationTerminalReplay(t, result, "search", "not_an_operation")

	changeRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name:      "change",
		Arguments: map[string]any{"operation": "detect"},
	}}
	changeResult, err := srv.handleFacade(ctx, "change", changeRequest)
	if err != nil || changeResult == nil || changeResult.IsError || changeCalls != 1 {
		t.Fatalf("post-terminal change.detect = (%+v, %v), calls=%d", changeResult, err, changeCalls)
	}
	if text, _ := singleTextContent(changeResult); strings.Contains(text, localizationReplayDirective) {
		t.Fatal("post-terminal change.detect was incorrectly intercepted")
	}

	// Capabilities has a dedicated handler and remains usable after localization.
	capabilitiesFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"capabilities","arguments":{}}}`)
	raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, capabilitiesFrame))
	if err != nil {
		t.Fatalf("marshal capabilities response: %v", err)
	}
	var called struct {
		Error  any                   `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &called); err != nil {
		t.Fatalf("decode capabilities response: %v", err)
	}
	if called.Error != nil || called.Result == nil || called.Result.IsError {
		t.Fatalf("terminal capabilities response = error %#v result %#v", called.Error, called.Result)
	}
	if text, _ := singleTextContent(called.Result); strings.Contains(text, localizationReplayDirective) {
		t.Fatalf("capabilities was incorrectly intercepted: %q", text)
	}
}
