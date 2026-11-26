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
	if name == "rename_symbol" && renamePreviewOnly(arguments) {
		return daemon.MCPDispatchFast
	}
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
		if spec.Legacy == "rename_symbol" && renamePreviewOnly(normalizeFacadeArguments(spec, arguments)) {
			return daemon.MCPDispatchFast
		}
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

// renamePreviewOnly reports whether rename_symbol, given these legacy
// arguments, runs as a dry run. It reads dry_run exactly as the handler does,
// so a call admitted as a read is one the handler will not write: a dry run
// plans and verifies the edits, then returns before any file, receipt or
// store write. Other dry runs keep their lane until they are shown to write
// nothing.
func renamePreviewOnly(arguments map[string]any) bool {
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = arguments
	return req.GetBool("dry_run", false)
}
