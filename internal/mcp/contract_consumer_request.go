package mcp

import (
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

type contractConsumerMode uint8

const (
	contractConsumerNone contractConsumerMode = iota
	contractConsumerOptional
	contractConsumerRequired
)

// contractConsumerRequest lowers facade parameters through their existing
// normalization. Readiness and the handler must interpret the same selectors.
func (s *Server) contractConsumerRequest(req mcp.CallToolRequest) mcp.CallToolRequest {
	if !isFacadeToolName(req.Params.Name) {
		return req
	}
	if spec, ok := s.viewFacadeOperation(&req); ok {
		req.Params.Arguments = normalizeFacadeArguments(spec, req.GetArguments())
		req.Params.Name = spec.Legacy
	}
	return req
}

func contractConsumerForRequest(req mcp.CallToolRequest, want capabilityRequest) contractConsumerMode {
	mode := contractConsumerNone
	for _, capability := range want.required {
		if capability == graphview.CapContracts {
			return contractConsumerRequired
		}
	}
	for _, capability := range want.optional {
		if capability == graphview.CapContracts {
			mode = contractConsumerOptional
		}
	}
	switch req.Params.Name {
	case "contracts", "api_impact":
		return contractConsumerRequired
	case "analyze":
		if req.GetString("kind", "") == "route_frameworks" {
			return contractConsumerRequired
		}
	}
	kind := req.GetString("kind", "")
	if kind == "" {
		kind = parseFieldQuery(req.GetString("query", "")).Kind
	}
	for _, kinds := range []string{kind, req.GetString("node_kind", ""), req.GetString("node_kinds", "")} {
		for _, item := range strings.Split(kinds, ",") {
			switch graph.NodeKind(strings.ToLower(strings.TrimSpace(item))) {
			case graph.KindContract, graph.KindContractBridge:
				return contractConsumerRequired
			}
		}
	}
	for _, kinds := range []string{req.GetString("edge_kind", ""), req.GetString("edge_kinds", "")} {
		for _, item := range strings.Split(kinds, ",") {
			switch graph.EdgeKind(strings.ToLower(strings.TrimSpace(item))) {
			case graph.EdgeMatches, graph.EdgeBridges, graph.EdgeHandlesRoute:
				return contractConsumerRequired
			case graph.EdgeProvides, graph.EdgeConsumes:
				// These also carry current core SQL/table dataflow. The contract
				// component is optional unless endpoints explicitly select it.
				mode = contractConsumerOptional
			}
		}
	}
	switch req.Params.Name {
	case "search_symbols", "get_architecture", "generate_wiki", "explain_change_impact", "review", "review_pack", "pr_review_context", "analyze_framework":
		return contractConsumerOptional
	}
	return mode
}
