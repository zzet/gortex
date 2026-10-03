// Package codex implements the Gortex init integration for the
// OpenAI Codex CLI. Codex stores MCP server definitions in a TOML
// file — ~/.codex/config.toml for the default scope — under the
// [mcp_servers.<name>] table:
//
//	[mcp_servers.gortex]
//	command = "gortex"
//	args = ["mcp", "--index", ".", "--watch"]
//
// Docs: https://github.com/openai/codex/blob/main/docs/config.md
package codex

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
	"github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/version"
)

const (
	Name    = "codex"
	DocsURL = "https://developers.openai.com/codex/mcp"
)

const (
	codexGortexToolNamespace            = "mcp__gortex"
	codexGortexNonPrefixedToolNamespace = "gortex"
	codexMCPStartupTimeoutSeconds       = 90
	codexDirectToolNamespacesMinMinor   = 142
)

const codexSessionStartMatcher = "startup|resume|clear|compact"

// codexHookTrustNotice is surfaced whenever this run wrote or changed a Codex
// lifecycle hook. Codex records trust against each non-managed hook's current
// hash and skips new or changed hooks until the user reviews them in `/hooks`,
// so a freshly installed hook set is inert — and inert silently: the config
// file looks identical whether the hooks are trusted or skipped. Since
// SessionStart is what puts the Gortex rule in front of a Codex session at
// all, an untrusted hook set reads to the user as "Gortex configured, Gortex
// ignored". Nothing on disk says so, which is why the installer has to.
const codexHookTrustNotice = "Codex skips new or changed hooks until they are trusted — run `/hooks` inside Codex, review the gortex entries, and trust them"

// v060CodexSessionStart* fingerprints the static hook shipped by gortex
// v0.60.0 so an upgrade replaces it instead of installing a duplicate. The
// concrete retirement gate is documented in docs/versioning.md.
const (
	v060CodexSessionStartMessage        = "IMPORTANT: Prefer Gortex MCP tools (search_symbols, get_callers, get_file_summary, edit_file) over Read/Grep/Glob/Edit."
	v060CodexSessionStartCommand        = "printf '%s\\n' '" + v060CodexSessionStartMessage + "'"
	v060CodexSessionStartWindowsCommand = "powershell -NoProfile -Command \"Write-Output '" + v060CodexSessionStartMessage + "'\""
	// Codex matchers are regular expressions. A match-all PreToolUse hook is
	// required because terminal localization covers every local tool routed
	// through Codex's hook path; hosted and specialized opt-out tools remain
	// outside the host's hook boundary. Without a marker the handler is a
	// strict local no-op.
	codexPreToolUseMatcher = ".*"
	// Retained as migration fingerprints in tests: upsertCodexHookSet removes
	// both split predecessors by managed command identity before installing the
	// singleton match-all hook.
	codexLegacyBashPreToolUseMatcher          = "^Bash$"
	codexLegacyMCPNavigationPreToolUseMatcher = "^(mcp__gortex__|gortex__)(explore|search|read|relations|trace|analyze)$"
	codexPostToolUseMatcher                   = "^(Bash|apply_patch|(mcp__gortex__|gortex__)(explore|search|read|relations|trace|analyze))$"
	codexHookTimeoutSeconds                   = 5
	codexHookModeEnvVar                       = "GORTEX_CODEX_HOOK_MODE"
	// Codex merges its home instructions file into every session ahead of
	// the repo's own AGENTS.md, preferring the override name when present.
	codexGlobalInstructionsFile         = "AGENTS.md"
	codexGlobalInstructionsOverrideFile = "AGENTS.override.md"
)

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// WritesSkillFiles reports that this adapter installs the generated
// community skills as files (~/.codex/skills).
func (a *Adapter) WritesSkillFiles() bool { return true }

// WritesCommunitiesRouting reports that this adapter merges the
// communities routing block into its instruction file (project mode).
func (a *Adapter) WritesCommunitiesRouting() bool { return true }

// CommunitiesRoutingPath reports the instruction file that carries the
// communities block in project mode.
func (a *Adapter) CommunitiesRoutingPath(env agents.Env) string {
	return filepath.Join(env.Root, "AGENTS.md")
}

// Detect checks for the codex CLI on PATH or ~/.codex/.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("codex"); err == nil && p != "" {
		return true, nil
	}
	if env.Home == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".codex")); err == nil {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	if env.Home != "" {
		keys := []string{"mcp_servers", "features.code_mode"}
		if env.InstallHooks {
			keys = append(keys, "hooks")
		}
		p.Files = append(p.Files, agents.FileAction{
			Path:   filepath.Join(env.Home, ".codex", "config.toml"),
			Action: agents.ActionWouldMerge,
			Keys:   keys,
		})
	}
	if env.Mode == agents.ModeGlobal && env.InstallGlobalInstructions && env.Home != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path: GlobalInstructionsPath(env.Home), Action: agents.ActionWouldMerge,
			Keys: []string{"gortex-rules-block"},
		})
	}
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path: filepath.Join(env.Root, "AGENTS.md"), Action: agents.ActionWouldMerge,
			Keys: []string{"communities-block"},
		})
	}
	skillFiles, err := planSkills(env)
	if err != nil {
		return nil, err
	}
	p.Files = append(p.Files, skillFiles...)
	p.Files = append(p.Files, planSubAgents(env)...)
	return p, nil
}

// GlobalInstructionsPath is Codex's user-level instructions file. Codex
// merges it into every session ahead of the repo's own AGENTS.md, which
// makes it the Codex analogue of ~/.claude/CLAUDE.md.
func GlobalInstructionsPath(home string) string {
	return filepath.Join(home, ".codex", codexGlobalInstructionsFile)
}

// upsertGlobalInstructions writes the machine-wide Gortex rule block into
// ~/.codex/AGENTS.md. Without it a Codex session carries no standing rule:
// the MCP server's `instructions` field is not guaranteed to reach the
// model, and the lifecycle hooks only re-surface guidance when the prompt
// probe returns graph hits — so most turns arrive with nothing and the
// model falls back to shell reads and greps. Claude Code gets a thin
// @-include pointer at the active profile; Codex cannot resolve one, so
// the profile body is inlined and refreshed in place on every install and
// every `gortex instructions switch`.
func upsertGlobalInstructions(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, error) {
	path := GlobalInstructionsPath(env.Home)
	// Codex prefers AGENTS.override.md in its home when that file exists,
	// and never reads AGENTS.md alongside it. Write ours either way — the
	// override is the user's file to own — but say so, otherwise the block
	// lands somewhere Codex silently ignores.
	if _, err := os.Stat(filepath.Join(env.Home, ".codex", codexGlobalInstructionsOverrideFile)); err == nil {
		internalutil.Warnf(env.Stderr, "Codex reads %s instead of %s; copy the Gortex block there or delete the override",
			codexGlobalInstructionsOverrideFile, codexGlobalInstructionsFile)
	}
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

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip Codex setup (codex not detected)")
		return res, nil
	}
	if env.Home == "" {
		return res, fmt.Errorf("codex: requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up OpenAI Codex CLI integration...")

	path := filepath.Join(env.Home, ".codex", "config.toml")
	hooksChanged := false
	action, err := agents.MergeTOML(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		changed := upsertCodexMCPServer(root, opts)
		if supported, detectedVersion := codexSupportsDirectToolNamespaces(); supported || codexHasDirectToolNamespaces(root) {
			if upsertCodexDirectToolNamespaces(root) {
				changed = true
			}
		} else {
			internalutil.Warnf(env.Stderr, "Codex %s does not support direct MCP namespaces; upgrade to Codex 0.%d+ to keep Gortex tools eager", detectedVersion, codexDirectToolNamespacesMinMinor)
		}

		if env.InstallHooks {
			if upsertCodexHooks(root, env, opts) {
				changed = true
				hooksChanged = true
			}
		}
		return changed, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	if hooksChanged {
		res.Warnings = append(res.Warnings, codexHookTrustNotice)
	}
	if env.InstallHooks {
		if notice := codexDuplicateHookFileNotice(path); notice != "" {
			res.Warnings = append(res.Warnings, notice)
		}
	}

	// User-level instructions → ~/.codex/AGENTS.md. This is the surface
	// that makes Codex reach for the graph tools on every turn; see
	// upsertGlobalInstructions for why the hooks alone do not cover it.
	if env.Mode == agents.ModeGlobal && env.InstallGlobalInstructions {
		insAction, err := upsertGlobalInstructions(env, opts)
		if err != nil {
			return res, fmt.Errorf("codex global instructions: %w", err)
		}
		res.Files = append(res.Files, insAction)
	}

	// Repo-local community routing → AGENTS.md (also read by
	// OpenCode; both adapters upsert the same marker-guarded block,
	// so repeat runs converge). Skipped in global mode (AGENTS.md
	// is per-repo) and when no communities were generated.
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		agentsMdPath := filepath.Join(env.Root, "AGENTS.md")
		routingAction, err := agents.UpsertMarkedBlock(env.Stderr, agentsMdPath, env.SkillsRouting,
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, routingAction)
	}

	// Skills. ModeGlobal installs the curated playbook pack under
	// $HOME/.agents/skills; ModeProject materialises the generated
	// community skills under <root>/.agents/skills. skills.go documents
	// why neither ever goes near ~/.codex.
	skillActions, err := applySkills(env, opts)
	if err != nil {
		return res, fmt.Errorf("codex skills: %w", err)
	}
	res.Files = append(res.Files, skillActions...)

	// Sub-agents → ~/.codex/agents/*.toml. ModeGlobal only: the
	// project-scoped agents directory is only scanned for a TRUSTED
	// project, so writing there is a gamble on state the installer cannot
	// see — and it would put Gortex files in the user's working tree.
	// subagents.go documents the rest of the vendor surface.
	res.Files = append(res.Files, installSubAgents(env, opts)...)

	res.Configured = true
	return res, nil
}

// codexDuplicateHookFileNotice reports that the hooks.json beside configPath
// already declares Gortex hooks, which Codex merges with the inline [hooks]
// tables written into configPath — running each event declared in both once
// per declaration.
//
// Keyed off configPath rather than the home directory because Codex merges per
// layer: the repo-local pair merges with each other, not with the user's.
//
// The installer still writes its own representation: that is what it was asked
// to do, and the hooks.json entries are the user's, not ours to edit. Saying so
// is the part that was missing, because the only other signal is a startup
// warning inside Codex that nobody attributes to `gortex install`.
func codexDuplicateHookFileNotice(configPath string) string {
	if configPath == "" {
		return ""
	}
	hooksJSON := filepath.Join(filepath.Dir(configPath), "hooks.json")
	src, ok := inspectJSONHookFile(hooksJSON)
	if !ok || src.Total == 0 {
		return ""
	}
	return fmt.Sprintf(
		"%s already declares %d Gortex hook(s); Codex merges it with the [hooks] tables in %s, so any event declared in both runs twice — remove those events from %s",
		hooksJSON, src.Total, filepath.Base(configPath), hooksJSON)
}

func codexHasDirectToolNamespaces(root map[string]any) bool {
	features, ok := root["features"].(map[string]any)
	if !ok {
		return false
	}
	codeMode, ok := features["code_mode"].(map[string]any)
	if !ok {
		return false
	}
	_, exists := codeMode["direct_only_tool_namespaces"]
	return exists
}

// upsertCodexMCPServer registers Gortex and gives its first-start daemon path
// enough time to publish the initial snapshot: Codex's default MCP startup
// timeout can expire before that path's 60-second wait. Existing
// Gortex-authored launch, environment, approval, and tool-timeout settings are
// preserved; only the startup timeout is managed, plus the one-time removal of
// the `required` flag earlier releases wrote (see pruneManagedCodexRequired).
//
// The command is pinned to an absolute path rather than the bare name, because
// Codex launches this process itself and its PATH is not the PATH `gortex init`
// ran under — see ResolveGortexLaunchBinary.
func upsertCodexMCPServer(root map[string]any, opts agents.ApplyOpts) bool {
	servers, ok := root["mcp_servers"].(map[string]any)
	if !ok {
		servers = make(map[string]any)
	}
	desired := map[string]any{
		"command":             agents.ResolveGortexLaunchBinary(),
		"args":                []string{"mcp"},
		"startup_timeout_sec": codexMCPStartupTimeoutSeconds,
	}
	existing, exists := servers["gortex"]
	if !exists || opts.Force {
		servers["gortex"] = desired
		root["mcp_servers"] = servers
		return true
	}
	if !agents.IsGortexAuthoredMCPEntry(existing) {
		return false
	}
	entry, ok := existing.(map[string]any)
	if !ok {
		return false
	}
	changed := migrateCodexFacadeToolApprovals(entry)
	if pruneManagedCodexRequired(entry) {
		changed = true
	}
	if pruneManagedCodexIndexWorkers(entry) {
		changed = true
	}
	if pinCodexBareGortexCommand(entry) {
		changed = true
	}
	if !codexStartupTimeoutAtLeast(entry["startup_timeout_sec"], codexMCPStartupTimeoutSeconds) {
		entry["startup_timeout_sec"] = codexMCPStartupTimeoutSeconds
		changed = true
	}
	if changed {
		servers["gortex"] = entry
		root["mcp_servers"] = servers
	}
	return changed
}

// pruneManagedCodexRequired removes the `required = true` flag that gortex
// v0.61.0 through v0.63.x wrote into its own Codex MCP entry, and reports
// whether it removed anything.
//
// Codex treats a required MCP server as a hard precondition for the session:
// if the server does not complete the initialize handshake, Codex aborts with
// "Failed to initialize session: required MCP servers failed to initialize"
// and the whole CLI does not start. The intent was that a broken integration
// should fail loudly rather than leave the agent with graph-tool instructions
// it cannot follow, but the flag makes ordinary Gortex states fatal to the
// user's editor:
//
//   - the daemon is not running, so `gortex mcp` fails closed and exits
//     without answering initialize — the state after `gortex daemon stop`,
//     under GORTEX_AUTOSTART=0, and during the spawn-failure cooldown;
//   - the binary is not on the PATH Codex was launched with;
//   - the handshake outruns startup_timeout_sec on a cold daemon start.
//
// None of those are Codex's to recover from, and a coding assistant must not
// be able to take its host down. Without the flag the same failures degrade to
// a Codex session with no Gortex tools, which `gortex doctor` reports.
//
// Only `true` is pruned. A user who has written `required = false` keeps it:
// that value is already the safe posture, and rewriting it would repeat the
// mistake this function exists to undo. See gortexhq/gortex#607.
func pruneManagedCodexRequired(entry map[string]any) bool {
	if required, ok := entry["required"].(bool); !ok || !required {
		return false
	}
	delete(entry, "required")
	return true
}

// pruneManagedCodexIndexWorkers removes the GORTEX_INDEX_WORKERS = "8" env
// entry earlier releases wrote into their own Codex MCP entry, dropping the
// env table when nothing else is left in it, and reports whether it removed
// anything.
//
// The value was inert while per-repository configs ignored the variable.
// Once they honour it, a daemon autostarted from Codex inherits it and
// parses with 8 workers instead of runtime.NumCPU(), which is a cap the user
// never chose. Only the exact value Gortex wrote is pruned; any other value
// is the user's and stays.
func pruneManagedCodexIndexWorkers(entry map[string]any) bool {
	env, ok := entry["env"].(map[string]any)
	if !ok {
		return false
	}
	if v, _ := env["GORTEX_INDEX_WORKERS"].(string); v != "8" {
		return false
	}
	delete(env, "GORTEX_INDEX_WORKERS")
	if len(env) == 0 {
		delete(entry, "env")
	}
	return true
}

// pinCodexBareGortexCommand upgrades the bare `command = "gortex"` that
// earlier releases wrote to the absolute path of the installed binary, and
// reports whether it rewrote anything.
//
// The bare name only resolves if the process that launches Codex inherited the
// PATH `gortex init` ran under. A GUI-launched Codex does not, so the server
// silently never starts and the session has no Gortex tools. Pinning is the
// same choice the hook writer in this file already makes.
//
// Only the bare name is rewritten. An absolute path is left alone whether a
// user pinned a specific build or an earlier run pinned it: re-pinning on every
// install would overwrite a deliberate choice to serve one that is almost
// always identical.
func pinCodexBareGortexCommand(entry map[string]any) bool {
	cmd, _ := entry["command"].(string)
	if cmd != "gortex" && cmd != "gortex.exe" {
		// A path (ours or the user's), or a wrapper we do not own.
		return false
	}
	pinned := agents.ResolveGortexLaunchBinary()
	if pinned == cmd {
		// Nothing installed that we can name more precisely.
		return false
	}
	entry["command"] = pinned
	return true
}

// migrateCodexFacadeToolApprovals upgrades per-tool approval entries from the
// legacy one-tool-per-operation surface to the compact public facade. Entries
// for current facade tools, unknown extension tools, and user-defined fields
// are preserved. Several legacy tools can collapse into one facade tool; when
// their approval modes disagree, prompt is the conservative merged posture.
func migrateCodexFacadeToolApprovals(entry map[string]any) bool {
	tools, ok := entry["tools"].(map[string]any)
	if !ok || len(tools) == 0 {
		return false
	}

	legacyNames := make([]string, 0, len(tools))
	for name := range tools {
		facade, _, recognized := mcp.PublicOperationForLegacy(name)
		if recognized && facade != name {
			legacyNames = append(legacyNames, name)
		}
	}
	if len(legacyNames) == 0 {
		return false
	}
	slices.Sort(legacyNames)

	changed := false
	for _, legacyName := range legacyNames {
		facade, _, _ := mcp.PublicOperationForLegacy(legacyName)
		legacyValue := tools[legacyName]
		legacyTable, validTable := legacyValue.(map[string]any)
		if !validTable {
			// Do not delete malformed or future config shapes that we cannot
			// migrate without losing user intent.
			continue
		}

		if currentValue, exists := tools[facade]; !exists {
			tools[facade] = cloneCodexToolApproval(legacyTable)
		} else if currentTable, ok := currentValue.(map[string]any); ok {
			mergeCodexToolApproval(currentTable, legacyTable)
		} else {
			// A non-table public entry is not a documented Codex shape. Keep it
			// and the legacy entry rather than guessing which one the user owns.
			continue
		}
		delete(tools, legacyName)
		changed = true
	}
	return changed
}

func cloneCodexToolApproval(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func mergeCodexToolApproval(current, incoming map[string]any) {
	for key, value := range incoming {
		if key == "approval_mode" {
			continue
		}
		if _, exists := current[key]; !exists {
			current[key] = value
		}
	}

	incomingMode, incomingHasMode := incoming["approval_mode"]
	currentMode, currentHasMode := current["approval_mode"]
	switch {
	case !currentHasMode && incomingHasMode:
		current["approval_mode"] = incomingMode
	case currentHasMode && incomingHasMode && !reflect.DeepEqual(currentMode, incomingMode):
		current["approval_mode"] = "prompt"
	}
}

func codexStartupTimeoutAtLeast(value any, minimum int) bool {
	switch n := value.(type) {
	case int:
		return n >= minimum
	case int8:
		return int(n) >= minimum
	case int16:
		return int(n) >= minimum
	case int32:
		return int(n) >= minimum
	case int64:
		return n >= int64(minimum)
	case uint:
		return n >= uint(minimum)
	case uint8:
		return uint(n) >= uint(minimum)
	case uint16:
		return uint(n) >= uint(minimum)
	case uint32:
		return uint(n) >= uint(minimum)
	case uint64:
		return n >= uint64(minimum)
	case float32:
		return n >= float32(minimum)
	case float64:
		return n >= float64(minimum)
	default:
		return false
	}
}

// codexVersionProbeTimeout bounds the `codex --version` probe. A version
// banner is instant; anything slower is a wedged binary, and without a
// deadline the probe blocks the install indefinitely and leaks a child
// process that never exits. Failing the probe is cheap — the caller reads
// an error as "unknown install" and keeps direct tool exposure on.
// Overridable so the timeout test does not have to sleep for the real one.
var codexVersionProbeTimeout = 5 * time.Second

// codexVersionOutput is a seam for hermetic adapter tests.
var codexVersionOutput = func() ([]byte, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexVersionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// The probe also runs from surfaces with no console of their own; without
	// this, Windows hands the child a fresh console window.
	platform.ConfigureBackgroundCommand(cmd)
	return cmd.Output()
}

func codexSupportsDirectToolNamespaces() (supported bool, detectedVersion string) {
	out, err := codexVersionOutput()
	if err != nil {
		// Codex App and IDE installs do not necessarily put their bundled
		// CLI on PATH. Treat an unknown install as current so those surfaces
		// retain automatic direct exposure; only a positively identified old
		// CLI is gated below.
		return true, ""
	}
	for _, token := range strings.Fields(string(out)) {
		token = strings.Trim(token, "(),")
		parsed, parseErr := version.Parse(token)
		if parseErr != nil {
			continue
		}
		detectedVersion = strings.TrimPrefix(token, "v")
		return parsed.Major > 0 || (parsed.Major == 0 && parsed.Minor >= codexDirectToolNamespacesMinMinor), detectedVersion
	}
	return true, ""
}

// upsertCodexDirectToolNamespaces keeps Gortex's compact MCP facade in the
// model-facing tool manifest. Codex 0.142+ can otherwise defer MCP tools
// behind tool search when the active model supports it. The namespace list is
// additive so user-selected direct namespaces and all other code-mode fields
// survive; the old boolean feature form is upgraded without changing its value.
// Both exact namespace spellings are installed because Codex's opt-in
// non_prefixed_mcp_tool_names feature changes `mcp__gortex` to `gortex`.
func upsertCodexDirectToolNamespaces(root map[string]any) bool {
	changed := false
	for _, namespace := range []string{codexGortexToolNamespace, codexGortexNonPrefixedToolNamespace} {
		if upsertCodexDirectToolNamespace(root, namespace) {
			changed = true
		}
	}
	return changed
}

func upsertCodexDirectToolNamespace(root map[string]any, namespace string) bool {
	features, ok := root["features"].(map[string]any)
	if !ok {
		if _, exists := root["features"]; exists {
			return false
		}
		features = make(map[string]any)
	}

	var codeMode map[string]any
	switch existing := features["code_mode"].(type) {
	case nil:
		codeMode = make(map[string]any)
	case bool:
		codeMode = map[string]any{"enabled": existing}
	case map[string]any:
		codeMode = existing
	default:
		return false
	}

	const field = "direct_only_tool_namespaces"
	namespaces, valid := codexStringList(codeMode[field])
	if !valid {
		return false
	}
	for _, existing := range namespaces {
		if existing == namespace {
			return false
		}
	}
	codeMode[field] = append(namespaces, namespace)
	features["code_mode"] = codeMode
	root["features"] = features
	return true
}

func codexStringList(value any) ([]string, bool) {
	switch list := value.(type) {
	case nil:
		return nil, true
	case []string:
		return append([]string(nil), list...), true
	case []any:
		out := make([]string, 0, len(list))
		for _, value := range list {
			s, ok := value.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}

func upsertSessionStartHook(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	return upsertCodexHookSet(root, "SessionStart", codexHookEntryIsGortexSessionStart, []map[string]any{codexSessionStartHookEntry(env)}, opts)
}

func upsertPreToolUseHook(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	// Codex permits several handlers in one matcher group. Normalize at handler
	// granularity before replacing legacy Gortex matchers: this preserves every
	// co-located user handler while collapsing duplicate managed invocations.
	splitChanged := splitMixedCodexPreToolUseGroups(root)
	desired := []map[string]any{codexPreToolUseHookEntry(env)}
	upsertChanged := upsertCodexHookSet(root, "PreToolUse", codexHookEntryIsGortexPreToolUse, desired, opts)
	return splitChanged || upsertChanged
}

func splitMixedCodexPreToolUseGroups(root map[string]any) bool {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false
	}
	entries, ok := codexHookList(hooks["PreToolUse"])
	if !ok {
		return false
	}

	normalized := make([]any, 0, len(entries))
	changed := false
	for _, entry := range entries {
		group, ok := entry.(map[string]any)
		if !ok {
			normalized = append(normalized, entry)
			continue
		}
		handlers, ok := codexHookList(group["hooks"])
		if !ok {
			normalized = append(normalized, entry)
			continue
		}
		managed := 0
		kept := make([]any, 0, len(handlers))
		for _, handler := range handlers {
			fields, ok := handler.(map[string]any)
			if ok {
				command, _ := fields["command"].(string)
				if codexCommandInvokesCodexHook(command) {
					managed++
					continue
				}
			}
			kept = append(kept, handler)
		}
		switch {
		case managed == 0:
			normalized = append(normalized, entry)
		case managed == 1 && len(handlers) == 1:
			// Leave a singleton managed group for upsertCodexHookSet to
			// validate or replace. A current singleton remains idempotent.
			normalized = append(normalized, entry)
		default:
			changed = true
			if len(kept) == 0 {
				continue
			}
			preserved := make(map[string]any, len(group))
			for key, value := range group {
				preserved[key] = value
			}
			preserved["hooks"] = kept
			normalized = append(normalized, preserved)
		}
	}
	if !changed {
		return false
	}
	hooks["PreToolUse"] = normalized
	root["hooks"] = hooks
	return true
}

func upsertPostToolUseHook(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	return upsertCodexHookSet(root, "PostToolUse", codexHookEntryIsGortexPostToolUse, []map[string]any{codexPostToolUseHookEntry(env)}, opts)
}

func upsertUserPromptSubmitHook(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	return upsertCodexHookSet(root, "UserPromptSubmit", codexHookEntryIsGortexUserPromptSubmit, []map[string]any{codexUserPromptSubmitHookEntry(env)}, opts)
}

func upsertStopHook(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	return upsertCodexHookSet(root, "Stop", codexHookEntryIsGortexStop, []map[string]any{codexStopHookEntry(env)}, opts)
}

// InstallHooksOnly refreshes the Codex lifecycle hooks in configPath without
// touching MCP server entries, AGENTS.md, or any other Codex adapter surface.
func InstallHooksOnly(w io.Writer, configPath string, env agents.Env, opts agents.ApplyOpts) (agents.FileAction, error) {
	action, err := agents.MergeTOML(w, configPath, func(root map[string]any, _ bool) (bool, error) {
		return upsertCodexHooks(root, env, opts), nil
	}, opts)
	if err != nil {
		return agents.FileAction{}, err
	}
	if action.Action != agents.ActionSkip {
		action.Keys = []string{"hooks"}
	}
	// `init --hooks-only` writes the same inline tables Apply does, so it can
	// create the same silent duplicate and has to say the same thing.
	if notice := codexDuplicateHookFileNotice(configPath); notice != "" {
		internalutil.Warnf(w, "%s", notice)
	}
	return action, nil
}

func upsertCodexHooks(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	sessionChanged := upsertSessionStartHook(root, env, opts)
	preChanged := upsertPreToolUseHook(root, env, opts)
	postChanged := upsertPostToolUseHook(root, env, opts)
	promptChanged := upsertUserPromptSubmitHook(root, env, opts)
	stopChanged := upsertStopHook(root, env, opts)
	return sessionChanged || preChanged || postChanged || promptChanged || stopChanged
}

func upsertCodexHookSet(root map[string]any, event string, isGortex func(any) bool, desired []map[string]any, opts agents.ApplyOpts) bool {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		if _, exists := root["hooks"]; exists {
			return false
		}
		hooks = make(map[string]any)
	}

	entries, ok := codexHookList(hooks[event])
	if !ok {
		return false
	}

	found := make([]bool, len(desired))
	kept := make([]any, 0, len(entries)+len(desired))
	changed := false
	for _, entry := range entries {
		if isGortex(entry) {
			if opts.Force {
				continue
			}
			matched := false
			for i, want := range desired {
				if !found[i] && codexHookEntryMatchesDesired(entry, want) {
					found[i] = true
					matched = true
					break
				}
			}
			if !matched {
				// This is a Gortex-authored hook with a stale matcher, command,
				// or posture. Replace it instead of accumulating duplicate hooks.
				changed = true
				continue
			}
		}
		kept = append(kept, entry)
	}

	for i, want := range desired {
		if opts.Force || !found[i] {
			kept = append(kept, want)
			changed = true
		}
	}
	if !changed {
		return false
	}

	hooks[event] = kept
	root["hooks"] = hooks
	return true
}

func codexHookEntryMatchesDesired(entry any, desired map[string]any) bool {
	matcher, _ := desired["matcher"].(string)
	if !codexHookEntryHasMatcher(entry, matcher) {
		return false
	}
	want := codexHookEntryCommand(desired)
	return want != "" && codexHookEntryCommand(entry) == want
}

func codexHookEntryCommand(entry any) string {
	group, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	handlers, ok := codexHookList(group["hooks"])
	if !ok {
		return ""
	}
	for _, handler := range handlers {
		if fields, ok := handler.(map[string]any); ok {
			if command, _ := fields["command"].(string); strings.TrimSpace(command) != "" {
				return command
			}
		}
	}
	return ""
}

func codexHookEntryHasMatcher(entry any, matcher string) bool {
	group, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	got, _ := group["matcher"].(string)
	return got == matcher
}

func codexHookList(v any) ([]any, bool) {
	if v == nil {
		return nil, true
	}
	switch list := v.(type) {
	case []any:
		return append([]any(nil), list...), true
	case []map[string]any:
		out := make([]any, 0, len(list))
		for _, entry := range list {
			out = append(out, entry)
		}
		return out, true
	default:
		return nil, false
	}
}

func codexHookEntryIsGortexSessionStart(entry any) bool {
	group, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	handlers, ok := codexHookList(group["hooks"])
	if !ok {
		return false
	}
	for _, handler := range handlers {
		hm, ok := handler.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); cmd == v060CodexSessionStartCommand || codexCommandInvokesCodexHook(cmd) {
			return true
		}
		if cmd, _ := hm["command_windows"].(string); cmd == v060CodexSessionStartWindowsCommand {
			return true
		}
	}
	return false
}

func codexHookEntryIsGortexPreToolUse(entry any) bool {
	return codexHookEntryInvokesCodexHook(entry)
}

func codexHookEntryIsGortexPostToolUse(entry any) bool {
	return codexHookEntryInvokesCodexHook(entry)
}

func codexHookEntryIsGortexUserPromptSubmit(entry any) bool {
	return codexHookEntryInvokesCodexHook(entry)
}

func codexHookEntryIsGortexStop(entry any) bool {
	return codexHookEntryInvokesCodexHook(entry)
}

func codexHookEntryInvokesCodexHook(entry any) bool {
	group, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	handlers, ok := codexHookList(group["hooks"])
	if !ok {
		return false
	}
	for _, handler := range handlers {
		hm, ok := handler.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); codexCommandInvokesCodexHook(cmd) {
			return true
		}
	}
	return false
}

func codexCommandInvokesCodexHook(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	if !strings.Contains(lower, "gortex") || !strings.Contains(lower, "hook") {
		return false
	}
	return strings.Contains(cmd, "--agent=codex") || strings.Contains(cmd, "--agent codex")
}

func codexSessionStartHookEntry(env agents.Env) map[string]any {
	return map[string]any{
		"matcher": codexSessionStartMatcher,
		"hooks": []any{
			map[string]any{
				"type":          "command",
				"command":       codexHookCommand(env),
				"timeout":       codexHookTimeoutSeconds,
				"statusMessage": "Loading Gortex graph orientation...",
			},
		},
	}
}

func codexPreToolUseHookEntry(env agents.Env) map[string]any {
	return map[string]any{
		"matcher": codexPreToolUseMatcher,
		"hooks": []any{
			map[string]any{
				"type":          "command",
				"command":       codexPreToolUseCommand(env),
				"timeout":       codexHookTimeoutSeconds,
				"statusMessage": "Loading Gortex tool guidance...",
			},
		},
	}
}

func codexPostToolUseHookEntry(env agents.Env) map[string]any {
	return map[string]any{
		"matcher": codexPostToolUseMatcher,
		"hooks": []any{
			map[string]any{
				"type":          "command",
				"command":       codexHookCommand(env),
				"timeout":       codexHookTimeoutSeconds,
				"statusMessage": "Loading Gortex post-tool context...",
			},
		},
	}
}

// codexUserPromptSubmitHookEntry fires on every user turn — Codex's
// UserPromptSubmit event takes no matcher (it can't filter by tool name), so
// the entry omits one. The handler probes the graph for symbols relevant to
// the prompt and injects them as additionalContext, re-surfacing Gortex on
// every turn instead of relying on the SessionStart orientation to persist.
func codexUserPromptSubmitHookEntry(env agents.Env) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":          "command",
				"command":       codexHookCommand(env),
				"timeout":       codexHookTimeoutSeconds,
				"statusMessage": "Surfacing Gortex graph context for your prompt...",
			},
		},
	}
}

// codexStopHookEntry has no matcher: Stop applies to every final response.
// Older Codex hosts that omit last_assistant_message remain fail-open in the
// shared Stop handler.
func codexStopHookEntry(env agents.Env) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":          "command",
				"command":       codexHookCommand(env),
				"timeout":       codexHookTimeoutSeconds,
				"statusMessage": "Checking Gortex evidence authority...",
			},
		},
	}
}

func codexPreToolUseCommand(env agents.Env) string {
	return codexHookCommand(env)
}

func codexHookCommand(env agents.Env) string {
	base := strings.TrimSpace(env.HookCommand)
	if base == "" {
		base = "gortex hook"
	}
	return base + " --agent=codex --mode=" + codexHookMode()
}

// codexHookMode keeps the shipped posture advisory while allowing a team to
// opt into current Codex capabilities without changing the generic Claude hook
// posture. Supported values: enrich, deny, rewrite, suppress. Suppress uses
// PostToolUse result replacement because Codex does not yet implement the
// suppressOutput field itself.
func codexHookMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(codexHookModeEnvVar))) {
	case "deny", "hard-deny":
		return "deny"
	case "rewrite", "input-rewrite":
		return "rewrite"
	case "suppress", "replace-output", "output-suppression":
		return "suppress"
	default:
		return "enrich"
	}
}
