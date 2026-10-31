package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/agents"
)

// CurrentPreToolUseMatcher is the canonical matcher pattern we bake
// into Claude Code's PreToolUse hook. Older versions used
// "Read|Grep", "Read|Grep|Glob", "Read|Grep|Glob|Task",
// "Read|Grep|Glob|Task|Bash", or "Read|Grep|Glob|Task|Bash|Edit|Write";
// upgradeGortexMatcher rewrites those in place. Edit and Write are
// included so the hook can redirect whole-file rewrites of indexed
// source to the Gortex MCP edit tools, and Bash carries the same
// redirect for the shell shapes that rewrite a file (`sed -i`, a `>`
// redirect, `tee`). All three say so in every posture; whether they also
// block depends on GORTEX_HOOK_BLOCK_EDIT, which no installed
// configuration sets, and on the posture itself (`enrich` never denies,
// `nudge` batches a deny per burst). So today the write redirect is
// advice everywhere. The native "*" match-all sentinel lets a local,
// constant-time terminal marker stop any tool after answer_ready without the
// host evaluating a regular expression for every call.
const CurrentPreToolUseMatcher = "*"

// v060PreToolUseMatcher fingerprints the exact matcher shipped by gortex
// v0.60.0. The concrete retirement gate is documented in docs/versioning.md.
const v060PreToolUseMatcher = "Read|Grep|Glob|Task|Bash|Edit|Write|mcp__gortex__read_file|mcp__gortex__get_editing_context"

// CurrentPostToolUseMatcher names the tools whose response the
// PostToolUse hook augments. Only the read-shaped tools have an obvious
// "enrich this output with graph context" payload — Bash / Edit / Write
// don't benefit from a post-call graph snapshot, so they're omitted.
const CurrentPostToolUseMatcher = "Read|Grep|Glob"

// Hook timeouts in seconds, the unit Claude Code reads.
const (
	hookTimeoutSeconds           = 3
	preToolUseHookTimeoutSeconds = 15
	stopHookTimeoutSeconds       = 45
)

func gortexHookTimeout(event string) int {
	switch event {
	case "PreToolUse":
		return preToolUseHookTimeoutSeconds
	case "Stop":
		return stopHookTimeoutSeconds
	}
	return hookTimeoutSeconds
}

// HookModeDeny / HookModeEnrich / HookModeConsultUnlock /
// HookModeAdaptiveNudge are the posture strings the installer accepts.
// They mirror hooks.Mode without importing it (the claudecode package
// is a leaf of the agents adapter tree and must stay import-free of
// hooks).
const (
	HookModeDeny          = "deny"
	HookModeEnrich        = "enrich"
	HookModeConsultUnlock = "consult-unlock"
	HookModeAdaptiveNudge = "nudge"
)

const (
	preToolUseStatusDeny          = "Enforcing Gortex graph access policy..."
	preToolUseStatusEnrich        = "Enriching with Gortex graph context..."
	preToolUseStatusConsultUnlock = "Consulting Gortex graph before tool use..."
	preToolUseStatusNudge         = "Checking Gortex graph guidance..."
)

// normalizeHookMode maps user input to a canonical mode. Empty or
// unknown values fall through to deny so existing installs and shell
// typos preserve the original behavior. "adaptive-nudge" is accepted
// as an alias for the canonical "nudge".
func normalizeHookMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case HookModeEnrich:
		return HookModeEnrich
	case HookModeConsultUnlock:
		return HookModeConsultUnlock
	case HookModeAdaptiveNudge, "adaptive-nudge":
		return HookModeAdaptiveNudge
	default:
		return HookModeDeny
	}
}

// hookCommandWithMode appends `--mode=<mode>` to the base hook command
// when mode is non-default. The deny mode is the historical default —
// emitting it bare keeps existing settings.json diffs minimal during an
// upgrade. Every other posture is emitted explicitly so the installed
// command unambiguously declares itself.
func hookCommandWithMode(base, mode string) string {
	switch normalizeHookMode(mode) {
	case HookModeEnrich:
		return base + " --mode=enrich"
	case HookModeConsultUnlock:
		return base + " --mode=consult-unlock"
	case HookModeAdaptiveNudge:
		return base + " --mode=nudge"
	default:
		return base
	}
}

func preToolUseStatusMessage(mode string) string {
	switch normalizeHookMode(mode) {
	case HookModeEnrich:
		return preToolUseStatusEnrich
	case HookModeConsultUnlock:
		return preToolUseStatusConsultUnlock
	case HookModeAdaptiveNudge:
		return preToolUseStatusNudge
	default:
		return preToolUseStatusDeny
	}
}

// ResolveHookCommand returns the shell command to bake into Claude
// Code's hook config. It routes through agents.ResolveGortexHookBinary,
// which shares the same same-file decision core as the MCP server
// stanza (agents.ResolveGortexCommand): the hook and the MCP stanza
// must resolve to the same binary file whenever the running binary is
// usable, so a side-by-side install can never point the hook at a
// different daemon than the one serving the session's graph tools. The
// hook keeps an absolute-path preference (it avoids PATH-at-fire-time
// fragility); it falls back to bare "gortex hook" only when no stable
// binary is resolvable at all.
//
// A warning is written to w on the bare fallback because it relies on
// PATH resolution at hook-fire time — fragile when the user's shell
// environment differs between Claude Code and a terminal.
func ResolveHookCommand(w io.Writer) string {
	bin := agents.ResolveGortexHookBinary()
	if bin == "gortex" && w != nil {
		fmt.Fprintln(w,
			"[gortex init] warning: `gortex` not found on PATH; "+
				"writing bare \"gortex hook\" into settings — install gortex to PATH for a stable hook command")
	}
	return shellSafeHookBinary(bin) + " hook"
}

// shellSafeHookBinary normalizes a resolved binary path into a form
// safe to embed in a shell-executed hook command. Claude Code runs
// hooks through a shell; on Windows that shell is Git Bash, which
// treats backslashes as escape characters — a native path like
// C:\Users\me\gortex.exe is mangled to C:Usersmegortex.exe (\U, \m, …
// swallowed) and the hook fails with "command not found". Forward
// slashes survive: Git Bash maps C:/Users/... back to a native path
// when it spawns the .exe. The replacement is unconditional rather
// than Windows-guarded — any backslash in an unquoted shell command is
// a bug regardless of OS, and a real Unix binary path never contains
// one as a separator.
//
// The normalized path is then single-quoted when it contains a character
// a shell would interpret (space, tab, or any of `$"'&|;()` etc.). A
// normal install path — /opt/homebrew/bin/gortex — is emitted unchanged;
// a space would otherwise word-split the command and a `$var`/backtick
// would expand at hook-fire time. A bare word from the PATH fallback is
// already a simple token and needs no quoting.
var shellSafePathToken = regexp.MustCompile(`^[A-Za-z0-9_./:+-]+$`)

func shellSafeHookBinary(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	if !shellSafePathToken.MatchString(path) {
		return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
	}
	return path
}

// HookCommandPathIsEphemeral reports whether cmd's binary path lives
// in a location that is wiped between sessions (system tmpdirs, the
// macOS go-build cache) or no longer exists on disk. Used by
// healStaleHookCommands to detect settings.json entries that
// outlived their backing binary.
func HookCommandPathIsEphemeral(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	bin := fields[0]
	ephemeralPrefixes := []string{"/tmp/", "/var/folders/", "/private/tmp/", "/private/var/folders/"}
	for _, p := range ephemeralPrefixes {
		if strings.HasPrefix(bin, p) {
			return true
		}
	}
	// An absolute path that no longer resolves to a file is also stale.
	if filepath.IsAbs(bin) {
		if _, err := os.Stat(bin); err != nil {
			return true
		}
	}
	return false
}

// healStaleHookCommands rewrites Gortex hook entries whose command
// points at an ephemeral or missing binary path. Returns the number
// of entries rewritten. Non-Gortex entries are left alone; Gortex
// entries whose path is healthy are also left alone.
func healStaleHookCommands(hooks map[string]any, newCommand string) int {
	healed := 0
	for _, event := range []string{"PreToolUse", "Stop", "SessionStart", "UserPromptSubmit", "SubagentStart", "SubagentStop"} {
		list, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		for _, h := range list {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if (event == "PreToolUse" || event == "PostToolUse") && !managedGortexHookGroup(hm, event) {
				continue
			}
			inner, ok := hm["hooks"].([]any)
			if !ok {
				continue
			}
			for _, e := range inner {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				cmd, _ := em["command"].(string)
				if !commandInvokesGortexHook(cmd) {
					continue
				}
				if !HookCommandPathIsEphemeral(cmd) {
					continue
				}
				em["command"] = newCommand
				healed++
			}
		}
	}
	return healed
}

func appendHookEntry(hooks map[string]any, event string, entry map[string]any) {
	if _, ok := hooks[event]; !ok {
		hooks[event] = []any{}
	}
	list := hooks[event].([]any)
	hooks[event] = append(list, entry)
}

// upgradeGortexMatcher rewrites older PreToolUse matchers to the
// current CurrentPreToolUseMatcher. Returns true when a change was
// made. Handles every historical matcher we've shipped; anything
// not in that set is left alone.
func upgradeGortexMatcher(hooks map[string]any) bool {
	pre, ok := hooks["PreToolUse"].([]any)
	if !ok {
		return false
	}
	supersededMatchers := map[string]bool{
		".*":                                  true,
		"Read|Grep":                           true,
		"Read|Grep|Glob":                      true,
		"Read|Grep|Glob|Task":                 true,
		"Read|Grep|Glob|Task|Bash":            true,
		"Read|Grep|Glob|Task|Bash|Edit|Write": true,
		v060PreToolUseMatcher:                 true,
	}
	upgraded := false
	for _, h := range pre {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if !managedGortexHookGroup(hm, "PreToolUse") {
			continue
		}
		matcher, _ := hm["matcher"].(string)
		if !supersededMatchers[matcher] {
			continue
		}
		if !entryInvokesGortexHook(hm) {
			continue
		}
		hm["matcher"] = CurrentPreToolUseMatcher
		upgraded = true
	}
	return upgraded
}

// entryInvokesGortexHook returns true when any hooks[*].command
// looks like a Gortex hook invocation.
func entryInvokesGortexHook(entry map[string]any) bool {
	inner, ok := entry["hooks"].([]any)
	if !ok {
		return false
	}
	for _, e := range inner {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := em["command"].(string)
		if commandInvokesGortexHook(cmd) {
			return true
		}
	}
	return false
}

// dedupGortexEntries collapses duplicate Gortex hook entries inside
// hooks[event] down to the first one. Non-Gortex entries are
// preserved in order. Returns the number of duplicates removed.
func dedupGortexEntries(hooks map[string]any, event string) int {
	list, ok := hooks[event].([]any)
	if !ok {
		return 0
	}
	seenGortex := false
	kept := make([]any, 0, len(list))
	removed := 0
	for _, h := range list {
		hm, ok := h.(map[string]any)
		if !ok {
			kept = append(kept, h)
			continue
		}
		if !entryInvokesGortexHook(hm) {
			kept = append(kept, h)
			continue
		}
		if seenGortex {
			removed++
			continue
		}
		seenGortex = true
		kept = append(kept, h)
	}
	if removed > 0 {
		hooks[event] = kept
	}
	return removed
}

// commandInvokesGortexHook returns true when cmd is a Gortex hook
// invocation. Splits on whitespace and checks that "hook" is a
// standalone token and that "gortex" appears in the binary path
// component.
func commandInvokesGortexHook(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return false
	}
	if !strings.Contains(strings.ToLower(fields[0]), "gortex") {
		return false
	}
	return slices.Contains(fields[1:], "hook")
}

// rewriteGortexHookMode rewrites every Gortex hook entry's command
// across all events so it matches newCommand. Used when the install
// posture changes (deny ↔ enrich) — the existing entries already
// invoke `gortex hook` but with the wrong `--mode=...` suffix; we
// re-stamp them in place instead of removing + re-adding so user-added
// fields (timeout, statusMessage) are preserved. Returns the count of
// rewritten entries.
func rewriteGortexHookMode(hooks map[string]any, newCommand string) int {
	rewritten := 0
	for _, event := range []string{"PreToolUse", "PostToolUse", "Stop", "SessionStart", "UserPromptSubmit", "SubagentStart", "SubagentStop"} {
		list, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		for _, h := range list {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if (event == "PreToolUse" || event == "PostToolUse") && !managedGortexHookGroup(hm, event) {
				continue
			}
			inner, ok := hm["hooks"].([]any)
			if !ok {
				continue
			}
			for _, e := range inner {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				cmd, _ := em["command"].(string)
				if !commandInvokesGortexHook(cmd) {
					continue
				}
				if cmd == newCommand {
					continue
				}
				em["command"] = newCommand
				rewritten++
			}
		}
	}
	return rewritten
}

// rewriteGortexHookTimeouts converts Gortex hook timeouts that earlier
// installs wrote in milliseconds (3000, 5000) to seconds. Any other value
// is user-owned and kept.
func rewriteGortexHookTimeouts(hooks map[string]any) int {
	rewritten := 0
	for _, event := range []string{"PreToolUse", "PostToolUse", "Stop", "SessionStart", "UserPromptSubmit", "SubagentStart", "SubagentStop"} {
		list, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		desired := gortexHookTimeout(event)
		for _, h := range list {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if (event == "PreToolUse" || event == "PostToolUse") && !managedGortexHookGroup(hm, event) {
				continue
			}
			inner, ok := hm["hooks"].([]any)
			if !ok {
				continue
			}
			for _, e := range inner {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				cmd, _ := em["command"].(string)
				if !commandInvokesGortexHook(cmd) {
					continue
				}
				if !isLegacyMillisecondTimeout(em["timeout"]) {
					continue
				}
				em["timeout"] = desired
				rewritten++
			}
		}
	}
	return rewritten
}

func isLegacyMillisecondTimeout(v any) bool {
	var n float64
	switch t := v.(type) {
	case float64:
		n = t
	case int:
		n = float64(t)
	default:
		return false
	}
	return n == 3000 || n == 5000
}

// rewriteGortexPreToolUseStatus keeps the installer-authored progress text in
// sync with the configured posture. A message outside the known generated set
// belongs to the user and is deliberately left untouched.
func rewriteGortexPreToolUseStatus(hooks map[string]any, mode string) int {
	list, ok := hooks["PreToolUse"].([]any)
	if !ok {
		return 0
	}
	desired := preToolUseStatusMessage(mode)
	generated := map[string]struct{}{
		preToolUseStatusDeny:          {},
		preToolUseStatusEnrich:        {},
		preToolUseStatusConsultUnlock: {},
		preToolUseStatusNudge:         {},
	}
	rewritten := 0
	for _, h := range list {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if !managedGortexHookGroup(hm, "PreToolUse") {
			continue
		}
		inner, ok := hm["hooks"].([]any)
		if !ok {
			continue
		}
		for _, raw := range inner {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			command, _ := entry["command"].(string)
			if !commandInvokesGortexHook(command) {
				continue
			}
			current, exists := entry["statusMessage"]
			if exists {
				currentText, isString := current.(string)
				if !isString {
					continue
				}
				if _, managed := generated[currentText]; !managed {
					continue
				}
				if currentText == desired {
					continue
				}
			}
			entry["statusMessage"] = desired
			rewritten++
		}
	}
	return rewritten
}

// removeGortexHookEntries drops every entry under hooks[event] that
// invokes `gortex hook`, preserving entries owned by other tools.
// Returns the number of entries removed. Used to clean up PostToolUse
// when the installer switches back from enrich to deny mode.
func removeGortexHookEntries(hooks map[string]any, event string) int {
	list, ok := hooks[event].([]any)
	if !ok {
		return 0
	}
	removed := 0
	kept := make([]any, 0, len(list))
	for _, h := range list {
		hm, ok := h.(map[string]any)
		if !ok {
			kept = append(kept, h)
			continue
		}
		if entryInvokesGortexHook(hm) {
			removed++
			continue
		}
		kept = append(kept, h)
	}
	if removed > 0 {
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	return removed
}

// retiredGortexHookEvents are events the installer used to wire to
// `gortex hook` but no longer does. PreCompact was retired after #522:
// Claude Code discards PreCompact output, so the post-compaction briefing
// moved to SessionStart(source="compact") and the PreCompact entry became
// dead wiring (#814). The `gortex hook` dispatcher still accepts the event,
// so an install that has not been refreshed keeps working.
var retiredGortexHookEvents = []string{"PreCompact"}

// removeGortexHookCommands strips every `gortex hook` command from the
// groups under hooks[event]. A group left with no commands is dropped and
// the event key is deleted once no group remains. Commands owned by other
// tools survive, including ones sharing a group with a Gortex command.
// Returns the number of commands removed.
func removeGortexHookCommands(hooks map[string]any, event string) int {
	list, ok := hooks[event].([]any)
	if !ok {
		return 0
	}
	removed := 0
	kept := make([]any, 0, len(list))
	for _, h := range list {
		hm, ok := h.(map[string]any)
		if !ok {
			kept = append(kept, h)
			continue
		}
		inner, ok := hm["hooks"].([]any)
		if !ok {
			kept = append(kept, h)
			continue
		}
		keptInner := make([]any, 0, len(inner))
		for _, e := range inner {
			if em, ok := e.(map[string]any); ok {
				if cmd, _ := em["command"].(string); commandInvokesGortexHook(cmd) {
					removed++
					continue
				}
			}
			keptInner = append(keptInner, e)
		}
		if len(keptInner) == len(inner) {
			kept = append(kept, h)
			continue
		}
		if len(keptInner) > 0 {
			hm["hooks"] = keptInner
			kept = append(kept, hm)
		}
	}
	if removed > 0 {
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	return removed
}

// hasGortexHookEntry returns true when the given event already has a
// hook entry that invokes `gortex hook`.
func hasGortexHookEntry(hooks map[string]any, event string) bool {
	existing, ok := hooks[event].([]any)
	if !ok {
		return false
	}
	for _, h := range existing {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if entryInvokesGortexHook(hm) {
			return true
		}
	}
	return false
}

// InstallHook is the top-level "make settings.local.json hooks
// match the current Gortex config" operation. It reads the file,
// heals stale commands, upgrades old matchers, dedupes repeat
// entries, removes Gortex hooks on retired events (PreCompact), then
// installs any missing Gortex hooks (PreToolUse, Stop, SessionStart,
// UserPromptSubmit, SubagentStart, SubagentStop, and PostToolUse).
// Writes back atomically via the shared helper.
//
// This function intentionally accepts a plain filesystem path
// rather than an Env — the same helper is used for project-level
// (.claude/settings.local.json) and user-level (~/.claude/…) files.
//
// Delegates to InstallHookWithMode with the deny posture for callers
// that don't care about hook mode (mostly tests and back-compat paths).
func InstallHook(w io.Writer, settingsPath string, opts agents.ApplyOpts) (agents.FileAction, error) {
	return InstallHookWithMode(w, settingsPath, HookModeDeny, opts)
}

// InstallHookWithMode is the mode-aware variant. HookModeDeny installs the
// blocking access posture; HookModeEnrich installs soft context. Both retain
// the PostToolUse terminal observer, while enrich additionally observes native
// read-shaped tools for graph augmentation. Switching modes rewrites only
// installer-owned groups in place.
func InstallHookWithMode(w io.Writer, settingsPath string, mode string, opts agents.ApplyOpts) (agents.FileAction, error) {
	mode = normalizeHookMode(mode)
	var settings map[string]any
	existed := false
	if data, err := os.ReadFile(settingsPath); err == nil {
		existed = true
		if err := json.Unmarshal(data, &settings); err != nil {
			settings = make(map[string]any)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return agents.FileAction{}, fmt.Errorf("read %s: %w", settingsPath, err)
	} else {
		settings = make(map[string]any)
	}

	baseCommand := ResolveHookCommand(w)
	hookCommand := hookCommandWithMode(baseCommand, mode)

	if _, ok := settings["hooks"]; !ok {
		settings["hooks"] = make(map[string]any)
	}
	hooks := settings["hooks"].(map[string]any)

	retiredCount := 0
	for _, event := range retiredGortexHookEvents {
		retiredCount += removeGortexHookCommands(hooks, event)
	}
	healedCount := healStaleHookCommands(hooks, hookCommand)
	matcherUpgraded := upgradeGortexMatcher(hooks)
	preToolMatcherRewriteCount := rewriteGortexEventMatcher(hooks, "PreToolUse", localizationPreToolUseMatcher)
	modeRewriteCount := rewriteGortexHookMode(hooks, hookCommand)
	statusRewriteCount := rewriteGortexPreToolUseStatus(hooks, mode)
	timeoutRewriteCount := rewriteGortexHookTimeouts(hooks)
	dedupedCount := dedupManagedGortexEntries(hooks, "PreToolUse") +
		dedupManagedGortexEntries(hooks, "PostToolUse") +
		dedupGortexEntries(hooks, "Stop") +
		dedupGortexEntries(hooks, "SessionStart") +
		dedupGortexEntries(hooks, "UserPromptSubmit") +
		dedupGortexEntries(hooks, "SubagentStart") +
		dedupGortexEntries(hooks, "SubagentStop")

	// PostToolUse is always present for the terminal-contract observer. In
	// enrich mode it additionally retains the native Read/Grep/Glob matchers.
	postToolMatcherRewriteCount := rewriteGortexEventMatcher(hooks, "PostToolUse", desiredPostToolUseMatcher(mode))
	postToolStatusRewriteCount := rewriteGortexPostToolUseStatus(hooks, mode)

	preToolUseInstalled := hasManagedGortexHookEntry(hooks, "PreToolUse")
	stopInstalled := hasGortexHookEntry(hooks, "Stop")
	sessionStartInstalled := hasGortexHookEntry(hooks, "SessionStart")
	userPromptSubmitInstalled := hasGortexHookEntry(hooks, "UserPromptSubmit")
	subagentStartInstalled := hasGortexHookEntry(hooks, "SubagentStart")
	subagentStopInstalled := hasGortexHookEntry(hooks, "SubagentStop")
	postToolUseInstalled := hasManagedGortexHookEntry(hooks, "PostToolUse")

	if !preToolUseInstalled {
		appendHookEntry(hooks, "PreToolUse", map[string]any{
			"matcher": localizationPreToolUseMatcher,
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       preToolUseHookTimeoutSeconds,
					"statusMessage": preToolUseStatusMessage(mode),
				},
			},
		})
	}
	if !stopInstalled {
		appendHookEntry(hooks, "Stop", map[string]any{
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       stopHookTimeoutSeconds,
					"statusMessage": "Running Gortex post-task diagnostics...",
				},
			},
		})
	}
	if !sessionStartInstalled {
		// SessionStart fires at the start of a new or resumed session
		// — a perfect moment to inject the Gortex orientation snapshot
		// so the first turn doesn't have to call graph_stats. With
		// source="compact" it also carries the post-compaction briefing,
		// which PreCompact cannot inject.
		appendHookEntry(hooks, "SessionStart", map[string]any{
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       hookTimeoutSeconds,
					"statusMessage": "Loading Gortex graph orientation...",
				},
			},
		})
	}
	if !userPromptSubmitInstalled {
		// UserPromptSubmit fires before every user turn — the moment to
		// proactively inject graph symbols relevant to the prompt so the
		// model reaches for Gortex instead of grepping. It is best-effort
		// and time-bounded; a miss is a silent no-op.
		appendHookEntry(hooks, "UserPromptSubmit", map[string]any{
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       hookTimeoutSeconds,
					"statusMessage": "Surfacing relevant Gortex symbols...",
				},
			},
		})
	}
	if !subagentStartInstalled {
		appendHookEntry(hooks, "SubagentStart", map[string]any{
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       hookTimeoutSeconds,
					"statusMessage": "Starting an isolated Gortex subagent turn...",
				},
			},
		})
	}
	if !subagentStopInstalled {
		appendHookEntry(hooks, "SubagentStop", map[string]any{
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       hookTimeoutSeconds,
					"statusMessage": "Clearing Gortex subagent turn state...",
				},
			},
		})
	}
	// PostToolUse always watches Gortex navigation results for the exact
	// enforceable localization terminal contract. Enrich mode also augments
	// native Grep / Glob / Read responses with graph context.
	postToolUseAdded := false
	if !postToolUseInstalled {
		appendHookEntry(hooks, "PostToolUse", map[string]any{
			"matcher": desiredPostToolUseMatcher(mode),
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       hookTimeoutSeconds,
					"statusMessage": desiredPostToolUseStatus(mode),
				},
			},
		})
		postToolUseAdded = true
	}

	allPresent := preToolUseInstalled && stopInstalled && sessionStartInstalled &&
		userPromptSubmitInstalled && subagentStartInstalled && subagentStopInstalled && postToolUseInstalled
	noChanges := allPresent && !matcherUpgraded && dedupedCount == 0 && healedCount == 0 &&
		retiredCount == 0 && modeRewriteCount == 0 && statusRewriteCount == 0 && timeoutRewriteCount == 0 &&
		preToolMatcherRewriteCount == 0 &&
		postToolMatcherRewriteCount == 0 && postToolStatusRewriteCount == 0 && !postToolUseAdded
	if noChanges {
		if w != nil {
			fmt.Fprintf(w, "[gortex init] all hooks already present in %s\n", settingsPath)
		}
		return agents.FileAction{Path: settingsPath, Action: agents.ActionSkip, Reason: "already-configured"}, nil
	}

	if opts.DryRun {
		action := agents.ActionWouldCreate
		if existed {
			action = agents.ActionWouldMerge
		}
		return agents.FileAction{Path: settingsPath, Action: action, Keys: []string{"hooks"}}, nil
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return agents.FileAction{}, err
	}
	if err := agents.AtomicWriteFile(settingsPath, data, 0o644); err != nil {
		return agents.FileAction{}, err
	}

	// Report exactly what changed — helpful for the doctor subcommand
	// and for reassuring users during `gortex init` re-runs.
	var changes []string
	if matcherUpgraded {
		changes = append(changes, "upgraded PreToolUse matcher")
	}
	if preToolMatcherRewriteCount > 0 {
		changes = append(changes, "enabled all-tool terminal gate")
	}
	if postToolMatcherRewriteCount > 0 {
		changes = append(changes, "updated PostToolUse terminal observer")
	}
	if postToolStatusRewriteCount > 0 {
		changes = append(changes, "updated PostToolUse status")
	}
	if dedupedCount > 0 {
		changes = append(changes, fmt.Sprintf("removed %d duplicate entries", dedupedCount))
	}
	if healedCount > 0 {
		changes = append(changes, fmt.Sprintf("rewrote %d stale hook path(s)", healedCount))
	}
	if retiredCount > 0 {
		changes = append(changes, fmt.Sprintf("removed %d retired PreCompact hook(s)", retiredCount))
	}
	if timeoutRewriteCount > 0 {
		changes = append(changes, fmt.Sprintf("converted %d hook timeout(s) to seconds", timeoutRewriteCount))
	}
	if !preToolUseInstalled {
		changes = append(changes, "installed PreToolUse")
	}
	if !stopInstalled {
		changes = append(changes, "installed Stop")
	}
	if !sessionStartInstalled {
		changes = append(changes, "installed SessionStart")
	}
	if !userPromptSubmitInstalled {
		changes = append(changes, "installed UserPromptSubmit")
	}
	if !subagentStartInstalled {
		changes = append(changes, "installed SubagentStart")
	}
	if !subagentStopInstalled {
		changes = append(changes, "installed SubagentStop")
	}
	if postToolUseAdded {
		changes = append(changes, "installed PostToolUse terminal observer")
	}
	if modeRewriteCount > 0 {
		changes = append(changes, fmt.Sprintf("rewrote %d hook command(s) for mode=%s", modeRewriteCount, mode))
	}
	if statusRewriteCount > 0 {
		changes = append(changes, fmt.Sprintf("rewrote %d PreToolUse status message(s) for mode=%s", statusRewriteCount, mode))
	}
	if w != nil {
		fmt.Fprintf(w, "[gortex init] %s in %s\n", strings.Join(changes, ", "), settingsPath)
	}
	action := agents.ActionCreate
	if existed {
		action = agents.ActionMerge
	}
	return agents.FileAction{Path: settingsPath, Action: action, Keys: []string{"hooks"}}, nil
}
