// Package copilotcli implements the Gortex init integration for the
// standalone GitHub Copilot CLI — the `copilot` binary shipped as the
// npm package @github/copilot. It is a different host from the `vscode`
// adapter in this tree: that one wires GitHub Copilot *inside* VS Code
// (.vscode/mcp.json), while this one wires the terminal CLI, whose
// configuration lives under ~/.copilot/ (or $COPILOT_HOME).
//
// Two schema quirks are worth recording, because the two Copilot hosts
// disagree on both and a maintainer reading only the VS Code adapter
// will get them backwards:
//
//  1. The MCP file's top-level key is "mcpServers" — NOT VS Code's
//     "servers". A config written under "servers" parses fine and
//     yields zero registered servers, so the failure is silent.
//  2. Every server entry needs an explicit transport discriminator,
//     "type": "local", next to command/args/env. VS Code infers stdio
//     from the presence of a command, which is why
//     agents.DefaultGortexMCPEntry() emits no "type" — reusing it
//     verbatim here produces an entry the CLI will not launch.
//
// A third quirk lives in discovery rather than schema: the CLI walks
// cwd upward looking for a repo-level .mcp.json / .github/mcp.json, and
// what it finds there takes precedence over ~/.copilot/mcp-config.json.
// Gortex's claudecode adapter owns a repo .mcp.json whose gortex entry
// predates both rules above, so in project mode this adapter reconciles
// that entry in place (reconcileRepoMCPJSON) instead of letting a stale
// file shadow the user-level config we just wrote.
//
// Three surfaces this adapter deliberately does NOT write:
//   - ~/.claude/* — the CLI stopped loading Claude's home directories in
//     v1.0.36, so anything written there is dead weight.
//   - ~/.agents/skills/ — same story, retired in v1.0.66.
//   - .github/prompts/*.prompt.md — prompt files are a VS Code chat
//     feature the CLI has never supported.
//
// The surfaces it does write beyond MCP + instructions live in sibling
// files, each with its own vendor notes: skills.go (Agent Skills),
// subagents.go (custom agents), hooks.go (lifecycle hooks) and
// inspect.go (the read-only view `gortex doctor` consumes).
//
// Docs: https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers
package copilotcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

const (
	Name    = "copilot-cli"
	DocsURL = "https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers"
)

const (
	// copilotHomeEnvVar relocates the CLI's whole config directory.
	copilotHomeEnvVar = "COPILOT_HOME"
	// copilotConfigDirName is the default config home under $HOME.
	copilotConfigDirName = ".copilot"
	// mcpConfigFileName is the CLI's MCP registry. Note the name differs
	// from every other host's mcp.json.
	mcpConfigFileName = "mcp-config.json"
	// instructionsFileName is used at both levels: ~/.copilot/ for the
	// user-wide rules and .github/ for the repo's own.
	instructionsFileName = "copilot-instructions.md"
	// localTransport is the "type" discriminator a stdio server needs.
	localTransport = "local"
	// serverName is the key our entry lives under in mcpServers.
	serverName = "gortex"
)

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// WritesSkillFiles reports that this adapter installs the generated
// community skills as files under .copilot/skills.
func (a *Adapter) WritesSkillFiles() bool { return true }

// lookCopilotBinary is a seam so the "no Copilot installed" detection
// test stays hermetic on a developer machine that happens to have the
// CLI on PATH.
var lookCopilotBinary = func() (string, error) { return exec.LookPath("copilot") }

// WritesCommunitiesRouting reports that this adapter merges the
// communities routing block into its instruction file (project mode).
func (a *Adapter) WritesCommunitiesRouting() bool { return true }

// CommunitiesRoutingPath reports the instruction file that carries the
// communities block in project mode.
func (a *Adapter) CommunitiesRoutingPath(env agents.Env) string {
	return repoInstructionsPath(env.Root)
}


// Detect looks for the `copilot` binary or an existing config home.
// Deliberately never keys off .vscode — Copilot-in-VS-Code is the
// `vscode` adapter's host, and detecting on it here would configure the
// CLI for users who have never installed it.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := lookCopilotBinary(); err == nil && p != "" {
		return true, nil
	}
	home := copilotConfigHome(env)
	if home == "" {
		return false, nil
	}
	if _, err := os.Stat(home); err == nil {
		return true, nil
	}
	return false, nil
}

// copilotConfigHome resolves the directory the CLI reads its config
// from. $COPILOT_HOME wins — but only when Env.Home is the machine's
// real home. A sandbox Env (adapter tests, the `gortex agents render`
// drift fence) has to stay inside its temp root; an exported
// COPILOT_HOME in the developer's shell would otherwise redirect those
// writes onto the real config and make the render golden
// machine-dependent.
func copilotConfigHome(env agents.Env) string {
	if override := strings.TrimSpace(os.Getenv(copilotHomeEnvVar)); override != "" && isRealUserHome(env.Home) {
		return override
	}
	if env.Home == "" {
		return ""
	}
	return filepath.Join(env.Home, copilotConfigDirName)
}

func isRealUserHome(home string) bool {
	if home == "" {
		return false
	}
	real, err := os.UserHomeDir()
	if err != nil || real == "" {
		return false
	}
	return filepath.Clean(real) == filepath.Clean(home)
}

// MCPConfigPath is where the CLI's MCP registry lives for this Env.
func MCPConfigPath(env agents.Env) string {
	home := copilotConfigHome(env)
	if home == "" {
		return ""
	}
	return filepath.Join(home, mcpConfigFileName)
}

// GlobalInstructionsPath is the CLI's user-level instructions file. The
// CLI merges it into every session ahead of the repo's own
// .github/copilot-instructions.md, which makes it the Copilot analogue
// of ~/.claude/CLAUDE.md and ~/.codex/AGENTS.md.
func GlobalInstructionsPath(env agents.Env) string {
	home := copilotConfigHome(env)
	if home == "" {
		return ""
	}
	return filepath.Join(home, instructionsFileName)
}

// repoInstructionsPath is the per-repo instructions file. Shared with
// the `vscode` adapter on purpose: both hosts read the same file, so
// both adapters upsert the same marker-fenced block into it and a repo
// running both converges on one block rather than two.
func repoInstructionsPath(root string) string {
	return filepath.Join(root, ".github", instructionsFileName)
}

// repoMCPJSONPath is claudecode's file, not ours. We only ever
// reconcile a Gortex-authored entry already inside it — see
// reconcileRepoMCPJSON.
func repoMCPJSONPath(root string) string {
	return filepath.Join(root, ".mcp.json")
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	if path := MCPConfigPath(env); path != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path: path, Action: agents.ActionWouldMerge, Keys: []string{"mcpServers"},
		})
	}
	if env.Mode == agents.ModeGlobal && env.InstallGlobalInstructions {
		if path := GlobalInstructionsPath(env); path != "" {
			p.Files = append(p.Files, agents.FileAction{
				Path: path, Action: agents.ActionWouldMerge, Keys: []string{"gortex-rules-block"},
			})
		}
	}
	if env.Mode == agents.ModeGlobal {
		// Curated skills + sub-agents are codebase-agnostic, so they are
		// user-level only; the hook config is machine-local posture.
		for _, id := range CuratedSkillNames() {
			if path := curatedSkillPath(env, id); path != "" {
				p.Files = append(p.Files, agents.FileAction{
					Path: path, Action: agents.ActionWouldCreate, Keys: []string{"skill"},
				})
			}
		}
		for _, id := range SubAgentNames() {
			if path := subAgentPath(env, id); path != "" {
				p.Files = append(p.Files, agents.FileAction{
					Path: path, Action: agents.ActionWouldCreate, Keys: []string{"sub-agent"},
				})
			}
		}
		if env.InstallHooks {
			if path := HookConfigPath(env); path != "" {
				p.Files = append(p.Files, agents.FileAction{
					Path: path, Action: agents.ActionWouldMerge, Keys: []string{"hooks"},
				})
			}
		}
	}
	if env.Mode != agents.ModeGlobal {
		if env.SkillsRouting != "" {
			p.Files = append(p.Files, agents.FileAction{
				Path: repoInstructionsPath(env.Root), Action: agents.ActionWouldMerge,
				Keys: []string{"communities-block"},
			})
		}
		// Generated community skills are per-repo by construction.
		for _, s := range env.GeneratedSkills {
			if !skillpack.ValidID(s.DirName) {
				continue
			}
			p.Files = append(p.Files, agents.FileAction{
				Path: generatedSkillPath(env.Root, s.DirName), Action: agents.ActionWouldCreate,
				Keys: []string{"skill"},
			})
		}
		// Only planned when the file is already there with an entry we
		// authored: this adapter never creates .mcp.json.
		if repoMCPJSONHasGortexEntry(repoMCPJSONPath(env.Root)) {
			p.Files = append(p.Files, agents.FileAction{
				Path: repoMCPJSONPath(env.Root), Action: agents.ActionWouldMerge,
				Keys: []string{"mcpServers"},
			})
		}
	}
	return p, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip GitHub Copilot CLI setup (copilot not detected)")
		return res, nil
	}
	mcpPath := MCPConfigPath(env)
	if mcpPath == "" {
		return res, fmt.Errorf("copilot-cli: requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up GitHub Copilot CLI integration...")

	action, err := agents.MergeJSON(env.Stderr, mcpPath, func(root map[string]any, _ bool) (bool, error) {
		return upsertCopilotMCPServer(root, env.Stderr, mcpPath, opts), nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)

	// User-level instructions → ~/.copilot/copilot-instructions.md. The
	// CLI reads plain markdown and has no @-include mechanism, so the
	// active profile body is inlined (Claude Code gets a pointer block
	// instead) and refreshed in place on every install.
	if env.Mode == agents.ModeGlobal && env.InstallGlobalInstructions {
		insAction, err := upsertGlobalInstructions(env, opts)
		if err != nil {
			return res, fmt.Errorf("copilot-cli global instructions: %w", err)
		}
		res.Files = append(res.Files, insAction)
	}

	if env.Mode == agents.ModeGlobal {
		// Curated Agent Skills → ~/.copilot/skills/. User-level because
		// the pack is codebase-agnostic; see skills.go for why every
		// other discovery root the CLI once honoured is now dead.
		res.Files = append(res.Files, installCuratedSkills(env, opts)...)

		// Custom agents → ~/.copilot/agents/. A home agent shadows a repo
		// agent of the same name, so this is the only level worth writing.
		res.Files = append(res.Files, installSubAgents(env, opts)...)

		// Lifecycle hooks → ~/.copilot/hooks/gortex.json. Never
		// .github/hooks/: that file is committed and would fire on every
		// teammate's clone.
		if env.InstallHooks {
			hookAction, err := installHooks(env, opts)
			if err != nil {
				return res, fmt.Errorf("copilot-cli hooks: %w", err)
			}
			res.Files = append(res.Files, hookAction)
		}
	}

	if env.Mode != agents.ModeGlobal {
		if env.SkillsRouting != "" {
			routingAction, err := agents.UpsertMarkedBlock(env.Stderr, repoInstructionsPath(env.Root), env.SkillsRouting,
				agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
			if err != nil {
				return res, err
			}
			res.Files = append(res.Files, routingAction)
		}
		// Generated community skills → <root>/.github/skills/, flat.
		res.Files = append(res.Files, installGeneratedSkills(env, opts)...)
		repoAction, wrote, err := reconcileRepoMCPJSON(env, opts)
		if err != nil {
			return res, err
		}
		if wrote {
			res.Files = append(res.Files, repoAction)
		}
	}

	res.Configured = true
	return res, nil
}

// upsertGlobalInstructions writes the machine-wide Gortex rule block
// into the CLI's user-level instructions file. Without it a Copilot CLI
// session carries no standing rule: the MCP server's `instructions`
// field is not guaranteed to reach the model, so most turns arrive with
// nothing and the model falls back to shell reads and greps.
func upsertGlobalInstructions(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, error) {
	path := GlobalInstructionsPath(env)
	action, err := agents.UpsertMarkedBlock(nil, path, agents.GlobalInlineBody(agents.InstructionsDir(env)),
		agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, opts)
	if err != nil {
		return agents.FileAction{}, err
	}
	// UpsertMarkedBlock is shared with the per-repo communities block, so
	// it labels every action with "communities-block". Relabel here so the
	// install report distinguishes the two.
	if action.Keys != nil {
		action.Keys = []string{"gortex-rules-block"}
	}
	if !opts.DryRun && action.Action != agents.ActionSkip {
		internalutil.Logf(env.Stderr, "[gortex install] wrote rule block to %s", path)
	}
	return action, nil
}

// gortexMCPEntry is the shared {command, args, env} stanza plus the
// transport discriminator the CLI requires. Built from
// agents.DefaultGortexMCPEntry so the launch command and args stay in
// lockstep with every other adapter — only "type" is Copilot-specific.
func gortexMCPEntry() map[string]any {
	entry := agents.DefaultGortexMCPEntry()
	entry["type"] = localTransport
	return entry
}

// upsertCopilotMCPServer registers Gortex under "mcpServers" in the
// CLI's own config. An entry we did not author is left exactly as it is
// (the user owns it); one we did author is upgraded in place, which is
// how a config written before the "type" discriminator was known heals
// itself on the next run.
func upsertCopilotMCPServer(root map[string]any, w io.Writer, path string, opts agents.ApplyOpts) bool {
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		if _, exists := root["mcpServers"]; exists {
			// Present but not an object: a shape we don't understand and
			// must not overwrite.
			return false
		}
		servers = make(map[string]any)
	}
	existing, exists := servers[serverName]
	if !exists || opts.Force {
		servers[serverName] = gortexMCPEntry()
		root["mcpServers"] = servers
		return true
	}
	if !agents.IsGortexAuthoredMCPEntry(existing) {
		return false
	}
	entry, ok := existing.(map[string]any)
	if !ok {
		return false
	}
	if !reconcileCopilotEntry(entry, w, path) {
		return false
	}
	servers[serverName] = entry
	root["mcpServers"] = servers
	return true
}

// reconcileRepoMCPJSON repairs a Gortex-authored entry in the repo's
// .mcp.json so the file the CLI finds first does not shadow the config
// we just wrote with one it cannot use. The claudecode adapter owns
// that file and writes an entry with no "type"; entries written before
// the worker default was dropped also carry a shell-style
// "${GORTEX_WORKERS:-8}" env value the CLI never expands.
//
// The file is only ever edited, never created: an absent .mcp.json is
// claudecode's to author, and every non-Gortex server entry in it is
// somebody else's.
func reconcileRepoMCPJSON(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, bool, error) {
	path := repoMCPJSONPath(env.Root)
	if !repoMCPJSONHasGortexEntry(path) {
		return agents.FileAction{}, false, nil
	}
	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		entry, ok := gortexEntryOf(root)
		if !ok {
			return false, nil
		}
		return reconcileCopilotEntry(entry, env.Stderr, path), nil
	}, opts)
	if err != nil {
		return agents.FileAction{}, false, err
	}
	return action, true, nil
}

// repoMCPJSONHasGortexEntry reports whether path exists and carries a
// Gortex-authored server entry. Pure (read-only), so Plan can call it.
func repoMCPJSONHasGortexEntry(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return false
	}
	_, ok := gortexEntryOf(root)
	return ok
}

func gortexEntryOf(root map[string]any) (map[string]any, bool) {
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		return nil, false
	}
	existing, exists := servers[serverName]
	if !exists || !agents.IsGortexAuthoredMCPEntry(existing) {
		return nil, false
	}
	entry, ok := existing.(map[string]any)
	return entry, ok
}

// reconcileCopilotEntry brings one Gortex-authored server entry up to
// what the CLI actually requires: an explicit "local" transport, and
// environment values it can use verbatim. Reports whether it changed
// anything, so callers can stay idempotent.
func reconcileCopilotEntry(entry map[string]any, w io.Writer, path string) bool {
	changed := false
	if t, _ := entry["type"].(string); t != localTransport {
		entry["type"] = localTransport
		changed = true
		internalutil.Logf(w, "[gortex init] %s: set the gortex server type to %q (Copilot CLI has no inferred transport)", path, localTransport)
	}
	envMap, ok := entry["env"].(map[string]any)
	if !ok {
		return changed
	}
	for _, key := range sortedKeys(envMap) {
		value, ok := envMap[key].(string)
		if !ok || !strings.Contains(value, "${") {
			continue
		}
		literal, ok := shellDefault(value)
		if !ok {
			internalutil.Warnf(w, "%s: gortex env %s=%q uses shell syntax Copilot CLI does not expand; replace it with a literal value", path, key, value)
			continue
		}
		envMap[key] = literal
		changed = true
		internalutil.Logf(w, "[gortex init] %s: rewrote gortex env %s to the literal %q (Copilot CLI does not expand shell defaults)", path, key, literal)
	}
	return changed
}

// shellDefault extracts D from a "${NAME:-D}" parameter expansion — the
// only shell form Gortex itself writes. Anything else returns false so
// the caller warns instead of guessing at a value.
func shellDefault(value string) (string, bool) {
	if !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return "", false
	}
	inner := value[2 : len(value)-1]
	sep := strings.Index(inner, ":-")
	if sep < 0 {
		return "", false
	}
	name, fallback := inner[:sep], inner[sep+2:]
	if name == "" || fallback == "" {
		return "", false
	}
	if strings.ContainsAny(name, "${}") || strings.ContainsAny(fallback, "${}") {
		return "", false
	}
	return fallback, true
}

// sortedKeys keeps log lines deterministic across runs.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
