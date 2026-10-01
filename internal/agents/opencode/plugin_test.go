package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// globalEnv returns an Env in ModeGlobal — the only mode that installs
// the bridge.
func globalEnv(t *testing.T) (agents.Env, string) {
	t.Helper()
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	return env, PluginPath(env.Home)
}

// TestPluginLandsInTheGlobalPluginDir pins the install location. The
// repo-level `.opencode/` is committed, so an executable dropped there
// would reach every teammate who clones; plugin.go documents the call.
func TestPluginLandsInTheGlobalPluginDir(t *testing.T) {
	env, want := globalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if got := filepath.Join(env.Home, ".config", "opencode", "plugins", "gortex", "index.js"); got != want {
		t.Fatalf("PluginPath drifted: %s != %s", want, got)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("plugin not installed at %s: %v", want, err)
	}
	// Also verify package.json exists for V2 plugin layout
	pkgPath := filepath.Join(env.Home, ".config", "opencode", "plugins", "gortex", "package.json")
	if _, err := os.Stat(pkgPath); err != nil {
		t.Fatalf("package.json not installed at %s: %v", pkgPath, err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".opencode")); err == nil {
		t.Fatalf("the bridge must never be written into the repo tree")
	}
}

// TestPluginIsInPluginsDir: V2 plugin layout uses a dedicated directory
// under plugins/ with a package.json, so the plugin can resolve
// @opencode/plugin. The directory structure is:
//   ~/.config/opencode/plugins/gortex/
//     ├── index.js
//     └── package.json
func TestPluginIsInPluginsDir(t *testing.T) {
	env, path := globalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	foundIndex := false
	foundPkg := false
	for _, entry := range entries {
		if entry.Name() == "index.js" {
			foundIndex = true
		}
		if entry.Name() == "package.json" {
			foundPkg = true
		}
	}
	if !foundIndex {
		t.Fatalf("%s missing index.js", dir)
	}
	if !foundPkg {
		t.Fatalf("%s missing package.json", dir)
	}
}

// TestNoHooksWritesNoPlugin: --no-hooks is the documented off switch on
// every hook-capable adapter, and this is the only one whose artifact is
// executable.
func TestNoHooksWritesNoPlugin(t *testing.T) {
	env, path := globalEnv(t)
	env.InstallHooks = false

	plan, err := New().Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		if f.Path == path {
			t.Fatalf("Plan promised the bridge under --no-hooks")
		}
	}
	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("--no-hooks still installed %s", path)
	}
}

// TestEverySentinelIsSubstituted: an un-substituted `{{…}}` is a syntax
// error at load time, which OpenCode reports by silently not loading the
// plugin.
func TestEverySentinelIsSubstituted(t *testing.T) {
	env, _ := globalEnv(t)
	rendered := renderPlugin(env)
	if strings.Contains(rendered, "{{") || strings.Contains(rendered, "}}") {
		for _, line := range strings.Split(rendered, "\n") {
			if strings.Contains(line, "{{") || strings.Contains(line, "}}") {
				t.Errorf("un-substituted sentinel: %s", line)
			}
		}
		t.FailNow()
	}
	// The template must still carry every sentinel, or a rename would
	// pass the check above by leaving the value hard-coded.
	for _, sentinel := range []string{sentinelBin, sentinelArgv, sentinelEnforce} {
		if !strings.Contains(pluginSource, sentinel) {
			t.Fatalf("plugin/gortex.js no longer carries %s", sentinel)
		}
	}
}

// TestRenderedArgvAndEnforce checks the two values the bridge cannot work
// without: the argv it shells, and the enforcement flag.
func TestRenderedArgvAndEnforce(t *testing.T) {
	env, _ := globalEnv(t)
	env.HookCommand = "/opt/gortex/bin/gortex hook"
	env.HookMode = "enrich"
	rendered := renderPlugin(env)

	const wantArgv = `["/opt/gortex/bin/gortex","hook","--agent=opencode","--mode=enrich"]`
	if !strings.Contains(rendered, wantArgv) {
		t.Fatalf("rendered argv missing %s", wantArgv)
	}
	if !strings.Contains(rendered, `const GORTEX_BIN = "/opt/gortex/bin/gortex";`) {
		t.Fatalf("rendered binary path is not the one from HookCommand")
	}
	if !strings.Contains(rendered, "const ENFORCE = true;") {
		t.Fatalf("ENFORCE should be true when InstallHooks is set")
	}

	env.InstallHooks = false
	if !strings.Contains(renderPlugin(env), "const ENFORCE = false;") {
		t.Fatalf("ENFORCE should render false under --no-hooks")
	}
}

// TestDefaultHookModeOmitsTheFlag: "deny" is the daemon's default, so
// spelling it out would only make the argv (and every install's rendered
// bytes) differ for no behavioural reason.
func TestDefaultHookModeOmitsTheFlag(t *testing.T) {
	env, _ := globalEnv(t)
	for _, mode := range []string{"", "deny", "nonsense"} {
		env.HookMode = mode
		if strings.Contains(renderPlugin(env), "--mode=") {
			t.Fatalf("hook mode %q rendered a --mode flag", mode)
		}
	}
}

// TestPluginSubscribesToTheStableHooksOnly. `experimental.*` hooks are
// explicitly out of bounds — they are renamed without notice, and a
// renamed hook fails by never firing.
func TestPluginSubscribesToTheStableHooksOnly(t *testing.T) {
	for _, hook := range []string{
		`"tool.execute.before"`, `"tool.execute.after"`, `"permission.ask"`, `"chat.message"`,
	} {
		if !strings.Contains(pluginSource, hook) {
			t.Fatalf("plugin no longer subscribes to %s", hook)
		}
	}
	if strings.Contains(pluginSource, "experimental") {
		t.Fatalf("plugin references an experimental hook")
	}
}

// TestPluginSpeaksTheBridgeWireContract pins the envelope and decision
// field names against internal/hooks/pi.go. They are two halves of one
// protocol in two languages, so nothing but a test holds them together.
func TestPluginSpeaksTheBridgeWireContract(t *testing.T) {
	// BridgeEvent fields the plugin sends.
	for _, field := range []string{
		"event:", "tool_name:", "tool_input:", "cwd,", "session_id:", "is_gortex_tool:", "prompt:",
	} {
		if !strings.Contains(pluginSource, field) {
			t.Fatalf("plugin no longer sends the BridgeEvent field %q", field)
		}
	}
	// BridgeDecision fields the plugin reads.
	for _, field := range []string{
		"decision.block", "decision.reason", "decision.additional_context", "briefing.orientation",
	} {
		if !strings.Contains(pluginSource, field) {
			t.Fatalf("plugin no longer reads %s", field)
		}
	}
	// Event phase strings the Go router recognises (bridgeEventSlot).
	for _, phase := range []string{
		`event: "tool.execute.before"`, `event: "permission.ask"`,
		`event: "chat.message"`, `event: "session"`,
	} {
		if !strings.Contains(pluginSource, phase) {
			t.Fatalf("plugin no longer emits %s", phase)
		}
	}
}

// TestPluginFailsOpen: every bridge call has to be inside a try/catch
// that returns an empty decision, because a spawn error, a non-zero exit,
// a timeout or garbled stdout must all let the tool call through.
func TestPluginFailsOpen(t *testing.T) {
	if !strings.Contains(pluginSource, "timeout: HOOK_TIMEOUT_MS") {
		t.Fatalf("the bridge call has no timeout — a wedged hook would hang the session")
	}
	_, after, ok := strings.Cut(pluginSource, "function callHook(envelope) {")
	if !ok {
		t.Fatalf("callHook is gone")
	}
	body, _, ok := strings.Cut(after, "\n}\n")
	if !ok {
		t.Fatalf("callHook has no closing brace this test can find")
	}
	if !strings.Contains(body, "try {") {
		t.Fatalf("callHook does not guard the spawn:\n%s", body)
	}
	if !strings.Contains(body, "catch") || !strings.Contains(body, "return {};") {
		t.Fatalf("callHook no longer swallows every failure into an empty decision:\n%s", body)
	}
	// One spawn call site, so "fails open" is a property of a single
	// guarded function rather than something each caller has to repeat.
	if n := strings.Count(pluginSource, "execFileSync("); n != 1 {
		t.Fatalf("expected exactly one execFileSync call site, found %d", n)
	}
}

// TestRenderedPluginParses runs `node --check` on the rendered bridge,
// which is the only cheap way to catch a template edit that produces
// invalid JavaScript — OpenCode's own failure mode is to skip the plugin
// without a word.
//
// The copy to `.mjs` is required, not cosmetic: `node --check` parses a
// `.js` file as CommonJS unless a package.json says otherwise, and this
// plugin is an ES module (OpenCode's documented plugin shape). We
// deliberately do not ship a package.json, so the extension is the only
// signal available.
func TestRenderedPluginParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	env, _ := globalEnv(t)
	path := filepath.Join(t.TempDir(), "gortex.mjs")
	if err := os.WriteFile(path, []byte(renderPlugin(env)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("rendered plugin is not valid JavaScript: %v\n%s", err, out)
	}
}

// TestPluginHasNoPackageDependencies: OpenCode installs plugin deps from
// `.opencode/package.json` with a bun install at startup. We write no
// package.json, so any import beyond a node: built-in would fail at load
// and take the bridge down with it.
//
// Exception: @opencode/plugin is provided by the OpenCode runtime itself
// (both V1 1.18.29+ and V2), not installed via package.json.
func TestPluginHasNoPackageDependencies(t *testing.T) {
	for _, line := range strings.Split(pluginSource, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "import ") && !strings.Contains(trimmed, "require(") {
			continue
		}
		if strings.Contains(trimmed, `"node:`) {
			continue
		}
		if strings.Contains(trimmed, `"@opencode/plugin"`) {
			continue
		}
		t.Fatalf("plugin has a non-builtin dependency: %s", trimmed)
	}
}
