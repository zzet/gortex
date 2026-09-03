package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"
)

// DefaultToolCallTimeout bounds how long one tool handler may occupy its
// transport before the call is answered with a terminal error. It matches the
// daemon socket's per-request budget (daemon.DefaultMCPToolCallTimeout) so a
// tool behaves the same however it was reached: over the daemon socket, over
// Streamable HTTP, or through the embedded stdio server.
//
// Parity is the point. The daemon path has had a request lifetime for a while;
// the embedded path — `gortex mcp` with no daemon available — had none, because
// mcp-go's stdio server hands every handler the *server-lifetime* context. One
// handler blocked on a lock therefore held its worker forever, and once the
// worker pool and its queue were exhausted mcp-go falls back to running tool
// calls inline on the read loop, at which point the session stops reading stdin
// and every later request — including ones that would have answered instantly —
// hangs with no output and no error.
const DefaultToolCallTimeout = 60 * time.Second

// ToolCallTimeoutEnv overrides DefaultToolCallTimeout with any Go duration.
// "0" / "off" disables the bound for operators who would rather have a call
// hang than be cut off (long analyze / ask runs on a slow box).
const ToolCallTimeoutEnv = "GORTEX_MCP_TOOL_TIMEOUT"

// maxAbandonedToolCalls caps how many timed-out handlers may still be running
// in the background. Abandoning a stuck handler frees the transport, but the
// goroutine lives on until whatever it is blocked on clears; without a cap a
// permanently wedged subsystem would accumulate one goroutine per retry
// forever. Past the cap new calls fail fast with a diagnosis instead, which is
// the honest answer — the server genuinely cannot serve them.
const maxAbandonedToolCalls = 64

// abandonedToolCalls counts handlers that outran their budget and are still
// running detached. Process-wide: the limit is about total leaked work, and a
// wedged store affects every session at once.
var abandonedToolCalls atomic.Int64

// AbandonedToolCalls reports how many tool handlers outran their deadline and
// are still running. Non-zero means some subsystem is blocking — it is the
// signal that distinguishes "this one tool is slow" from "the server is stuck".
func AbandonedToolCalls() int64 { return abandonedToolCalls.Load() }

// transportDeadlineMargin is how far ahead of an inherited transport deadline
// the handler bound fires. Without it the two deadlines race and the client
// sometimes gets a bare JSON-RPC transport error instead of the tool-shaped
// result that tells an agent what happened and what to do next.
const transportDeadlineMargin = time.Second

// toolCallTimeout resolves the effective per-call budget.
func (s *Server) toolCallTimeout() time.Duration {
	if s != nil && s.ToolCallTimeout != 0 {
		return s.ToolCallTimeout
	}
	return toolCallTimeoutFromEnv()
}

func toolCallTimeoutFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(ToolCallTimeoutEnv))
	if raw == "" {
		return DefaultToolCallTimeout
	}
	switch strings.ToLower(raw) {
	case "0", "off", "false", "none":
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultToolCallTimeout
	}
	return d
}

// boundToolHandler is the hang firewall: the liveness counterpart to the panic
// firewall in wrapToolHandlerMode. A panic in one handler used to take down
// every session's transport; so does a hang, just more quietly. Both are
// contained at the same chokepoint so every tool on every transport inherits
// the protection.
//
// On expiry the caller gets a terminal, structured tool error and the
// transport's slot is returned immediately. The handler keeps running on its
// own goroutine — it may be inside a store call that cannot be interrupted —
// but it no longer owns the client's turn, so the session stays responsive and
// the next request is served normally.
//
// The derived context is cancelled on the way out, so handlers that *do* watch
// their context unwind promptly rather than finishing work nobody will read.
//
// boundHandler is the shared core; the three callable surfaces (tools,
// resources, prompts) differ only in how they render "abandoned" — a tool call
// carries it as an in-band error result, a resource read and a prompt fetch as
// a JSON-RPC error.
func boundHandler[Req, Res any](
	s *Server,
	kind, name string,
	h func(context.Context, Req) (Res, error),
	busy func(stuck int64, timeout time.Duration) (Res, error),
	expired func(timeout time.Duration, note *mutationCommitNote) (Res, error),
	panicked func(recovered any) (Res, error),
) func(context.Context, Req) (Res, error) {
	return func(ctx context.Context, req Req) (Res, error) {
		timeout := s.toolCallTimeout()
		if timeout <= 0 {
			return h(ctx, req)
		}
		// Fire just inside any deadline the transport already imposed (the
		// daemon socket's per-request lifetime), so the client receives this
		// diagnosis rather than the transport's opaque timeout.
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := time.Until(deadline) - transportDeadlineMargin; remaining < timeout {
				timeout = remaining
			}
			if timeout <= 0 {
				return h(ctx, req)
			}
		}

		if stuck := abandonedToolCalls.Load(); stuck >= maxAbandonedToolCalls {
			return busy(stuck, timeout)
		}

		callCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		// Every call carries a commit note. A read handler never writes to it
		// and pays one context value; a mutating handler stamps its disk commit
		// there, which is the only way this frame can say what landed after it
		// has stopped waiting for the handler.
		callCtx, commitNote := withMutationCommitNote(callCtx)

		type outcome struct {
			res Res
			err error
		}
		// Buffered: the detached handler must be able to finish and exit even
		// after this frame has returned, or abandoning a call would leak a
		// goroutine blocked on an unread channel — a second leak on top of the
		// one we are already tolerating.
		done := make(chan outcome, 1)
		started := time.Now()
		go func() {
			// Panic firewall. Moving the handler onto its own goroutine takes
			// it out from under whatever recover the transport provided, so it
			// has to bring its own — otherwise one panicking handler takes down
			// the process instead of the request. (Tool handlers also carry the
			// firewall in wrapToolHandlerMode; this covers the rest, and
			// double-recovering is harmless.)
			defer func() {
				if r := recover(); r != nil {
					if s != nil && s.logger != nil {
						s.logger.Error("mcp: handler panic recovered",
							zap.String("kind", kind), zap.String("name", name),
							zap.Any("panic", r), zap.Stack("stack"))
					}
					done <- func() outcome {
						res, err := panicked(r)
						return outcome{res: res, err: err}
					}()
				}
			}()
			res, err := h(callCtx, req)
			done <- outcome{res: res, err: err}
		}()

		select {
		case got := <-done:
			return got.res, got.err
		case <-callCtx.Done():
			if ctx.Err() != nil {
				// The *caller* went away (client cancelled, or the transport's
				// own request lifetime fired first). Report that, not a
				// deadline we did not reach.
				var zero Res
				return zero, ctx.Err()
			}
			stuck := abandonedToolCalls.Add(1)
			go func() {
				<-done
				abandonedToolCalls.Add(-1)
			}()
			if s != nil && s.logger != nil {
				s.logger.Warn("mcp: request exceeded its deadline; abandoning to keep the session alive",
					zap.String("kind", kind),
					zap.String("name", name),
					zap.Duration("deadline", timeout),
					zap.Duration("elapsed", time.Since(started)),
					zap.Int64("abandoned_in_flight", stuck))
			}
			return expired(timeout, commitNote)
		}
	}
}

// overlayPrepared installs the calling session's overlay view on the request
// context before the handler runs — the same installation the tool wrapper
// applies in wrapToolHandlerMode.
//
// Resources and prompts read the graph through s.readerFor(ctx) exactly like
// tools do, but nothing had ever put a view on their context, so every
// resource read and prompt fetch silently answered from the base graph while
// the session's editor buffers were live. That made gortex://report and the
// orientation prompt disagree with every tool call in the same session.
//
// Applied inside the bound handler, so preparation runs under the same
// deadline and on the same detached goroutine as the handler it feeds.
func overlayPrepared[Req, Res any](s *Server, h func(context.Context, Req) (Res, error)) func(context.Context, Req) (Res, error) {
	return func(ctx context.Context, req Req) (Res, error) {
		prepared, _, err := s.prepareOverlayRequest(ctx)
		if err != nil {
			var zero Res
			if ctxErr := requestContextError(ctx, err); ctxErr != nil {
				return zero, ctxErr
			}
			return zero, err
		}
		return h(prepared, req)
	}
}

// busyMessage is the shared diagnosis for "too many handlers are already
// stuck to admit another".
func busyMessage(what string, stuck int64, timeout time.Duration) string {
	return fmt.Sprintf(
		"gortex is not serving %s: %d handlers are blocked past their %s deadline. "+
			"Something the graph depends on is wedged (most often a long index / enrichment holding the store). "+
			"Check `gortex daemon status`; `gortex daemon restart` clears it.",
		what, stuck, timeout)
}

// abandonedMessage is the shared diagnosis for a request that outran its
// budget. It names the side-effect uncertainty explicitly: the handler is
// still running, so the agent must not conclude the work did not happen.
func abandonedMessage(what, name string, timeout time.Duration) string {
	return fmt.Sprintf(
		"%s %q exceeded its %s deadline and was abandoned so the session stays responsive. "+
			"The work may still complete in the background, so treat any side effect as unknown. "+
			"This usually means the graph is busy (indexing, enrichment, or a slow store) — "+
			"retry, or check `gortex daemon status`.",
		what, name, timeout)
}

// abandonedToolMessage is abandonedMessage plus what the handler managed to do
// to the filesystem before this frame gave up on it.
//
// The generic "treat any side effect as unknown" wording is correct only when
// nothing is known. Once a disk commit has been confirmed it is actively
// harmful: a client that reads it as "nothing happened" retries, or falls back
// to its own editor, and applies the same logical change twice. So the three
// states are reported separately and the JSON tail is machine-readable.
func abandonedToolMessage(name string, timeout time.Duration, verdict mutationCommitVerdict) string {
	base := abandonedMessage("tool", name, timeout)
	switch verdict.Status {
	case "":
		// No mutating handler registered a commit, so nothing was written by
		// this call. Read tools land here, and so does a write refused during
		// validation.
		return base
	case mutationDiskNotApplied:
		base = fmt.Sprintf(
			"tool %q exceeded its %s deadline and was abandoned so the session stays responsive. "+
				"No filesystem change was applied — the handler stopped at its cancellation gate before the disk commit, "+
				"so retrying is safe. This usually means the graph is busy (indexing, enrichment, or a slow store) — "+
				"retry, or check `gortex daemon status`.",
			name, timeout)
	case mutationDiskCommitted:
		base = fmt.Sprintf(
			"tool %q exceeded its %s deadline, but its filesystem change WAS COMMITTED before the deadline fired. "+
				"Do NOT retry this edit and do NOT apply it by another route — you would duplicate it. "+
				"Only the graph refresh is still outstanding; the files below are already on disk with the listed SHAs.",
			name, timeout)
	case mutationDiskInFlight:
		base = fmt.Sprintf(
			"tool %q exceeded its %s deadline while a filesystem write was in progress, so the outcome is genuinely unknown. "+
				"Do not retry blindly: query the receipt below (`mutation_status`, or `edit` with operation:\"receipt\") "+
				"to learn whether the bytes landed.",
			name, timeout)
	}
	if encoded, err := json.Marshal(verdict); err == nil {
		base += "\nmutation_commit=" + string(encoded)
	}
	return base
}

func (s *Server) boundToolHandler(h mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.Params.Name
		return boundHandler(s, "tool", name, (func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error))(h),
			func(stuck int64, timeout time.Duration) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultError(busyMessage("tool calls", stuck, timeout)), nil
			},
			func(timeout time.Duration, note *mutationCommitNote) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultError(abandonedToolMessage(name, timeout, note.verdict())), nil
			},
			func(r any) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultError(fmt.Sprintf("tool %q internal error: %v", name, r)), nil
			},
		)(ctx, req)
	}
}

// addResource registers a resource with the same hang firewall tool calls get.
//
// Resources need it more, not less. mcp-go's stdio transport queues tool calls
// onto a worker pool but handles every other method — resources/read included —
// inline on the single goroutine that reads stdin. A slow
// `gortex://index-health` or `gortex://report` read therefore stops the server
// reading its own input: not one hung call, a hung session, with the client's
// notifications/cancelled sitting unread in the pipe behind it.
func (s *Server) addResource(resource mcp.Resource, handler mcpserver.ResourceHandlerFunc) {
	s.mcpServer.AddResource(resource, s.boundResourceHandler(resource.URI, handler))
}

// addResourceTemplate is addResource for URI-templated resources; same
// transport, same firewall.
func (s *Server) addResourceTemplate(tmpl mcp.ResourceTemplate, handler mcpserver.ResourceTemplateHandlerFunc) {
	uri := tmpl.URITemplate.Raw()
	s.mcpServer.AddResourceTemplate(tmpl,
		mcpserver.ResourceTemplateHandlerFunc(s.boundResourceHandler(uri, mcpserver.ResourceHandlerFunc(handler))))
}

// addPrompt registers a prompt with the same firewall. Prompts travel the same
// inline read-loop path as resources on the embedded stdio transport, and
// gortex's prompt handlers do real graph work (whole-edge scans, communities,
// processes, a git diff) — exactly the shape that blocks.
func (s *Server) addPrompt(prompt mcp.Prompt, handler mcpserver.PromptHandlerFunc) {
	s.mcpServer.AddPrompt(prompt, s.boundPromptHandler(prompt.Name, handler))
}

func (s *Server) boundResourceHandler(uri string, h mcpserver.ResourceHandlerFunc) mcpserver.ResourceHandlerFunc {
	bounded := boundHandler(s, "resource", uri, overlayPrepared(s, (func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error))(h)),
		func(stuck int64, timeout time.Duration) ([]mcp.ResourceContents, error) {
			return nil, errors.New(busyMessage("resource reads", stuck, timeout))
		},
		func(timeout time.Duration, _ *mutationCommitNote) ([]mcp.ResourceContents, error) {
			return nil, errors.New(abandonedMessage("resource", uri, timeout))
		},
		func(r any) ([]mcp.ResourceContents, error) {
			return nil, fmt.Errorf("resource %s internal error: %v", uri, r)
		},
	)
	return func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		if w, deferred := s.deferAnalyzerResource(uri); deferred {
			return jsonResource(uri, map[string]any{"warming": newEnrichmentPendingEnvelope(w)})
		}
		// Resources are not cheap reads: gortex://report and
		// gortex://surprises materialise AllEdges and run whole-graph
		// dead-code and cycle passes. They need the same bracket every
		// tool call gets — without it the process reads as idle mid-scan,
		// so a scheduled FreeOSMemory can fire on top of the scan, and
		// nothing schedules a release once it finishes.
		beginMCPToolCall()
		defer func() {
			endMCPToolCall(s.logger, "resource:"+uri)
			s.releaseTransientAnalysisIfIdle()
		}()
		return bounded(ctx, req)
	}
}

func (s *Server) boundPromptHandler(name string, h mcpserver.PromptHandlerFunc) mcpserver.PromptHandlerFunc {
	bounded := boundHandler(s, "prompt", name, overlayPrepared(s, (func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error))(h)),
		func(stuck int64, timeout time.Duration) (*mcp.GetPromptResult, error) {
			return nil, errors.New(busyMessage("prompt requests", stuck, timeout))
		},
		func(timeout time.Duration, _ *mutationCommitNote) (*mcp.GetPromptResult, error) {
			return nil, errors.New(abandonedMessage("prompt", name, timeout))
		},
		func(r any) (*mcp.GetPromptResult, error) {
			return nil, fmt.Errorf("prompt %s internal error: %v", name, r)
		},
	)
	// Same reasoning as boundResourceHandler: the orientation prompt runs
	// scopedNodes twice and materialises AllEdges, so it belongs inside
	// the activity bracket and owes a release on the way out.
	return func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		beginMCPToolCall()
		defer func() {
			endMCPToolCall(s.logger, "prompt:"+name)
			s.releaseTransientAnalysisIfIdle()
		}()
		return bounded(ctx, req)
	}
}
