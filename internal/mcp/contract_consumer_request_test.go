package mcp

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graphview"
)

func TestContractConsumerClassificationSeparatesCoreAndContractRequests(t *testing.T) {
	for _, test := range []struct {
		name string
		args map[string]any
		want contractConsumerMode
	}{
		{"read_file", nil, contractConsumerNone},
		{"search_text", map[string]any{"query": "GET /items"}, contractConsumerNone},
		{"get_relations", map[string]any{"edge_kind": "calls"}, contractConsumerNone},
		{"search_symbols", map[string]any{"query": "Handler"}, contractConsumerOptional},
		{"search_symbols", map[string]any{"query": "kind:contract path:api/ route"}, contractConsumerRequired},
		{"search_symbols", map[string]any{"query": "kind:contract route", "kind": "function"}, contractConsumerOptional},
		{"search_symbols", map[string]any{"kind": "contract_bridge"}, contractConsumerRequired},
		{"search_symbols", map[string]any{"kind": "config_key"}, contractConsumerRequired},
		{"get_symbol", map[string]any{"id": "config::DATABASE_URL"}, contractConsumerRequired},
		{"graph_query", map[string]any{"query": "nodes kind=contract | traverse provides both"}, contractConsumerRequired},
		{"graph_query", map[string]any{"query": "nodes kind=config_key"}, contractConsumerRequired},
		{"graph_query", map[string]any{"query": "nodes kind=function | traverse reads_config out"}, contractConsumerRequired},
		{"graph_query", map[string]any{"query": "nodes kind=function | traverse calls both"}, contractConsumerOptional},
		{"contracts", map[string]any{"action": "check"}, contractConsumerRequired},
		{"contracts", map[string]any{"action": "bridge"}, contractConsumerRequired},
		{"get_symbol", map[string]any{"id": "http::GET::/items"}, contractConsumerRequired},
		{"get_relations", map[string]any{"id": "bridge::ws::project::http::GET::/items"}, contractConsumerRequired},
		{"get_symbol", map[string]any{"id": "repo/http.go::Handle"}, contractConsumerNone},
		{"api_impact", nil, contractConsumerRequired},
		{"get_architecture", nil, contractConsumerOptional},
		{"explain_change_impact", nil, contractConsumerOptional},
		{"generate_wiki", nil, contractConsumerOptional},
		{"review", nil, contractConsumerOptional},
		{"analyze", map[string]any{"kind": "route_frameworks"}, contractConsumerRequired},
		{"get_relations", map[string]any{"edge_kinds": "provides,consumes"}, contractConsumerOptional},
		{"get_relations", map[string]any{"edge_kinds": "provides,bridges"}, contractConsumerRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := mcp.CallToolRequest{}
			req.Params.Name, req.Params.Arguments = test.name, test.args
			require.Equal(t, test.want, contractConsumerForRequest(req, capabilityRequest{}))
		})
	}
	// Explicit requirements dominate optional defaults without consulting a
	// materializer, registry, or any repository dependency inputs.
	req := mcp.CallToolRequest{}
	req.Params.Name = "search_symbols"
	require.Equal(t, contractConsumerRequired, contractConsumerForRequest(req, capabilityRequest{required: []graphview.CapabilityID{graphview.CapContracts}}))
}

func TestContractConsumerClassifierUsesFacadeSelectors(t *testing.T) {
	s, _ := setupTestServer(t)
	for _, test := range []struct {
		name string
		args map[string]any
		want contractConsumerMode
	}{
		{"analyze", map[string]any{"kind": "contracts", "options": map[string]any{"action": "list"}}, contractConsumerRequired},
		{"change", map[string]any{"operation": "api_impact", "options": map[string]any{"route": "/items"}}, contractConsumerRequired},
		{"search", map[string]any{"operation": "symbols", "query": "Handler"}, contractConsumerOptional},
		{"search", map[string]any{"operation": "symbols", "query": "kind:contract route"}, contractConsumerRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := mcp.CallToolRequest{}
			req.Params.Name, req.Params.Arguments = test.name, test.args
			req = s.contractConsumerRequest(req)
			require.Equal(t, test.want, contractConsumerForRequest(req, capabilityRequest{}))
		})
	}
}
