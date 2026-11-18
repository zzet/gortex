package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestFacadeSessionCursorCapabilityMatchesPublicProtocol(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.NoteSessionToolPolicy("facade_cursor_schema", FacadeSurfaceVersion, "hide")
	ctx := WithSessionID(context.Background(), "facade_cursor_schema")
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initialize))

	call := func(name string, arguments map[string]any) map[string]any {
		t.Helper()
		var envelope struct {
			Error  any                   `json:"error"`
			Result *mcpgo.CallToolResult `json:"result"`
		}
		require.NoError(t, json.Unmarshal([]byte(callToolFrame(t, srv, ctx, name, arguments)), &envelope))
		require.Nil(t, envelope.Error)
		require.NotNil(t, envelope.Result)
		return unmarshalResult(t, envelope.Result)
	}

	capability := call("capabilities", map[string]any{
		"domain": "session", "operation": "cursor", "detail": "schema",
	})
	require.Equal(t, true, capability["available"])
	schema := capability["input_schema"].(map[string]any)
	request := capability["request_shape"].(map[string]any)
	require.Equal(t, "cursor", request["operation"])
	arguments := request["arguments"].(map[string]any)
	require.Contains(t, arguments, "action")
	require.NoError(t, validateFacadeSchema(schema, request, "$"), "the published schema must accept its request_shape")
	require.Error(t, validateFacadeSchema(schema, map[string]any{"operation": "cursor", "arguments": map[string]any{}}, "$"), "the required navigation action must remain required inside arguments")

	var helperID string
	for _, node := range srv.engineFor(ctx).FindSymbols("helper") {
		if node != nil && node.Name == "helper" {
			helperID = node.ID
			break
		}
	}
	require.NotEmpty(t, helperID)
	arguments["action"] = "goto"
	arguments["id"] = helperID
	arguments["format"] = "json"
	require.NoError(t, validateFacadeSchema(schema, request, "$"))
	payload := call("session", request)
	require.Equal(t, "goto", payload["action"])
	require.Equal(t, helperID, navCursorCurrent(payload))

	delete(arguments, "id")
	for _, action := range []string{"where", "read"} {
		arguments["action"] = action
		require.NoError(t, validateFacadeSchema(schema, request, "$"))
		payload = call("session", request)
		require.Equal(t, action, payload["action"])
		require.Equal(t, helperID, navCursorCurrent(payload), "the public operation must retain its session cursor")
		if action == "read" {
			require.Contains(t, payload["source"], "func helper() {}")
		}
	}
}

func TestFacadeSessionSubscriptionCapabilityKeepsChannelSchema(t *testing.T) {
	srv, _ := setupTestServer(t)
	for _, operation := range []string{"subscribe", "unsubscribe"} {
		t.Run(operation, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{"domain": "session", "operation": operation, "detail": "schema"}
			result, err := srv.handleCapabilities(context.Background(), req)
			require.NoError(t, err)
			capability := unmarshalResult(t, result)
			schema := capability["input_schema"].(map[string]any)
			properties := schema["properties"].(map[string]any)
			require.ElementsMatch(t, []string{"channel", "arguments", viewArgName}, sortedFacadeMapKeys(properties))
			require.Equal(t, []string{"channel"}, facadeSchemaRequiredNames(schema))
			channel := properties["channel"].(map[string]any)
			require.Equal(t, "string", channel["type"])
			require.ElementsMatch(t, facadeSessionChannels, channel["enum"])
			require.NoError(t, validateFacadeSchema(schema, map[string]any{
				"operation": operation, "channel": "graph_invalidated", "arguments": map[string]any{},
			}, "$"))
		})
	}
}
