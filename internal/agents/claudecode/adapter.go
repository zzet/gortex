package claudecode

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/skillpack"
	"github.com/zzet/gortex/internal/profiles"
)

// Name is the stable identifier for this adapter, matching the
// --agents=<name> CLI flag.
const Name = "claude-code"

// DocsURL is the page we point users at when something about our
// Claude Code integration surprises them. Used in the --json report.
const DocsURL = "https://docs.claude.com/en/docs/claude-code/overview"

// Adapter implements agents.Adapter for Claude Code.
type Adapter struct{}

// New returns the Claude Code adapter. Callers register it via
// `agents.Registry.Register(claudecode.New())`.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect always returns true. Claude Code is the "home" agent for
// `gortex init` — a project may not be opened in Claude Code today
// but we always want the integration files on disk so the team's
// next contributor is set up. Other adapters gate on detection
// because their artifacts make no sense without the IDE installed.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	return true, nil
}

// Plan reports the full set of files the adapter would touch for
// the given Env. Mode branches between project (`gortex init`) and
// global (`gortex install`) surfaces; InstallHooks elides hook files
// without affecting anything else.
func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	if env.Mode == agents.ModeGlobal {
		// User-level artifacts — machine-wide. Slash commands and
		// curated Gortex tool-usage skills both belong here: they're
		// codebase-agnostic, so duplicating them into every repo is
		// wasted disk and drift risk.
		p.Files = append(p.Files, agents.FileAction{Path: userClaudeJSONPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"mcpServers"}})
		p.Files = append(p.Files, agents.FileAction{Path: userSettingsPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"permissions"}})
		if env.InstallHooks {
			p.Files = append(p.Files, agents.FileAction{Path: userSettingsLocalPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"hooks"}})
		}
		if env.InstallGlobalInstructions {
			p.Files = append(p.Files, agents.FileAction{Path: userClaudeMdPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"gortex-rules-block"}})
		}
		configDir := userClaudeConfigDir(env.Home)
		if configDir != "" {
			for name := range GlobalSkills {
				p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(configDir, "skills", name, "SKILL.md"), Action: agents.ActionWouldCreate})
			}
			for name := range SlashCommands {
				p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(configDir, "commands", name), Action: agents.ActionWouldCreate})
			}
			for name := range SubAgents {
				p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(configDir, "agents", name), Action: agents.ActionWouldCreate})
			}
		}
		return p, nil
	}

	// Project mode — only genuinely repo-specific artifacts. No
	// tool-usage duplication: that lives at ~/.claude/skills/
	// (installed by `gortex install`). Community routing is canonical
	// in AGENTS.md; CLAUDE.md imports it and retains only --analyze
	// content in its marker-guarded block.
	p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".mcp.json"), Action: agents.ActionWouldCreate, Keys: []string{"mcpServers"}})
	p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".claude", "settings.json"), Action: agents.ActionWouldMerge, Keys: []string{"permissions"}})
	if env.InstallHooks {
		p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".claude", "settings.local.json"), Action: agents.ActionWouldMerge, Keys: []string{"hooks"}})
	}
	if env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, "AGENTS.md"), Action: agents.ActionWouldMerge, Keys: []string{"communities-block"}})
		p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, "CLAUDE.md"), Action: agents.ActionWouldMerge, Keys: []string{"agents-import", "communities-block"}})
	} else if env.AnalyzedOverview != "" {
		p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, "CLAUDE.md"), Action: agents.ActionWouldMerge, Keys: []string{"communities-block"}})
	}
	for _, s := range env.GeneratedSkills {
		p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".claude", "skills", "generated", s.DirName, "SKILL.md"), Action: agents.ActionWouldCreate})
	}
	return p, nil
}

// Apply performs the actual writes. Errors mid-way do not abort the
// whole adapter — we log each failure and continue, matching the
// pre-refactor behaviour where Kiro/Cursor/etc. setup failures only
// emit warnings. Claude Code's .mcp.json, CLAUDE.md, and hook install
// are the exception: they're the core integration and propagate
// failures upward.
func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	w := env.Stderr
	res := &agents.Result{Name: Name, Detected: true, DocsURL: DocsURL}

	if env.Mode == agents.ModeGlobal {
		if err := a.applyGlobal(env, opts, res); err != nil {
			return res, err
		}
		res.Configured = true
		return res, nil
	}

	// 1. Project .mcp.json — create if absent, skip otherwise.
	//
	// If gortex is already registered at user scope (~/.claude.json), a
	// project .mcp.json adds a second registration under the same name.
	// Claude Code keys OAuth tokens per endpoint and flags this as a
	// "conflicting scopes" diagnostic. The user-scope entry already
	// serves this repo machine-wide, so skip the project file unless
	// --force. (A pre-existing .mcp.json is left in place — we never
	// delete it — but we warn so the user can resolve the duplication.)
	mcpPath := filepath.Join(env.Root, ".mcp.json")
	if !opts.Force && env.Home != "" && userScopeGortexRegistered(env.Home) && !pathExists(mcpPath) {
		logWarn(w, "gortex is already registered at user scope (%s); skipping project .mcp.json to avoid a Claude Code \"conflicting scopes\" warning. Re-run with --force to write it anyway (e.g. for teammates without a global install).", userClaudeJSONPath(env.Home))
		res.Files = append(res.Files, agents.FileAction{Path: mcpPath, Action: agents.ActionSkip, Reason: "gortex already registered at user scope"})
	} else {
		if !opts.Force && env.Home != "" && userScopeGortexRegistered(env.Home) && pathExists(mcpPath) {
			logWarn(w, "gortex is registered at both user scope (%s) and project scope (%s); Claude Code may warn about conflicting scopes — keep one with `claude mcp remove gortex -s user` or `-s project`.", userClaudeJSONPath(env.Home), mcpPath)
		}
		mcpAction, err := agents.WriteIfNotExists(w, mcpPath, ProjectMCPJSON, opts)
		if err != nil {
			return res, fmt.Errorf(".mcp.json: %w", err)
		}
		res.Files = append(res.Files, mcpAction)
	}

	// 2. MCP permissions in .claude/settings.json — merge, not create.
	permAction, err := installPermissions(w, filepath.Join(env.Root, ".claude", "settings.json"), opts)
	if err != nil {
		logWarn(w, "could not install permissions: %v", err)
	}
	res.Files = append(res.Files, permAction)

	// 3. Hooks in .claude/settings.local.json — merge with healing.
	if env.InstallHooks {
		hookAction, err := InstallHookWithMode(w, filepath.Join(env.Root, ".claude", "settings.local.json"), env.HookMode, opts)
		if err != nil {
			logWarn(w, "could not install hook: %v", err)
		}
		res.Files = append(res.Files, hookAction)
	} else {
		logf(w, "[gortex init] skipping hook installation (--no-hooks)")
	}

	// 4. Community routing is canonical in AGENTS.md so every agent can
	// share one generated table. Claude Code imports that canonical file
	// from CLAUDE.md; an existing Gortex-managed CLAUDE.md block is
	// migrated away while any --analyze overview remains Claude-specific.
	if env.SkillsRouting != "" {
		agentsMdPath := filepath.Join(env.Root, "AGENTS.md")
		routingAction, err := agents.UpsertMarkedBlock(w, agentsMdPath, env.SkillsRouting,
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			return res, fmt.Errorf("AGENTS.md: %w", err)
		}
		res.Files = append(res.Files, routingAction)
	}
	if env.AnalyzedOverview != "" || env.SkillsRouting != "" {
		claudeMdPath := filepath.Join(env.Root, "CLAUDE.md")
		claudeAction, err := agents.UpsertMarkedBlock(w, claudeMdPath, env.AnalyzedOverview,
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			return res, fmt.Errorf("CLAUDE.md: %w", err)
		}
		res.Files = append(res.Files, claudeAction)

		if env.SkillsRouting != "" {
			importAction, err := ensureProjectAgentsImport(w, claudeMdPath, opts)
			if err != nil {
				return res, fmt.Errorf("CLAUDE.md import: %w", err)
			}
			res.Files = append(res.Files, importAction)
		}
	}

	// 5. Generated community skills — per-community SKILL.md files
	// under .claude/skills/generated/. Claude Code auto-discovers
	// them next to the repo-local CLAUDE.md. Regenerated each init
	// run so they track the current graph.
	for _, s := range env.GeneratedSkills {
		path := filepath.Join(env.Root, ".claude", "skills", "generated", s.DirName, "SKILL.md")
		action, err := agents.WriteOwnedFile(w, path, s.Content, opts)
		if err != nil {
			logWarn(w, "could not write generated skill %s: %v", s.DirName, err)
			continue
		}
		res.Files = append(res.Files, action)
	}

	res.Configured = true
	return res, nil
}

// applyGlobal handles Mode=ModeGlobal writes (entered via `gortex
// install`). Everything here is codebase-agnostic user-level
// machinery: MCP config pointing at `gortex mcp`, user-level
// hooks, curated Gortex tool-usage skills, and Gortex slash
// commands. No per-repo artifacts.
func (a *Adapter) applyGlobal(env agents.Env, opts agents.ApplyOpts, res *agents.Result) error {
	w := env.Stderr
	if env.Home == "" {
		return fmt.Errorf("global mode requires a resolved home directory")
	}

	// 1. ~/.claude.json — MCP stanza pointing at `gortex mcp`.
	mcpPath := userClaudeJSONPath(env.Home)
	action, err := upsertGlobalMCPConfig(w, mcpPath, opts)
	if err != nil {
		return fmt.Errorf("global MCP config: %w", err)
	}
	res.Files = append(res.Files, action)

	// 2. ~/.claude/settings.json — user-level MCP permission allowlist.
	// Mirrors the project-mode call so `mcp__gortex__*` is auto-allowed
	// machine-wide; without this every Gortex tool call shows an
	// approval prompt until the user adds the rule by hand.
	permAction, err := installPermissions(w, userSettingsPath(env.Home), opts)
	if err != nil {
		logWarn(w, "could not install global permissions: %v", err)
	}
	res.Files = append(res.Files, permAction)

	// 3. ~/.claude/settings.local.json — user-level hooks.
	if env.InstallHooks {
		hookAction, err := InstallHookWithMode(w, userSettingsLocalPath(env.Home), env.HookMode, opts)
		if err != nil {
			return fmt.Errorf("global hooks: %w", err)
		}
		res.Files = append(res.Files, hookAction)
	}

	// 4. Instruction profiles + ~/.claude/CLAUDE.md pointer block.
	// The generated profiles live under the gortex home; CLAUDE.md
	// carries only a thin marker-fenced block that @-includes the
	// active profile, so `gortex instructions switch` changes
	// guidance depth without ever rewriting CLAUDE.md. Without the
	// block, the rule only surfaces at deny-time (PreToolUse) which
	// is late: the agent has already wasted a turn on a forbidden
	// tool.
	insDir := agents.InstructionsDir(env)
	if env.InstallGlobalInstructions {
		insAction := agents.FileAction{Path: insDir, Action: agents.ActionMerge, Keys: []string{"instruction-profiles"}}
		if opts.DryRun {
			insAction.Action = agents.ActionWouldMerge
			res.Files = append(res.Files, insAction)
		} else if err := profiles.Generate(insDir); err != nil {
			logWarn(w, "could not generate instruction profiles: %v", err)
		} else {
			logf(w, "[gortex install] wrote instruction profiles to %s", insDir)
			res.Files = append(res.Files, insAction)
		}
		claudeMdPath := userClaudeMdPath(env.Home)
		mdAction, err := agents.UpsertMarkedBlock(w, claudeMdPath, agents.GlobalPointerBody(insDir),
			agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, opts)
		if err != nil {
			logWarn(w, "could not install global CLAUDE.md: %v", err)
		} else {
			logf(w, "[gortex install] wrote rule block to %s", claudeMdPath)
		}
		// UpsertMarkedBlock is shared with the per-repo communities
		// block, so it labels every action with "communities-block".
		// Relabel here so the install report distinguishes the two.
		if mdAction.Keys != nil {
			mdAction.Keys = []string{"gortex-rules-block"}
		}
		res.Files = append(res.Files, mdAction)
	}

	// 3. ~/.claude/skills/gortex-*/SKILL.md — curated tool-usage
	// skills, reconciled against the active instruction profile's
	// subset (nil = all shipped skills). Existing files are never
	// overwritten so user edits survive; out-of-profile skills are
	// removed only when they still match a shipped body.
	skillActions, err := SyncGlobalSkills(w, env.Home, profiles.Active(insDir).Skills, opts)
	if err != nil {
		logWarn(w, "could not install user-level skills: %v", err)
	}
	res.Files = append(res.Files, skillActions...)

	// 4. ~/.claude/commands/gortex-*.md — slash commands, also
	// codebase-agnostic and user-level. Claude Code discovers
	// user-level commands alongside project-level ones.
	cmdActions, err := installGlobalSlashCommands(w, env.Home, opts)
	if err != nil {
		logWarn(w, "could not install user-level slash commands: %v", err)
	}
	res.Files = append(res.Files, cmdActions...)

	// 5. ~/.claude/agents/gortex-*.md — sub-agent definitions. Claude
	// Code auto-routes user prompts to sub-agents based on their
	// frontmatter description, so shipping these gives gortex a
	// delegation surface beyond skills + slash commands. The tool
	// allowlist in each file pins sub-agents to gortex graph tools
	// only — Bash / Grep / Glob are unavailable by construction.
	agentActions, err := installGlobalSubAgents(w, env.Home, opts)
	if err != nil {
		logWarn(w, "could not install user-level sub-agents: %v", err)
	}
	res.Files = append(res.Files, agentActions...)

	return nil
}

// RemoveGlobal undoes applyGlobal: it strips the Gortex footprint
// from the user-level Claude Code config — the MCP server stanza, the
// permission allow-entry, the hook entries, the CLAUDE.md rule block,
// and the curated skills / commands / sub-agents. Merged files keep
// every non-Gortex key (other MCP servers, the user's own
// permissions / hooks, hand-written CLAUDE.md prose); owned files
// (skills / commands / agents) are deleted outright. The config root
// honors the --claude-config-dir override / $CLAUDE_CONFIG_DIR /
// the ~/.claude default. Returns the number of artifacts
// removed-or-cleaned and any per-artifact failures — a partial clean
// still reports rather than aborting. Invoked by `gortex uninstall
// --global`.
func (a *Adapter) RemoveGlobal(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	w := env.Stderr
	if env.Home == "" {
		return 0, []string{"global cleanup requires a resolved home directory"}
	}

	count := func(action agents.FileAction, err error, label string) {
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", label, err))
			return
		}
		if action.Action != agents.ActionSkip {
			removed++
		}
	}

	// 1. ~/.claude.json — drop the "gortex" MCP server stanza.
	mcpPath := userClaudeJSONPath(env.Home)
	mcpAction, err := removeGlobalMCPConfig(w, mcpPath, opts)
	count(mcpAction, err, mcpPath)

	// 2. settings.json — drop the mcp__gortex__* permission entry.
	settingsPath := userSettingsPath(env.Home)
	permAction, err := removeGlobalPermissions(w, settingsPath, opts)
	count(permAction, err, settingsPath)

	// 3. settings.local.json — drop the Gortex hook entries.
	localPath := userSettingsLocalPath(env.Home)
	hookAction, err := removeGlobalHooks(w, localPath, opts)
	count(hookAction, err, localPath)

	// 4. CLAUDE.md — strip the marker-fenced rule block. An empty
	// body makes UpsertMarkedBlock remove the block in place,
	// preserving any surrounding user prose.
	mdPath := userClaudeMdPath(env.Home)
	mdAction, err := agents.UpsertMarkedBlock(w, mdPath, "",
		agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, opts)
	count(mdAction, err, mdPath)

	// 5. skills / commands / agents — delete the owned files.
	ownedRemoved, ownedFailures := removeGlobalOwnedFiles(w, env.Home, opts)
	removed += ownedRemoved
	failures = append(failures, ownedFailures...)

	// 6. Generated instruction profiles — gortex-owned generated
	// files (plus the tiny active-state record), safe to delete.
	insDir := agents.InstructionsDir(env)
	if _, err := os.Stat(insDir); err == nil {
		if opts.DryRun {
			removed++
		} else if err := profiles.Remove(insDir); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", insDir, err))
		} else {
			logf(w, "[gortex] removed instruction profiles at %s", insDir)
			removed++
		}
	}

	return removed, failures
}

// GlobalArtifacts returns the user-level Claude Code paths gortex
// install manages that currently carry a Gortex footprint — the MCP
// config, settings files, and CLAUDE.md when they contain Gortex
// entries, plus every installed skill / command / sub-agent. Sorted
// for stable output. The config root honors the --claude-config-dir
// override / $CLAUDE_CONFIG_DIR. Used by `gortex uninstall --global`
// to preview the blast radius before deleting.
func GlobalArtifacts(home string) []string {
	configDir := userClaudeConfigDir(home)
	var present []string

	// Merged files: list only when they actually carry a Gortex
	// footprint, so the preview (and its count) matches what
	// RemoveGlobal will really touch.
	if fileContains(userClaudeJSONPath(home), `"gortex"`) {
		present = append(present, userClaudeJSONPath(home))
	}
	if fileContains(userSettingsPath(home), "mcp__gortex__") {
		present = append(present, userSettingsPath(home))
	}
	if fileContains(userSettingsLocalPath(home), "gortex") {
		present = append(present, userSettingsLocalPath(home))
	}
	if fileContains(userClaudeMdPath(home), agents.GlobalRulesStartMarker) {
		present = append(present, userClaudeMdPath(home))
	}

	// Owned files: present on disk == installed by us.
	for name := range GlobalSkills {
		if p := filepath.Join(configDir, "skills", name); pathExists(p) {
			present = append(present, p)
		}
	}
	for name := range SlashCommands {
		if p := filepath.Join(configDir, "commands", name); pathExists(p) {
			present = append(present, p)
		}
	}
	for name := range SubAgents {
		if p := filepath.Join(configDir, "agents", name); pathExists(p) {
			present = append(present, p)
		}
	}

	sort.Strings(present)
	return present
}

// removeGlobalMCPConfig drops the gortex MCP server from a
// {"mcpServers": {...}} config, leaving the user's other servers
// intact. A missing file or absent stanza is a no-op skip.
func removeGlobalMCPConfig(w io.Writer, path string, opts agents.ApplyOpts) (agents.FileAction, error) {
	return agents.MergeJSON(w, path, func(root map[string]any, _ bool) (bool, error) {
		return agents.RemoveMCPServer(root, "gortex"), nil
	}, opts)
}

// removeGlobalPermissions drops the mcp__gortex__* entry from
// permissions.allow, pruning the allow list / permissions map when
// removal leaves them empty. User-added allow entries survive.
func removeGlobalPermissions(w io.Writer, settingsPath string, opts agents.ApplyOpts) (agents.FileAction, error) {
	return agents.MergeJSON(w, settingsPath, func(settings map[string]any, _ bool) (bool, error) {
		perms, ok := settings["permissions"].(map[string]any)
		if !ok {
			return false, nil
		}
		allow, ok := perms["allow"].([]any)
		if !ok {
			return false, nil
		}
		kept := make([]any, 0, len(allow))
		removedAny := false
		for _, entry := range allow {
			if s, ok := entry.(string); ok && strings.Contains(s, "mcp__gortex__") {
				removedAny = true
				continue
			}
			kept = append(kept, entry)
		}
		if !removedAny {
			return false, nil
		}
		if len(kept) == 0 {
			delete(perms, "allow")
		} else {
			perms["allow"] = kept
		}
		if len(perms) == 0 {
			delete(settings, "permissions")
		}
		return true, nil
	}, opts)
}

// removeGlobalHooks drops every Gortex-owned hook entry across the
// events the installer writes, pruning the hooks map when removal
// leaves it empty. Hooks owned by other tools survive.
func removeGlobalHooks(w io.Writer, settingsPath string, opts agents.ApplyOpts) (agents.FileAction, error) {
	return agents.MergeJSON(w, settingsPath, func(settings map[string]any, _ bool) (bool, error) {
		hooks, ok := settings["hooks"].(map[string]any)
		if !ok {
			return false, nil
		}
		removed := 0
		for _, event := range []string{"PreToolUse", "PreCompact", "PostToolUse", "Stop", "SessionStart", "UserPromptSubmit", "SubagentStart", "SubagentStop"} {
			removed += removeGortexHookEntries(hooks, event)
		}
		if removed == 0 {
			return false, nil
		}
		if len(hooks) == 0 {
			delete(settings, "hooks")
		}
		return true, nil
	}, opts)
}

// removeGlobalOwnedFiles deletes the user-level skills / commands /
// sub-agents the installer owns. Skills are directories
// ($DIR/skills/<name>/); commands and agents are single files. A
// not-yet-installed artifact is silently skipped; DryRun counts the
// target without touching disk.
func removeGlobalOwnedFiles(w io.Writer, home string, opts agents.ApplyOpts) (removed int, failures []string) {
	configDir := userClaudeConfigDir(home)
	type target struct {
		path string
		dir  bool
	}
	var targets []target
	for name := range GlobalSkills {
		targets = append(targets, target{filepath.Join(configDir, "skills", name), true})
	}
	for name := range SlashCommands {
		targets = append(targets, target{filepath.Join(configDir, "commands", name), false})
	}
	for name := range SubAgents {
		targets = append(targets, target{filepath.Join(configDir, "agents", name), false})
	}
	for _, t := range targets {
		if !pathExists(t.path) {
			continue // not installed — nothing to remove
		}
		if opts.DryRun {
			removed++
			continue
		}
		var err error
		if t.dir {
			err = os.RemoveAll(t.path)
		} else {
			err = os.Remove(t.path)
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", t.path, err))
			continue
		}
		logf(w, "[gortex uninstall] removed %s", t.path)
		removed++
	}
	return removed, failures
}

func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

const projectAgentsImport = "@AGENTS.md"

// ensureProjectAgentsImport adds Claude Code's project-level import for the
// canonical shared instructions without rewriting or duplicating user content.
func ensureProjectAgentsImport(w io.Writer, path string, opts agents.ApplyOpts) (agents.FileAction, error) {
	action := agents.FileAction{Path: path, Keys: []string{"agents-import"}}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return action, err
	}
	if hasProjectAgentsImport(string(existing)) {
		action.Action = agents.ActionSkip
		action.Reason = "unchanged"
		return action, nil
	}

	newline := "\n"
	if strings.Contains(string(existing), "\r\n") {
		newline = "\r\n"
	}
	content := projectAgentsImport + newline
	if len(existing) > 0 {
		content += newline + string(existing)
	}
	if opts.DryRun {
		if os.IsNotExist(err) {
			action.Action = agents.ActionWouldCreate
		} else {
			action.Action = agents.ActionWouldMerge
		}
		return action, nil
	}
	if err := agents.AtomicWriteFile(path, []byte(content), 0o644); err != nil {
		return action, err
	}
	if os.IsNotExist(err) {
		action.Action = agents.ActionCreate
	} else {
		action.Action = agents.ActionMerge
	}
	logf(w, "[gortex init] ensured %s imports AGENTS.md", path)
	return action, nil
}

func hasProjectAgentsImport(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == projectAgentsImport {
			return true
		}
	}
	return false
}

// installPermissions merges an {"permissions": {"allow":
// ["mcp__gortex__*"]}} stanza into settings.json. Preserves any
// user-added entries; short-circuits when a gortex rule is already
// present.
func installPermissions(w io.Writer, settingsPath string, opts agents.ApplyOpts) (agents.FileAction, error) {
	return agents.MergeJSON(w, settingsPath, func(settings map[string]any, _ bool) (bool, error) {
		if _, ok := settings["permissions"]; !ok {
			settings["permissions"] = make(map[string]any)
		}
		perms := settings["permissions"].(map[string]any)
		if _, ok := perms["allow"]; !ok {
			perms["allow"] = []any{}
		}
		allow := perms["allow"].([]any)
		hasGortexRule := false
		hasPreCompactWildcard := false
		for _, entry := range allow {
			s, ok := entry.(string)
			if !ok || !strings.HasPrefix(s, "mcp__gortex__") {
				continue
			}
			hasGortexRule = true
			hasPreCompactWildcard = hasPreCompactWildcard || s == "mcp__gortex__*"
		}
		// Exact existing entries are user policy. Do not widen them. The one
		// legacy wildcard is Gortex's old shipped rule and is safe to migrate.
		if hasGortexRule && !hasPreCompactWildcard {
			return false, nil
		}
		if hasPreCompactWildcard {
			filtered := allow[:0]
			for _, entry := range allow {
				if entry != "mcp__gortex__*" {
					filtered = append(filtered, entry)
				}
			}
			allow = filtered
		}
		seen := make(map[string]bool, len(allow))
		for _, entry := range allow {
			if value, ok := entry.(string); ok {
				seen[value] = true
			}
		}
		for _, tool := range agents.CompactMCPAutoApproveTools() {
			permission := "mcp__gortex__" + tool
			if !seen[permission] {
				allow = append(allow, permission)
			}
		}
		perms["allow"] = allow
		return true, nil
	}, opts)
}

// SyncGlobalSkills reconciles ~/.claude/skills/gortex-* with the
// allowed subset (nil = every shipped skill):
//
//   - allowed skills are installed when missing; an existing file is
//     never overwritten, so user edits survive;
//   - shipped skills OUTSIDE the subset are removed only when their
//     on-disk SKILL.md is still byte-identical to the shipped body —
//     a customised copy is kept (ActionSkip) with a warning, because
//     deleting user work to enforce a profile is never worth it.
//
// Also used by `gortex instructions switch` to re-shape the installed
// skill surface when the active profile changes.
func SyncGlobalSkills(w io.Writer, home string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	skillsDir := filepath.Join(userClaudeConfigDir(home), "skills")
	known := make(map[string][]string, len(GlobalSkills))
	preWorktree := skillpack.PreWorktreeClaudeSkillHashes()
	for name := range GlobalSkills {
		known[name] = append(known[name], v060GlobalSkillHashes[name])
		known[name] = append(known[name], preWorktree[name]...)
	}
	return skillpack.Sync(w, skillpack.SyncSpec{Dir: skillsDir, Rendered: GlobalSkills, KnownHashes: known}, allowed, opts)
}

// installGlobalSlashCommands writes ~/.claude/commands/gortex-*.md
// for each entry in SlashCommands. Skips existing files so users
// keep any local tweaks. Mirrors installGlobalSkills — both are
// user-level, codebase-agnostic artifacts installed by
// `gortex install`.
func installGlobalSlashCommands(w io.Writer, home string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	out := make([]agents.FileAction, 0, len(SlashCommands))
	dir := filepath.Join(userClaudeConfigDir(home), "commands")
	for name, content := range SlashCommands {
		path := filepath.Join(dir, name)
		action, err := writeAgentArtifact(w, path, content, v060SlashCommandHashes[name], opts)
		if err != nil {
			return out, err
		}
		out = append(out, action)
	}
	return out, nil
}

// installGlobalSubAgents writes ~/.claude/agents/gortex-*.md for each
// entry in SubAgents. Skips existing files so user tweaks to the
// frontmatter (description tuning, tool allowlist edits) survive
// re-installs. Mirrors installGlobalSkills / installGlobalSlashCommands.
func installGlobalSubAgents(w io.Writer, home string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	out := make([]agents.FileAction, 0, len(SubAgents))
	dir := filepath.Join(userClaudeConfigDir(home), "agents")
	for name, content := range SubAgents {
		path := filepath.Join(dir, name)
		action, err := writeAgentArtifact(w, path, content, v060SubAgentHashes[name], opts)
		if err != nil {
			return out, err
		}
		out = append(out, action)
	}
	return out, nil
}

// upsertGlobalMCPConfig is the user-level (~/.claude.json) MCP stanza
// installer. Unlike the project-level .mcp.json we never write from
// scratch with a static string here — we always merge so we don't
// clobber a user's other MCP servers or their permissions. If the
// existing file is malformed JSON, it's backed up before we
// overwrite.
func upsertGlobalMCPConfig(w io.Writer, path string, opts agents.ApplyOpts) (agents.FileAction, error) {
	// Prefer the bare "gortex" command when it resolves on PATH to the
	// binary we're running, so this user-scope entry matches the
	// portable project .mcp.json template byte-for-byte. Claude Code
	// keys OAuth tokens per endpoint, so a user-scope stanza that
	// disagrees with a project-scope one trips its "conflicting scopes"
	// diagnostic. Falls back to the absolute path only when gortex is
	// not on PATH (e.g. a Windows install whose dir isn't on PATH).
	entry := map[string]any{
		"command": agents.ResolveGortexCommand(),
		"args":    []string{"mcp"},
		"env":     map[string]any{},
	}

	// Try a direct merge first. MergeJSON handles malformed JSON with a
	// timestamped backup already. UpsertMCPServerWithMigration rewrites
	// a stale Gortex-authored stanza (including the older absolute-path
	// form) in place without clobbering a user's hand-rolled wrapper.
	action, err := agents.MergeJSON(w, path, func(root map[string]any, existed bool) (bool, error) {
		_ = existed
		return agents.UpsertMCPServerWithMigration(root, "gortex", entry, agents.ApplyOpts{Force: opts.Force}), nil
	}, opts)
	if err != nil {
		return agents.FileAction{}, err
	}

	// Historical behaviour: if the file existed but was malformed,
	// rename it to a .bak-<ts> to preserve the original. MergeJSON
	// uses a plain ".bak" name; we mirror the timestamp-suffix
	// convention for global mode because the user is more likely
	// to have edited ~/.claude.json than a project .mcp.json.
	if existing, statErr := os.Stat(path + ".bak"); statErr == nil && !existing.IsDir() {
		if err := os.Rename(path+".bak", fmt.Sprintf("%s.bak-%d", path, time.Now().Unix())); err != nil {
			logWarn(w, "could not timestamp malformed-config backup: %v", err)
		}
	}
	return action, nil
}

// userScopeGortexRegistered reports whether ~/.claude.json already
// registers a "gortex" MCP server at user scope. A project .mcp.json
// written on top of that produces a second registration under the same
// name, which Claude Code flags as a "conflicting scopes" warning
// because it stores OAuth tokens per endpoint. A missing or malformed
// file is treated as "not registered" — we only suppress the project
// write on positive evidence of a user-scope entry.
func userScopeGortexRegistered(home string) bool {
	data, err := os.ReadFile(userClaudeJSONPath(home))
	if err != nil {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return false
	}
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = servers["gortex"]
	return ok
}

// Paths — user-level files.

func userClaudeJSONPath(home string) string {
	if dir := claudeConfigDirOverride(); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

func userSettingsLocalPath(home string) string {
	return filepath.Join(userClaudeConfigDir(home), "settings.local.json")
}

// userSettingsPath is the user-level counterpart to
// `.claude/settings.json` in a project. Permissions live here (not
// in settings.local.json) so they survive when the user wipes the
// "local" overrides file.
func userSettingsPath(home string) string {
	return filepath.Join(userClaudeConfigDir(home), "settings.json")
}

// userClaudeMdPath is the machine-wide CLAUDE.md Claude Code reads on
// every session, regardless of cwd. We merge a marker-fenced rule
// block into it so the agent sees the Gortex rules from turn one.
func userClaudeMdPath(home string) string {
	return filepath.Join(userClaudeConfigDir(home), "CLAUDE.md")
}

// UserClaudeMdPath is the resolved user-level CLAUDE.md path Claude
// Code reads every session, honoring the --claude-config-dir override
// / $CLAUDE_CONFIG_DIR / the ~/.claude default. Exported so the
// installer banner names the real destination instead of a hardcoded
// ~/.claude path.
func UserClaudeMdPath(home string) string { return userClaudeMdPath(home) }

func userClaudeConfigDir(home string) string {
	if dir := claudeConfigDirOverride(); dir != "" {
		return dir
	}
	return filepath.Join(home, ".claude")
}

// configDirOverride, when non-empty, pins the Claude Code config root
// for the rest of the process regardless of $CLAUDE_CONFIG_DIR. It is
// set by `gortex install --claude-config-dir` / `gortex uninstall
// --global --claude-config-dir` so an operator can target a
// non-active profile or a CI sandbox without exporting the env var.
// Precedence: flag override > $CLAUDE_CONFIG_DIR > ~/.claude default.
var configDirOverride string

// SetConfigDirOverride pins the Claude Code config root for the rest
// of this process. An empty string clears the override so the env var
// / default resume.
func SetConfigDirOverride(dir string) { configDirOverride = strings.TrimSpace(dir) }

func claudeConfigDirOverride() string {
	if configDirOverride != "" {
		return configDirOverride
	}
	return strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format+"\n", args...)
}

func logWarn(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "[gortex init] warning: "+format+"\n", args...)
}
