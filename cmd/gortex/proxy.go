package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	semver "github.com/zzet/gortex/internal/version"
)

// coldStartTools is the static core catalogue the proxy answers a cold-start
// tools/list with — the hot tools every client needs first — before the
// daemon's full live list arrives. Deliberately small; the client refreshes on
// the daemon's tools/list_changed notification once the connection is live.
var coldStartTools = []string{
	"smart_context", "search_symbols", "find_usages", "get_callers",
	"get_symbol_source", "get_file_summary", "read_file", "get_repo_outline",
}

// answerColdStart returns a locally-synthesized JSON-RPC response for a frame
// that can be answered without the daemon — initialize and tools/list — so an
// MCP client completes its handshake immediately while the daemon connects in
// the background. ok is false for any other frame (notably tools/call), which
// must reach the daemon (or the embedded fallback). The active tool-surface
// preset is applied to the cold-start list so a restricted session never sees a
// tool it isn't allowed to call.
func answerColdStart(frame []byte, surface *gortexmcp.ToolSurface) (reply []byte, ok bool) {
	var peek struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if json.Unmarshal(frame, &peek) != nil {
		return nil, false
	}
	switch peek.Method {
	case "initialize":
		return staticInitializeResult(peek.ID), true
	case "tools/list":
		return staticToolsListResult(peek.ID, surface), true
	default:
		return nil, false
	}
}

// staticInitializeResult builds the cold-start initialize response: enough for
// the client to proceed, with instructions that the full catalogue is arriving.
func staticInitializeResult(id json.RawMessage) []byte {
	return jsonRPCResult(id, map[string]any{
		"protocolVersion": "2025-06-18",
		"serverInfo":      map[string]any{"name": "gortex", "version": gortexmcp.Version},
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
		"instructions":    "Gortex is connecting to its daemon — the full tool catalogue arrives momentarily.",
	})
}

// staticToolsListResult answers tools/list with the cold-start core set, minus
// anything an active surface preset disallows.
func staticToolsListResult(id json.RawMessage, surface *gortexmcp.ToolSurface) []byte {
	tools := make([]map[string]any, 0, len(coldStartTools))
	for _, n := range coldStartTools {
		if surface != nil && surface.Active() && !surface.Allows(n) {
			continue
		}
		tools = append(tools, map[string]any{"name": n})
	}
	return jsonRPCResult(id, map[string]any{"tools": tools})
}

// jsonRPCResult marshals a JSON-RPC 2.0 success response echoing the request id.
func jsonRPCResult(id json.RawMessage, result any) []byte {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
	return body
}

// runProxy relays MCP JSON-RPC traffic between stdio (the MCP client) and
// the daemon's Unix socket. Exactly what `gortex mcp` does when it
// detects a running daemon and isn't forced to embedded mode.
//
// Returns (true, nil) when the proxy ran and finished cleanly. Returns
// (false, nil) when the daemon isn't reachable — the caller applies its
// configured embedded-mode policy. Any other error is a real problem.
func runProxy(ctx context.Context, surface *gortexmcp.ToolSurface) (ran bool, err error) {
	cwd, wdErr := resolveLaunchCWD()
	if wdErr != nil {
		return false, fmt.Errorf("cwd: %w", wdErr)
	}
	toolSpec, toolMode := clientToolPreference()
	h := daemon.Handshake{
		Mode:             daemon.ModeMCP,
		CWD:              cwd,
		ClientName:       detectClientName(),
		PID:              os.Getpid(),
		LogicalSessionID: newProxyLogicalSessionID(),
		Tools:            toolSpec,
		ToolsMode:        toolMode,
	}
	client, recoverable, err := dialDaemonWithRetry(ctx, h)
	if err != nil && !recoverable {
		return false, fmt.Errorf("dial daemon: %w", err)
	}
	if client == nil {
		if errors.Is(err, daemon.ErrProtocolVersionMismatch) {
			fmt.Fprintln(os.Stderr, "[gortex mcp] daemon protocol mismatch")
		} else {
			fmt.Fprintln(os.Stderr, "[gortex mcp] daemon unreachable after retry window")
		}
		return false, nil
	}

	logProxyConnection(os.Stderr, client, false)
	if warn := daemonSkewWarning(client.Ack.DaemonVersion, canonicalVersion()); warn != "" {
		fmt.Fprintln(os.Stderr, "[gortex mcp] "+warn)
	}
	if surface != nil && surface.Active() {
		fmt.Fprintf(os.Stderr, "[gortex mcp] tool surface restricted (preset %q)\n", surface.Preset())
	}

	orphanCh := make(chan struct{}, 1)
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	go orphanWatch(watchCtx, orphanPollInterval, os.Getppid, func() {
		fmt.Fprintln(os.Stderr, "[gortex mcp] parent process exited; closing proxy")
		select {
		case orphanCh <- struct{}{}:
		default:
		}
	})

	if err := relayProxySession(ctx, h, client, os.Stdin, os.Stdout, os.Stderr, surface, orphanCh); err != nil {
		return true, fmt.Errorf("proxy relay: %w", err)
	}
	return true, nil
}

// daemonSkewWarning returns a one-line stderr warning when the daemon
// reports a different build than this binary, or "" when they match,
// the daemon did not report a version, or either side is a dev build
// (no injected identity — comparing against a dev build would noise
// every dev run, and a dev-built daemon cannot be "upgraded" by a
// restart, so the remedy advice would be wrong for it too).
// The remedy is direction-aware: an older daemon should be restarted
// (a restart respawns it from this newer binary), while a newer daemon
// means this binary is the stale side and upgrading it is the fix —
// restarting would downgrade the daemon to this older build. The
// upgrade remedy is 'gortex upgrade' rather than a package-manager
// specific command: it upgrades the way gortex was installed (brew,
// scoop, go install, or the install script) and restarts the daemon
// around the binary swap. When
// either side does not parse as semver, or only the build metadata
// differs (which SemVer precedence ignores), a generic remedy that
// covers both directions is emitted. Implements the documented intent
// in docs/versioning.md: the daemon exposes DaemonVersion so "clients
// can feature-gate or warn on mismatch"; this warns and continues —
// never gates.
func daemonSkewWarning(daemonVer, localVer string) string {
	if daemonVer == "" || localVer == "" || localVer == "v0.0.0-dev" || daemonVer == "v0.0.0-dev" || daemonVer == localVer {
		return ""
	}
	base := fmt.Sprintf("warning: daemon %s != binary %s", daemonVer, localVer)
	d, dErr := semver.Parse(daemonVer)
	l, lErr := semver.Parse(localVer)
	if dErr != nil || lErr != nil {
		return base + " — run 'gortex daemon restart' or 'gortex upgrade'"
	}
	switch semver.Compare(d, l) {
	case -1: // daemon older — restarting respawns it from this newer binary
		return base + " — run 'gortex daemon restart' to upgrade the daemon"
	case 1: // daemon newer — this binary is the stale side
		return base + " — this binary is older than the running daemon — run 'gortex upgrade'"
	default: // same precedence, different build metadata
		return base + " — run 'gortex daemon restart' or 'gortex upgrade'"
	}
}

func newProxyLogicalSessionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	// crypto/rand failure should not make MCP unavailable. PID + monotonic
	// wall-clock entropy is sufficient for the user-local daemon namespace.
	return fmt.Sprintf("proxy-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// orphanPollInterval is how often the proxy checks whether its parent
// process is still alive. It is a var so tests can shorten it.
var orphanPollInterval = 5 * time.Second

// dialDaemon is the seam runProxy dials through. A package var so tests can
// substitute a fake without a real socket.
var dialDaemon = daemon.Dial

// proxyDialRetryWindow bounds how long dialDaemonWithRetry keeps retrying a
// recoverable "daemon unavailable" dial error before conceding to the embedded
// server; proxyDialRetryInterval is the gap between attempts. By the time the
// proxy dials, resolveDaemonDecision has already confirmed the socket is up
// (daemonReady) or waited for it (daemonAutostarted) — but under a CPU- and
// GC-saturated warmup the daemon's accept() can briefly exceed the 500ms dial
// timeout, surfacing as ErrDaemonUnavailable. Retrying rides that window out so
// a connecting session lands on the real daemon (and self-heals as warmup
// fills the graph) instead of dead-ending on an empty embedded graph. Vars so
// tests can shorten them.
var (
	proxyDialRetryWindow   = 20 * time.Second
	proxyDialRetryInterval = 250 * time.Millisecond
)

// dialDaemonWithRetry dials the daemon, retrying transient "unavailable"
// errors (socket up but accept() starved by warmup) for a bounded window.
// Returns:
//   - (client, false, nil)  on success.
//   - (nil, false, err)     on a non-recoverable error — the caller surfaces it.
//   - (nil, true, lastErr)  when the window expires with the daemon still
//     unreachable, or on a protocol-version mismatch (which never resolves by
//     waiting) — the caller applies its embedded-mode policy. lastErr lets the
//     caller distinguish the mismatch case for logging.
func dialDaemonWithRetry(ctx context.Context, h daemon.Handshake) (client *daemon.Client, recoverable bool, lastErr error) {
	deadline := time.Now().Add(proxyDialRetryWindow)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		c, err := dialDaemon(h)
		if err == nil {
			return c, false, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		if !daemon.ShouldFallBackToEmbedded(err) {
			return nil, false, err
		}
		// A protocol-version mismatch is a stale daemon after an upgrade —
		// waiting can't fix it, so return to the startup policy immediately.
		if errors.Is(err, daemon.ErrProtocolVersionMismatch) {
			return nil, true, err
		}
		if time.Now().After(deadline) {
			return nil, true, err
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(proxyDialRetryInterval):
		}
	}
}

// orphanWatch polls getppid every interval and invokes onOrphan exactly
// once when the proxy's parent process has gone away — detected as a change
// of parent PID (reparented to init=1 on classic Unix, or to the nearest
// subreaper). Watching for a *change* is strictly more robust than testing
// for PID 1 alone, which misses subreaper reparenting (containers, systemd
// user sessions, a wrapping CLI that calls prctl(PR_SET_CHILD_SUBREAPER)).
// It self-disarms when there is no meaningful parent to watch (orig <= 1),
// and is an inert no-op on platforms that never reparent — there the parent
// PID stays equal to orig for the whole process lifetime.
func orphanWatch(ctx context.Context, interval time.Duration, getppid func() int, onOrphan func()) {
	orig := getppid()
	if orig <= 1 || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if getppid() != orig {
				onOrphan()
				return
			}
		}
	}
}

// detectClientName makes a best-effort guess at which MCP client spawned
// us. Purely for the initial handshake telemetry — the authoritative
// answer comes from the MCP `initialize` request's clientInfo, which
// the daemon dispatcher (cmd/gortex/daemon_mcp.go::maybeSnoopInitialize)
// applies once the first frame arrives. The handshake-time guess
// only matters for the few hundred milliseconds before initialize
// reaches us.
//
// Env-var sniffing here favours the actual variables current MCP
// hosts set. Claude Code: CLAUDECODE=1 (current builds set this) plus
// CLAUDE_CODE_ENTRYPOINT=cli|sdk|... Other hosts kept best-effort.
func detectClientName() string {
	switch {
	case os.Getenv("CLAUDECODE") != "" || os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "" || os.Getenv("CLAUDE_CODE_WORKSPACE") != "":
		return "claude-code"
	case os.Getenv("CURSOR_TRACE_ID") != "" || os.Getenv("CURSOR_WORKSPACE") != "":
		return "cursor"
	case os.Getenv("KIRO_WORKSPACE") != "":
		return "kiro"
	case os.Getenv("WINDSURF_WORKSPACE") != "":
		return "windsurf"
	case os.Getenv("CODEX_WORKSPACE") != "":
		return "codex"
	case os.Getenv("ANTIGRAVITY_AGENT") != "":
		return "antigravity"
	case os.Getenv("VSCODE_PID") != "" || os.Getenv("VSCODE_IPC_HOOK") != "":
		// VS Code with the MCP extension. Coarse — Continue / Cline
		// embedders run inside VS Code too, so this is just a hint
		// until the MCP initialize frame lands and overrides it.
		return "vscode"
	case os.Getenv("ZED_TERM") != "" || os.Getenv("ZED_TERMINAL") != "":
		return "zed"
	}
	return "unknown"
}

// resolveLaunchCWD picks the most plausible project cwd for an MCP
// launch, defending against editors that spawn the MCP server with
// cwd unset or set to a non-project directory:
//
//   - Antigravity sometimes spawns with cwd=`/`.
//   - Cursor launches user-level `~/.cursor/mcp.json` entries with
//     cwd=$HOME (see gortexhq/gortex#19).
//
// Resolution order:
//  1. os.Getwd() when it looks like a project root (not `/` or $HOME).
//  2. $PWD when it differs and isn't `/` or $HOME.
//  3. The first non-empty editor workspace env var (CURSOR_WORKSPACE,
//     CLAUDE_CODE_WORKSPACE, WINDSURF_WORKSPACE, KIRO_WORKSPACE,
//     CODEX_WORKSPACE, ANTIGRAVITY_WORKSPACE, VSCODE_WORKSPACE).
//  4. Fall through to whatever Getwd() returned — the daemon resolves
//     it per session (ScopeForCWD) and surfaces a clear repo_not_tracked
//     error when the cwd maps to no tracked repo, isolating the session
//     to nothing rather than the whole graph.
func resolveLaunchCWD() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if !isAmbiguousLaunchCWD(cwd) {
		return cwd, nil
	}
	if pwd := os.Getenv("PWD"); pwd != cwd && !isAmbiguousLaunchCWD(pwd) {
		return pwd, nil
	}
	for _, key := range []string{
		"CURSOR_WORKSPACE",
		"CLAUDE_CODE_WORKSPACE",
		"WINDSURF_WORKSPACE",
		"KIRO_WORKSPACE",
		"CODEX_WORKSPACE",
		"ANTIGRAVITY_WORKSPACE",
		"VSCODE_WORKSPACE",
	} {
		if v := os.Getenv(key); !isAmbiguousLaunchCWD(v) {
			return v, nil
		}
	}
	return cwd, nil
}

// isAmbiguousLaunchCWD returns true when `p` is an editor-launch cwd
// we can't trust to point at the active project — empty, `/`, or the
// user's home directory.
//
// The home comparison goes through filepath.EvalSymlinks so the
// macOS `/var → /private/var` redirect (and similar symlinks) don't
// cause a false negative when an editor sets cwd via os.Chdir and
// then Getwd reports the resolved form.
func isAmbiguousLaunchCWD(p string) bool {
	if p == "" || p == "/" {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	if p == home {
		return true
	}
	resP, errP := filepath.EvalSymlinks(p)
	resH, errH := filepath.EvalSymlinks(home)
	return errP == nil && errH == nil && resP == resH
}

// The former shouldTryProxy stdin-TTY heuristic was removed: `gortex mcp`
// is now daemon-first via resolveDaemonDecision (ensure-daemon → relay,
// with an embedded fallback) regardless of whether stdin is a terminal
// or a pipe.
