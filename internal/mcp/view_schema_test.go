package mcp

import (
	"sort"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestViewSelectorSchemaMatchesRuntimeShapes(t *testing.T) {
	schema := viewSelectorSchema()
	valid := map[string]map[string]any{
		"implicit_auto": {},
		"explicit_auto": {"kind": "auto"},
		"base":          {"kind": "base", "graph_id": "graph-1"},
		"worktree":      {"kind": "worktree", "checkout_id": "checkout-1"},
		"git_ref":       {"kind": "git_ref", "graph_id": "graph-1", "value": "refs/heads/main"},
		"commit":        {"kind": "commit", "value": "0123456789012345678901234567890123456789"},
	}
	for name, value := range valid {
		t.Run("accepts_"+name, func(t *testing.T) {
			require.NoError(t, validateFacadeSchema(schema, value, "$.view"))
		})
	}

	invalid := map[string]map[string]any{
		"unknown_kind":       {"kind": "snapshot"},
		"base_without_graph": {"kind": "base"},
		"mixed_identifiers":  {"kind": "worktree", "checkout_id": "checkout-1", "graph_id": "graph-1"},
		"unknown_field":      {"kind": "auto", "branch": "main"},
	}
	for name, value := range invalid {
		t.Run("rejects_"+name, func(t *testing.T) {
			require.Error(t, validateFacadeSchema(schema, value, "$.view"))
		})
	}
}

func TestViewSelectorPublishedOnEveryToolSchema(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.facades.mu.RLock()
	legacyNames := make([]string, 0, len(srv.facades.captured))
	legacyTools := make(map[string]mcpgo.Tool, len(srv.facades.captured))
	for name, captured := range srv.facades.captured {
		legacyNames = append(legacyNames, name)
		legacyTools[name] = captured.tool
	}
	srv.facades.mu.RUnlock()
	sort.Strings(legacyNames)
	require.Greater(t, len(legacyNames), 50, "conformance test must cover the registered legacy catalog")
	for _, name := range legacyNames {
		t.Run("legacy/"+name, func(t *testing.T) {
			require.Equal(t, compactViewSelectorSchema(), requirePublishedViewSelector(t, legacyTools[name].InputSchema.Properties))
			requirePublishedViewConsistency(t, legacyTools[name].InputSchema.Properties)
		})
	}

	for _, name := range facadeToolNames() {
		t.Run("facade/"+name, func(t *testing.T) {
			properties := facadeToolDefinition(name).InputSchema.Properties
			require.Equal(t, compactViewSelectorSchema(), requirePublishedViewSelector(t, properties))
			requirePublishedViewConsistency(t, properties)
		})
		for _, spec := range srv.facades.availableOperations(name) {
			spec := spec
			t.Run("capability/"+name+"."+spec.Operation, func(t *testing.T) {
				capability := srv.facadeCapability(spec, true)
				schema := facadeSchemaMapForTest(t, capability["input_schema"])
				properties := schema["properties"].(map[string]any)
				published := requirePublishedViewSelector(t, properties)
				require.Len(t, published["oneOf"], 5, "capability must publish every selector shape")
				require.Equal(t, facadeSchemaMapForTest(t, viewSelectorSchema()), facadeSchemaMapForTest(t, published))
				requirePublishedViewConsistency(t, properties)
			})
		}
	}
}

func requirePublishedViewConsistency(t testing.TB, properties map[string]any) {
	t.Helper()
	for name, wantType := range map[string]string{
		requireExactArgName: "boolean",
		requireFreshArgName: "boolean",
		waitDeadlineArgName: "string",
	} {
		raw, ok := properties[name]
		require.True(t, ok, "schema omitted universal %q", name)
		schema, ok := raw.(map[string]any)
		require.True(t, ok, "schema published %q as %T", name, raw)
		require.Equal(t, wantType, schema["type"])
		if name == waitDeadlineArgName {
			require.Equal(t, "date-time", schema["format"])
		}
	}
}

func requirePublishedViewSelector(t testing.TB, properties map[string]any) map[string]any {
	t.Helper()
	raw, published := properties[viewArgName]
	require.True(t, published, "schema omitted the universal %q selector", viewArgName)
	schema, ok := raw.(map[string]any)
	require.True(t, ok, "schema published %q as %T", viewArgName, raw)
	return schema
}

func BenchmarkViewSelectorSchemaBuildAndValidate(b *testing.B) {
	request := map[string]any{"kind": "worktree", "checkout_id": "checkout-1"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tool := facadeToolDefinition("search")
		schema := requirePublishedViewSelector(b, tool.InputSchema.Properties)
		if err := validateFacadeSchema(schema, request, "$.view"); err != nil {
			b.Fatal(err)
		}
	}
}
