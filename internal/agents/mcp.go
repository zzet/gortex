package agents

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

// UpsertMCPServer merges a gortex-flavored MCP server stanza into a
// map that follows the standard {"mcpServers": {<name>: {...}}}
// shape used by Claude Code, Cursor, VS Code, Continue.dev, Cline,
// and Kiro. Returns true when the map was modified (false when a
// gortex stanza was already present and opts.Force is off).
//
// serverName is the key under mcpServers (canonically "gortex").
// entry is the stanza value — adapters produce their own variant
// when the target client uses a different shape (e.g. Cline's
// alwaysAllow list, Kiro's autoApprove list).
func UpsertMCPServer(root map[string]any, serverName string, entry map[string]any, opts ApplyOpts) (changed bool) {
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		servers = make(map[string]any)
	}
	if _, exists := servers[serverName]; exists && !opts.Force {
		return false
	}
	servers[serverName] = entry
	root["mcpServers"] = servers
	return true
}

// RemoveMCPServer deletes serverName from the {"mcpServers": {...}}
// map, pruning the parent key when removal leaves it empty. Returns
// true when the map was modified. Counterpart to UpsertMCPServer for
// uninstall paths — all other servers (and the user's other config)
// are left untouched.
func RemoveMCPServer(root map[string]any, serverName string) (changed bool) {
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		return false
	}
	if _, exists := servers[serverName]; !exists {
		return false
	}
	delete(servers, serverName)
	if len(servers) == 0 {
		delete(root, "mcpServers")
	} else {
		root["mcpServers"] = servers
	}
	return true
}

// UpsertMCPServerWithMigration is like UpsertMCPServer but also
// rewrites entries that look Gortex-authored (any `gortex mcp ...`
// stanza) even without opts.Force. This lets `gortex install` swap
// the legacy `mcp --index . --watch` shape — which fails Cursor's
// global-cwd handshake — for the daemon-proxy shape on next install.
//
// If the existing entry is already byte-identical to `entry`, returns
// false so the caller reports "already-configured" instead of a noisy
// rewrite. User-customized entries (anything that doesn't look like
// Gortex authored it) are left untouched unless opts.Force is set.
func UpsertMCPServerWithMigration(root map[string]any, serverName string, entry map[string]any, opts ApplyOpts) (changed bool) {
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		servers = make(map[string]any)
	}
	if existing, exists := servers[serverName]; exists {
		if MCPEntriesEqual(existing, entry) {
			return false
		}
		if !opts.Force && !IsGortexAuthoredMCPEntry(existing) {
			return false
		}
	}
	servers[serverName] = entry
	root["mcpServers"] = servers
	return true
}

// IsGortexAuthoredMCPEntry returns true for MCP server stanzas that
// look like Gortex wrote them — a command naming the gortex binary
// (bare "gortex" or an absolute path whose basename is gortex) and an
// args list starting with the `mcp` subcommand. Used by global-mode
// installers to migrate their own legacy stanzas — including the older
// absolute-path form — without clobbering user-customized servers.
func IsGortexAuthoredMCPEntry(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := m["command"].(string)
	if !commandIsGortex(cmd) {
		return false
	}
	args, ok := m["args"].([]any)
	if !ok || len(args) == 0 {
		return false
	}
	first, _ := args[0].(string)
	return first == "mcp"
}

// commandIsGortex reports whether an MCP stanza's command string names
// the gortex binary — either the bare "gortex"/"gortex.exe" name or an
// absolute path whose basename is gortex (the legacy os.Executable()
// form a pre-fix `gortex install` baked into ~/.claude.json). Lets the
// global installer recognize and migrate its own older stanza in place
// instead of leaving a stale absolute path that disagrees with the
// bare-`gortex` project .mcp.json template.
func commandIsGortex(cmd string) bool {
	return gortexCommandBase(cmd) == "gortex"
}

// gortexCommandBase extracts the binary base name from a command
// string, tolerating both / and \ separators so a path written on one
// OS is still recognized when parsed on another (matters for
// cross-platform tests; in production the path is always native). The
// trailing extension (.exe on Windows) is stripped.
func gortexCommandBase(cmd string) string {
	if cmd == "" {
		return ""
	}
	base := cmd
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// ResolveGortexCommand returns the command string an installer should
// bake into a gortex MCP server stanza. It prefers the bare "gortex"
// name — portable across machines and byte-identical to the project
// .mcp.json template — but only when "gortex" on PATH resolves to the
// same binary that is currently running. Matching the project template
// matters for Claude Code specifically: it stores OAuth tokens per
// endpoint (command + args), so a user-scope entry that disagrees with
// a project-scope entry trips its "conflicting scopes" diagnostic.
//
// When the running binary is not reachable on PATH (e.g. a Windows
// install whose program directory is not on PATH) it falls back to the
// absolute os.Executable() path so the entry still launches. Under
// `go run` (a transient temp build) or any other ambiguity it falls
// back to the bare name rather than bake a path that won't exist later.
func ResolveGortexCommand() string {
	exe, exeErr := os.Executable()
	lp, lpErr := exec.LookPath("gortex")
	return resolveGortexCommandFrom(exe, exeErr, lp, lpErr, sameFile)
}

// resolveGortexCommandFrom is the pure decision core of
// ResolveGortexCommand, split out so the PATH/executable inputs can be
// injected in tests.
func resolveGortexCommandFrom(exe string, exeErr error, lookPath string, lookErr error, same func(a, b string) bool) string {
	exeUsable := exeErr == nil && exe != "" && gortexCommandBase(exe) == "gortex" && !isUnderTempDir(exe)
	if lookErr == nil && lookPath != "" {
		// On PATH: collapse to the bare name when it points at the
		// binary we are running (or we cannot trust os.Executable),
		// so the entry matches the portable project template.
		if !exeUsable || same(lookPath, exe) {
			return "gortex"
		}
	}
	if exeUsable {
		// Not on PATH, but we know exactly where we live: pin the
		// absolute path so the entry launches.
		return exe
	}
	return "gortex"
}

// ResolveGortexHookBinary returns the gortex binary reference a Claude
// Code hook command should embed. It shares ResolveGortexCommand's
// same-file decision core so the hook process and the MCP server/daemon
// never resolve to different binaries — a side-by-side install (CI
// sandbox, a downloaded release run before linking, a dev build) would
// otherwise bake the running binary into the MCP stanza but a stray
// PATH gortex into the hook, and the hook could then consult a
// different daemon than the one serving the session's graph tools.
//
// It differs from ResolveGortexCommand only in output preference: a
// hook fires in a shell whose PATH may not match install time, so it
// pins an absolute path whenever the running binary is usable rather
// than the bare name. The returned value is a bare binary reference
// with no subcommand — callers append " hook" (and any --mode suffix).
func ResolveGortexHookBinary() string {
	return ResolveGortexLaunchBinary()
}

// ResolveGortexLaunchBinary returns the gortex binary reference to bake
// into an MCP server stanza for a host that launches the server itself
// rather than through the user's login shell. It is the same value (and
// the same decision core) as ResolveGortexHookBinary; the two names
// exist so each call site reads as what it is.
//
// ResolveGortexCommand is the wrong resolver for such a host. It
// collapses to the bare "gortex" whenever PATH resolves to the running
// binary — correct for Claude Code, which keys OAuth tokens on the
// command string and must match the portable project template, but the
// bare name is only findable if the launching process inherits the PATH
// that `gortex init` ran under. A GUI-launched host does not: on macOS
// it inherits launchd's PATH, which carries neither Homebrew's
// /opt/homebrew/bin nor ~/.local/bin nor ~/go/bin, so the spawn fails
// with ENOENT. Pinning the absolute path is what the hook writer
// already does for exactly this reason (gortexhq/gortex#607).
func ResolveGortexLaunchBinary() string {
	exe, exeErr := os.Executable()
	lp, lpErr := exec.LookPath("gortex")
	return resolveGortexHookBinaryFrom(exe, exeErr, lp, lpErr, sameFile)
}

// resolveGortexHookBinaryFrom is the pure decision core of
// ResolveGortexHookBinary, split out so the PATH/executable inputs can
// be injected in tests. It resolves to the same on-disk binary as
// resolveGortexCommandFrom in every case where the running binary is
// usable (both pin the running exe, or the bare name that PATH resolves
// back to it); it only prefers an absolute path over the bare name.
//
// The ephemeral guard is deliberately on the running binary alone
// (exeUsable's !isUnderTempDir(exe)): a `go run` transient build must
// never be baked into a long-lived hook, so we fall back to the PATH
// gortex — the same file the MCP stanza collapses to — instead. A
// PATH lookup that itself points into a temp dir is the user's own
// PATH and is left to resolve, matching resolveGortexCommandFrom.
func resolveGortexHookBinaryFrom(exe string, exeErr error, lookPath string, lookErr error, same func(a, b string) bool) string {
	exeUsable := exeErr == nil && exe != "" && gortexCommandBase(exe) == "gortex" && !isUnderTempDir(exe)
	if exeUsable {
		// The running binary is a stable, non-ephemeral gortex: pin it.
		// This is the same file the MCP stanza launches — whether that
		// stanza used the bare name (PATH gortex IS this binary) or the
		// absolute exe (off-PATH, or a different gortex on PATH).
		return exe
	}
	if lookErr == nil && lookPath != "" {
		// Running binary isn't a bakeable path (go run temp build,
		// unstattable, or not named gortex). Fall back to PATH's gortex
		// — the file the MCP stanza collapses to — as an absolute path.
		return lookPath
	}
	return "gortex"
}

// isUnderTempDir reports whether p lives under the OS temp directory —
// the tell-tale of a `go run` / `go test` transient build that must not
// be baked into a long-lived config.
func isUnderTempDir(p string) bool {
	return strings.HasPrefix(p, filepath.Clean(os.TempDir())+string(os.PathSeparator))
}

// sameFile reports whether two paths reference the same on-disk binary,
// resolving symlinks and falling back to os.SameFile. A pure string
// match short-circuits the stat calls.
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		if rb, err := filepath.EvalSymlinks(b); err == nil && ra == rb {
			return true
		}
	}
	fa, e1 := os.Stat(a)
	fb, e2 := os.Stat(b)
	return e1 == nil && e2 == nil && os.SameFile(fa, fb)
}

// MCPEntriesEqual compares two MCP stanzas by their JSON-marshaled
// form. Round-tripping is the simplest way to handle the []string vs
// []any drift between freshly-built entries and entries decoded from
// an existing mcp.json on disk. Exported so the doctor can compare an
// on-disk stanza against DefaultGortexMCPEntry() to flag a stale entry.
func MCPEntriesEqual(a, b any) bool {
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	var av, bv any
	if err := json.Unmarshal(aj, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(bj, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// DefaultGortexMCPEntry returns the shared {command, args}
// stanza most clients accept for project-local MCP configs (where
// the editor launches the process with cwd set to the project root).
// Adapters that want extra keys wrap this and add them (e.g. Cline's
// alwaysAllow, Kiro's autoApprove).
//
// The command intentionally points at the bare "gortex" binary on
// PATH rather than os.Executable() — users who installed via
// Homebrew or `go install` get a stable path, and installers that
// run `go build -o /tmp/...` don't bake the transient path into
// long-lived configs.
// gortexMCPArgs is the one canonical args list every adapter emits.
// Both project and global configs use it: the daemon resolves the active
// workspace per session from the request handshake, so no cwd-relative
// flag (--index/--watch) and no proxy flag (--proxy) is needed. `gortex
// mcp` proxies to (and may auto-start) the daemon; if none is available,
// it fails closed unless the user-global config allows embedded mode.
func gortexMCPArgs() []string { return []string{"mcp"} }

func DefaultGortexMCPEntry() map[string]any {
	return map[string]any{
		"command": "gortex",
		"args":    gortexMCPArgs(),
	}
}

// GlobalGortexMCPEntry returns the daemon-proxy MCP entry suitable
// for user-level (global) configs, where the editor may launch the
// MCP process with cwd set to the user's home directory rather than
// any open project — Cursor's global-mcp behaviour reported in
// gortexhq/gortex#19.
//
// The proxy shape carries no cwd-relative state: the daemon resolves
// the active workspace per session from the request handshake, so
// the global config never trips the strict entry-point check on the
// home directory. If no daemon is running, the canonical command exits by
// default; embedded mode still requires the machine-global opt-in.
// GlobalGortexMCPEntry is now identical to DefaultGortexMCPEntry — the
// canonical `["mcp"]` shape carries no cwd-relative or proxy flag. Kept
// as a named function so existing call sites compile.
func GlobalGortexMCPEntry() map[string]any {
	return DefaultGortexMCPEntry()
}
