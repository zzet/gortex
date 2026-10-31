package claudecode

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/agents"
)

// agentsApplyOptsZero is the zero value of agents.ApplyOpts. Helper so
// the mode-switching tests don't drag the agents import into every
// call site.
func agentsApplyOptsZero() agents.ApplyOpts { return agents.ApplyOpts{} }

// readSettingsHooks reads settings.local.json from path and returns
// the hooks map (or fails the test if the file is missing / malformed).
// The mode-switching tests assert on hook structure between InstallHook
// calls, so we centralise the read-and-shape-check here.
func readSettingsHooks(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read %s", path)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	hooks, ok := settings["hooks"].(map[string]any)
	require.True(t, ok, "settings.hooks missing or wrong type")
	return hooks
}

// TestHookCommandPathIsEphemeral covers every branch of the
// ephemeral-path detector because this function's output directly
// decides whether a user's settings.local.json gets rewritten on
// re-run. A false positive here would thrash the user's hook
// config; a false negative leaves them with stale /tmp paths.
func TestHookCommandPathIsEphemeral(t *testing.T) {
	// /bin/sh is a stable POSIX path, not under any ephemeral root.
	// os.Executable() would land in /private/var/folders under go
	// test, which is itself ephemeral — hence hard-coding.
	const existing = "/bin/sh"
	if _, err := os.Stat(existing); err != nil {
		t.Skipf("test fixture %s not present: %v", existing, err)
	}

	missing := filepath.Join("/nonexistent-root-for-gortex-test", "ghost-binary")

	cases := []struct {
		name    string
		cmd     string
		want    bool
		comment string
	}{
		{"empty", "", false, "no fields to inspect"},
		{"bareName", "gortex hook", false, "PATH lookup happens at fire time"},
		{"relative", "./gortex hook", false, "relative paths are user choice, not ephemeral"},
		{"tmp", "/tmp/gortex-hook-fix hook", true, "/tmp is wiped between sessions"},
		{"varFolders", "/var/folders/x/y/z/gortex hook", true, "macOS go-build cache"},
		{"privateTmp", "/private/tmp/gortex hook", true, "macOS resolves /tmp via /private/tmp"},
		{"privateVarFolders", "/private/var/folders/x/y/z/gortex hook", true, "fully resolved go-build cache"},
		{"missingAbsolute", missing + " hook", true, "absolute path that no longer exists"},
		{"healthyAbsolute", existing + " hook", false, "absolute path that exists on disk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HookCommandPathIsEphemeral(tc.cmd)
			assert.Equal(t, tc.want, got, tc.comment)
		})
	}
}

func TestHealStaleHookCommands(t *testing.T) {
	const newCmd = "/opt/homebrew/bin/gortex hook"

	t.Run("emptyHooks", func(t *testing.T) {
		hooks := map[string]any{}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 0, got)
	})

	t.Run("noGortexEntries", func(t *testing.T) {
		hooks := map[string]any{
			"PreToolUse": []any{
				makeHookEntry("Read", "/usr/local/bin/some-other-tool run"),
			},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 0, got)
		entries := hooks["PreToolUse"].([]any)
		inner := entries[0].(map[string]any)["hooks"].([]any)
		cmd := inner[0].(map[string]any)["command"].(string)
		assert.Equal(t, "/usr/local/bin/some-other-tool run", cmd)
	})

	t.Run("healthyGortexEntryUntouched", func(t *testing.T) {
		hooks := map[string]any{
			"PreToolUse": []any{makeHookEntry("Read", "./gortex hook")},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 0, got)
		assert.Equal(t, "./gortex hook", extractCmd(t, hooks, "PreToolUse", 0))
	})

	t.Run("staleEntryRewritten", func(t *testing.T) {
		hooks := map[string]any{
			"Stop": []any{makeHookEntry("", "/tmp/gortex-hook-fix hook")},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 1, got)
		assert.Equal(t, newCmd, extractCmd(t, hooks, "Stop", 0))
	})

	t.Run("multipleEventsAndMixed", func(t *testing.T) {
		hooks := map[string]any{
			"PreToolUse": []any{
				makeHookEntry("Read", "./gortex hook"),
				makeHookEntry("Read", "/usr/local/bin/lint --strict"),
			},
			"SessionStart": []any{makeHookEntry("", "/tmp/gortex-hook-fix hook")},
			"Stop":         []any{makeHookEntry("", "/tmp/gortex-hook-fix hook")},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 2, got)
		assert.Equal(t, "./gortex hook", extractCmd(t, hooks, "PreToolUse", 0))
		assert.Equal(t, "/usr/local/bin/lint --strict", extractCmd(t, hooks, "PreToolUse", 1))
		assert.Equal(t, newCmd, extractCmd(t, hooks, "SessionStart", 0))
		assert.Equal(t, newCmd, extractCmd(t, hooks, "Stop", 0))
	})
}

// writeGortexStub drops a stand-in `gortex` binary in dir and returns its
// path. The name carries .exe on Windows because exec.LookPath only
// considers files whose extension is in PATHEXT there — an extensionless
// stub is invisible to it, and every PATH-resolution assertion below would
// silently degrade to the bare-name fallback.
func writeGortexStub(t *testing.T, dir string) string {
	t.Helper()
	name := "gortex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	return path
}

func TestResolveHookCommand(t *testing.T) {
	t.Run("foundOnPath", func(t *testing.T) {
		dir := t.TempDir()
		fake := writeGortexStub(t, dir)
		t.Setenv("PATH", dir)

		got := ResolveHookCommand(io.Discard)
		// shellSafeHookBinary rewrites separators for the shell that runs
		// the hook, so the expectation is the slash spelling of the path.
		assert.Equal(t, filepath.ToSlash(fake)+" hook", got, "should resolve to absolute path on PATH")
	})

	t.Run("notFoundFallsBackToBare", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		got := ResolveHookCommand(io.Discard)
		assert.Equal(t, "gortex hook", got, "fallback to bare name keeps init working in sandboxes")
	})

	t.Run("commandNeverContainsBackslash", func(t *testing.T) {
		dir := t.TempDir()
		writeGortexStub(t, dir)
		t.Setenv("PATH", dir)

		got := ResolveHookCommand(io.Discard)
		assert.NotContains(t, got, "\\",
			"baked hook command must use forward slashes — Git Bash on Windows mangles backslash paths")
	})

	t.Run("agreesWithMCPStanzaBinary", func(t *testing.T) {
		// The hook command and the MCP server stanza must launch the same
		// gortex binary; a side-by-side install must never split them
		// across two daemons. With a gortex on PATH the hook pins the
		// absolute path while the stanza collapses to the bare name — both
		// resolve to the same file.
		dir := t.TempDir()
		fake := writeGortexStub(t, dir)
		t.Setenv("PATH", dir)

		hookBin := strings.TrimSuffix(ResolveHookCommand(io.Discard), " hook")
		mcpCmd := agents.ResolveGortexCommand()
		// Map the bare name to the PATH gortex it launches, then compare
		// the concrete on-disk binaries. The hook command carries the
		// slash spelling, so both sides come back to native separators
		// before they are compared.
		launched := func(cmd string) string {
			if cmd == "gortex" {
				return fake
			}
			return filepath.Clean(filepath.FromSlash(cmd))
		}
		assert.Equal(t, launched(mcpCmd), launched(hookBin),
			"hook command and MCP stanza must resolve to the same gortex binary")
	})
}

// TestShellSafeHookBinary pins the Windows-path fix: a resolved binary
// path with backslashes (as exec.LookPath returns on Windows) must be
// rewritten to forward slashes so Claude Code's shell-run hook does not
// swallow the separators. Runs on every platform because the helper is
// a pure string transform.
func TestShellSafeHookBinary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"windowsAbsolute", `C:\Users\Borislav\AppData\Local\Programs\gortex\gortex.exe`, "C:/Users/Borislav/AppData/Local/Programs/gortex/gortex.exe"},
		{"windowsMixed", `C:\Users\me/gortex.exe`, "C:/Users/me/gortex.exe"},
		{"unixAbsolute", "/opt/homebrew/bin/gortex", "/opt/homebrew/bin/gortex"},
		{"bareName", "gortex", "gortex"},
		{"forwardSlashesUnchanged", "C:/Users/me/gortex.exe", "C:/Users/me/gortex.exe"},
		{"spaceInPath", "/opt/homebrew/bin/gortex beta/gortex.exe", "'/opt/homebrew/bin/gortex beta/gortex.exe'"},
		{"dollarInPath", "/opt/$(whoami)/gortex", "'/opt/$(whoami)/gortex'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shellSafeHookBinary(tc.in)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "\\", "result must be free of backslashes")
		})
	}
}

func makeHookEntry(matcher, command string) map[string]any {
	entry := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
				"timeout": 3000,
			},
		},
	}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	return entry
}

func extractCmd(t *testing.T, hooks map[string]any, event string, idx int) string {
	t.Helper()
	list, ok := hooks[event].([]any)
	require.True(t, ok, "event %q missing", event)
	require.Greater(t, len(list), idx, "event %q has fewer than %d entries", event, idx+1)
	entry, ok := list[idx].(map[string]any)
	require.True(t, ok)
	inner, ok := entry["hooks"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, inner)
	em, ok := inner[0].(map[string]any)
	require.True(t, ok)
	cmd, _ := em["command"].(string)
	return cmd
}

// ---------------------------------------------------------------------------
// Mode parsing + command rendering
// ---------------------------------------------------------------------------

func TestNormalizeHookMode(t *testing.T) {
	cases := map[string]string{
		"":               HookModeDeny,
		"deny":           HookModeDeny,
		"DENY":           HookModeDeny,
		"  deny  ":       HookModeDeny,
		"enrich":         HookModeEnrich,
		"Enrich":         HookModeEnrich,
		"consult-unlock": HookModeConsultUnlock,
		"Consult-Unlock": HookModeConsultUnlock,
		"nudge":          HookModeAdaptiveNudge,
		"NUDGE":          HookModeAdaptiveNudge,
		"adaptive-nudge": HookModeAdaptiveNudge, // accepted alias
		"unknown":        HookModeDeny,          // safe fallback
		"off":            HookModeDeny,          // install flag handles disable separately
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			assert.Equal(t, want, normalizeHookMode(input))
		})
	}
}

func TestHookCommandWithMode(t *testing.T) {
	base := "/usr/local/bin/gortex hook"
	assert.Equal(t, base, hookCommandWithMode(base, HookModeDeny),
		"deny mode must NOT append --mode flag (back-compat with settings written before --hook-mode existed)")
	assert.Equal(t, base+" --mode=enrich", hookCommandWithMode(base, HookModeEnrich),
		"enrich mode must append --mode=enrich")
	assert.Equal(t, base+" --mode=consult-unlock", hookCommandWithMode(base, HookModeConsultUnlock),
		"consult-unlock mode must append --mode=consult-unlock")
	assert.Equal(t, base+" --mode=nudge", hookCommandWithMode(base, HookModeAdaptiveNudge),
		"nudge mode must append --mode=nudge")
	assert.Equal(t, base+" --mode=nudge", hookCommandWithMode(base, "adaptive-nudge"),
		"adaptive-nudge alias must normalize to --mode=nudge")
	assert.Equal(t, base, hookCommandWithMode(base, ""),
		"empty mode defaults to deny — no flag")
}

// ---------------------------------------------------------------------------
// removeGortexHookEntries — used when switching enrich → deny
// ---------------------------------------------------------------------------

func TestRemoveGortexHookEntries(t *testing.T) {
	hooks := map[string]any{
		"PostToolUse": []any{
			makeHookEntry("Read|Grep|Glob", "/opt/gortex hook --mode=enrich"),
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": "/usr/bin/other-tool"},
				},
			},
		},
	}
	removed := removeGortexHookEntries(hooks, "PostToolUse")
	assert.Equal(t, 1, removed)
	list := hooks["PostToolUse"].([]any)
	require.Len(t, list, 1, "non-Gortex entry must be preserved")
	em := list[0].(map[string]any)
	inner := em["hooks"].([]any)
	cmd := inner[0].(map[string]any)["command"]
	assert.Equal(t, "/usr/bin/other-tool", cmd)
}

func TestRemoveGortexHookEntries_DeletesEmptyEvent(t *testing.T) {
	hooks := map[string]any{
		"PostToolUse": []any{
			makeHookEntry("Read|Grep|Glob", "/opt/gortex hook --mode=enrich"),
		},
	}
	removed := removeGortexHookEntries(hooks, "PostToolUse")
	assert.Equal(t, 1, removed)
	_, exists := hooks["PostToolUse"]
	assert.False(t, exists, "empty event should be deleted, not left as []")
}

func TestRemoveGortexHookEntries_NoOpOnMissingEvent(t *testing.T) {
	hooks := map[string]any{}
	assert.Equal(t, 0, removeGortexHookEntries(hooks, "PostToolUse"))
}

// ---------------------------------------------------------------------------
// rewriteGortexHookMode — switches mode without touching user fields
// ---------------------------------------------------------------------------

func TestRewriteGortexHookMode_PreservesCustomGortexGroup(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": "Read|Grep",
				"hooks": []any{
					map[string]any{
						"type":          "command",
						"command":       "/opt/gortex hook",
						"timeout":       3000,
						"statusMessage": "custom user-set message",
					},
				},
			},
		},
	}
	rewritten := rewriteGortexHookMode(hooks, "/opt/gortex hook --mode=enrich")
	assert.Equal(t, 0, rewritten)
	list := hooks["PreToolUse"].([]any)
	em := list[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	assert.Equal(t, "/opt/gortex hook", em["command"])
	assert.Equal(t, "custom user-set message", em["statusMessage"],
		"installer must not claim ownership of a custom Gortex hook group")
}

func TestRewriteGortexHookMode_NoOpOnIdenticalCommand(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{makeHookEntry("Read|Grep|Glob", "/opt/gortex hook")},
	}
	assert.Equal(t, 0, rewriteGortexHookMode(hooks, "/opt/gortex hook"))
}

func TestRewriteGortexHookMode_IgnoresNonGortexEntries(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{makeHookEntry("Bash", "/usr/bin/other --thing")},
	}
	assert.Equal(t, 0, rewriteGortexHookMode(hooks, "/opt/gortex hook --mode=enrich"))
	cmd := extractCmd(t, hooks, "PreToolUse", 0)
	assert.Equal(t, "/usr/bin/other --thing", cmd,
		"non-Gortex entries must NOT be touched even when we're rewriting")
}

// TestUpgradeGortexMatcher_RewritesPreviousCurrent verifies the
// migration path: an existing install pinned to the previous canonical
// matcher (no MCP read tools) upgrades in place so the read-discipline
// nudge starts firing without the user touching settings.json.
func TestUpgradeGortexMatcher_RewritesPreviousCurrent(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{managedTestHookEntry("Read|Grep|Glob|Task|Bash|Edit|Write", "/opt/gortex hook", preToolUseStatusDeny)},
	}
	assert.True(t, upgradeGortexMatcher(hooks), "previous-current matcher should upgrade")
	got := hooks["PreToolUse"].([]any)[0].(map[string]any)["matcher"].(string)
	assert.Equal(t, CurrentPreToolUseMatcher, got)
	assert.Equal(t, "*", got,
		"the terminal gate must observe every tool without enumerating host-specific names")
}

func TestUpgradeGortexMatcher_RewritesV060MCPReadMatcher(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{managedTestHookEntry(v060PreToolUseMatcher, "/opt/gortex hook", preToolUseStatusDeny)},
	}
	assert.True(t, upgradeGortexMatcher(hooks))
	got := hooks["PreToolUse"].([]any)[0].(map[string]any)["matcher"].(string)
	assert.Equal(t, CurrentPreToolUseMatcher, got)
}

func TestUpgradeGortexMatcher_NoOpOnCurrent(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{makeHookEntry(CurrentPreToolUseMatcher, "/opt/gortex hook")},
	}
	assert.False(t, upgradeGortexMatcher(hooks), "already-current matcher must not be rewritten")
}

func TestUpgradeGortexMatcher_IgnoresNonGortexEntries(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{makeHookEntry("Read|Grep|Glob|Task|Bash|Edit|Write", "/usr/bin/other thing")},
	}
	assert.False(t, upgradeGortexMatcher(hooks), "a non-Gortex entry must not be upgraded")
}

// ---------------------------------------------------------------------------
// InstallHookWithMode — end-to-end: deny → enrich → deny round-trip
// ---------------------------------------------------------------------------

func TestInstallHookWithMode_DenyMode(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir()) // force fallback to bare "gortex hook"

	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)

	hooks := readSettingsHooks(t, settingsPath)
	require.Contains(t, hooks, "PostToolUse",
		"terminal observation is independent of the source-access mode")
	post := hooks["PostToolUse"].([]any)[0].(map[string]any)
	assert.Equal(t, localizationPostToolUseMatcher, post["matcher"])
	cmd := extractCmd(t, hooks, "PreToolUse", 0)
	assert.Equal(t, "gortex hook", cmd,
		"deny mode keeps the bare command — no --mode flag")
	pre := hooks["PreToolUse"].([]any)[0].(map[string]any)
	assert.Equal(t, localizationPreToolUseMatcher, pre["matcher"])
}

func TestInstallHook_InstallsTurnLifecycleHooks(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir()) // force fallback to bare "gortex hook"

	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)

	hooks := readSettingsHooks(t, settingsPath)
	for _, event := range []string{"UserPromptSubmit", "SubagentStart", "SubagentStop"} {
		require.Contains(t, hooks, event, "%s hook must be installed for turn isolation", event)
		assert.Equal(t, "gortex hook", extractCmd(t, hooks, event, 0))
	}
}

func TestInstallHookWithMode_EnrichMode(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir())

	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeEnrich, agentsApplyOptsZero())
	require.NoError(t, err)

	hooks := readSettingsHooks(t, settingsPath)
	require.Contains(t, hooks, "PostToolUse")
	postCmd := extractCmd(t, hooks, "PostToolUse", 0)
	assert.Equal(t, "gortex hook --mode=enrich", postCmd)

	preCmd := extractCmd(t, hooks, "PreToolUse", 0)
	assert.Equal(t, "gortex hook --mode=enrich", preCmd,
		"PreToolUse must use the same --mode=enrich command so it falls back to soft-context")

	// Enrich keeps the native read-shaped tools and adds the six terminal
	// contract producers.
	post := hooks["PostToolUse"].([]any)[0].(map[string]any)
	assert.Equal(t, desiredPostToolUseMatcher(HookModeEnrich), post["matcher"])
}

func TestInstallHookWithMode_DenyToEnrichRoundTrip(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir())

	// Initial deny install.
	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)
	hooks := readSettingsHooks(t, settingsPath)
	require.Contains(t, hooks, "PostToolUse")
	assert.Equal(t, localizationPostToolUseMatcher,
		hooks["PostToolUse"].([]any)[0].(map[string]any)["matcher"])
	assert.Equal(t, "gortex hook", extractCmd(t, hooks, "PreToolUse", 0))

	// Switch to enrich.
	_, err = InstallHookWithMode(io.Discard, settingsPath, HookModeEnrich, agentsApplyOptsZero())
	require.NoError(t, err)
	hooks = readSettingsHooks(t, settingsPath)
	require.Contains(t, hooks, "PostToolUse", "enrich install must add PostToolUse")
	assert.Equal(t, "gortex hook --mode=enrich", extractCmd(t, hooks, "PreToolUse", 0),
		"PreToolUse command must be rewritten on mode switch")
	assert.Equal(t, "gortex hook --mode=enrich", extractCmd(t, hooks, "PostToolUse", 0))

	// Switch back to deny — terminal observation remains, while native
	// post-read enrichment is removed and commands return to deny mode.
	_, err = InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)
	hooks = readSettingsHooks(t, settingsPath)
	require.Contains(t, hooks, "PostToolUse")
	assert.Equal(t, localizationPostToolUseMatcher,
		hooks["PostToolUse"].([]any)[0].(map[string]any)["matcher"])
	assert.Equal(t, "gortex hook", extractCmd(t, hooks, "PreToolUse", 0),
		"switch back to deny must restore bare command on PreToolUse")
	assert.Equal(t, "gortex hook", extractCmd(t, hooks, "PostToolUse", 0),
		"switch back to deny must restore bare command on PostToolUse")
}

func TestInstallHookWithMode_EnrichIdempotent(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir())

	for i := range 3 {
		_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeEnrich, agentsApplyOptsZero())
		require.NoError(t, err, "iteration %d", i)
	}
	hooks := readSettingsHooks(t, settingsPath)
	// PostToolUse should have exactly one entry — re-running install
	// must not append duplicates.
	post := hooks["PostToolUse"].([]any)
	assert.Len(t, post, 1, "repeated install must not duplicate the PostToolUse entry")
}

// InstallHook (the back-compat wrapper) defaults to deny mode while retaining
// the mode-independent terminal observer.
func TestInstallHook_DefaultsToDeny(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir())

	_, err := InstallHook(io.Discard, settingsPath, agentsApplyOptsZero())
	require.NoError(t, err)
	hooks := readSettingsHooks(t, settingsPath)
	require.Contains(t, hooks, "PostToolUse")
	post := hooks["PostToolUse"].([]any)[0].(map[string]any)
	assert.Equal(t, localizationPostToolUseMatcher, post["matcher"])
}

// A fresh install must not wire PreCompact: Claude Code discards its output,
// and the post-compaction briefing rides on SessionStart(source="compact")
// instead (#522, #814).
func TestInstallHook_FreshInstallSkipsPreCompact(t *testing.T) {
	for _, mode := range []string{HookModeDeny, HookModeEnrich} {
		t.Run(mode, func(t *testing.T) {
			settingsPath := filepath.Join(t.TempDir(), "settings.local.json")
			t.Setenv("PATH", t.TempDir())

			_, err := InstallHookWithMode(io.Discard, settingsPath, mode, agentsApplyOptsZero())
			require.NoError(t, err)

			hooks := readSettingsHooks(t, settingsPath)
			assert.NotContains(t, hooks, "PreCompact")
			require.Contains(t, hooks, "SessionStart")
		})
	}
}

// Refreshing an install written before #814 must drop the Gortex PreCompact
// hook while keeping PreCompact hooks owned by the user, including a user
// command that shares a group with the Gortex one.
func TestInstallHook_RefreshRemovesGortexPreCompact(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.local.json")
	t.Setenv("PATH", t.TempDir())

	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	hooks := settings["hooks"].(map[string]any)
	hooks["PreCompact"] = []any{
		makeHookEntry("", "gortex hook"),
		makeHookEntry("", "/usr/local/bin/notify-compact"),
		map[string]any{"hooks": []any{
			map[string]any{"type": "command", "command": "gortex hook", "timeout": 3000},
			map[string]any{"type": "command", "command": "~/bin/save-transcript", "timeout": 3000},
		}},
	}
	data, err = json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(settingsPath, data, 0o644))

	action, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)
	assert.Equal(t, agents.ActionMerge, action.Action, "removing the retired hook is a change")

	hooks = readSettingsHooks(t, settingsPath)
	require.Contains(t, hooks, "PreCompact", "user-owned PreCompact hooks must survive")
	assert.False(t, hasGortexHookEntry(hooks, "PreCompact"), "Gortex PreCompact hook must be removed")
	require.Len(t, hooks["PreCompact"].([]any), 2)
	assert.Equal(t, "/usr/local/bin/notify-compact", extractCmd(t, hooks, "PreCompact", 0))
	assert.Equal(t, "~/bin/save-transcript", extractCmd(t, hooks, "PreCompact", 1))

	// A second refresh is a no-op.
	action, err = InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)
	assert.Equal(t, agents.ActionSkip, action.Action)
}

// When the Gortex hook was the only PreCompact entry, the event key goes away.
func TestInstallHook_RefreshDropsEmptyPreCompact(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.local.json")
	t.Setenv("PATH", t.TempDir())

	legacy := map[string]any{"hooks": map[string]any{
		"PreCompact": []any{makeHookEntry("", "gortex hook --mode=enrich")},
	}}
	data, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(settingsPath, data, 0o644))

	_, err = InstallHookWithMode(io.Discard, settingsPath, HookModeEnrich, agentsApplyOptsZero())
	require.NoError(t, err)

	hooks := readSettingsHooks(t, settingsPath)
	assert.NotContains(t, hooks, "PreCompact")
}

// assertHookTimeoutsInSeconds fails when any Gortex hook timeout differs
// from the installer's value. Claude Code reads timeouts in seconds, so
// anything above a couple of minutes is a unit bug.
func assertHookTimeoutsInSeconds(t *testing.T, hooks map[string]any) {
	t.Helper()
	checked := 0
	for event, raw := range hooks {
		want := float64(gortexHookTimeout(event))
		groups, _ := raw.([]any)
		for _, g := range groups {
			inner, _ := g.(map[string]any)["hooks"].([]any)
			for _, e := range inner {
				got, _ := e.(map[string]any)["timeout"].(float64)
				if got <= 0 || got > 120 {
					t.Errorf("%s timeout = %v, want seconds in (0, 120]", event, got)
				} else if got != want {
					t.Errorf("%s timeout = %v, want %v", event, got, want)
				}
				checked++
			}
		}
	}
	require.NotZero(t, checked, "no hook entries checked")
}

func TestInstallHook_TimeoutsInSeconds(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.local.json")
	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)
	assertHookTimeoutsInSeconds(t, readSettingsHooks(t, settingsPath))
}

func TestInstallHook_MigratesMillisecondTimeouts(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.local.json")
	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	hooks := settings["hooks"].(map[string]any)
	for event, raw := range hooks {
		legacy := 3000
		if event == "Stop" {
			legacy = 5000
		}
		for _, g := range raw.([]any) {
			for _, e := range g.(map[string]any)["hooks"].([]any) {
				e.(map[string]any)["timeout"] = legacy
			}
		}
	}
	hooks["SessionStart"] = append(hooks["SessionStart"].([]any), map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "~/bin/notify", "timeout": 3000}},
	})
	data, err = json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(settingsPath, data, 0o644))

	action, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)
	assert.Equal(t, agents.ActionMerge, action.Action, "converting timeouts is a change")

	hooks = readSettingsHooks(t, settingsPath)
	user := hooks["SessionStart"].([]any)[1]
	delete(hooks, "SessionStart")
	assertHookTimeoutsInSeconds(t, hooks)
	assert.Equal(t, float64(3000), user.(map[string]any)["hooks"].([]any)[0].(map[string]any)["timeout"],
		"a user-owned hook keeps its timeout")

	action, err = InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)
	assert.Equal(t, agents.ActionSkip, action.Action)
}

func TestInstallHook_KeepsCustomGortexTimeout(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.local.json")
	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	stop := settings["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	stop["timeout"] = 45
	data, err = json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(settingsPath, data, 0o644))

	action, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)
	assert.Equal(t, agents.ActionSkip, action.Action)
	got := readSettingsHooks(t, settingsPath)["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	assert.Equal(t, float64(45), got["timeout"])
}
