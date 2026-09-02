package hooks

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// hookCWD carries the working directory of the in-flight hook invocation so the
// daemon-socket fallback in callServerTool can scope its tools/call handshake to
// the right workspace. A hook binary is single-shot — it reads one stdin
// payload, handles one event, and exits — so a package-level value set once at
// dispatch entry is race-free in production; tests set-and-restore it. The
// mutex is only for tidiness under `go test -race`, where several dispatchers
// run in one process.
var (
	hookCWDMu sync.RWMutex
	hookCWD   string
)

// setHookCWD records the payload CWD for the current hook invocation. Pass ""
// to clear it (dispatchers defer a clear so a value never leaks across the
// sequential tests sharing one process).
func setHookCWD(cwd string) {
	hookCWDMu.Lock()
	hookCWD = cwd
	hookCWDMu.Unlock()
}

// loadHookCWD returns the CWD recorded for the current hook invocation, or ""
// when none was set (the pure-HTTP unit tests, or a dispatcher that opted out
// of the socket fallback).
func loadHookCWD() string {
	hookCWDMu.RLock()
	defer hookCWDMu.RUnlock()
	return hookCWD
}

// callServerToolDaemonFn is the seam production uses to reach the daemon over
// its unix socket; tests swap it so a hook never touches a real daemon.
var callServerToolDaemonFn = callServerToolViaDaemon

// callServerToolTimeout bounds the whole daemon round-trip (dial + handshake +
// tools/call) per call, so a wedged daemon can never stall a Stop / subagent
// hook past the host's own hook timeout.
const callServerToolTimeout = 5 * time.Second

const (
	// hookInternalToolSurface must impose no restriction, because the hooks
	// call several tools that are not in the `core` preset: contracts
	// (renderContractMismatches), explain_change_impact (ownedRisk),
	// get_symbol_history (renderSymbolHistory), and feedback.
	//
	// Under a restricted preset those tools are held out of the underlying
	// MCP server until something promotes them, so a tools/call by name comes
	// back "tool not found" and the hook — which treats every failure as an
	// unreachable bridge — silently renders nothing. That is how the Stop
	// briefing's contracts section and the PreCompact churn section came to be
	// dead in the default posture without anyone noticing.
	//
	// "full" resolves to a nil policy (no allow-set, nothing deferred or
	// hidden), so it costs nothing here: a hook dispatches by name and never
	// reads tools/list, so a wide surface carries no payload.
	hookInternalToolSurface = "full"
	hookInternalToolMode    = "defer"
)

// hookMCPHandshake explicitly selects the compatibility surface used by hook
// internals. Hooks call canonical implementation tools such as
// get_file_summary and detect_changes; they are not an agent session and must
// not inherit the compact named-client default, nor an agent-sized preset that
// would silence the tools listed above.
func hookMCPHandshake(cwd string) daemon.Handshake {
	return daemon.Handshake{
		Mode:       daemon.ModeMCP,
		ClientName: "gortex-hook",
		CWD:        cwd,
		Tools:      hookInternalToolSurface,
		ToolsMode:  hookInternalToolMode,
	}
}

// callServerToolViaDaemon runs one MCP tools/call against the local daemon over
// its AF_UNIX socket and returns the first text content block, or "" on any
// error. cwd scopes the handshake to the caller's workspace so tools that read
// the working tree (detect_changes) or the active project (analyze) resolve the
// right repo. Mirrors fileIndexScopeViaDaemon's transport; kept separate so the
// file-scope probe stays a tight single-purpose path.
func callServerToolViaDaemon(cwd, name string, args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	client, err := daemon.Dial(hookMCPHandshake(cwd))
	if err != nil {
		return ""
	}
	defer client.Close()
	_ = client.Conn.SetDeadline(time.Now().Add(callServerToolTimeout))

	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	})
	if err != nil {
		return ""
	}
	if err := client.WriteMCPFrame(frame); err != nil {
		return ""
	}
	resp, err := client.ReadMCPFrame()
	if err != nil {
		return ""
	}
	return parseToolCallText(resp)
}

// parseToolCallText unwraps a tools/call JSON-RPC response to the first content
// block's text. Returns "" on a tool error or a shape mismatch so the caller
// treats it the same as an unreachable bridge.
func parseToolCallText(resp []byte) string {
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &rpc); err != nil {
		return ""
	}
	if rpc.Result.IsError || len(rpc.Result.Content) == 0 {
		return ""
	}
	return rpc.Result.Content[0].Text
}
