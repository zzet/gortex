package main

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// writeMCPConfig writes a {"mcpServers":{<name>: entry}} file and returns its path.
func writeMCPConfig(t *testing.T, name string, entry any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	body, err := json.Marshal(map[string]any{"mcpServers": map[string]any{name: entry}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func readFileT(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestDoctorVerifiesPathDaemonAndStaleStanza proves the F10 additions: the
// environment preamble probes the binary + daemon with consistent verdicts,
// and the MCP-stanza check classifies a Gortex-authored entry as current vs
// stale (and ignores non-Gortex / non-MCP files) so doctor can point at the
// `gortex install` migration instead of leaving a broken wiring silent.
func TestDoctorVerifiesPathDaemonAndStaleStanza(t *testing.T) {
	t.Run("environment_probe_is_consistent", func(t *testing.T) {
		env := doctorEnvironment()
		if env.DaemonSocket == "" {
			t.Error("environment must report the daemon socket path")
		}
		// Exactly one of {on-path, error} and {running, error} is set —
		// the verdict is internally consistent regardless of whether a
		// daemon or a PATH binary actually exists in this test env.
		if env.BinaryOnPath == (env.BinaryError != "") {
			t.Errorf("binary verdict inconsistent: onPath=%v err=%q", env.BinaryOnPath, env.BinaryError)
		}
		if env.DaemonRunning == (env.DaemonError != "") {
			t.Errorf("daemon verdict inconsistent: running=%v err=%q", env.DaemonRunning, env.DaemonError)
		}
	})

	t.Run("current_stanza", func(t *testing.T) {
		path := writeMCPConfig(t, "gortex", agents.DefaultGortexMCPEntry())
		st, ok := inspectMCPStanza(path)
		if !ok || st != "current" {
			t.Errorf("canonical stanza = (%q,%v), want (current,true)", st, ok)
		}
	})

	// Keys adapters and users add on top of {command, args} never make an
	// entry stale, and neither does an absolute command path.
	currentCases := map[string]map[string]any{
		"empty_env": {"command": "gortex", "args": []any{"mcp"}, "env": map[string]any{}},
		"legacy_worker_env": {"command": "gortex", "args": []any{"mcp"},
			"env": map[string]any{"GORTEX_INDEX_WORKERS": "${GORTEX_WORKERS:-8}"}},
		"cline_always_allow": {"command": "gortex", "args": []any{"mcp"},
			"alwaysAllow": []any{"search_symbols"}},
		"kiro_disabled_auto_approve": {"command": "gortex", "args": []any{"mcp"},
			"disabled": false, "autoApprove": []any{"search_symbols"}},
		"copilot_cli_type":         {"command": "gortex", "args": []any{"mcp"}, "type": "local"},
		"user_cwd_timeout":         {"command": "gortex", "args": []any{"mcp"}, "cwd": "/repo", "timeout": 30},
		"unix_absolute_command":    {"command": "/usr/local/bin/gortex", "args": []any{"mcp"}},
		"windows_absolute_command": {"command": `C:\Program Files\Gortex\gortex.exe`, "args": []any{"mcp"}},
	}
	for name, entry := range currentCases {
		t.Run("current_"+name, func(t *testing.T) {
			path := writeMCPConfig(t, "gortex", entry)
			before := readFileT(t, path)
			st, ok := inspectMCPStanza(path)
			if !ok || st != "current" {
				t.Errorf("stanza %v = (%q,%v), want (current,true)", entry, st, ok)
			}
			if !bytes.Equal(before, readFileT(t, path)) {
				t.Error("doctor must not modify the inspected file")
			}
		})
	}

	// Drifted args are the one thing that makes an entry stale.
	staleCases := map[string][]any{
		"legacy_flag":        {"mcp", "--legacy-flag"},
		"legacy_index_watch": {"mcp", "--index", ".", "--watch"},
		"legacy_proxy":       {"mcp", "--proxy"},
	}
	for name, args := range staleCases {
		t.Run("stale_"+name, func(t *testing.T) {
			path := writeMCPConfig(t, "gortex", map[string]any{"command": "gortex", "args": args})
			before := readFileT(t, path)
			st, ok := inspectMCPStanza(path)
			if !ok || st != "stale" {
				t.Errorf("args %v = (%q,%v), want (stale,true)", args, st, ok)
			}
			if !bytes.Equal(before, readFileT(t, path)) {
				t.Error("doctor must not modify the inspected file")
			}
		})
	}

	t.Run("stale_under_nonstandard_key", func(t *testing.T) {
		// A Gortex-authored entry keyed by a non-"gortex" name is still found.
		stale := map[string]any{"command": "gortex", "args": []any{"mcp", "--x"}}
		path := writeMCPConfig(t, "gortex-daemon", stale)
		st, ok := inspectMCPStanza(path)
		if !ok || st != "stale" {
			t.Errorf("authored stanza under alt key = (%q,%v), want (stale,true)", st, ok)
		}
	})

	t.Run("non_gortex_entry_ignored", func(t *testing.T) {
		other := map[string]any{"command": "node", "args": []any{"server.js"}}
		path := writeMCPConfig(t, "other", other)
		if st, ok := inspectMCPStanza(path); ok {
			t.Errorf("a non-Gortex entry must be ignored; got (%q,%v)", st, ok)
		}
	})

	t.Run("non_mcp_file_ignored", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "notes.txt")
		if err := os.WriteFile(path, []byte("not json at all"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if st, ok := inspectMCPStanza(path); ok {
			t.Errorf("a non-JSON file must be ignored; got (%q,%v)", st, ok)
		}
	})
}

// TestDoctorFreshInstallHasNoStaleStanza: whatever extra keys an adapter
// writes next to command and args, doctor must call a stanza Gortex just
// wrote current. Otherwise a clean `gortex init` is followed by a stale
// warning whose fix (`gortex install`) writes the same entry again.
func TestDoctorFreshInstallHasNoStaleStanza(t *testing.T) {
	for _, a := range buildRegistry().All() {
		t.Run(a.Name(), func(t *testing.T) {
			home, root := t.TempDir(), t.TempDir()
			pinAdapterEnv(t, home)
			// Cline and Kilo Code only write into a config directory that
			// already exists, even with ForceDetect.
			for _, dir := range []string{
				filepath.Join(home, ".config", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings"),
				filepath.Join(root, ".kilocode"),
			} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			env := agents.Env{
				Root:         root,
				Home:         home,
				HookCommand:  "gortex hook",
				Mode:         agents.ModeProject,
				InstallHooks: true,
				Stderr:       io.Discard,
			}
			if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
				t.Fatalf("apply: %v", err)
			}

			// Every JSON file the adapter wrote, not only the ones its
			// plan lists, so an mcpServers file outside the plan is caught.
			sums := map[string][]byte{}
			for _, dir := range []string{home, root} {
				err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return err
					}
					before := readFileT(t, path)
					st, ok := inspectMCPStanza(path)
					if !ok {
						return nil
					}
					sums[path] = before
					if st != "current" {
						t.Errorf("%s: fresh stanza is %q, want current:\n%s", path, st, sums[path])
					}
					return nil
				})
				if err != nil {
					t.Fatalf("walk %s: %v", dir, err)
				}
			}

			// Doctor's own pass over the adapter's planned files agrees, and
			// leaves them byte-identical. An adapter that plans an mcpServers
			// file needs a current row: no row means it wrote nothing or
			// doctor skipped it, and either would otherwise pass unnoticed.
			// Cline plans its file only once the directory seeded above
			// exists, so a wrong seed would look like nothing to check.
			wantStanza, gotCurrent := a.Name() == "cline", false
			for _, f := range inspectAdapter(a, env).Files {
				if f.StanzaStatus == "stale" {
					t.Errorf("doctor reports %s stale right after install", f.Path)
				}
				if slices.Contains(f.Keys, "mcpServers") {
					wantStanza = true
				}
				if f.StanzaStatus == "current" {
					gotCurrent = true
				}
			}
			if wantStanza && !gotCurrent {
				t.Errorf("%s plans an mcpServers file, but doctor reports no current stanza", a.Name())
			}
			for path, before := range sums {
				if !bytes.Equal(before, readFileT(t, path)) {
					t.Errorf("doctor modified %s", path)
				}
			}
		})
	}
}

// pinAdapterEnv points every home and config-dir variable an adapter may
// read at the sandbox, on top of agents.renderEnvPins's list, so Apply cannot
// write to (or read from) the developer's real config. COPILOT_HOME and
// OPENCODE_CONFIG matter here because the test's Env.Home equals $HOME, and
// those adapters honor their override only when the two match.
func pinAdapterEnv(t *testing.T, home string) {
	t.Helper()
	pins := map[string]string{
		"HOME":                        home,
		"USERPROFILE":                 home,
		"AppData":                     filepath.Join(home, "AppData", "Roaming"),
		"LocalAppData":                filepath.Join(home, "AppData", "Local"),
		"CLAUDE_CONFIG_DIR":           "",
		"COPILOT_HOME":                "",
		"OPENCODE_CONFIG":             "",
		"KIMI_CODE_HOME":              "",
		"PI_CODING_AGENT_DIR":         "",
		"GORTEX_CODEX_HOOK_MODE":      "",
		"GORTEX_INSTRUCTIONS_PROFILE": "",
		"XDG_CONFIG_HOME":             "",
		"XDG_DATA_HOME":               "",
		"XDG_CACHE_HOME":              "",
		"XDG_STATE_HOME":              "",
	}
	for k, v := range pins {
		t.Setenv(k, v) // registers the restore
		if v == "" {
			_ = os.Unsetenv(k)
		}
	}
}

// TestDoctorCollapsesUninstalledAdapters: the adapter section describes the
// machine the user has. An agent that is neither installed nor holding Gortex
// files contributes only planned paths for writes that will never happen —
// and one such agent can contribute twenty, burying the lines that matter.
func TestDoctorCollapsesUninstalledAdapters(t *testing.T) {
	reports := []DoctorAgentReport{
		{Name: "claude-code", Detected: true, Configured: true, Files: []DoctorFileStatus{
			{Path: "/repo/.mcp.json", Status: "present", ByteSize: 12},
		}},
		{Name: "hermes", Detected: false, Configured: false, Files: []DoctorFileStatus{
			{Path: "/home/u/.hermes/skills/a/SKILL.md", Status: "missing", Planned: "would-create"},
			{Path: "/home/u/.hermes/skills/b/SKILL.md", Status: "missing", Planned: "would-create"},
		}},
		{Name: "zed", Detected: false, Configured: false},
	}

	t.Run("collapsed by default", func(t *testing.T) {
		doctorAll = false
		var buf bytes.Buffer
		printDoctorHuman(&buf, reports)
		got := buf.String()

		if strings.Contains(got, "SKILL.md") {
			t.Errorf("planned files for an uninstalled agent should be hidden:\n%s", got)
		}
		if !strings.Contains(got, "not installed (2): hermes, zed") {
			t.Errorf("absent agents should still be named:\n%s", got)
		}
		if !strings.Contains(got, "/repo/.mcp.json") {
			t.Errorf("detected agents keep their detail:\n%s", got)
		}
	})

	t.Run("--all restores the listing", func(t *testing.T) {
		doctorAll = true
		t.Cleanup(func() { doctorAll = false })
		var buf bytes.Buffer
		printDoctorHuman(&buf, reports)
		if !strings.Contains(buf.String(), "SKILL.md") {
			t.Errorf("--all should list everything:\n%s", buf.String())
		}
	})
}

// TestDoctorKeepsLeftoverConfig: an agent that is gone but still holds Gortex
// files is not noise — it is stale config the user probably wants to clean up,
// so it survives the collapse and says why it is there.
func TestDoctorKeepsLeftoverConfig(t *testing.T) {
	doctorAll = false
	var buf bytes.Buffer
	printDoctorHuman(&buf, []DoctorAgentReport{
		{Name: "windsurf", Detected: false, Configured: true, Files: []DoctorFileStatus{
			{Path: "/home/u/.codeium/mcp_config.json", Status: "present", ByteSize: 40},
		}},
	})
	got := buf.String()

	if strings.Contains(got, "not installed (1)") {
		t.Errorf("an agent holding Gortex files must not be collapsed away:\n%s", got)
	}
	if !strings.Contains(got, "leftover config") {
		t.Errorf("say why an uninstalled agent is still listed:\n%s", got)
	}
	if !strings.Contains(got, "/home/u/.codeium/mcp_config.json") {
		t.Errorf("the leftover file itself is the point:\n%s", got)
	}
}

// TestDoctorReservesTheCrossForRealGaps: an absent repo-local file is not a
// fault. A machine configured by `gortex install` needs no repo-local config
// at all, so marking it ✗ read as an accusation and implied work the user
// does not have to do. ✗ belongs to things that are broken or outdated.
func TestDoctorReservesTheCrossForRealGaps(t *testing.T) {
	doctorAll = false
	var buf bytes.Buffer
	printDoctorHuman(&buf, []DoctorAgentReport{{
		Name: "claude-code", Detected: true, Configured: true,
		Files: []DoctorFileStatus{
			{Path: "/repo/.mcp.json", Status: "missing", Planned: "would-create"},
			{Path: "/repo/.claude/settings.local.json", Status: "present", ByteSize: 26774},
			{Path: "/home/u/.gemini/mcp_config.json", Status: "present", ByteSize: 223, StanzaStatus: "stale"},
			{Path: "/repo/.broken.json", Status: "unreadable"},
		},
	}})
	got := buf.String()

	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, ".mcp.json") && !strings.Contains(line, "gemini") {
			if strings.Contains(line, glyphCross) {
				t.Errorf("an optional absent file must not be marked as a gap: %q", line)
			}
			if !strings.Contains(line, glyphAbsent) || !strings.Contains(line, "optional") {
				t.Errorf("an absent optional file should read as optional: %q", line)
			}
		}
		if strings.Contains(line, "settings.local.json") && !strings.Contains(line, glyphCheck) {
			t.Errorf("a present file is fine: %q", line)
		}
		// Present-but-outdated and unreadable are the real gaps.
		if strings.Contains(line, "gemini") && !strings.Contains(line, glyphWarn) {
			t.Errorf("an outdated stanza needs attention: %q", line)
		}
		if strings.Contains(line, ".broken.json") && !strings.Contains(line, glyphWarn) {
			t.Errorf("an unreadable file needs attention: %q", line)
		}
	}
	if !strings.Contains(got, "needs attention") {
		t.Errorf("the legend should explain the markers:\n%s", got)
	}
}
