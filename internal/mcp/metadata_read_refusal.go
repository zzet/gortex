package mcp

import (
 "context"
 "errors"

 mcplib "github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) metadataReadRetryScope(req mcplib.CallToolRequest) bool {
 name,known:=s.legacyToolName(&req)
 return known && (name=="get_file_summary" || name=="get_editing_context") && auditedProjectionReadRetry(name,req)
}

func metadataTransientResult(code ErrorCode,message,reason string) *mcplib.CallToolResult {
 data:=map[string]any{"retry_scope":"read_only","delivery_unknown":false}
 if reason!="" {data["reason"]=reason}
 return NewStructuredErrorResult(StructuredError{ErrorCode:code,Message:message,Retriable:true,Data:data})
}

// Classification is at the existing refusal boundary: no selection, replay,
// new allowance, physical guard or successful response changes here.
func (s *Server) metadataReadError(ctx context.Context,req mcplib.CallToolRequest,err error)*mcplib.CallToolResult {
 original:=searchRefusalToolResult(err)
 if ctx.Err()!=nil || !s.metadataReadRetryScope(req) || !errors.Is(err,projectionSuperseded) {return original}
 result:=metadataTransientResult(ErrCodeProjectionSuperseded,err.Error(),"")
 result.Meta=original.Meta
 return result
}

// Only an actual request-local unavailable-file observation qualifies. Empty
// canonical results and scope-denial results keep their original error shape.
func (s *Server) pendingEmptyMetadataRead(ctx context.Context,req mcplib.CallToolRequest,file string)*mcplib.CallToolResult {
 if ctx.Err()!=nil || !s.metadataReadRetryScope(req) {return nil}
 reason:=""
 if _,pending:=requestDeferredProjectionFiles(ctx)[file];pending {reason="projection_deferred"}
 if reason=="" {for _,busy:=range requestStoreBusyFiles(ctx) {if busy==file {reason="store_busy";break}}}
 if reason=="" {return nil}
 return metadataTransientResult(ErrCodeFileViewPending,"file metadata is pending for this read",reason)
}
