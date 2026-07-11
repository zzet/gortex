// Package agents defines the contract Gortex uses to wire itself into
// external AI coding assistants (Claude Code, Cursor, Kiro, Continue.dev,
// Cline, Windsurf, OpenCode, VS Code / Copilot, Antigravity, …).
//
// Each integration lives in its own sub-package and implements Adapter.
// `gortex init` iterates over a Registry of adapters and for each one
// calls Detect → Plan → Apply. The split lets CI exercise planning
// without touching disk (Plan + --dry-run) and makes every writer
// funnel through a single code path (writer.go), so atomic-rename,
// dry-run reporting, and golden-fixture testing stay uniform across
// agents.
package agents

import "io"

// Adapter is the contract every agent integration implements.
//
// Semantics:
//   - Name is stable across releases; users reference it via
//     --agents=<csv>. Lowercase kebab-case.
//   - DocsURL points to the agent's *own* MCP/hook documentation
//     (not Gortex docs) so --json consumers can trace what schema
//     we're targeting.
//   - Detect never modifies disk. False means "skip" — not an error.
//   - Plan is pure: it returns what Apply *would* do for a given
//     Env, without writing. Callers use it to power --dry-run and
//     `gortex init doctor`.
//   - Apply executes the plan. It must respect ApplyOpts.DryRun
//     (return planned actions without writing) and ApplyOpts.Force
//     (overwrite merge-preserved keys).
type Adapter interface {
	Name() string
	DocsURL() string
	Detect(env Env) (bool, error)
	Plan(env Env) (*Plan, error)
	Apply(env Env, opts ApplyOpts) (*Result, error)
}

// Mode selects between per-repo and user-level installation.
// `gortex init` runs adapters in ModeProject; `gortex install` runs
// them in ModeGlobal. Adapters branch on this to choose between
// project-local paths (.mcp.json, .cursor/mcp.json, CLAUDE.md, …) and
// user-level paths (~/.claude.json, ~/.gemini/settings.json, …).
type Mode int

const (
	// ModeProject writes project-local files (.mcp.json, .cursor/mcp.json, …).
	// Used by `gortex init`.
	ModeProject Mode = iota

	// ModeGlobal writes user-level files (~/.claude.json, ~/.gemini/settings.json, …).
	// Used by `gortex install`.
	ModeGlobal
)

// Env bundles the inputs every adapter needs: where to write, which
// binary to reference in hook commands, user home for home-rooted
// integrations, and a stderr-like writer for progress messages. Test
// code swaps Stderr for a buffer to assert on messages without
// capturing process stderr.
type Env struct {
	// Root is the absolute path to the repository. Adapters join
	// relative paths under this.
	Root string

	// Home is the user's home directory, resolved once by the caller
	// so adapters don't each call os.UserHomeDir().
	Home string

	// HookCommand is the shell command to bake into agent hook
	// config (e.g. "/usr/local/bin/gortex hook"). Resolved once by
	// the caller so every adapter writes the same string.
	HookCommand string

	// Mode is per-repo vs user-level. See ModeProject / ModeGlobal.
	Mode Mode

	// InstallHooks is false when the user passed --no-hooks or
	// answered "no" to the wizard. Hook-capable adapters use it to
	// skip their lifecycle hook surfaces while still installing MCP.
	InstallHooks bool

	// HookMode is the posture for the PreToolUse / PostToolUse hook
	// integration: "deny" (default — redirect by deny on the
	// PreToolUse side, no PostToolUse) or "enrich" (never deny,
	// install PostToolUse that augments tool output with graph
	// context). Empty falls back to "deny". Only the Claude Code
	// adapter currently honours it; other adapters ignore the field.
	HookMode string

	// CodexHookMode is the Codex-specific posture. Empty defaults to
	// "enrich" even when HookMode is "deny", preserving Codex's historical
	// advisory-only behavior until an operator explicitly opts into deny.
	CodexHookMode string

	// InstallGlobalInstructions toggles whether `gortex install`
	// merges the rule block into ~/.claude/CLAUDE.md. Only honoured
	// in ModeGlobal; ignored elsewhere. Default true so a fresh
	// install delivers full enforcement; set false by --no-claude-md.
	InstallGlobalInstructions bool

	// InstructionsDir overrides where the generated instruction
	// profiles (internal/profiles) are materialised and where the
	// CLAUDE.md pointer block resolves its @-include. Empty means
	// the machine default (profiles.DefaultDir()); tests set a temp
	// directory for hermeticity.
	InstructionsDir string

	// AnalyzeRepo is true when the caller wants a dynamic CLAUDE.md
	// preamble built from a fresh index. Only Claude Code uses it.
	AnalyzeRepo bool

	// AnalyzedOverview, if non-empty, is a pre-built dynamic
	// CLAUDE.md preamble the Claude Code adapter will prepend to
	// its static block. Indexing is driven by the caller to keep
	// the agents package free of the indexer dependency.
	AnalyzedOverview string

	// SkillsRouting, if non-empty, is the pre-rendered
	// community-routing block each adapter writes into its
	// per-repo instructions surface between CommunitiesStartMarker
	// and CommunitiesEndMarker. Callers set this after running the
	// community generator; adapters treat it as an opaque markdown
	// payload.
	SkillsRouting string

	// GeneratedSkills, if non-empty, lists per-community skill
	// files. The Claude Code adapter materialises these under
	// .claude/skills/generated/<DirName>/SKILL.md. Other adapters
	// rely on SkillsRouting alone.
	GeneratedSkills []GeneratedSkill

	// Stderr receives progress messages. nil means discard.
	Stderr io.Writer
}

// GeneratedSkill is a small, package-local mirror of the skills
// generator's output — kept here so the agents package doesn't
// depend on internal/skills. The caller populates this from
// internal/skills.Generator output.
type GeneratedSkill struct {
	CommunityID string
	Label       string // kebab-case, e.g. "mcp-server"
	DirName     string // e.g. "gortex-mcp-server"
	Content     string // full SKILL.md content
}

// ApplyOpts controls Apply's runtime behaviour. Plan doesn't take
// these — plans are observational.
type ApplyOpts struct {
	// DryRun reports what Apply *would* do without writing.
	// Actions in the returned Result use Would* variants.
	DryRun bool

	// Force overwrites keys we would otherwise preserve during a
	// merge (e.g. a custom autoApprove list the user edited). Does
	// not bypass the "skip if already configured" fast path.
	Force bool

	// ForceDetect makes an adapter render its artifacts even when the
	// target tool isn't detected on this machine. Used by the
	// skill-render drift fence (`gortex agents render`) to exercise
	// every adapter regardless of which agents are installed; never set
	// during a normal `gortex init`.
	ForceDetect bool
}

// ActionKind tags what happened (or would happen) for a single file.
// "would-*" variants are emitted under DryRun; plain "create" /
// "merge" / "skip" are emitted by Apply when it actually wrote.
type ActionKind string

const (
	ActionCreate      ActionKind = "create"
	ActionMerge       ActionKind = "merge"
	ActionSkip        ActionKind = "skip"
	ActionDelete      ActionKind = "delete"
	ActionWouldCreate ActionKind = "would-create"
	ActionWouldMerge  ActionKind = "would-merge"
	ActionWouldDelete ActionKind = "would-delete"
)

// FileAction describes one file write (real or planned). Keys is the
// set of top-level JSON/YAML/TOML keys touched during a merge — used
// by the doctor subcommand to diff intended vs actual state.
type FileAction struct {
	Path   string     `json:"path"`
	Action ActionKind `json:"action"`
	Keys   []string   `json:"keys,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// Plan is the declarative output of Adapter.Plan. Every file the
// adapter *would* touch is listed with a predicted Action. For
// never-overwrite files the prediction is ActionWouldCreate when the
// file is absent and ActionSkip when it already exists.
type Plan struct {
	Files []FileAction `json:"files,omitempty"`
}

// Result is what Apply returned. Detected mirrors what Detect
// returned (captured on the result so callers don't need to
// re-invoke detection). Configured is true when at least one write
// succeeded (or would succeed under DryRun). Warnings carries
// non-fatal problems an adapter chose to continue past — e.g. a single
// profile config that failed to write while the rest succeeded — so the
// summary / --json report can surface them instead of burying them in a
// stderr log line.
type Result struct {
	Name       string       `json:"name"`
	Detected   bool         `json:"detected"`
	Configured bool         `json:"configured"`
	Files      []FileAction `json:"files,omitempty"`
	Warnings   []string     `json:"warnings,omitempty"`
	DocsURL    string       `json:"docs_url,omitempty"`
}
