package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/telemetry"
	"go.uber.org/zap"
)

func TestFacadeRegistryCoversRegisteredLegacyCatalog(t *testing.T) {
	srv, _ := setupTestServer(t)
	var missing []string
	for _, descriptor := range srv.ToolDescriptors() {
		if isFacadeToolName(descriptor.Name) {
			continue
		}
		if len(srv.facades.byLegacy[descriptor.Name]) == 0 {
			missing = append(missing, descriptor.Name)
		}
	}
	require.Empty(t, missing, "every registered legacy tool must map into facade-v1")
	require.Len(t, srv.facades.byLegacy, 179, "facade-v1 migration table must cover the full legacy catalog")
	require.Len(t, facadeToolNames(), 21)
	for _, name := range facadeToolNames() {
		if name == "capabilities" {
			continue
		}
		require.NotEmpty(t, srv.facades.operations(name), "%s has no operations", name)
	}
}

func TestFacadeEffectBoundaryParity(t *testing.T) {
	registry := newFacadeRegistry()
	for _, facade := range facadeToolNames() {
		definition := facadeToolDefinition(facade)
		readOnly := definition.Annotations.ReadOnlyHint != nil && *definition.Annotations.ReadOnlyHint
		effects := registry.operations(facade)
		if facade == "capabilities" {
			require.True(t, readOnly)
			continue
		}
		require.NotEmpty(t, effects)
		class := effects[0].Effect
		for _, spec := range effects {
			require.Equal(t, class, spec.Effect, "%s mixes effect classes", facade)
			if spec.Effect == facadeEffectRead && daemon.IsMutating(spec.Legacy) && spec.Facade+"."+spec.Operation != "recall.surface" {
				t.Fatalf("read facade %s.%s routes durable writer %s", facade, spec.Operation, spec.Legacy)
			}
			if spec.Effect == facadeEffectRead && daemon.IsEffectful(spec.Legacy) {
				switch spec.Facade + "." + spec.Operation {
				case "change.simulate":
					require.Equal(t, false, spec.Fixed["keep"], "change.simulate must disable session persistence")
				case "recall.surface":
					require.Equal(t, false, spec.Fixed["mark_accessed"], "recall.surface must disable ranking mutation")
				default:
					t.Fatalf("read facade routes effectful legacy tool without an audited fixed-safe adapter: %s.%s -> %s", spec.Facade, spec.Operation, spec.Legacy)
				}
			}
			if spec.Effect == facadeEffectSessionWrite && daemon.IsMutating(spec.Legacy) {
				require.Equal(t, "overlay", spec.Facade, "session facade routes durable legacy writer without an audited fixed-safe adapter")
				require.Equal(t, "merge", spec.Operation, "session facade routes durable legacy writer without an audited fixed-safe adapter")
				require.Equal(t, false, spec.Fixed["to_disk"], "overlay.merge must disable disk application")
			}
			if spec.Facade == "edit" && spec.Operation == "wiki" {
				require.Equal(t, false, spec.Fixed["enhance"], "local edit.wiki must disable LLM egress")
			}
		}
		switch class {
		case facadeEffectRead:
			require.True(t, readOnly, facade)
			require.False(t, daemon.IsEffectful(facade), facade)
		case facadeEffectSessionWrite:
			require.False(t, readOnly, facade)
			require.True(t, daemon.IsEffectful(facade), facade)
			require.False(t, daemon.IsMutating(facade), facade)
		case facadeEffectLocalWrite, facadeEffectControlWrite, facadeEffectExternalWrite:
			require.False(t, readOnly, facade)
			require.True(t, daemon.IsMutating(facade), facade)
		default:
			t.Fatalf("unknown effect class %q", class)
		}
	}
}

func TestCompactToolsListIsStaticAndBudgeted(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "facade_budget")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"generic-harness","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))

	listFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	reply := srv.MCPServer().HandleMessage(ctx, listFrame)
	require.NotNil(t, reply)
	raw, err := json.Marshal(reply)
	require.NoError(t, err)
	t.Logf("compact tools/list: %d bytes", len(raw))
	serialized := strings.ToLower(string(raw))
	require.NotContains(t, serialized, "facade-v1")
	require.NotContains(t, serialized, "facade")
	require.LessOrEqual(t, len(raw), 15_000,
		"facade-v1 tools/list is %d bytes; compact the facade schemas before raising the ceiling", len(raw))

	var parsed struct {
		Result struct {
			Tools []mcpgo.Tool `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Len(t, parsed.Result.Tools, 21)
	foundExplore := false
	foundRead := false
	for _, tool := range parsed.Result.Tools {
		require.True(t, isFacadeToolName(tool.Name), "legacy tool leaked into facade-v1: %s", tool.Name)
		switch tool.Name {
		case "explore":
			foundExplore = true
			operation := tool.InputSchema.Properties["operation"].(map[string]any)
			require.Contains(t, operation["enum"], "localize")
			require.Contains(t, operation["enum"], "task")
			require.Contains(t, operation["description"], "Use localize")
			require.Contains(t, operation["description"], "Use task only")
		case "read":
			foundRead = true
			options := tool.InputSchema.Properties["options"].(map[string]any)
			require.Contains(t, options["description"], "new_user_task=true")
			require.Contains(t, options["description"], "read.file")
		}
	}
	require.True(t, foundExplore, "runtime tools/list omitted explore")
	require.True(t, foundRead, "runtime tools/list omitted read")
}

func TestFacadeSchemasAcceptUniversalCLIOutput(t *testing.T) {
	for _, name := range facadeToolNames() {
		tool := facadeToolDefinition(name)
		raw, err := json.Marshal(tool.InputSchema)
		require.NoError(t, err)
		var schema map[string]any
		require.NoError(t, json.Unmarshal(raw, &schema))
		properties, ok := schema["properties"].(map[string]any)
		require.True(t, ok, name)
		require.Contains(t, properties, "output", "%s must accept gortex call's universal --format shaping", name)
	}
}

func TestFacadeChangeSchemaAdvertisesSymbolTargets(t *testing.T) {
	tool := facadeToolDefinition("change")
	raw, err := json.Marshal(tool.InputSchema)
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	properties := schema["properties"].(map[string]any)
	target := properties["target"].(map[string]any)
	targetProperties := target["properties"].(map[string]any)
	require.Contains(t, targetProperties, "symbol")
	require.Contains(t, targetProperties, "symbols")
}

func TestIdentifiedClientsShareDefaultSurface(t *testing.T) {
	clients := make([]string, 0, len(knownAgentClients)+3)
	for client := range knownAgentClients {
		clients = append(clients, client)
	}
	// Product aliases resolved through host context must follow the same rule.
	clients = append(clients, "openai-codex", "Claude Code 1.4", "Visual Studio Code")
	sort.Strings(clients)

	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	wantTools := mapKeysAsSet(facadeToolNames())
	require.Len(t, wantTools, 21)
	for i, client := range clients {
		t.Run(client, func(t *testing.T) {
			sessionID := fmt.Sprintf("identified_client_%d", i)
			ctx := WithSessionID(context.Background(), sessionID)
			frame := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":%q,"version":"1.0"}}}`, i+1, client))
			reply := srv.MCPServer().HandleMessage(ctx, frame)
			require.NotNil(t, reply)
			raw, err := json.Marshal(reply)
			require.NoError(t, err)
			var parsed struct {
				Result struct {
					Instructions string `json:"instructions"`
				} `json:"result"`
			}
			require.NoError(t, json.Unmarshal(raw, &parsed))
			require.Equal(t, codingAgentInstructions, parsed.Result.Instructions)
			require.Equal(t, wantTools, listToolNamesForSession(t, srv, sessionID))
			policy := srv.effectiveSessionPolicy(ctx)
			require.Equal(t, FacadeSurfaceVersion, policy.preset)
			require.Equal(t, toolPolicyModeHide, policy.mode)
		})
	}

	unknownCtx := WithSessionID(context.Background(), "unknown_editor")
	unknownFrame := []byte(`{"jsonrpc":"2.0","id":99,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"some-editor","version":"1.0"}}}`)
	reply := srv.MCPServer().HandleMessage(unknownCtx, unknownFrame)
	require.NotNil(t, reply)
	raw, err := json.Marshal(reply)
	require.NoError(t, err)
	var parsed struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Equal(t, codingAgentInstructions, parsed.Result.Instructions)
	unknownTools := listToolNamesForSession(t, srv, "unknown_editor")
	require.Equal(t, wantTools, unknownTools)
	require.Equal(t, "", srv.resolveSessionFormat(unknownCtx),
		"unknown clients keep the JSON-safe wire format")

	// Until initialize provides a non-empty clientInfo.name, preserve the
	// server's global compatibility policy and instructions.
	anonymousTools := listToolNamesForSession(t, srv, "anonymous_client")
	require.True(t, anonymousTools["read_file"])
	require.False(t, anonymousTools["read"])
	require.Equal(t, serverInstructions, srv.stateAwareInstructionsForClient("", ""))
	require.Equal(t, serverInstructions, srv.stateAwareInstructionsForClient("", " \t "))
}

func TestFacadeDispatchReachesColdLegacyHandlerWithoutPromotion(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "facade_cold_dispatch")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))
	require.True(t, srv.lazy.IsDeferred("get_architecture"))

	callFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"analyze","arguments":{"kind":"architecture"}}}`)
	reply := srv.MCPServer().HandleMessage(ctx, callFrame)
	require.NotNil(t, reply)
	raw, err := json.Marshal(reply)
	require.NoError(t, err)
	var parsed struct {
		Error  any                   `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Nil(t, parsed.Error)
	require.NotNil(t, parsed.Result)
	require.False(t, parsed.Result.IsError)
	require.True(t, srv.lazy.IsDeferred("get_architecture"), "facade dispatch must not promote the legacy schema")
	require.Equal(t, listToolNamesForSession(t, srv, "facade_cold_dispatch"),
		mapKeysAsSet(facadeToolNames()), "facade dispatch must not change the static tools/list")

	helpFrame := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"analyze","arguments":{}}}`)
	raw, err = json.Marshal(srv.MCPServer().HandleMessage(ctx, helpFrame))
	require.NoError(t, err)
	parsed = struct {
		Error  any                   `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}{}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Nil(t, parsed.Error)
	require.NotNil(t, parsed.Result)
	require.False(t, parsed.Result.IsError, "omitted public analyze kind must return help")
}

func TestFacadeLegacyClientKeepsLegacySurfaceAndSchema(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "legacy_client")
	srv.NoteSessionToolPolicy("legacy_client", "core", "defer")
	before := listToolNamesForSession(t, srv, "legacy_client")

	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"some-editor","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))
	after := listToolNamesForSession(t, srv, "legacy_client")
	require.Equal(t, before, after, "an explicitly selected legacy preset must retain the core surface")
	require.True(t, after["read_file"])
	require.True(t, after["analyze"])
	for _, name := range facadeToolNames() {
		if isDedicatedFacadeTool(name) {
			require.Falsef(t, after[name], "dedicated facade %q leaked into a legacy tools/list", name)
		}
	}

	listFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, listFrame))
	require.NoError(t, err)
	var listed struct {
		Result struct {
			Tools []mcpgo.Tool `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &listed))
	var analyzeSchema map[string]any
	for _, tool := range listed.Result.Tools {
		if tool.Name != "analyze" {
			continue
		}
		schema, marshalErr := json.Marshal(tool.InputSchema)
		require.NoError(t, marshalErr)
		require.NoError(t, json.Unmarshal(schema, &analyzeSchema))
	}
	properties, ok := analyzeSchema["properties"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, properties, "algorithm", "legacy analyze must retain its full legacy schema")
	// The compact envelope containers are what would mark a swapped-in facade
	// schema; `target` no longer serves as that marker because the analyze
	// dispatcher genuinely accepts a target selector on every surface, and a
	// schema that hid it is what let a dropped target look like an answer.
	require.NotContains(t, properties, "options", "legacy analyze must not receive the facade schema")
	require.NotContains(t, properties, "output", "legacy analyze must not receive the facade schema")
	require.Contains(t, properties, "target", "legacy analyze must advertise the selector it honours")
	require.Contains(t, properties, "id", "legacy analyze must keep the field target lowers into")

	callFrame := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"analyze","arguments":{"kind":"help"}}}`)
	raw, err = json.Marshal(srv.MCPServer().HandleMessage(ctx, callFrame))
	require.NoError(t, err)
	var called struct {
		Error  any                   `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &called))
	require.Nil(t, called.Error)
	require.NotNil(t, called.Result)
	require.False(t, called.Result.IsError, "legacy analyze call shape must still reach the legacy handler: %#v", called.Result)
}

func TestDedicatedFacadeNamesRequireFacadeNegotiation(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "legacy_facade_gate")
	srv.NoteSessionToolPolicy("legacy_facade_gate", "core", "defer")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"some-editor","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))

	listed := listToolNamesForSession(t, srv, "legacy_facade_gate")
	for _, name := range facadeToolNames() {
		if !isDedicatedFacadeTool(name) {
			continue
		}
		require.Falsef(t, listed[name], "dedicated facade %q leaked into the legacy tools/list", name)
		require.Equal(t, "blocked", srv.sessionToolStatus(ctx, name), name)
		require.NotNilf(t, srv.checkFacadeSurfaceGate(ctx, name), "dedicated facade %q remained directly callable", name)
	}
	for _, reused := range []string{"analyze", "explore", "review"} {
		require.Nilf(t, srv.checkFacadeSurfaceGate(ctx, reused), "reused legacy name %q must remain compatible", reused)
	}
	// ask is reusable only when its optional LLM-backed legacy handler exists.
	require.False(t, listed["ask"])
	require.Equal(t, "blocked", srv.sessionToolStatus(ctx, "ask"))
	require.NotNil(t, srv.checkFacadeSurfaceGate(ctx, "ask"))

	// Exercise the actual globally-registered handler path: although `read`
	// exists process-wide for facade sessions, this legacy/defer session must
	// receive the same blocked result that tools/list and tool_profile report.
	callFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read","arguments":{"operation":"file","target":{"file":"main.go"}}}}`)
	raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, callFrame))
	require.NoError(t, err)
	var called struct {
		Error  any                   `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &called))
	if called.Error != nil {
		// mcp-go may enforce the session tools filter before entering the
		// registered handler. That protocol-level not-found is an equally hard
		// block; checkFacadeSurfaceGate above covers direct handler transports.
		require.Nil(t, called.Result)
		require.Contains(t, fmt.Sprint(called.Error), "tool not found")
	} else {
		require.NotNil(t, called.Result)
		require.True(t, called.Result.IsError)
		require.Contains(t, toolResultText(called.Result), ErrCodeToolBlockedByMode)
		require.Contains(t, toolResultText(called.Result), "GORTEX_TOOLS="+FacadeSurfaceVersion)
	}

	profileFrame := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tool_profile","arguments":{"tool":"read"}}}`)
	raw, err = json.Marshal(srv.MCPServer().HandleMessage(ctx, profileFrame))
	require.NoError(t, err)
	called = struct {
		Error  any                   `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}{}
	require.NoError(t, json.Unmarshal(raw, &called))
	require.Nil(t, called.Error)
	require.NotNil(t, called.Result)
	profile := unmarshalResult(t, called.Result)
	require.Equal(t, false, profile["enabled"])
	require.Equal(t, "blocked", profile["status"])

	srv.facades.capture(mcpgo.NewTool("ask"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("configured"), nil
	})
	require.Nil(t, srv.checkFacadeSurfaceGate(ctx, "ask"), "configured legacy ask must remain callable")
}

func TestFacadeCapabilitiesProtocolDoesNotChangeSurface(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "facade_capabilities")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))
	wantSurface := mapKeysAsSet(facadeToolNames())
	require.Equal(t, wantSurface, listToolNamesForSession(t, srv, "facade_capabilities"))

	callFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"capabilities","arguments":{"domain":"read","operation":"file","detail":"schema"}}}`)
	raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, callFrame))
	require.NoError(t, err)
	var called struct {
		Error  any                   `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &called))
	require.Nil(t, called.Error)
	require.NotNil(t, called.Result)
	require.False(t, called.Result.IsError)
	capability := unmarshalResult(t, called.Result)
	require.Equal(t, FacadeSurfaceVersion, capability["surface_version"])
	require.Equal(t, "file", capability["operation"])
	require.Equal(t, "read", capability["effect"])
	require.Equal(t, true, capability["available"])
	require.NotNil(t, capability["input_schema"])
	require.NotEmpty(t, capability["schema_hash"])

	exploreFrame := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"capabilities","arguments":{"domain":"explore"}}}`)
	exploreRaw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, exploreFrame))
	require.NoError(t, err)
	var explored struct {
		Error  any                   `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(exploreRaw, &explored))
	require.Nil(t, explored.Error)
	exploreDomain := unmarshalResult(t, explored.Result)
	operations := exploreDomain["operations"].([]any)
	summaries := make(map[string]string, len(operations))
	for _, rawOperation := range operations {
		operation := rawOperation.(map[string]any)
		summaries[operation["operation"].(string)] = operation["summary"].(string)
	}
	require.Contains(t, summaries, "localize")
	require.Contains(t, summaries["localize"], "stop navigation")
	require.Contains(t, summaries, "task")
	require.Contains(t, summaries["task"], "nonterminal")

	require.Equal(t, wantSurface, listToolNamesForSession(t, srv, "facade_capabilities"),
		"capability discovery must not promote schemas or mutate tools/list")
}

func TestFacadeHiddenLegacyCallsAreHardBlocked(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "facade_hidden_legacy")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))

	for id, test := range []struct {
		name string
		args string
	}{
		{name: "read_file", args: `{"path":"main.go"}`},
		{name: "tool_profile", args: `{"format":"json"}`},
		{name: LazyToolsSearchName, args: `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame := []byte(`{"jsonrpc":"2.0","id":` + fmt.Sprint(id+2) + `,"method":"tools/call","params":{"name":"` + test.name + `","arguments":` + test.args + `}}`)
			raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, frame))
			require.NoError(t, err)
			var parsed struct {
				Error  any                   `json:"error"`
				Result *mcpgo.CallToolResult `json:"result"`
			}
			require.NoError(t, json.Unmarshal(raw, &parsed))
			if parsed.Error != nil {
				require.Nil(t, parsed.Result)
				require.Contains(t, fmt.Sprint(parsed.Error), "tool not found",
					"the MCP surface filter may reject a hidden tool before its hard gate runs")
				return
			}
			require.NotNil(t, parsed.Result)
			require.True(t, parsed.Result.IsError, "hidden legacy tool must be blocked in facade-v1")
			require.Contains(t, parsed.Result.Content[0].(mcpgo.TextContent).Text, ErrCodeToolBlockedByMode)
		})
	}
}

func TestFacadeGateRecoveryGuidanceUsesCallableSurface(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	srv.NoteSessionClient("facade_guidance", "codex", "1")
	ctx := WithSessionID(context.Background(), "facade_guidance")

	preset := srv.checkToolPresetGate(ctx, "read_file")
	require.NotNil(t, preset)
	require.Contains(t, toolResultText(preset), "Call capabilities")
	require.NotContains(t, toolResultText(preset), "Call tool_profile")

	sess := srv.sessionFor(ctx)
	sess.mu.Lock()
	sess.planningMode = true
	sess.mu.Unlock()
	planning := srv.checkPlanningModeGate(ctx, "edit")
	require.NotNil(t, planning)
	require.Contains(t, toolResultText(planning), `session with operation \"planning_mode\"`)
	require.NotContains(t, toolResultText(planning), "Call set_planning_mode")

	sess.mu.Lock()
	sess.planningMode = false
	sess.workflow = &workflowState{phases: defaultWorkflowPhases(), mode: workflowModeBlock}
	sess.mu.Unlock()
	workflow := srv.checkWorkflowGate(ctx, "edit")
	require.NotNil(t, workflow)
	require.Contains(t, toolResultText(workflow), `session(operation=\"workflow\"`)

	// Legacy sessions retain the established recovery vocabulary.
	srv.NoteSessionToolPolicy("legacy_guidance", "core", "defer")
	srv.NoteSessionClient("legacy_guidance", "some-editor", "1")
	legacyCtx := WithSessionID(context.Background(), "legacy_guidance")
	legacySess := srv.sessionFor(legacyCtx)
	legacySess.mu.Lock()
	legacySess.planningMode = true
	legacySess.workflow = &workflowState{phases: defaultWorkflowPhases(), mode: workflowModeBlock}
	legacySess.mu.Unlock()
	require.Contains(t, toolResultText(srv.checkPlanningModeGate(legacyCtx, "edit_file")), "Call set_planning_mode")
	require.Contains(t, toolResultText(srv.checkWorkflowGate(legacyCtx, "edit_file")), `workflow action=\"advance\"`)
}

func mapKeysAsSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

func TestCodingAgentInstructionsStayTerseAndDirective(t *testing.T) {
	srv := &Server{}
	got := srv.stateAwareInstructionsForClient("", "generic-harness")
	require.Equal(t, codingAgentInstructions, got)
	require.Less(t, len(got), 550)
	for _, implementationTerm := range []string{"codex", "facade", "version", "preset", "tools/list", "tools_search"} {
		require.NotContains(t, strings.ToLower(got), implementationTerm)
	}
	for _, directive := range []string{"MUST use Gortex MCP", "First explicit-file read per new user request", `read(operation:"file", target:{file:"<path>"}, options:{new_user_task:true})`, "do not localize", "Unknown file/symbol/evidence", `explore(operation:"localize")`, "completion.required_action", "stop at answer_ready", "Diagnosis/change", `explore(operation:"task")`, `change(operation:"impact")`, "Mutate only via edit/refactor", `change(operation:"detect")`, "capabilities only if fields unknown"} {
		require.Contains(t, got, directive)
	}
}

func TestFacadeInitializeInstructionsFollowEffectivePolicy(t *testing.T) {
	initialize := func(t *testing.T, srv *Server, ctx context.Context, client string) string {
		t.Helper()
		frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"` + client + `","version":"1.0"}}}`)
		raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, frame))
		require.NoError(t, err)
		var parsed struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw, &parsed))
		return parsed.Result.Instructions
	}

	t.Run("codex_explicit_core_gets_legacy_instructions", func(t *testing.T) {
		srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
		srv.NoteSessionToolPolicy("codex_core", "core", "defer")
		ctx := WithSessionID(context.Background(), "codex_core")
		instructions := initialize(t, srv, ctx, "codex")
		require.Contains(t, instructions, "tools_search")
		names := listToolNamesForSession(t, srv, "codex_core")
		require.True(t, names["read_file"])
		require.False(t, names["read"])
	})

	t.Run("legacy_client_explicit_facade_gets_facade_instructions", func(t *testing.T) {
		srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
		srv.NoteSessionToolPolicy("editor_facade", FacadeSurfaceVersion, "hide")
		ctx := WithSessionID(context.Background(), "editor_facade")
		instructions := initialize(t, srv, ctx, "some-editor")
		require.Equal(t, codingAgentInstructions, instructions)
		names := listToolNamesForSession(t, srv, "editor_facade")
		require.True(t, names["read"])
		require.False(t, names["read_file"])
	})
}

func TestFacadePolicyResolutionPrecedence(t *testing.T) {
	previousProfile := activeInstructionPreset
	activeInstructionPreset = func() string { return "" }
	t.Cleanup(func() { activeInstructionPreset = previousProfile })

	initialize := func(t *testing.T, srv *Server, sessionID, client string) string {
		t.Helper()
		ctx := WithSessionID(context.Background(), sessionID)
		frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"` + client + `","version":"1.0"}}}`)
		raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, frame))
		require.NoError(t, err)
		var parsed struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw, &parsed))
		return parsed.Result.Instructions
	}

	t.Run("codex_host_alias_uses_facade", func(t *testing.T) {
		srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
		require.Equal(t, codingAgentInstructions, initialize(t, srv, "codex_alias", "openai-codex"))
		require.Equal(t, mapKeysAsSet(facadeToolNames()), listToolNamesForSession(t, srv, "codex_alias"))
	})

	t.Run("operator_pin_beats_codex_default", func(t *testing.T) {
		srv := setupPresetServer(t, ToolPolicyConfig{Preset: "nav", Mode: "hide"})
		instructions := initialize(t, srv, "codex_pinned", "codex")
		require.Contains(t, instructions, "tools_search")
		ctx := WithSessionID(context.Background(), "codex_pinned")
		policy := srv.effectiveSessionPolicy(ctx)
		require.Equal(t, "nav", policy.preset)
		require.Equal(t, "hide", policy.mode)
		names := listToolNamesForSession(t, srv, "codex_pinned")
		require.True(t, names["read_file"])
		require.False(t, names["read"])
		require.False(t, names["edit_file"])
	})

	t.Run("bare_forwarded_facade_defaults_hide", func(t *testing.T) {
		srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
		srv.NoteSessionToolPolicy("forwarded_facade", FacadeSurfaceVersion, "")
		require.Equal(t, codingAgentInstructions, initialize(t, srv, "forwarded_facade", "some-editor"))
		ctx := WithSessionID(context.Background(), "forwarded_facade")
		policy := srv.effectiveSessionPolicy(ctx)
		require.Equal(t, FacadeSurfaceVersion, policy.preset)
		require.Equal(t, "hide", policy.mode)
		require.Equal(t, mapKeysAsSet(facadeToolNames()), listToolNamesForSession(t, srv, "forwarded_facade"))
	})
}

func TestFacadeToolProfileReflectsPlanningMode(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "facade_planning")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))

	callMode := func(t *testing.T, id int, mode string) *mcpgo.CallToolResult {
		t.Helper()
		frame := []byte(`{"jsonrpc":"2.0","id":` + fmt.Sprint(id) + `,"method":"tools/call","params":{"name":"session","arguments":{"operation":"planning_mode","arguments":{"mode":"` + mode + `"}}}}`)
		raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, frame))
		require.NoError(t, err)
		var parsed struct {
			Error  any                   `json:"error"`
			Result *mcpgo.CallToolResult `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw, &parsed))
		require.Nil(t, parsed.Error)
		require.NotNil(t, parsed.Result)
		require.False(t, parsed.Result.IsError)
		return parsed.Result
	}

	callMode(t, 2, "planning")
	planning := listToolNamesForSession(t, srv, "facade_planning")
	require.False(t, planning["edit"], "durable mutation facades must leave tools/list in planning mode")
	require.True(t, planning["session"], "the session-only escape hatch must remain visible")
	require.Equal(t, "blocked", srv.sessionToolStatus(ctx, "edit"))
}

func TestFacadeDispatchNormalizesTargetAndEditAliases(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	captureArguments := func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultJSON(req.GetArguments())
	}
	srv.facades.capture(mcpgo.NewTool("read_file"), captureArguments)
	srv.facades.capture(mcpgo.NewTool("edit_symbol"), captureArguments)

	readReq := mcpgo.CallToolRequest{}
	readReq.Params.Name = "read"
	readReq.Params.Arguments = map[string]any{
		"operation": "file",
		"target":    map[string]any{"file": "internal/mcp/server.go"},
		"context":   map[string]any{"offset": 20, "limit": 10, "path": "must-not-win.go"},
		"output":    map[string]any{"format": "json"},
	}
	readResult, err := srv.handleFacade(context.Background(), "read", readReq)
	require.NoError(t, err)
	readArgs := unmarshalResult(t, readResult)
	require.Equal(t, "internal/mcp/server.go", readArgs["path"])
	require.Equal(t, float64(20), readArgs["offset"])
	require.Equal(t, float64(10), readArgs["limit"])
	require.Equal(t, "json", readArgs["format"])

	editReq := mcpgo.CallToolRequest{}
	editReq.Params.Name = "edit"
	editReq.Params.Arguments = map[string]any{
		"operation":   "symbol",
		"target":      map[string]any{"symbol": "internal/mcp/server.go::Server.addTool"},
		"match":       "old",
		"replacement": "new",
		"guard":       map[string]any{"base_sha": "abc"},
	}
	editResult, err := srv.handleFacade(context.Background(), "edit", editReq)
	require.NoError(t, err)
	editArgs := unmarshalResult(t, editResult)
	require.Equal(t, "internal/mcp/server.go::Server.addTool", editArgs["id"])
	require.Equal(t, "old", editArgs["old_source"])
	require.Equal(t, "new", editArgs["new_source"])
	require.Equal(t, "abc", editArgs["base_sha"])

	inferredRead := mcpgo.CallToolRequest{}
	inferredRead.Params.Arguments = map[string]any{"target": map[string]any{"file": "internal/mcp/server.go"}}
	readResult, err = srv.handleFacade(context.Background(), "read", inferredRead)
	require.NoError(t, err)
	require.Equal(t, "internal/mcp/server.go", unmarshalResult(t, readResult)["path"])

	inferredEdit := mcpgo.CallToolRequest{}
	inferredEdit.Params.Arguments = map[string]any{
		"target": map[string]any{"symbol": "internal/mcp/server.go::Server.addTool"},
		"match":  "old", "replacement": "new",
	}
	editResult, err = srv.handleFacade(context.Background(), "edit", inferredEdit)
	require.NoError(t, err)
	require.Equal(t, "internal/mcp/server.go::Server.addTool", unmarshalResult(t, editResult)["id"])
}

func TestFacadeOperationAliasesMatchLegacyContracts(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	captureArguments := func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultJSON(req.GetArguments())
	}
	for _, legacy := range []string{
		"search_ast", "winnow_symbols", "find_files", "search_symbols", "find_declaration",
		"flow_between", "trace_path", "taint_paths", "move_symbol", "batch_edit", "subscribe_diagnostics",
		"explain_change_impact", "get_test_targets", "check_guards", "get_edit_plan", "suggest_pattern",
		"verify_change", "get_diagnostics", "get_code_actions", "symbols_for_ranges", "preview_edit",
		"simulate_chain", "change_contract",
	} {
		srv.facades.capture(mcpgo.NewTool(legacy), captureArguments)
	}
	tests := []struct {
		facade string
		args   map[string]any
		want   map[string]any
	}{
		{"search", map[string]any{"operation": "ast", "query": "(call) @match"}, map[string]any{"pattern": "(call) @match"}},
		{"search", map[string]any{"operation": "winnow", "query": "load bearing"}, map[string]any{"text_match": "load bearing"}},
		{"relations", map[string]any{"operation": "declaration", "target": map[string]any{"query": "Serve("}}, map[string]any{"use_site": "Serve("}},
		{"trace", map[string]any{"operation": "flow", "target": map[string]any{"symbol": "a"}, "to": map[string]any{"symbol": "b"}}, map[string]any{"source_id": "a", "sink_id": "b"}},
		{"trace", map[string]any{"operation": "path", "target": map[string]any{"symbol": "a"}, "to": map[string]any{"symbol": "b"}}, map[string]any{"source_id": "a", "sink_id": "b"}},
		{"trace", map[string]any{"operation": "taint", "target": map[string]any{"query": "user.*"}, "to": map[string]any{"query": "exec.*"}}, map[string]any{"source_pattern": "user.*", "sink_pattern": "exec.*"}},
		{"refactor", map[string]any{"operation": "move", "target": map[string]any{"symbol": "pkg/a.go::A"}, "destination": "pkg/b.go"}, map[string]any{"id": "pkg/a.go::A", "target_file": "pkg/b.go"}},
		{"edit", map[string]any{"operation": "batch", "changes": []any{map[string]any{"op": "edit_file"}}}, map[string]any{"edits": []any{map[string]any{"op": "edit_file"}}}},
		{"change", map[string]any{"operation": "impact", "source": map[string]any{"symbols": []any{"a", "b"}}}, map[string]any{"ids": "a,b"}},
		{"change", map[string]any{"operation": "impact", "target": map[string]any{"symbol": "a"}}, map[string]any{"ids": "a"}},
		{"change", map[string]any{"operation": "impact", "target": map[string]any{"symbols": []any{"a", "b"}}}, map[string]any{"ids": "a,b"}},
		{"change", map[string]any{"operation": "impact", "source": map[string]any{"symbols": []any{"source"}}, "target": map[string]any{"symbol": "target"}}, map[string]any{"ids": "target"}},
		{"change", map[string]any{"operation": "impact", "source": map[string]any{"symbols": []any{"source"}}, "target": map[string]any{"symbols": []any{"target-a", "target-b"}}}, map[string]any{"ids": "target-a,target-b"}},
		{"change", map[string]any{"operation": "tests", "source": map[string]any{"symbols": []any{"a", "b"}}}, map[string]any{"ids": "a,b"}},
		{"change", map[string]any{"operation": "guards", "source": map[string]any{"symbols": []any{"a", "b"}}}, map[string]any{"ids": "a,b"}},
		{"change", map[string]any{"operation": "edit_plan", "source": map[string]any{"symbols": []any{"a", "b"}}}, map[string]any{"ids": "a,b"}},
		{"change", map[string]any{"operation": "pattern", "source": map[string]any{"symbols": []any{"a"}}}, map[string]any{"id": "a"}},
		{"change", map[string]any{"operation": "verify", "source": map[string]any{"changes": []any{map[string]any{"symbol_id": "a", "new_signature": "A()"}}}}, map[string]any{"changes": `[{"new_signature":"A()","symbol_id":"a"}]`}},
		{"change", map[string]any{"operation": "diagnostics", "source": map[string]any{"file": "a.go"}}, map[string]any{"path": "a.go"}},
		{"change", map[string]any{"operation": "code_actions", "source": map[string]any{"file": "a.go", "range": map[string]any{"start_line": 2, "end_line": 4}}}, map[string]any{"path": "a.go", "start_line": float64(2), "end_line": float64(4)}},
		{"change", map[string]any{"operation": "ranges", "source": map[string]any{"file": "a.go", "range": map[string]any{"start_line": 2, "end_line": 4}}}, map[string]any{"path": "a.go", "start_line": float64(2), "end_line": float64(4)}},
		{"change", map[string]any{"operation": "preview", "source": map[string]any{"workspace_edit": map[string]any{"changes": map[string]any{}}}}, map[string]any{"workspace_edit": `{"changes":{}}`}},
		{"change", map[string]any{"operation": "simulate", "source": map[string]any{"steps": []any{map[string]any{"changes": map[string]any{}}}}}, map[string]any{"steps": `[{"changes":{}}]`, "keep": false}},
		{"change", map[string]any{"operation": "contract", "source": map[string]any{"symbols": []any{"a", "b"}, "ranges": []any{map[string]any{"file": "a.go", "start_line": 2}}}}, map[string]any{"symbols": "a,b", "ranges": `[{"file":"a.go","start_line":2}]`}},
	}
	for _, test := range tests {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = test.args
		result, err := srv.handleFacade(context.Background(), test.facade, req)
		require.NoError(t, err)
		got := unmarshalResult(t, result)
		for key, want := range test.want {
			require.Equal(t, want, got[key], "%s.%v alias %s", test.facade, test.args["operation"], key)
		}
	}
	globOnly := mcpgo.CallToolRequest{}
	globOnly.Params.Arguments = map[string]any{"operation": "files", "options": map[string]any{"glob": "*_test.go"}}
	result, err := srv.handleFacade(context.Background(), "search", globOnly)
	require.NoError(t, err)
	require.False(t, result.IsError, "search.files must support a glob without query")

	missingQuery := mcpgo.CallToolRequest{}
	missingQuery.Params.Arguments = map[string]any{"operation": "symbols"}
	result, err = srv.handleFacade(context.Background(), "search", missingQuery)
	require.NoError(t, err)
	require.True(t, result.IsError, "search.symbols must validate its required query")

	subscribe := mcpgo.CallToolRequest{}
	subscribe.Params.Arguments = map[string]any{"operation": "subscribe", "channel": "diagnostics"}
	result, err = srv.handleFacade(context.Background(), "session", subscribe)
	require.NoError(t, err)
	require.False(t, result.IsError, "session subscribe+channel must resolve to the canonical operation")
}

func TestFacadeFreshnessUsesCanonicalLegacyRequest(t *testing.T) {
	srv, root := setupTestServer(t)
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(root, "main.go"), future, future))

	srv.facades.capture(mcpgo.NewTool("read_file"), func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText(`{"content":"package main"}`), nil
	})
	readReq := mcpgo.CallToolRequest{}
	readReq.Params.Arguments = map[string]any{
		"operation": "file",
		"target":    map[string]any{"file": "main.go"},
	}
	result, err := srv.handleFacade(context.Background(), "read", readReq)
	require.NoError(t, err)
	readPayload := unmarshalResult(t, result)
	readFreshness, ok := readPayload["freshness"].(map[string]any)
	require.True(t, ok, "facade file read must carry the legacy freshness rider")
	require.Equal(t, true, readFreshness["stale"])
	require.Equal(t, "main.go", readFreshness["file"])

	srv.facades.capture(mcpgo.NewTool("search_symbols"), func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText(`{"results":[{"name":"main","file":"main.go"}]}`), nil
	})
	searchReq := mcpgo.CallToolRequest{}
	searchReq.Params.Arguments = map[string]any{"operation": "symbols", "query": "main"}
	result, err = srv.handleFacade(context.Background(), "search", searchReq)
	require.NoError(t, err)
	searchPayload := unmarshalResult(t, result)
	searchFreshness, ok := searchPayload["freshness"].(map[string]any)
	require.True(t, ok, "facade list query must run the canonical legacy freshness sweep")
	require.Len(t, searchFreshness["stale_files"], 1)
}

func TestFacadeSessionCanExitPlanningMode(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "facade_planning_recovery")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))

	call := func(id int, mode string) *mcpgo.CallToolResult {
		frame := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"session","arguments":{"operation":"planning_mode","mode":%q}}}`, id, mode))
		reply := srv.MCPServer().HandleMessage(ctx, frame)
		require.NotNil(t, reply)
		raw, err := json.Marshal(reply)
		require.NoError(t, err)
		var parsed struct {
			Error  any                   `json:"error"`
			Result *mcpgo.CallToolResult `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw, &parsed))
		require.Nil(t, parsed.Error)
		require.NotNil(t, parsed.Result)
		return parsed.Result
	}
	require.False(t, call(2, "planning").IsError)
	planning := listToolNamesForSession(t, srv, "facade_planning_recovery")
	require.True(t, planning["session"], "session recovery facade must remain visible in planning mode")
	require.False(t, planning["edit"])
	require.False(t, planning["workspace_admin"])

	require.False(t, call(3, "editing").IsError)
	editing := listToolNamesForSession(t, srv, "facade_planning_recovery")
	require.True(t, editing["edit"])
	require.True(t, editing["workspace_admin"])
}

func TestControlToolCannotBypassFacadeHideGate(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	called := false
	srv.addControlTool(mcpgo.NewTool("overlay_merge"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		called = true
		return mcpgo.NewToolResultText("should not run"), nil
	})
	ctx := WithSessionID(context.Background(), "facade_direct_legacy_gate")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))
	frame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"overlay_merge","arguments":{"from":"branch","to_disk":true}}}`)
	reply := srv.MCPServer().HandleMessage(ctx, frame)
	require.NotNil(t, reply)
	require.False(t, called, "direct legacy writer must be blocked before its handler runs")
	_, captured := srv.facades.legacy("overlay_merge")
	require.True(t, captured, "control-tool registration must capture optional legacy handlers for facade dispatch")
}

func TestFacadeReadOnlyOperationsCannotEnablePersistence(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	analyzeCalled := false
	captureArguments := func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if req.Params.Name == "analyze" {
			analyzeCalled = true
		}
		return mcpgo.NewToolResultJSON(req.GetArguments())
	}
	srv.facades.capture(mcpgo.NewTool("simulate_chain"), captureArguments)
	srv.facades.capture(mcpgo.NewTool("analyze"), captureArguments)
	srv.facades.capture(mcpgo.NewTool("overlay_merge"), captureArguments)
	srv.facades.capture(mcpgo.NewTool("generate_wiki"), captureArguments)
	srv.facades.capture(mcpgo.NewTool("surface_memories"), captureArguments)
	srv.facades.capture(mcpgo.NewTool("search_symbols"), captureArguments)
	srv.facades.capture(mcpgo.NewTool("change_contract"), captureArguments)
	srv.facades.capture(mcpgo.NewTool("find_co_changing_symbols"), captureArguments)

	simulate := mcpgo.CallToolRequest{}
	simulate.Params.Arguments = map[string]any{
		"operation": "simulate",
		"options":   map[string]any{"keep": true},
	}
	result, err := srv.handleFacade(context.Background(), "change", simulate)
	require.NoError(t, err)
	args := unmarshalResult(t, result)
	require.Equal(t, false, args["keep"], "fixed read-only guard must override user keep=true")

	overlayMerge := mcpgo.CallToolRequest{}
	overlayMerge.Params.Arguments = map[string]any{"operation": "merge", "options": map[string]any{"to_disk": true}}
	result, err = srv.handleFacade(context.Background(), "overlay", overlayMerge)
	require.NoError(t, err)
	args = unmarshalResult(t, result)
	require.Equal(t, false, args["to_disk"], "session overlay merge must never write to disk")

	applyOverlay := mcpgo.CallToolRequest{}
	applyOverlay.Params.Arguments = map[string]any{"operation": "apply_overlay", "options": map[string]any{"to_disk": false}}
	result, err = srv.handleFacade(context.Background(), "edit", applyOverlay)
	require.NoError(t, err)
	args = unmarshalResult(t, result)
	require.Equal(t, true, args["to_disk"], "edit.apply_overlay must always cross the disk-write boundary")

	wiki := mcpgo.CallToolRequest{}
	wiki.Params.Arguments = map[string]any{"operation": "wiki", "options": map[string]any{"enhance": true}}
	result, err = srv.handleFacade(context.Background(), "edit", wiki)
	require.NoError(t, err)
	args = unmarshalResult(t, result)
	require.Equal(t, false, args["enhance"], "compact edit.wiki must not cross the LLM/open-world boundary")

	recall := mcpgo.CallToolRequest{}
	recall.Params.Arguments = map[string]any{"operation": "surface", "arguments": map[string]any{"mark_accessed": true}}
	result, err = srv.handleFacade(context.Background(), "recall", recall)
	require.NoError(t, err)
	args = unmarshalResult(t, result)
	require.Equal(t, false, args["mark_accessed"], "read-only recall must not mutate future memory ranking")

	search := mcpgo.CallToolRequest{}
	search.Params.Arguments = map[string]any{
		"operation": "symbols", "query": "where is authentication handled",
		"options": map[string]any{"assist": "deep"},
	}
	result, err = srv.handleFacade(context.Background(), "search", search)
	require.NoError(t, err)
	args = unmarshalResult(t, result)
	require.Equal(t, "off", args["assist"], "local search must not invoke an LLM")

	contract := mcpgo.CallToolRequest{}
	contract.Params.Arguments = map[string]any{
		"operation": "contract",
		"source":    map[string]any{"source": "symbols", "symbols": []any{"a.go::A"}},
		"options":   map[string]any{"ack": true},
	}
	result, err = srv.handleFacade(context.Background(), "change", contract)
	require.NoError(t, err)
	args = unmarshalResult(t, result)
	require.Equal(t, false, args["ack"], "read-only change.contract must not persist a risk acknowledgement")

	riskAck := mcpgo.CallToolRequest{}
	riskAck.Params.Arguments = map[string]any{
		"operation": "risk_ack",
		"arguments": map[string]any{"source": "symbols", "symbols": "a.go::A", "ack": false},
	}
	result, err = srv.handleFacade(context.Background(), "remember", riskAck)
	require.NoError(t, err)
	args = unmarshalResult(t, result)
	require.Equal(t, true, args["ack"], "remember.risk_ack must always take the durable acknowledgement path")

	coChange := mcpgo.CallToolRequest{}
	coChange.Params.Arguments = map[string]any{
		"kind": "co_change", "target": map[string]any{"symbol": "a.go::A"},
		"options": map[string]any{"refresh": true},
	}
	result, err = srv.handleFacade(context.Background(), "analyze", coChange)
	require.NoError(t, err)
	args = unmarshalResult(t, result)
	require.Equal(t, false, args["refresh"], "compact co-change lookup must not start a durable mine")

	for kind, flag := range map[string]string{"concepts": "use_llm", "impact": "refresh_cochange", "sql_call_sites": "materialize"} {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"kind": kind, "options": map[string]any{flag: true}}
		result, err = srv.handleFacade(context.Background(), "analyze", req)
		require.NoError(t, err)
		args = unmarshalResult(t, result)
		require.Equal(t, kind, args["kind"])
		require.Equal(t, false, args[flag], "analyze.%s must keep its public read-only posture", kind)
	}

	normalizedKind := mcpgo.CallToolRequest{}
	normalizedKind.Params.Arguments = map[string]any{"kind": "dead-code"}
	result, err = srv.handleFacade(context.Background(), "analyze", normalizedKind)
	require.NoError(t, err)
	args = unmarshalResult(t, result)
	require.Equal(t, "dead_code", args["kind"], "the normalized public kind must be fixed before legacy dispatch")

	defaultHelp := mcpgo.CallToolRequest{}
	result, err = srv.handleFacade(context.Background(), "analyze", defaultHelp)
	require.NoError(t, err)
	args = unmarshalResult(t, result)
	require.Equal(t, "help", args["kind"], "omitted kind must select the safe help operation")

	for _, kind := range adminAnalyzeKinds {
		admin := mcpgo.CallToolRequest{}
		admin.Params.Arguments = map[string]any{
			"operation": kind,
			"arguments": map[string]any{"kind": "hotspots"},
		}
		result, err = srv.handleFacade(context.Background(), "workspace_admin", admin)
		require.NoError(t, err)
		args = unmarshalResult(t, result)
		require.Equal(t, kind, args["kind"], "effect-safe admin routing must fix kind=%s", kind)
	}

	for _, kind := range adminAnalyzeKinds {
		analyzeCalled = false
		nestedBypass := mcpgo.CallToolRequest{}
		nestedBypass.Params.Arguments = map[string]any{
			"options": map[string]any{"kind": kind},
		}
		result, err = srv.handleFacade(context.Background(), "analyze", nestedBypass)
		require.NoError(t, err)
		require.True(t, result.IsError)
		require.False(t, analyzeCalled, "nested %s must be rejected before the read-only analyze handler runs", kind)
	}

	ambiguous := mcpgo.CallToolRequest{}
	ambiguous.Params.Arguments = map[string]any{
		"operation": "source",
		"target": map[string]any{
			"file": "a.go", "symbol": "a.go::A",
		},
	}
	result, err = srv.handleFacade(context.Background(), "read", ambiguous)
	require.NoError(t, err)
	require.True(t, result.IsError)
}

func TestFacadeCapabilitiesReturnsOperationSchema(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	legacy := mcpgo.NewTool("read_file", mcpgo.WithString("path", mcpgo.Required()))
	srv.facades.capture(legacy, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	})
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"domain": "read", "operation": "file", "detail": "schema"}
	result, err := srv.handleCapabilities(context.Background(), req)
	require.NoError(t, err)
	out := unmarshalResult(t, result)
	require.Equal(t, FacadeSurfaceVersion, out["surface_version"])
	require.Equal(t, "read", out["domain"])
	require.Equal(t, "file", out["operation"])
	require.Equal(t, true, out["available"])
	require.NotEmpty(t, out["schema_hash"])
	inputSchema, ok := out["input_schema"].(map[string]any)
	require.True(t, ok)
	schemaProperties := inputSchema["properties"].(map[string]any)
	options := schemaProperties["options"].(map[string]any)
	optionProperties := options["properties"].(map[string]any)
	boundary := optionProperties["new_user_task"].(map[string]any)
	require.Equal(t, "boolean", boundary["type"])
	require.Contains(t, boundary["description"], "first read.file")
	requestShape, ok := out["request_shape"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "file", requestShape["operation"])
	require.Equal(t, map[string]any{"file": "<file>"}, requestShape["target"])
}

func TestFacadeCapabilitiesChangeImpactUsesPublicTargetSchema(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	legacy := mcpgo.NewTool("explain_change_impact",
		mcpgo.WithString("ids", mcpgo.Required()),
		mcpgo.WithBoolean("summary_only"),
		mcpgo.WithNumber("offset"),
		mcpgo.WithNumber("limit"),
		mcpgo.WithString("format"),
		mcpgo.WithNumber("max_bytes"),
	)
	srv.facades.capture(legacy, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	})
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"domain": "change", "operation": "impact", "detail": "schema"}
	result, err := srv.handleCapabilities(context.Background(), req)
	require.NoError(t, err)
	out := unmarshalResult(t, result)

	schema := out["input_schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	require.NotContains(t, properties, "ids")
	require.Equal(t, "impact", properties["operation"].(map[string]any)["const"])
	require.ElementsMatch(t, []any{"operation", "target"}, schema["required"].([]any))

	target := properties["target"].(map[string]any)
	targetProperties := target["properties"].(map[string]any)
	require.Contains(t, targetProperties, "symbol")
	require.Contains(t, targetProperties, "symbols")
	require.Equal(t, float64(1), target["minProperties"])
	require.Equal(t, float64(1), target["maxProperties"])
	require.Equal(t, false, target["additionalProperties"])

	outputProperties := properties["output"].(map[string]any)["properties"].(map[string]any)
	for _, field := range []string{"summary_only", "offset", "limit", "format", "max_bytes"} {
		require.Contains(t, outputProperties, field)
	}
	requestShape := out["request_shape"].(map[string]any)
	require.Equal(t, map[string]any{"symbol": "<symbol>"}, requestShape["target"])
}

func TestFacadeCapabilitiesRequestShapesUsePublicMutationFields(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	handler := func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	srv.facades.capture(mcpgo.NewTool("edit_file",
		mcpgo.WithString("path", mcpgo.Required()),
		mcpgo.WithString("old_string", mcpgo.Required()),
		mcpgo.WithString("new_string", mcpgo.Required()),
		mcpgo.WithBoolean("dry_run"),
	), handler)
	srv.facades.capture(mcpgo.NewTool("rename_symbol",
		mcpgo.WithString("id", mcpgo.Required()),
		mcpgo.WithString("new_name", mcpgo.Required()),
		mcpgo.WithBoolean("dry_run"),
	), handler)
	srv.facades.capture(mcpgo.NewTool("find_files"), handler)
	srv.facades.capture(mcpgo.NewTool("api_impact"), handler)
	srv.facades.capture(mcpgo.NewTool("explain_change_impact",
		mcpgo.WithString("ids", mcpgo.Required()),
	), handler)
	srv.facades.capture(mcpgo.NewTool("symbols_for_ranges"), handler)
	srv.facades.capture(mcpgo.NewTool("batch_edit",
		mcpgo.WithArray("edits", mcpgo.Required()),
	), handler)
	srv.facades.capture(mcpgo.NewTool("context_closure"), handler)
	srv.facades.capture(mcpgo.NewTool("verify_citation"), handler)
	srv.facades.capture(mcpgo.NewTool("find_co_changing_symbols"), handler)
	srv.facades.capture(mcpgo.NewTool("generate_skill"), handler)

	requestShape := func(domain, operation string) map[string]any {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"domain": domain, "operation": operation, "detail": "schema"}
		result, err := srv.handleCapabilities(context.Background(), req)
		require.NoError(t, err)
		out := unmarshalResult(t, result)
		requestShape, ok := out["request_shape"].(map[string]any)
		require.True(t, ok)
		return requestShape
	}

	edit := requestShape("edit", "file")
	require.Equal(t, map[string]any{"file": "<file>"}, edit["target"])
	require.Equal(t, "<existing text>", edit["match"])
	require.Equal(t, "<replacement text>", edit["replacement"])
	require.Equal(t, true, edit["dry_run"])
	require.NotContains(t, edit, "old_string")
	require.NotContains(t, edit, "new_string")

	rename := requestShape("refactor", "rename")
	require.Equal(t, map[string]any{"symbol": "<symbol>"}, rename["target"])
	require.Equal(t, "<new name>", rename["new_name"])
	require.Equal(t, true, rename["dry_run"])

	files := requestShape("search", "files")
	require.Equal(t, "<query>", files["query"])
	apiImpact := requestShape("change", "api_impact")
	require.Equal(t, "<file>", apiImpact["source"].(map[string]any)["file"])
	impact := requestShape("change", "impact")
	require.Equal(t, map[string]any{"symbol": "<symbol>"}, impact["target"])
	require.NotContains(t, impact, "source")
	ranges := requestShape("change", "ranges")
	require.NotEmpty(t, ranges["source"].(map[string]any)["ranges"])
	batch := requestShape("edit", "batch")
	require.NotEmpty(t, batch["changes"])
	closure := requestShape("explore", "closure")
	require.NotEmpty(t, closure["options"].(map[string]any)["files"])
	citation := requestShape("analyze", "citation")
	require.NotEmpty(t, citation["options"].(map[string]any)["span"])
	require.NotEmpty(t, citation["options"].(map[string]any)["file_path"])
	coChange := requestShape("analyze", "co_change")
	require.Equal(t, map[string]any{"symbol": "<symbol>"}, coChange["target"])
	skill := requestShape("edit", "skill")
	require.Equal(t, "<directory>", skill["options"].(map[string]any)["directory"])
}

func TestFacadeCapabilitiesDiscoversNativeAnalyzeKinds(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	srv.facades.capture(mcpgo.NewTool("analyze",
		mcpgo.WithString("kind", mcpgo.Required()),
		mcpgo.WithString("tag", mcpgo.Description("(todos) TODO tag; also accepted by (releases)")),
		mcpgo.WithNumber("limit", mcpgo.Description("Maximum rows")),
		mcpgo.WithString("profile", mcpgo.Description("(coverage) Cover profile")),
		mcpgo.WithString("from_id", mcpgo.Description("(would_create_cycle) Source symbol")),
		mcpgo.WithString("to_id", mcpgo.Description("(would_create_cycle) Target symbol")),
		mcpgo.WithString("id", mcpgo.Description("(def_use) Symbol")),
		mcpgo.WithString("ids", mcpgo.Description("(def_use, impact) Symbols")),
		mcpgo.WithBoolean("use_llm", mcpgo.Description("(concepts) Use a model")),
		mcpgo.WithBoolean("refresh_cochange", mcpgo.Description("(impact) Refresh co-change")),
		mcpgo.WithBoolean("materialize", mcpgo.Description("(sql_call_sites) Rebuild SQL edges")),
	), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	})

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"domain": "analyze", "operation": "todos", "detail": "schema"}
	result, err := srv.handleCapabilities(context.Background(), req)
	require.NoError(t, err)
	out := unmarshalResult(t, result)
	require.Equal(t, true, out["available"])
	require.Equal(t, AnalyzeKindDescription("todos"), out["summary"])
	requestShape := out["request_shape"].(map[string]any)
	require.Equal(t, "todos", requestShape["kind"])
	schema := out["input_schema"].(map[string]any)
	schemaProperties := schema["properties"].(map[string]any)
	optionsProperties := schemaProperties["options"].(map[string]any)["properties"].(map[string]any)
	require.Contains(t, optionsProperties, "tag")
	require.NotContains(t, optionsProperties, "profile")
	require.NotContains(t, optionsProperties, "from_id")
	require.Contains(t, schemaProperties["output"].(map[string]any)["properties"], "limit")

	capability := func(domain, operation string) map[string]any {
		t.Helper()
		call := mcpgo.CallToolRequest{}
		call.Params.Arguments = map[string]any{"domain": domain, "operation": operation, "detail": "schema"}
		got, callErr := srv.handleCapabilities(context.Background(), call)
		require.NoError(t, callErr)
		return unmarshalResult(t, got)
	}

	would := capability("analyze", "would_create_cycle")
	wouldSchema := would["input_schema"].(map[string]any)
	require.ElementsMatch(t, []any{"kind", "options"}, wouldSchema["required"].([]any))
	wouldOptions := wouldSchema["properties"].(map[string]any)["options"].(map[string]any)
	require.ElementsMatch(t, []any{"from_id", "to_id"}, wouldOptions["required"].([]any))

	defUse := capability("analyze", "def_use")
	defSchema := defUse["input_schema"].(map[string]any)
	require.Contains(t, defSchema["required"].([]any), "target")
	defTarget := defSchema["properties"].(map[string]any)["target"].(map[string]any)
	require.Contains(t, defTarget["properties"].(map[string]any), "symbol")

	coverage := capability("workspace_admin", "coverage")
	require.Equal(t, AnalyzeKindDescription("coverage"), coverage["summary"])
	coverageSchema := coverage["input_schema"].(map[string]any)
	coverageArgs := coverageSchema["properties"].(map[string]any)["arguments"].(map[string]any)
	require.Contains(t, coverageArgs["properties"].(map[string]any), "profile")
	require.Contains(t, coverageArgs["required"].([]any), "profile")
	require.Equal(t, "coverage", coverage["fixed_arguments"].(map[string]any)["kind"])

	concepts := capability("analyze", "concepts")
	require.Equal(t, false, concepts["fixed_arguments"].(map[string]any)["use_llm"])
	require.Equal(t, "concepts", concepts["fixed_arguments"].(map[string]any)["kind"])

	releases := capability("analyze", "releases")
	releasesOptions := releases["input_schema"].(map[string]any)["properties"].(map[string]any)["options"].(map[string]any)["properties"].(map[string]any)
	require.Contains(t, releasesOptions, "tag")

	listReq := mcpgo.CallToolRequest{}
	listReq.Params.Arguments = map[string]any{"domain": "analyze"}
	listResult, err := srv.handleCapabilities(context.Background(), listReq)
	require.NoError(t, err)
	list := unmarshalResult(t, listResult)
	operations := list["operations"].([]any)
	foundTodos := false
	foundHelp := false
	for _, raw := range operations {
		operation := raw.(map[string]any)["operation"]
		foundTodos = foundTodos || operation == "todos"
		foundHelp = foundHelp || operation == "help"
		require.NotEqual(t, "graph", operation)
		for _, adminKind := range adminAnalyzeKinds {
			require.NotEqual(t, adminKind, operation)
		}
	}
	require.True(t, foundTodos)
	require.True(t, foundHelp)
}

func TestFacadeCapabilitiesCollapseSessionSubscriptionChannels(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	handler := func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	srv.facades.capture(mcpgo.NewTool("subscribe_diagnostics"), handler)
	srv.facades.capture(mcpgo.NewTool("unsubscribe_diagnostics"), handler)
	srv.facades.capture(mcpgo.NewTool("nav", mcpgo.WithString("action", mcpgo.Required()), mcpgo.WithString("id")), handler)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"domain": "session"}
	result, err := srv.handleCapabilities(context.Background(), req)
	require.NoError(t, err)
	operations := unmarshalResult(t, result)["operations"].([]any)
	seen := map[string]bool{}
	for _, raw := range operations {
		name := raw.(map[string]any)["operation"].(string)
		seen[name] = true
		require.NotContains(t, name, "diagnostics")
		require.NotContains(t, name, "daemon_health")
	}
	require.True(t, seen["subscribe"])
	require.True(t, seen["unsubscribe"])
	require.True(t, seen["cursor"])

	schemaReq := mcpgo.CallToolRequest{}
	schemaReq.Params.Arguments = map[string]any{"domain": "session", "operation": "subscribe", "detail": "schema"}
	schemaResult, err := srv.handleCapabilities(context.Background(), schemaReq)
	require.NoError(t, err)
	requestShape := unmarshalResult(t, schemaResult)["request_shape"].(map[string]any)
	require.Equal(t, "subscribe", requestShape["operation"])
	require.Equal(t, "<channel>", requestShape["channel"])

	cursorReq := mcpgo.CallToolRequest{}
	cursorReq.Params.Arguments = map[string]any{"domain": "session", "operation": "cursor", "detail": "schema"}
	cursorResult, err := srv.handleCapabilities(context.Background(), cursorReq)
	require.NoError(t, err)
	cursorShape := unmarshalResult(t, cursorResult)["request_shape"].(map[string]any)
	require.Equal(t, "cursor", cursorShape["operation"])
	require.Equal(t, "<action>", cursorShape["arguments"].(map[string]any)["action"])

	invalid := mcpgo.CallToolRequest{}
	invalid.Params.Arguments = map[string]any{"operation": "subscribe", "channel": "private_channel"}
	invalidResult, err := srv.handleFacade(context.Background(), "session", invalid)
	require.NoError(t, err)
	require.True(t, invalidResult.IsError)
	invalidText := toolResultText(invalidResult)
	require.Contains(t, invalidText, "valid_channels")
	require.NotContains(t, invalidText, "subscribe_diagnostics")
	require.NotContains(t, invalidText, "unsubscribe_daemon_health")
}

func TestFacadeCapabilitiesUsesPublicDomainVocabulary(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}

	listReq := mcpgo.CallToolRequest{}
	listResult, err := srv.handleCapabilities(context.Background(), listReq)
	require.NoError(t, err)
	list := unmarshalResult(t, listResult)
	require.NotNil(t, list["domains"])
	require.NotContains(t, list, "facades")

	domainReq := mcpgo.CallToolRequest{}
	domainReq.Params.Arguments = map[string]any{"domain": "read"}
	domainResult, err := srv.handleCapabilities(context.Background(), domainReq)
	require.NoError(t, err)
	domain := unmarshalResult(t, domainResult)
	require.Equal(t, "read", domain["domain"])
	require.NotContains(t, domain, "facade")

	unknownReq := mcpgo.CallToolRequest{}
	unknownReq.Params.Arguments = map[string]any{"domain": "missing"}
	unknownResult, err := srv.handleCapabilities(context.Background(), unknownReq)
	require.NoError(t, err)
	require.True(t, unknownResult.IsError)
	unknownText := unknownResult.Content[0].(mcpgo.TextContent).Text
	require.Contains(t, unknownText, "unknown tool domain")
	require.Contains(t, unknownText, "valid_domains")
	require.NotContains(t, unknownText, "valid_facades")
}

func TestFacadeDispatchRecordsOperationTelemetry(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	srv.facades.capture(mcpgo.NewTool("read_file"), func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	})
	store := telemetry.NewStore(t.TempDir())
	srv.SetTelemetryRecorder(telemetry.NewRecorder(telemetry.Consent{Enabled: true}, store))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"operation": "file", "target": map[string]any{"file": "x.go"}}
	_, err := srv.handleFacade(context.Background(), "read", req)
	require.NoError(t, err)

	invalid := mcpgo.CallToolRequest{}
	invalid.Params.Arguments = map[string]any{
		"operation": "file",
		"target":    map[string]any{"file": "x.go", "symbol": "secretSymbol"},
	}
	result, err := srv.handleFacade(context.Background(), "read", invalid)
	require.NoError(t, err)
	require.True(t, result.IsError)

	// Unknown request values are observable only through a fixed sentinel;
	// neither the raw operation nor its deterministic hash may be recorded.
	const sensitiveOperation = "/Users/alice/private/repository/read-secret"
	unknown := mcpgo.CallToolRequest{}
	unknown.Params.Arguments = map[string]any{"operation": sensitiveOperation}
	result, err = srv.handleFacade(context.Background(), "read", unknown)
	require.NoError(t, err)
	require.True(t, result.IsError)

	unknownCapability := mcpgo.CallToolRequest{}
	unknownCapability.Params.Arguments = map[string]any{"domain": sensitiveOperation}
	result, err = srv.handleCapabilities(context.Background(), unknownCapability)
	require.NoError(t, err)
	require.True(t, result.IsError)

	srv.recordFacadeTelemetry("analyze", "todos", facadeOutcomeSuccess, time.Millisecond)
	srv.recordFacadeTelemetry("analyze", "coverage", facadeOutcomeBlocked, time.Millisecond)
	srv.recordFacadeTelemetry("analyze", sensitiveOperation, facadeOutcomeSuccess, time.Millisecond)

	srv.FlushTelemetry()
	days, err := store.Days()
	require.NoError(t, err)
	require.Len(t, days, 1)
	rollup, err := store.Load(days[0])
	require.NoError(t, err)
	require.Equal(t, 2, rollup.Counts["mcp_facade_call:read.file"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_status:read.file.ok"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_status:read.file.error"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_outcome:read.file.success"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_outcome:read.file.invalid_argument"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_invalid:read.file.invalid_argument"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_call:read.unknown"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_status:read.unknown.error"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_outcome:read.unknown.invalid_operation"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_invalid:read.unknown.invalid_argument"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_call:capabilities.unknown"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_outcome:"+
		boundedFacadeTelemetryDimension("capabilities", "unknown", facadeOutcomeInvalidOperation)])
	require.Equal(t, 1, rollup.Counts["mcp_facade_invalid:"+
		boundedFacadeTelemetryDimension("capabilities", "unknown", string(ErrCodeInvalidArgument))])
	require.Equal(t, 1, rollup.Counts["mcp_facade_call:analyze.todos"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_outcome:analyze.todos.success"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_call:analyze.coverage"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_outcome:analyze.coverage.blocked"])
	require.Equal(t, 1, rollup.Counts["mcp_facade_call:analyze.unknown"])
	latencyCounts := map[string]int{
		"read.file": 0, "read.unknown": 0, "capabilities.unknown": 0,
		"analyze.todos": 0, "analyze.coverage": 0, "analyze.unknown": 0,
	}
	for key := range rollup.Counts {
		require.LessOrEqual(t, len(strings.TrimPrefix(key, strings.SplitN(key, ":", 2)[0]+":")), 32)
		require.NotContains(t, key, "alice")
		require.NotContains(t, key, "private")
		require.NotContains(t, key, "read-secret")
		require.NotContains(t, key, "secretSymbol")
		for identity := range latencyCounts {
			if strings.HasPrefix(key, "mcp_facade_latency:"+identity+".") {
				latencyCounts[identity] += rollup.Counts[key]
			}
		}
	}
	require.Equal(t, 2, latencyCounts["read.file"])
	require.Equal(t, 1, latencyCounts["read.unknown"])
	require.Equal(t, 1, latencyCounts["capabilities.unknown"])
	require.Equal(t, 1, latencyCounts["analyze.todos"])
	require.Equal(t, 1, latencyCounts["analyze.coverage"])
	require.Equal(t, 1, latencyCounts["analyze.unknown"])

	long := boundedFacadeTelemetryDimension("session", "unsubscribe_workspace_readiness")
	require.LessOrEqual(t, len(long), 32)
}

func TestFacadeTelemetryRespectsDisabledConsent(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	srv.facades.capture(mcpgo.NewTool("read_file"), func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	})
	store := telemetry.NewStore(t.TempDir())
	srv.SetTelemetryRecorder(telemetry.NewRecorder(telemetry.Consent{Enabled: false}, store))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"operation": "file", "target": map[string]any{"file": "x.go"}}
	_, err := srv.handleFacade(context.Background(), "read", req)
	require.NoError(t, err)
	srv.FlushTelemetry()
	days, err := store.Days()
	require.NoError(t, err)
	require.Empty(t, days)
}

func TestFacadeReadSchemaAdvertisesExactlyOneTargetSelector(t *testing.T) {
	tool := facadeToolDefinitionWithOperations("read", []string{"source", "file", "symbols"})
	target, ok := tool.InputSchema.Properties["target"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 1, target["minProperties"])
	require.Equal(t, 1, target["maxProperties"])
	require.Contains(t, target["description"], "exactly one selector")
	require.Contains(t, target["description"], "symbol for one source symbol")
	options := tool.InputSchema.Properties["options"].(map[string]any)
	properties := options["properties"].(map[string]any)
	boundary := properties["new_user_task"].(map[string]any)
	require.Equal(t, "boolean", boundary["type"])
	require.Contains(t, boundary["description"], "first read.file")
}

func TestFacadeReadSelectorCardinalityDefaultsAndAliases(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	capture := func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultJSON(req.GetArguments())
	}
	srv.facades.capture(mcpgo.NewTool("read_file"), capture)
	srv.facades.capture(mcpgo.NewTool("get_symbol_source"), capture)
	srv.facades.capture(mcpgo.NewTool("batch_symbols"), capture)

	call := func(arguments map[string]any) map[string]any {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = arguments
		result, err := srv.handleFacade(context.Background(), "read", req)
		require.NoError(t, err)
		return unmarshalResult(t, result)
	}

	batch := call(map[string]any{
		"operation": "source",
		"target":    map[string]any{"symbols": []any{"a.go::A", "b.go::B"}},
	})
	require.Equal(t, `["a.go::A","b.go::B"]`, batch["ids"])
	require.Equal(t, true, batch["include_source"])

	metadataOnly := call(map[string]any{
		"operation": "symbols",
		"target":    map[string]any{"symbols": []any{"a.go::A"}},
		"options":   map[string]any{"include_source": false},
	})
	require.Equal(t, false, metadataOnly["include_source"])

	single := call(map[string]any{
		"operation": "symbols",
		"target":    map[string]any{"symbol": "a.go::A"},
	})
	require.Equal(t, "a.go::A", single["id"])
	require.NotContains(t, single, "ids")

	line := call(map[string]any{
		"operation": "source",
		"target":    map[string]any{"file": "a.go"},
		"options":   map[string]any{"line": 42},
	})
	require.Equal(t, "a.go", line["path"])
	require.Equal(t, float64(42), line["offset"])
	require.Equal(t, float64(1), line["limit"])

	window := call(map[string]any{
		"operation": "source",
		"target":    map[string]any{"file": "a.go"},
		"options":   map[string]any{"window": map[string]any{"offset": 20, "limit": 7}},
	})
	require.Equal(t, float64(20), window["offset"])
	require.Equal(t, float64(7), window["limit"])
}

func TestFacadeSchemasEnumerateOnlyAvailableOperations(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	capture := func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultJSON(req.GetArguments())
	}
	srv.facades.capture(mcpgo.NewTool("read_file", mcpgo.WithString("path", mcpgo.Required())), capture)

	runtime := srv.facadeToolDefinition("read")
	operation := runtime.InputSchema.Properties["operation"].(map[string]any)
	require.Equal(t, []string{"file"}, operation["enum"])

	canonical := facadeToolDefinition("read")
	canonicalOperation := canonical.InputSchema.Properties["operation"].(map[string]any)
	require.Contains(t, canonicalOperation["enum"], "source")
	require.Contains(t, canonicalOperation["enum"], "symbols")
	analyzeKind := facadeToolDefinition("analyze").InputSchema.Properties["kind"].(map[string]any)
	require.Contains(t, analyzeKind["enum"], "todos")

	srv.facades.capture(mcpgo.NewTool("batch_symbols",
		mcpgo.WithString("ids", mcpgo.Required()),
		mcpgo.WithBoolean("include_source"),
	), capture)
	spec, ok := srv.capabilityOperation("read", "symbols")
	require.True(t, ok)
	capability := srv.facadeCapability(spec, true)
	schema := capability["input_schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	exactOperation := properties["operation"].(map[string]any)
	require.Equal(t, []string{"symbols"}, exactOperation["enum"])
	options := properties["options"].(map[string]any)["properties"].(map[string]any)
	includeSource := options["include_source"].(map[string]any)
	require.Equal(t, true, includeSource["default"])
	require.Contains(t, includeSource["description"], "default: true")
}

func TestBatchEditPreflightsAllItemsBeforeFirstWrite(t *testing.T) {
	srv, root := setupTestServer(t)
	path := filepath.Join(root, "batch-preflight.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha beta\n"), 0o644))

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"edits": []any{
			map[string]any{
				"op": "edit_file", "path": path,
				"old_string": "alpha", "new_string": "ALPHA",
			},
			map[string]any{
				"op": "edit_file", "path": path,
				"old_string": "beta", "new_string": "beta",
			},
		},
	}
	result, err := srv.handleBatchEdit(context.Background(), req)
	require.NoError(t, err)
	out := unmarshalResult(t, result)
	summary := out["summary"].(map[string]any)
	require.Equal(t, float64(0), summary["applied"])
	require.Equal(t, float64(1), summary["failed"])
	require.Equal(t, float64(1), summary["skipped"])
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "alpha beta\n", string(content), "a later no-op must fail before the first edit writes")
}

func TestFacadeReadResolvesOnlyUniqueSymbolShorthand(t *testing.T) {
	g := graph.New()
	first := &graph.Node{ID: "pkg/a.go::UniqueReadTarget", Name: "UniqueReadTarget", Kind: graph.KindFunction, FilePath: "pkg/a.go"}
	g.AddNode(first)
	eng := query.NewEngine(g)
	// The shorthand resolver reads FindNodesByName off the graph, never
	// the text backend, so an inert one keeps the fixture honest.
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	resolved, ambiguous := srv.resolveFacadeSymbolShorthand(context.Background(), "UniqueReadTarget")
	require.Equal(t, first.ID, resolved)
	require.Empty(t, ambiguous)

	second := &graph.Node{ID: "pkg/b.go::UniqueReadTarget", Name: "UniqueReadTarget", Kind: graph.KindFunction, FilePath: "pkg/b.go"}
	g.AddNode(second)
	resolved, ambiguous = srv.resolveFacadeSymbolShorthand(context.Background(), "UniqueReadTarget")
	require.Equal(t, "UniqueReadTarget", resolved)
	require.ElementsMatch(t, []string{first.ID, second.ID}, ambiguous)
}
