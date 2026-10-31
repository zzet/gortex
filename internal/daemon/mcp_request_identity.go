package daemon

import "context"

// MCPRequestIDKeyForContext returns the canonical wire-ID key already validated
// by this connection's request registry. Embedded and untracked callers have
// no such observation. Reading it adds no clock, parsing, allocation or registry.
//
// A client can reuse an ID after a request finishes. This is an attribution
// cause, not a publisher identity: pair it with the actual session and admitted
// coordinator generation when relating work to a request.
func MCPRequestIDKeyForContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	lease := mcpDispatchSlotLeaseForContext(ctx)
	if lease == nil || lease.requestIDKey == "" {
		return "", false
	}
	return lease.requestIDKey, true
}
