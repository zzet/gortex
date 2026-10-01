package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

// inspect.go is the read-only counterpart to Apply: it reports what is
// actually on disk so `gortex doctor` can compare intent against reality.
// It resolves paths through the same helpers the writers use, so a file
// this package installs and a file it recognises can never drift apart.

// InstallState is the observed state of the OpenCode integration.
//
// It answers "what did we write", not "is it working". For OpenCode that
// distinction is sharper than for a hook-config host: there is no hook
// configuration to read back, so "hooks configured" here means exactly
// "the bridge plugin is present and is ours" (PluginPresent). Whether it
// ever ran is a question only the hook invocation log can answer, and the
// two are only meaningful together.
type InstallState struct {
	ConfigPath    string         `json:"config_path"`
	ConfigPresent bool           `json:"config_present"`
	MCPServer     bool           `json:"mcp_server"`
	Hooks         map[string]int `json:"hooks"`

	// PluginPath is the bridge's install location; PluginPresent is true
	// only when the file is there AND carries PluginMarker, so a
	// same-named plugin somebody else wrote is not counted as ours.
	PluginPath    string `json:"plugin_path"`
	PluginPresent bool   `json:"plugin_present"`

	// Skills / Commands count the curated pack members actually on disk.
	// A partial pack is the tell-tale of an install interrupted midway or
	// a user pruning files, which nothing else in the report would show.
	SkillsDir   string `json:"skills_dir"`
	Skills      int    `json:"skills"`
	CommandsDir string `json:"commands_dir"`
	Commands    int    `json:"commands"`
}

// HookEvents are the telemetry events this integration can actually
// produce, which is not the same list as the plugin's hook names.
//
// Doctor divides recorded runs by this list and calls out any declared
// event with zero runs as a blocker, so an event we cannot substantiate
// becomes a permanent false alarm. The bridge core (internal/hooks/pi.go)
// records under Claude's event vocabulary, and the plugin drives exactly
// three of those:
//
//   - SessionStart      — the once-per-session briefing, sent from the
//     first chat.message (OpenCode exposes no session hook).
//   - UserPromptSubmit  — every chat.message.
//   - PreToolUse        — every tool.execute.before, plus a permission.ask
//     with no matching before-hook.
//
// PostToolUse is deliberately absent even though the plugin subscribes to
// tool.execute.after: that hook delivers guidance the before-hook already
// parked and never shells the bridge, so it can never log a run. Naming
// it here would put a permanent "configured but never ran" line in every
// doctor report.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse"}

// Inspect reads the OpenCode user-level install under home. Every failure
// degrades to "not configured" rather than an error: doctor's job is to
// describe a broken machine, not to fail on one.
func Inspect(home string) InstallState {
	state := InstallState{Hooks: map[string]int{}}
	for _, event := range HookEvents {
		state.Hooks[event] = 0
	}
	if home == "" {
		return state
	}

	// Resolve the same file the writer targets, and strip comments before
	// parsing. A hand-authored config is usually `opencode.jsonc`, and
	// reading a fixed `opencode.json` with a bare json.Unmarshal reports
	// exactly the machine this probe exists to catch — server installed,
	// doctor says it is not — as a clean bill of health.
	state.ConfigPath = GlobalConfigPath(home)
	if data, err := os.ReadFile(state.ConfigPath); err == nil {
		state.ConfigPresent = true
		root := map[string]any{}
		if json.Unmarshal(agents.StripJSONComments(data), &root) == nil {
			if servers, ok := root["mcp"].(map[string]any); ok {
				_, state.MCPServer = servers["gortex"]
			}
		}
	}

	state.PluginPath = PluginPath(home)
	v2Present := false
	if data, err := os.ReadFile(state.PluginPath); err == nil {
		v2Present = strings.Contains(string(data), PluginMarker)
	}

	// Also check V1 location for backward compatibility
	v1Path := V1PluginPath(home)
	v1Present := false
	if data, err := os.ReadFile(v1Path); err == nil {
		v1Present = strings.Contains(string(data), PluginMarker)
	}

	state.PluginPresent = v2Present || v1Present
	// The plugin is the whole hook surface: present means every event it
	// drives is wired, absent means none are. There is no per-event
	// configuration to count, which is why this is a fan-out of one bool
	// rather than a tally like the codex adapter's.
	if state.PluginPresent {
		for _, event := range HookEvents {
			state.Hooks[event] = 1
		}
	}

	state.SkillsDir = globalSkillsDir(home)
	state.Skills = countInstalled(state.SkillsDir, func(dir string) string {
		return filepath.Join(dir, skillFileName)
	}, curatedSkillIDs())

	state.CommandsDir = globalCommandsDir(home)
	state.Commands = countInstalled(state.CommandsDir, func(dir string) string {
		return dir + commandFileExt
	}, curatedCommandIDs())

	return state
}

// countInstalled counts how many of ids exist on disk under root, where
// pathFor maps an id's directory (or basename stem) to the file that must
// exist. Counting the shipped ids rather than listing the directory keeps
// an unrelated file a user dropped in there out of the tally.
func countInstalled(root string, pathFor func(string) string, ids []string) int {
	if root == "" {
		return 0
	}
	n := 0
	for _, id := range ids {
		if _, err := os.Stat(pathFor(filepath.Join(root, id))); err == nil {
			n++
		}
	}
	return n
}

// curatedSkillIDs / curatedCommandIDs return the shipped ids, or nil when
// the corpus fails to parse — a parse failure is a build-time bug the
// package's own tests catch, and doctor reporting zero is a better
// outcome than doctor failing.
func curatedSkillIDs() []string {
	skills, err := Skills()
	if err != nil {
		return nil
	}
	return skillIDs(skills)
}

func curatedCommandIDs() []string {
	commands, err := Commands()
	if err != nil {
		return nil
	}
	return skillIDs(commands)
}

func skillIDs(in []skillpack.Skill) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.ID)
	}
	return out
}
