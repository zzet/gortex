package mcp

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/daemon"
)

// DispatchClass classifies a tool before admission using the same facade
// operation selection and registry as execution. It does not resolve symbols,
// acquire views, promote tools, change session state or run a handler.
func (s *Server) DispatchClass(name string, arguments map[string]any) daemon.MCPDispatchClass {
	// Native kinds and facade aliases of analyze all use the slow lane.
	if name == "analyze" {
		return daemon.MCPDispatchSlow
	}
	class, _ := daemon.ToolDispatchClass(name)
	if !isFacadeToolName(name) || s == nil || s.facades == nil {
		return class
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = arguments
	operation := facadeRequestOperation(name, req)
	if name == "session" && (operation == "subscribe" || operation == "unsubscribe") {
		channel := normalizeFacadeOperation(req.GetString("channel", ""))
		if !validFacadeSessionChannel(channel) {
			return daemon.MCPDispatchFast
		}
		operation += "_" + channel
	}
	spec, known := s.facades.operation(name, operation)
	if !known {
		return daemon.MCPDispatchFast
	}
	switch spec.Effect {
	case facadeEffectLocalWrite, facadeEffectControlWrite, facadeEffectExternalWrite:
		return daemon.MCPDispatchMutating
	}
	class, _ = daemon.ToolDispatchClass(spec.Legacy)
	if class == daemon.MCPDispatchSlow {
		return class
	}
	// Read/session facades fix the conditional legacy write arguments, e.g.
	// change.contract ack=false and analyze.co_change refresh=false. Never
	// inherit a legacy mutation reservation for that effect-safe operation.
	return daemon.MCPDispatchFast
}
