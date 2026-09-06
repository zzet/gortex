package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// PluginBundleSpec describes one generation of the marketplace
// plugin bundle. The same emitter drives the Anthropic
// Plugin Marketplace artifact today; the Cursor variant in a follow-up
// will use the same struct with a different LayoutVariant.
type PluginBundleSpec struct {
	// TargetDir is the directory to emit into. The directory is created
	// if it does not exist; existing files within it are overwritten.
	TargetDir string

	// Version is the gortex SemVer string to bake into plugin.json
	// (e.g. "0.18.2"). Marketplace entries pin a release ref; this
	// string identifies what's inside.
	Version string

	// LayoutVariant chooses the output shape. Today only
	// LayoutVariantAnthropic is implemented; the Cursor variant will
	// arrive when its marketplace schema settles.
	LayoutVariant LayoutVariant
}

// LayoutVariant enumerates the marketplace layouts the emitter knows
// how to write.
type LayoutVariant string

const (
	// LayoutVariantAnthropic targets the Anthropic Plugin Marketplace
	// schema (https://anthropic.com/claude-code/marketplace.schema.json
	// and the per-plugin layout discovered via the example-plugin
	// reference shipped in claude-plugins-official).
	LayoutVariantAnthropic LayoutVariant = "anthropic"
)

// pluginAuthorName / pluginAuthorEmail / pluginHomepage are the
// fields baked into every plugin.json. They are package-level so the
// marketplace.json submission script can read the same values
// without re-deriving them.
const (
	pluginName        = "gortex"
	pluginDescription = "Your AI's live map of your codebase — every call, dependency, and contract indexed across repositories and shared through a local daemon. Exposes 21 focused MCP tools for exploration, graph navigation, change safety, editing, refactoring, and review without flooding the agent with one schema per operation. Works with Claude Code and any MCP-aware client. Real AST parsing across 92 languages is deterministic and local-first, with confidence-tagged results and no LLM calls during indexing. Apache 2.0 licensed."
	pluginAuthorName  = "Andrey Kumanyaev"
	pluginAuthorEmail = "support@gortex.dev"
	pluginHomepage    = "https://gortex.dev"
)

// pluginMCPJSON is the .mcp.json content for the marketplace plugin.
// Stdio form with `gortex mcp --proxy`: the marketplace user installs
// the plugin, the binary is on PATH (via the get.gortex.dev installer
// or Homebrew), and `--proxy` auto-detects the daemon and falls back
// to spawning a local MCPServer when no daemon is reachable.
//
// Indentation matches Claude Code's reference plugins (2 spaces, no
// trailing newline beyond the final brace's).
const pluginMCPJSON = `{
  "gortex": {
    "command": "gortex",
    "args": ["mcp"]
  }
}
`

// pluginHooksJSON is the hooks/hooks.json content for the
// marketplace plugin. Seven event bindings (PreToolUse, UserPromptSubmit,
// SubagentStart, SubagentStop, PreCompact, Stop, SessionStart) all dispatch
// through ${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh
// — a thin wrapper that locates the gortex binary and invokes
// `gortex hook`. The wrapper's job is:
//
//   - Fail soft when gortex is missing (print to stderr, exit 0) so
//     a marketplace user without the binary doesn't see hard errors
//     on every Claude Code action.
//   - Forward stdin / stderr / argv unchanged so all today's
//     `gortex hook` event-dispatcher logic in internal/hooks/dispatch.go
//     keeps working byte-for-byte.
//
// The %s is filled with CurrentPreToolUseMatcher at emit time so the
// marketplace plugin and the `gortex init` settings.json never drift.
const pluginHooksJSON = `{
  "description": "Gortex graph-aware hooks: parent and subagent turn state, PreToolUse routing, PreCompact orientation, post-task diagnostics, SessionStart cold briefing.",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "%s",
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 3000,
            "statusMessage": "Enforcing Gortex graph access policy..."
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 3000,
            "statusMessage": "Starting a fresh Gortex turn..."
          }
        ]
      }
    ],
    "SubagentStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 3000,
            "statusMessage": "Starting an isolated Gortex subagent turn..."
          }
        ]
      }
    ],
    "SubagentStop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 3000,
            "statusMessage": "Clearing Gortex subagent turn state..."
          }
        ]
      }
    ],
    "PreCompact": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 3000,
            "statusMessage": "Injecting Gortex orientation snapshot..."
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 5000,
            "statusMessage": "Running Gortex post-task diagnostics..."
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 3000,
            "statusMessage": "Loading Gortex graph orientation..."
          }
        ]
      }
    ]
  }
}
`

// pluginHookHandler is the bash wrapper script that every hook event
// invokes. It locates the gortex binary on PATH, runs `gortex hook`
// with stdin/stderr/argv forwarded, and degrades gracefully when the
// binary is missing — print one warning to stderr and exit 0 so a
// missing binary never blocks the user's Claude Code session.
const pluginHookHandler = `#!/usr/bin/env bash
# gortex marketplace-plugin hook wrapper.
# Locates the gortex binary on PATH and forwards the Claude Code hook
# event to ` + "`gortex hook`" + `. Falls back to a soft-fail (warn-and-exit-0)
# when gortex is not installed, so a missing binary never blocks the
# user's session — the marketplace plugin can always be installed in
# advance of the binary.
#
# Install gortex via:  curl -fsSL https://get.gortex.dev | sh
set -u

if ! command -v gortex >/dev/null 2>&1; then
  echo "gortex binary not found on PATH — install via 'curl -fsSL https://get.gortex.dev | sh' to enable graph-aware hooks." >&2
  exit 0
fi

exec gortex hook "$@"
`

const pluginReadmeBody = `# Gortex — Claude Code Plugin

Gortex is a graph-based code intelligence engine. This plugin connects Claude
Code to 21 focused MCP tools for exploration, navigation, change safety,
editing, refactoring, and review across 92 languages. A shared graph daemon
keeps agents, harnesses, and editor sessions in sync.

## Install

This plugin assumes the ` + "`gortex`" + ` binary is on PATH. Install it via:

` + "```sh\ncurl -fsSL https://get.gortex.dev | sh\n```" + `

The installer fetches a signed release tarball, verifies cosign + SHA256,
installs to ` + "`~/.local/bin`" + ` (or ` + "`/usr/local/bin`" + `), and runs ` + "`gortex install`" + `
+ ` + "`gortex init`" + `. Homebrew users on macOS can also use:

` + "```sh\nbrew install zzet/tap/gortex\n```" + `

## What this plugin adds

| Surface | What you get |
|---------|--------------|
| **MCP server** | 21 tools over stdio via ` + "`gortex mcp`" + `: begin with ` + "`explore`" + `, inspect with ` + "`search`" + ` / ` + "`read`" + ` / ` + "`relations`" + ` / ` + "`trace`" + `, assess with ` + "`analyze`" + ` / ` + "`change`" + ` / ` + "`review`" + `, and mutate with ` + "`edit`" + ` / ` + "`refactor`" + `. ` + "`capabilities`" + ` returns an operation's exact schema on demand. |
| **Slash commands** | Discovery: ` + "`/gortex-guide`" + `, ` + "`/gortex-explore`" + `, ` + "`/gortex-debug`" + `, ` + "`/gortex-impact`" + `, ` + "`/gortex-dataflow-trace`" + `, ` + "`/gortex-cross-repo-usage`" + `, ` + "`/gortex-co-change`" + `, ` + "`/gortex-onboarding`" + ` · Edit & refactor: ` + "`/gortex-refactor`" + `, ` + "`/gortex-safe-edit`" + `, ` + "`/gortex-rename`" + `, ` + "`/gortex-extract-function`" + `, ` + "`/gortex-fix-all`" + `, ` + "`/gortex-add-test`" + ` · Review & operate: ` + "`/gortex-pr-review`" + `, ` + "`/gortex-pr-review-agent`" + `, ` + "`/gortex-architecture-review`" + `, ` + "`/gortex-quality-audit`" + `, ` + "`/gortex-incident-investigation`" + `, ` + "`/gortex-episode-replay`" + ` |
| **Skills** | Twenty model-invoked, task-shaped workflows require callable native MCP, start with ` + "`explore`" + `, gate mutations with ` + "`change`" + `, write through ` + "`edit`" + ` / ` + "`refactor`" + `, and verify the result. The separate CLI skill mirrors those names through ` + "`gortex call`" + ` only for a harness with no MCP transport by design. |
| **Hooks** | UserPromptSubmit parent-turn isolation, SubagentStart/SubagentStop agent isolation, PreToolUse graph routing, PreCompact orientation, Stop post-task diagnostics, and SessionStart briefing. Hook guidance uses the same public MCP tool names. |

## First run

After install, point Claude Code at any code repository and ask a task that
involves understanding code structure ("how does authentication work?",
"what breaks if I rename ` + "`UserStore`" + `?"). Begin with ` + "`explore`" + `; the
slash commands provide short, ordered workflows for common tasks.

The MCP server owns graph startup and should expose the tools without a manual
daemon step. If configured tools are missing or unreachable, report an MCP
integration failure; do not start a daemon manually or switch to a CLI fallback.

## Links

- Homepage: https://gortex.dev
- Source:   https://github.com/zzet/gortex
- License:  https://github.com/zzet/gortex/blob/main/LICENSE.md (Apache 2.0)
`

// pluginLicenseBody is the LICENSE shipped inside the plugin
// directory. Plugins under claude-plugins-official tend to ship the
// project's own license rather than a marketplace-imposed one — we
// follow that pattern by emitting a one-line pointer to the canonical
// LICENSE.md in the upstream repo. Keeps the plugin directory small
// and avoids drift from the source-of-truth license.
const pluginLicenseBody = `Gortex is licensed under the Apache License, Version 2.0. The full
license text lives in LICENSE.md at the root of the upstream repository:
https://github.com/zzet/gortex/blob/main/LICENSE.md

Copyright 2024-2026 Andrey Kumanyaev <me@zzet.org>
`

// EmitPluginBundle writes a marketplace plugin layout under
// spec.TargetDir. The directory is created if missing; existing files
// within it are overwritten so re-runs converge on a deterministic
// output. Returns the list of paths written, relative to TargetDir,
// in stable order.
//
// Single source of truth: skills come from agents.GlobalSkills, slash
// commands from agents.SlashCommands. Both are reused from the agents
// package without duplication, so a change to a skill's body flows
// through both the user-level install (~/.claude/skills/) and the
// marketplace plugin (claude-plugin/skills/) on the next emit.
func EmitPluginBundle(spec PluginBundleSpec) ([]string, error) {
	if spec.LayoutVariant == "" {
		spec.LayoutVariant = LayoutVariantAnthropic
	}
	if spec.LayoutVariant != LayoutVariantAnthropic {
		return nil, fmt.Errorf("unsupported plugin layout variant %q (only %q is implemented)", spec.LayoutVariant, LayoutVariantAnthropic)
	}
	if spec.TargetDir == "" {
		return nil, fmt.Errorf("EmitPluginBundle: TargetDir is required")
	}
	if spec.Version == "" {
		return nil, fmt.Errorf("EmitPluginBundle: Version is required (e.g. \"0.18.2\")")
	}

	if err := os.MkdirAll(spec.TargetDir, 0o755); err != nil {
		return nil, fmt.Errorf("create target dir: %w", err)
	}

	written := make([]string, 0, 16)
	write := func(rel string, body []byte, mode os.FileMode) error {
		full := filepath.Join(spec.TargetDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(rel), err)
		}
		if err := os.WriteFile(full, body, mode); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
		written = append(written, rel)
		return nil
	}

	// 1. .claude-plugin/plugin.json
	manifest := map[string]any{
		"name":        pluginName,
		"description": pluginDescription,
		"version":     spec.Version,
		"author": map[string]any{
			"name":  pluginAuthorName,
			"email": pluginAuthorEmail,
		},
		"homepage": pluginHomepage,
	}
	manifestJSON, err := marshalJSONStable(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal plugin.json: %w", err)
	}
	if err := write(".claude-plugin/plugin.json", manifestJSON, 0o644); err != nil {
		return nil, err
	}

	// 2. README.md
	if err := write("README.md", []byte(pluginReadmeBody), 0o644); err != nil {
		return nil, err
	}

	// 3. LICENSE
	if err := write("LICENSE", []byte(pluginLicenseBody), 0o644); err != nil {
		return nil, err
	}

	// 4. .mcp.json
	if err := write(".mcp.json", []byte(pluginMCPJSON), 0o644); err != nil {
		return nil, err
	}

	// 5. commands/gortex-*.md — sorted for deterministic output.
	for _, name := range sortedKeys(SlashCommands) {
		body := SlashCommands[name]
		if err := write(filepath.Join("commands", name), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}

	// 6. skills/<name>/SKILL.md — sorted for deterministic output.
	for _, name := range sortedKeys(GlobalSkills) {
		body := GlobalSkills[name]
		if err := write(filepath.Join("skills", name, "SKILL.md"), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}

	// 7. agents/<name> — sub-agent definitions, sorted for
	// deterministic output. Mirrors the on-disk path Claude Code
	// expects under .claude/agents/ when the plugin is installed.
	for _, name := range sortedKeys(SubAgents) {
		body := SubAgents[name]
		if err := write(filepath.Join("agents", name), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}

	// 8. hooks/hooks.json + hooks-handlers/gortex-hook.sh
	pluginHooks, err := pluginHooksWithLocalizationTerminality(fmt.Appendf(nil, pluginHooksJSON, CurrentPreToolUseMatcher))
	if err != nil {
		return nil, err
	}
	if err := write("hooks/hooks.json", pluginHooks, 0o644); err != nil {
		return nil, err
	}
	if err := write("hooks-handlers/gortex-hook.sh", []byte(pluginHookHandler), 0o755); err != nil {
		return nil, err
	}

	return written, nil
}

// marshalJSONStable returns indented JSON with sorted keys at every
// level. Two-space indent matches Claude Code's reference plugins;
// trailing newline keeps the file POSIX-text-friendly and avoids
// editor diff noise.
func marshalJSONStable(v any) ([]byte, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// sortedKeys returns the keys of m in lexicographic order. Used so
// the on-disk emit order is independent of map iteration order, which
// keeps the directory diff-stable across Go versions.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PluginManifestPath / PluginMCPPath / PluginHooksPath return the
// in-bundle paths the emitter writes. Exposed so callers (e.g. the
// CI drift guard) can refer to them without hardcoding path strings.
func PluginManifestPath() string { return filepath.Join(".claude-plugin", "plugin.json") }
func PluginMCPPath() string      { return ".mcp.json" }
func PluginHooksPath() string    { return filepath.Join("hooks", "hooks.json") }

// PluginCommandPaths returns the in-bundle paths of all slash command
// files in stable order.
func PluginCommandPaths() []string {
	out := make([]string, 0, len(SlashCommands))
	for _, name := range sortedKeys(SlashCommands) {
		out = append(out, filepath.Join("commands", name))
	}
	return out
}

// PluginSkillPaths returns the in-bundle paths of all SKILL.md files
// in stable order.
func PluginSkillPaths() []string {
	out := make([]string, 0, len(GlobalSkills))
	for _, name := range sortedKeys(GlobalSkills) {
		out = append(out, filepath.Join("skills", name, "SKILL.md"))
	}
	return out
}

// PluginSubAgentPaths returns the in-bundle paths of all sub-agent
// files in stable order.
func PluginSubAgentPaths() []string {
	out := make([]string, 0, len(SubAgents))
	for _, name := range sortedKeys(SubAgents) {
		out = append(out, filepath.Join("agents", name))
	}
	return out
}
