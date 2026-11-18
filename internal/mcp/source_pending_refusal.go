package mcp

import (
	"context"
	"path"
	"path/filepath"
	"strings"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// pendingSourceMiss classifies only an unresolved lookup after the checked
// removal path. A request-local deferral cannot certify absence; it also does
// not prove that the requested symbol exists. This boundary authorizes no
// internal replay, source extraction, new allowance or successful response.
func (s *Server) pendingSourceMiss(ctx context.Context, req mcplib.CallToolRequest, id string) *mcplib.CallToolResult {
	name, known := s.legacyToolName(&req)
	if ctx.Err() != nil || !known || name != "get_symbol_source" {
		return nil
	}
	args := req.GetArguments()
	values := []map[string]any{args}
	if raw, present := args["options"]; present {
		options, ok := raw.(map[string]any)
		if !ok {
			return nil
		}
		values = append(values, options)
	}
	for _, options := range values {
		if raw, present := options["compress_bodies"]; present {
			value, ok := raw.(bool)
			if !ok || value {
				return nil
			}
		}
	}
	file, symbol, qualified := strings.Cut(id, "::")
	if !qualified || symbol == "" || file == "" || filepath.IsAbs(file) || file != path.Clean(file) {
		return nil
	}
	reason := requestDeferredProjectionFiles(ctx)[file]
	if reason != "external_write" && reason != "storm" {
		return nil
	}
	// A selected complete layer can answer absence. Historical deferral alone
	// must not replace its checked removal/not-found result with a transient.
	for _, layer := range requestProjectionLayers(ctx) {
		if layer.HasFile(file) {
			return nil
		}
	}
	if editor := OverlayViewFromContext(ctx); editor != nil && editor.LayerReader() != nil && editor.LayerReader().HasFile(file) {
		return nil
	}
	owner, _ := s.indexerForRel(file)
	if owner == nil {
		return nil
	}
	_, normalized, err := s.resolveFilePath(ctx, file)
	if err != nil || graphPathKey(normalized) != file {
		return nil
	}
	attribution := &graph.Node{FilePath: file, RepoPrefix: owner.RepoPrefix(), WorkspaceID: owner.WorkspaceID(), ProjectID: owner.ProjectID()}
	if !s.nodeInSessionScope(ctx, attribution) || ctx.Err() != nil {
		return nil
	}
	return metadataTransientResult(ErrCodeFileViewPending, "source file view is pending for this read", "projection_deferred")
}
