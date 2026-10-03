package opencode

// plugin.go renders and installs the OpenCode enforcement bridge.
//
// # Why a plugin at all
//
// OpenCode (1.18.18) has no lifecycle-hook configuration: no settings key
// runs a command on session start, before a tool call, or on a prompt.
// Its JS/TS plugin API is the only place enforcement can live, so Gortex
// ships one — a thin shim that shells `gortex hook --agent=opencode` and
// applies the decision. The policy stays in Go; see plugin/gortex.js.
//
// # Why the user-level plugin dir, never the repo's
//
// This is the only executable artifact Gortex writes anywhere. The
// repo-level `<root>/.opencode/` is committed, so dropping a file that
// shells out on every tool call into the user's git tree would push it
// onto every teammate who clones — an executable they never asked for,
// arriving through a code review that reads like a config change. The
// global dir (`~/.config/opencode/plugins/gortex/`) covers every project
// the user opens without that, so it is where the bridge goes and the repo
// tree keeps only inert markdown.
//
// # V2 plugin layout
//
// OpenCode V2 discovers local plugins from `.opencode/plugins/` (and
// `.config/opencode/plugins/` for global). Each plugin lives in its own
// directory with a package.json. The V1 single-file `plugin/` layout is
// still read for backward compatibility but cannot resolve the
// `@opencode/plugin` import that V2 plugins need. This file installs the
// V2 layout so the bridge works on OpenCode 2.x.

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	_ "embed"

	"github.com/zzet/gortex/internal/agents"
)

//go:embed plugin/gortex.js
var pluginSource string

// Templated sentinels in plugin/gortex.js. Each is replaced with a JSON
// literal so a value containing spaces, quotes or backslashes survives
// into valid JavaScript.
const (
	sentinelBin     = "{{GORTEX_BIN}}"
	sentinelArgv    = "{{GORTEX_HOOK_ARGV}}"
	sentinelEnforce = "{{GORTEX_ENFORCE}}"
)

// PluginDir is the directory name for the bridge inside the plugins directory.
const PluginDir = "gortex"

// PluginFileName is the bridge's file name inside the plugin directory.
// Stable across releases so a re-install overwrites in place rather than
// leaving two plugins racing each other on every tool call.
const PluginFileName = "index.js"

// PluginMarker is a string every rendered bridge contains. inspect.go
// matches on it to tell "a Gortex bridge is installed" from "some other
// plugin happens to be called gortex.js" — it cannot simply re-render and
// compare, because the rendered bytes carry the install-time binary path.
//
// It is deliberately the argv element rather than a phrase from the doc
// comment: the argv is what makes the file a Gortex bridge, so a rewrite
// that drops it has stopped being one, while a comment could be reflowed
// away by a formatter without changing a thing.
const PluginMarker = `"--agent=` + Name + `"`

// packageJSON is the package.json for the V2 plugin layout.
// @opencode/plugin is provided by the OpenCode runtime; "*" lets bun/npm
// accept it without needing an exact version from the registry.
const packageJSON = `{
  "name": "gortex",
  "version": "0.0.0",
  "type": "module",
  "main": "index.js",
  "dependencies": {
    "@opencode/plugin": "*"
  }
}
`

// PluginPath is where the bridge is installed for a given home. Exported
// so inspect.go and the doctor wiring name the same file the writer does.
func PluginPath(home string) string {
	return filepath.Join(globalConfigDir(home), "plugins", PluginDir, PluginFileName)
}

// PluginPkgPath is the package.json location for the V2 plugin layout.
func PluginPkgPath(home string) string {
	return filepath.Join(globalConfigDir(home), "plugins", PluginDir, "package.json")
}

// hookArgv is the command the plugin shells for every bridged event.
func hookArgv(env agents.Env) []string {
	argv := []string{resolveGortexBin(env), "hook", "--agent=" + Name}
	if mode := normalizeHookMode(env.HookMode); mode != "" && mode != "deny" {
		argv = append(argv, "--mode="+mode)
	}
	return argv
}

// renderPlugin fills the embedded V2 JavaScript template with the resolved
// gortex binary, the hook argv, and the enforcement flag.
func renderPlugin(env agents.Env) string {
	argv := hookArgv(env)
	src := pluginSource
	src = substituteSentinel(src, sentinelBin, jsonValue(argv[0]))
	src = substituteSentinel(src, sentinelArgv, jsonValue(argv))
	src = substituteSentinel(src, sentinelEnforce, jsonValue(env.InstallHooks))
	return src
}

// V1PluginPath is the legacy V1 plugin location (~/.config/opencode/plugin/gortex.js).
// Kept for inspect.go and remove.go to detect/clean up old installs.
func V1PluginPath(home string) string {
	return filepath.Join(globalConfigDir(home), "plugin", "gortex.js")
}

// applyPlugin writes the bridge, or reports that it deliberately did not.
//
// Gated on Env.InstallHooks so `--no-hooks` is the one documented off
// switch across every hook-capable adapter — a user who turned
// enforcement off machine-wide should not find an executable that
// enforces it in their OpenCode config.
//
// WriteOwnedFile because Gortex owns this file end-to-end: it must track
// the current binary path and hook posture on every install, and a
// user-edited copy of a shim whose wire contract we control is not
// something to preserve. It reports ActionSkip "unchanged" for
// byte-identical content, which keeps a re-run idempotent.
func applyPlugin(env agents.Env, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if !env.InstallHooks || env.Home == "" {
		return nil, nil
	}
	actions := make([]agents.FileAction, 0, 2)

	// Write the V2 plugin file (plugins/gortex/index.js)
	// This single file serves both V2 (default export) and V1 (named `server` export).
	action, err := agents.WriteOwnedFile(env.Stderr, PluginPath(env.Home), renderPlugin(env), opts)
	if err != nil {
		return nil, err
	}
	actions = append(actions, action)

	// Write the package.json for V2 plugin layout
	action, err = agents.WriteOwnedFile(env.Stderr, PluginPkgPath(env.Home), packageJSON, opts)
	if err != nil {
		return nil, err
	}
	actions = append(actions, action)

	// NOTE: We no longer write the legacy V1 plugin file (plugin/gortex.js)
	// because OpenCode V2 loads plugins from both plugin/ and plugins/
	// directories, and the V1 file lacks the V2 default export (id + setup).
	// The V2 plugin at plugins/gortex/index.js already exports both
	// `export default v2Plugin` (for V2) and `export { server }` (for V1),
	// making the separate V1 file redundant and a source of load errors.

	return actions, nil
}

// planPlugin mirrors applyPlugin's gating so `--dry-run` and `doctor`
// never promise a file Apply would skip.
func planPlugin(env agents.Env) []agents.FileAction {
	if env.Mode != agents.ModeGlobal || !env.InstallHooks || env.Home == "" {
		return nil
	}
	return []agents.FileAction{
		{Path: PluginPath(env.Home), Action: agents.ActionWouldCreate},
		{Path: PluginPkgPath(env.Home), Action: agents.ActionWouldCreate},
		// NOTE: V1 plugin file (plugin/gortex.js) is no longer written.
		// The V2 plugin at plugins/gortex/index.js serves both V2 and V1.
	}
}

// resolveGortexBin prefers the binary the caller already resolved into
// Env.HookCommand, so every adapter in one `gortex install` run bakes the
// same path, and falls back to agents.ResolveGortexHookBinary — which
// pins the running binary unless it is an ephemeral `go run` build under
// the OS temp dir, where it prefers PATH instead.
func resolveGortexBin(env agents.Env) string {
	if fields := strings.Fields(env.HookCommand); len(fields) > 0 {
		return fields[0]
	}
	return agents.ResolveGortexHookBinary()
}

// normalizeHookMode mirrors the hook postures the daemon accepts; any
// unknown value (including empty) collapses to the deny default.
func normalizeHookMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "enrich":
		return "enrich"
	case "consult-unlock":
		return "consult-unlock"
	case "nudge", "adaptive-nudge":
		return "nudge"
	default:
		return "deny"
	}
}

// substituteSentinel replaces a {{NAME}} placeholder with value, tolerant
// of inner whitespace. The embedded plugin is plain JavaScript and may be
// run through a formatter (Prettier reflows `{{NAME}}` to `{{ NAME }}`),
// so an exact-string match would silently miss a formatted sentinel and
// ship an un-substituted template. The replacement is literal so a value
// containing `$` is not treated as a regexp expansion.
func substituteSentinel(src, sentinel, value string) string {
	name := strings.TrimSuffix(strings.TrimPrefix(sentinel, "{{"), "}}")
	re := regexp.MustCompile(`\{\{\s*` + regexp.QuoteMeta(name) + `\s*\}\}`)
	return re.ReplaceAllLiteralString(src, value)
}

// jsonValue emits a JSON literal for templating into the JavaScript
// source. A marshal failure renders `null`, which the plugin's own
// fail-open paths tolerate.
func jsonValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}
