package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestLocalizationFinalResponseIsBoundedDeterministicAndLineSafe(t *testing.T) {
	huge := strings.Repeat("界\n\t\x00", 2_000)
	digest := &localizationEvidenceDigest{
		Files:   []string{"repo/" + huge + ".go"},
		Symbols: []string{"repo/" + huge + ".go::Target"},
		Evidence: []localizationDigestRow{{
			Rank:      1,
			ID:        "repo/" + huge + ".go::Target",
			Name:      "Target\nInjected",
			File:      "repo/" + huge + ".go",
			Line:      42,
			Signature: "func\tTarget(" + huge + ")",
		}},
	}

	first := buildLocalizationFinalResponse(digest)
	second := buildLocalizationFinalResponse(digest)
	if first != second {
		t.Fatal("identical digests produced different final responses")
	}
	if len(first) > localizationFinalResponseMaxBytes {
		t.Fatalf("final response = %d bytes, want <= %d", len(first), localizationFinalResponseMaxBytes)
	}
	if !utf8.ValidString(first) {
		t.Fatal("final response contains partial UTF-8")
	}
	if strings.ContainsRune(first, '\x00') || strings.Contains(first, "\t") {
		t.Fatalf("final response retained control whitespace: %q", first)
	}
	for _, heading := range []string{"FILES:\n", "SYMBOLS:\n", "EVIDENCE:\n"} {
		if strings.Count(first, heading) != 1 {
			t.Fatalf("heading %q count = %d, want 1", heading, strings.Count(first, heading))
		}
	}
	compact := buildLocalizationFinalResponse(&localizationEvidenceDigest{
		Files:   []string{"repo/storage.go"},
		Symbols: []string{"repo/storage.go::Target"},
		Evidence: []localizationDigestRow{{
			Rank: 1, ID: "repo/storage.go::Target", Name: "Target\nInjected",
			File: "repo/storage.go", Line: 42, Signature: "func\tTarget()",
		}},
	})
	if !strings.Contains(compact, "Target Injected") || !strings.Contains(compact, "func Target()") {
		t.Fatalf("embedded whitespace was not compacted into the evidence row: %q", compact)
	}
}

func TestPostTerminalHostLoopConvergesFromSuccessfulReplay(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "terminal_replay_host_stub")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"stub-host","version":"1.0"}}}`)
	if reply := srv.MCPServer().HandleMessage(ctx, initFrame); reply == nil {
		t.Fatal("initialize returned nil")
	}

	readSpec, ok := srv.facades.operation("read", "source")
	if !ok {
		t.Fatal("read.source facade operation is missing")
	}
	legacyCalls := 0
	srv.facades.capture(mcpgo.NewTool(readSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		legacyCalls++
		return mcpgo.NewToolResultText(`{"source":"should not be reached"}`), nil
	})
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	srv.localizationFor(ctx).armForTask(completion, "find the storage load implementations")

	// The stub deliberately ignores answer_ready once and asks for source. Its
	// next action is determined solely from the replay's stable structured data.
	toolCalls := 0
	finalResponse := ""
	for finalResponse == "" && toolCalls < 2 {
		toolCalls++
		frame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read","arguments":{"operation":"source","target":{"symbol":"repo/storage/disk.go::DiskStorage.Load"}}}}`)
		raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, frame))
		if err != nil {
			t.Fatalf("marshal replay response: %v", err)
		}
		var called struct {
			Error  any                   `json:"error"`
			Result *mcpgo.CallToolResult `json:"result"`
		}
		if err := json.Unmarshal(raw, &called); err != nil {
			t.Fatalf("decode replay response: %v", err)
		}
		if called.Error != nil || called.Result == nil || called.Result.IsError {
			t.Fatalf("post-terminal tool response = error %#v result %#v", called.Error, called.Result)
		}
		structured, err := json.Marshal(called.Result.StructuredContent)
		if err != nil {
			t.Fatalf("marshal replay structuredContent: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(structured, &fields); err != nil {
			t.Fatalf("decode replay structuredContent %q: %v", structured, err)
		}
		if len(fields) != 2 || fields["directive"] == nil || fields["final_response"] == nil {
			t.Fatalf("replay structuredContent is not compact: %s", structured)
		}
		var payload localizationReplayWirePayload
		if err := json.Unmarshal(structured, &payload); err != nil {
			t.Fatalf("decode compact replay payload %q: %v", structured, err)
		}
		if payload.Directive != localizationReplayDirective || payload.FinalResponse == "" {
			t.Fatalf("replay payload does not direct convergence: %#v", payload)
		}
		if called.Result.Meta == nil || called.Result.Meta.AdditionalFields == nil {
			t.Fatal("replay omitted localization host metadata")
		}
		hostValue, ok := called.Result.Meta.AdditionalFields[localizationHostMetaKey]
		if !ok {
			t.Fatalf("replay metadata omitted %q", localizationHostMetaKey)
		}
		hostBody, err := json.Marshal(hostValue)
		if err != nil {
			t.Fatalf("marshal localization host metadata: %v", err)
		}
		var host localizationHostEnvelope
		if err := json.Unmarshal(hostBody, &host); err != nil {
			t.Fatalf("decode localization host metadata %q: %v", hostBody, err)
		}
		if !host.Replay || !host.Contract.Terminal ||
			host.Contract.Completion.State != localizationStateAnswerReady ||
			host.Contract.Completion.FinalResponse != payload.FinalResponse ||
			host.Evidence == nil || len(host.Evidence.Evidence) == 0 ||
			host.Evidence.Evidence[0].File != "repo/storage/disk.go" {
			t.Fatalf("replay host metadata is incomplete: %#v", host)
		}
		finalResponse = payload.FinalResponse
	}

	if toolCalls != 1 {
		t.Fatalf("stub needed %d post-terminal calls, want 1", toolCalls)
	}
	if legacyCalls != 0 {
		t.Fatalf("legacy read handler invoked %d times after answer_ready", legacyCalls)
	}
	if !strings.Contains(finalResponse, "FILES:") || !strings.Contains(finalResponse, "repo/storage/disk.go") ||
		!strings.Contains(finalResponse, "SYMBOLS:") || !strings.Contains(finalResponse, "EVIDENCE:") {
		t.Fatalf("stub final response is not answerable: %q", finalResponse)
	}
	if strings.Contains(finalResponse, "should not be reached") {
		t.Fatalf("stub final response leaked handler source: %q", finalResponse)
	}
}
