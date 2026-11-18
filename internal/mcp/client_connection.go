package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/server"
)

// clientConnectionCtxKey marks a request that arrived over an MCP client
// transport. Unexported: use WithClientConnection / isClientConnection.
type clientConnectionCtxKey struct{}

// WithClientConnection marks ctx as a frame from an MCP client connection
// (the daemon's socket or Streamable HTTP dispatcher), as opposed to an
// internal caller that invokes handlers directly (dashboard, REST). The
// default tool surface is chosen per connection, never from the
// informational clientInfo.name, so an unnamed client resolves exactly like
// a named one.
func WithClientConnection(ctx context.Context) context.Context {
	return context.WithValue(ctx, clientConnectionCtxKey{}, true)
}

// isClientConnection reports whether ctx belongs to an MCP client
// connection: one marked by WithClientConnection, or one served by mcp-go's
// own transports (embedded stdio), which bind a client session to ctx.
func isClientConnection(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if marked, _ := ctx.Value(clientConnectionCtxKey{}).(bool); marked {
		return true
	}
	return server.ClientSessionFromContext(ctx) != nil
}
