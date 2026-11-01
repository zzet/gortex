package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
)

type mcpDispatchRequestContextKey struct{}
type mcpDispatchRequestInfo struct {
	class    MCPDispatchClass
	session  string
	protocol bool
}

var mcpDispatchConnectionSequence atomic.Uint64

func isMCPInitializationRequest(method string) bool {
	return method == "initialize" || method == "server/discover"
}

func withMCPDispatchRequest(ctx context.Context, info mcpDispatchRequestInfo) context.Context {
	return context.WithValue(ctx, mcpDispatchRequestContextKey{}, info)
}
func mcpDispatchRequestForContext(ctx context.Context) mcpDispatchRequestInfo {
	info, _ := ctx.Value(mcpDispatchRequestContextKey{}).(mcpDispatchRequestInfo)
	return info
}
func prepareMCPDispatchContext(ctx context.Context, session string, envelope mcpRequestEnvelope, classifier MCPDispatchClassifier) context.Context {
	if _, prepared := ctx.Value(mcpDispatchRequestContextKey{}).(mcpDispatchRequestInfo); prepared {
		return ctx
	}
	// The table proves only known canonical local names. Unknown/qualified
	// remote tools remain fast; the shared total cap and session fairness still
	// apply. Do not invent remote effects from a spelling or claim census parity.
	class, _ := ToolDispatchClass(envelope.Params.Name)
	if envelope.Method == "tools/call" && classifier != nil {
		var arguments map[string]any
		_ = json.Unmarshal(envelope.Params.Arguments, &arguments)
		class = classifier.DispatchClass(envelope.Params.Name, arguments)
	}
	if envelope.Method != "tools/call" {
		class = MCPDispatchFast
	}
	ctx = withMCPDispatchRequest(ctx, mcpDispatchRequestInfo{class: class, session: session, protocol: isMCPInitializationRequest(envelope.Method) || len(envelope.ID) == 0 && envelope.Method != ""})
	if isLongLivedMCPRequest(envelope.Method) || isMCPCancellation(envelope.Method) {
		ctx = withMCPDispatchUnbounded(ctx)
	}
	return ctx
}

// isMCPCancellation marks the one notification that must not wait for a slot.
// It only gives capacity back, so queueing it behind a full pool would keep
// that capacity held exactly when it is needed. The Unix reader cancels in
// place and never dispatches it; HTTP hands it to the MCP server, which owns
// the in-flight request it cancels.
func isMCPCancellation(method string) bool {
	return method == "notifications/cancelled"
}

// DispatchMCPFrame is the shared admission/replay entry for the daemon's Unix
// and HTTP transports. Their ordering, cancellation and framing stay with the
// transport; replay hits and in-flight followers never acquire a second slot.
func DispatchMCPFrame(ctx context.Context, sess *Session, frame []byte, dispatcher MCPDispatcher) ([]byte, error) {
	var envelope mcpRequestEnvelope
	if _, prepared := ctx.Value(mcpDispatchRequestContextKey{}).(mcpDispatchRequestInfo); !prepared {
		_ = json.Unmarshal(frame, &envelope) // Public dispatch retains normal validation.
		id := ""
		if sess != nil {
			id = sess.ID
		}
		classifier, _ := dispatcher.(MCPDispatchClassifier)
		ctx = prepareMCPDispatchContext(ctx, id, envelope, classifier)
	}
	if mcpDispatchSlotLeaseForContext(ctx) == nil && len(envelope.ID) > 0 {
		ctx = withMCPDispatchSlotLease(ctx)
	}
	reply, _, err := sess.dispatchMCPOnceContext(ctx, frame, func() ([]byte, error) {
		reply, err := dispatcher.Dispatch(ctx, sess, frame)
		if ctxErr := ctx.Err(); ctxErr != nil && (!isMCPDispatchUnbounded(ctx) || err != nil || len(reply) == 0) {
			return nil, ctxErr
		}
		return reply, err
	})
	return reply, err
}

// MCPDispatchBusyResponse gives HTTP the same retryable JSON-RPC refusal as
// Unix without turning dispatch errors into generic transport errors. It is
// nil for notifications or non-admission failures; those keep transport policy.
func MCPDispatchBusyResponse(frame []byte, err error) []byte {
	if !errors.Is(err, errMCPDispatchSaturated) {
		return nil
	}
	var envelope mcpRequestEnvelope
	if json.Unmarshal(frame, &envelope) != nil || len(envelope.ID) == 0 {
		return nil
	}
	if _, valid := canonicalMCPRequestID(envelope.ID); !valid {
		return nil
	}
	return mcpDispatchBusyResponse(envelope.ID, envelope.Method, err)
}
func mcpDispatchBusyResponse(id json.RawMessage, method string, err error) []byte {
	data := map[string]any{"outcome": "server_busy", "retryable": true, "retry_after_ms": mcpDispatchBusyRetryAfter.Milliseconds()}
	if method == "tools/call" {
		data["delivery_unknown"] = false
	}
	var busy *mcpDispatchBusyError
	if errors.As(err, &busy) {
		data["queue_wait_ms"] = busy.waited.Milliseconds()
		retry := busy.retryAfter
		if retry <= 0 {
			retry = mcpDispatchBusyRetryAfter
		}
		data["retry_after_ms"] = retry.Milliseconds()
	}
	return mcpErrorResponse(id, -32002, "MCP dispatcher is busy", data)
}
