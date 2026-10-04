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
// OpenCode 2 changed the plugin contract (a default export `{ id, setup }`
// that registers hooks on domains) and refuses to run a 1.x plugin, so the
// one file carries both entrypoints: `setup` for 2.x, `server` for 1.18.x.
//
// # Why the user-level plugin dir, never the repo's
//
// This is the only executable artifact Gortex writes anywhere. The
// repo-level `<root>/.opencode/` is committed, so dropping a file that
// shells out on every tool call into the user's git tree would push it
// onto every teammate who clones — an executable they never asked for,
// arriving through a code review that reads like a config change. The
// global dir (`~/.config/opencode/plugin/`) covers every project the user
// opens without that, so it is where the bridge goes and the repo tree
// keeps only inert markdown.
//
// # Why `plugin/`, singular, when everything else is plural
//
// OpenCode accepts `plugin/` and `plugins/` alike, and this file picks
// the singular for the one reason the choice is not arbitrary: unlike
// skills, commands and agents, the plugin glob is single-level
// (`{plugin,plugins}/*.{ts,js}`) and does not recurse. Using the
// odd-one-out spelling keeps that odd-one-out rule attached to a name a
// reader will notice, so a later "tidy this up to match the others" edit
// has to stop and read this comment before nesting the file into a
// subdirectory where it would silently never load again.

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

// PluginFileName is the bridge's file name inside the plugin directory.
// Stable across releases so a re-install overwrites in place rather than
// leaving two plugins racing each other on every tool call.
const PluginFileName = "gortex.js"

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

// PluginPath is where the bridge is installed for a given home. Exported
// so inspect.go and the doctor wiring name the same file the writer does.
func PluginPath(home string) string {
	return filepath.Join(globalConfigDir(home), "plugin", PluginFileName)
}

// hookArgv is the command the plugin shells for every bridged event.
func hookArgv(env agents.Env) []string {
	argv := []string{resolveGortexBin(env), "hook", "--agent=" + Name}
	if mode := normalizeHookMode(env.HookMode); mode != "" && mode != "deny" {
		argv = append(argv, "--mode="+mode)
	}
	return argv
}

// renderPlugin fills the embedded JavaScript template with the resolved
// gortex binary, the hook argv, and the enforcement flag.
func renderPlugin(env agents.Env) string {
	argv := hookArgv(env)
	src := pluginSource
	src = substituteSentinel(src, sentinelBin, jsonValue(argv[0]))
	src = substituteSentinel(src, sentinelArgv, jsonValue(argv))
	src = substituteSentinel(src, sentinelEnforce, jsonValue(env.InstallHooks))
	return src
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
	action, err := agents.WriteOwnedFile(env.Stderr, PluginPath(env.Home), renderPlugin(env), opts)
	if err != nil {
		return nil, err
	}
	return []agents.FileAction{action}, nil
}

// planPlugin mirrors applyPlugin's gating so `--dry-run` and `doctor`
// never promise a file Apply would skip.
func planPlugin(env agents.Env) []agents.FileAction {
	if env.Mode != agents.ModeGlobal || !env.InstallHooks || env.Home == "" {
		return nil
	}
	return []agents.FileAction{{Path: PluginPath(env.Home), Action: agents.ActionWouldCreate}}
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
