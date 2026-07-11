// Package codex implements the Gortex init integration for the
// OpenAI Codex CLI. Codex stores MCP server definitions in a TOML
// file — ~/.codex/config.toml for the default scope — under the
// [mcp_servers.<name>] table:
//
//	[mcp_servers.gortex]
//	command = "gortex"
//	args = ["mcp", "--index", ".", "--watch"]
//	[mcp_servers.gortex.env]
//	GORTEX_INDEX_WORKERS = "8"
//
// Docs: https://github.com/openai/codex/blob/main/docs/config.md
package codex

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "codex"
const DocsURL = "https://developers.openai.com/codex/mcp"

const codexSessionStartMatcher = "startup|resume|clear|compact"
const codexSessionStartMessage = "IMPORTANT: Prefer Gortex MCP tools (search_symbols, get_callers, get_file_summary, edit_file) over Read/Grep/Glob/Edit."
const codexSessionStartCommand = "printf '%s\\n' '" + codexSessionStartMessage + "'"
const codexSessionStartWindowsCommand = "powershell -NoProfile -Command \"Write-Output '" + codexSessionStartMessage + "'\""
const codexPreToolUseMatcher = "^Bash$"
const codexMCPReadPreToolUseMatcher = "^mcp__gortex__(read_file|get_editing_context)$"
const codexPostToolUseMatcher = "^(Bash|apply_patch)$"
const codexHookTimeoutSeconds = 5
const codexToolsEnvValue = "agent,+read_file"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

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
		keys := []string{"mcp_servers"}
		if env.InstallHooks {
			keys = append(keys, "hooks")
		}
		p.Files = append(p.Files, agents.FileAction{
			Path:   filepath.Join(env.Home, ".codex", "config.toml"),
			Action: agents.ActionWouldMerge,
			Keys:   keys,
		})
	}
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path: filepath.Join(env.Root, "AGENTS.md"), Action: agents.ActionWouldMerge,
			Keys: []string{"communities-block"},
		})
	}
	return p, nil
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
	action, err := agents.MergeTOML(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		changed := false
		servers, ok := root["mcp_servers"].(map[string]any)
		if !ok {
			servers = make(map[string]any)
		}
		server, exists := servers["gortex"].(map[string]any)
		if !exists || opts.Force {
			servers["gortex"] = map[string]any{
				"command": "gortex",
				"args":    []string{"mcp"},
				"env": map[string]any{
					"GORTEX_INDEX_WORKERS": "8",
					// Pin the agent surface explicitly: some MCP hosts defer
					// tools before clientInfo is available, but read_file is a
					// mandatory Codex source-reading primitive.
					"GORTEX_TOOLS": codexToolsEnvValue,
				},
			}
			root["mcp_servers"] = servers
			changed = true
		} else {
			// Preserve an operator's explicit surface choice, but make fresh
			// and older Gortex-owned entries deterministic: without this pin a
			// host that delays/omits clientInfo can fall back to a deferred
			// catalogue and strand read_file behind tools_search.
			envMap, ok := server["env"].(map[string]any)
			if !ok {
				envMap = make(map[string]any)
			}
			if _, pinned := envMap["GORTEX_TOOLS"]; !pinned {
				envMap["GORTEX_TOOLS"] = codexToolsEnvValue
				server["env"] = envMap
				servers["gortex"] = server
				root["mcp_servers"] = servers
				changed = true
			}
		}

		if env.InstallHooks {
			if upsertCodexHooks(root, env, opts) {
				changed = true
			}
		}
		return changed, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)

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

	res.Configured = true
	return res, nil
}

func upsertSessionStartHook(root map[string]any, opts agents.ApplyOpts) bool {
	return upsertCodexHook(root, "SessionStart", codexHookEntryIsGortexSessionStart, codexSessionStartHookEntry(), opts)
}

func upsertPreToolUseHook(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	desired := []map[string]any{
		codexPreToolUseHookEntry(env),
		codexMCPReadPreToolUseHookEntry(env),
	}
	return upsertCodexHookSet(root, "PreToolUse", codexHookEntryIsGortexPreToolUse, desired, opts)
}

func upsertPostToolUseHook(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	return upsertCodexHook(root, "PostToolUse", codexHookEntryIsGortexPostToolUse, codexPostToolUseHookEntry(env), opts)
}

func upsertUserPromptSubmitHook(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	return upsertCodexHook(root, "UserPromptSubmit", codexHookEntryIsGortexUserPromptSubmit, codexUserPromptSubmitHookEntry(env), opts)
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
	return action, nil
}

func upsertCodexHooks(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	sessionChanged := upsertSessionStartHook(root, opts)
	preChanged := upsertPreToolUseHook(root, env, opts)
	postChanged := upsertPostToolUseHook(root, env, opts)
	promptChanged := upsertUserPromptSubmitHook(root, env, opts)
	return sessionChanged || preChanged || postChanged || promptChanged
}

func upsertCodexHook(root map[string]any, event string, isGortex func(any) bool, desired map[string]any, opts agents.ApplyOpts) bool {
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

	found := false
	kept := make([]any, 0, len(entries)+1)
	for _, entry := range entries {
		if isGortex(entry) {
			if opts.Force || !codexHookEntryMatchesDesired(entry, desired) {
				continue
			}
			found = true
		}
		kept = append(kept, entry)
	}
	if found && !opts.Force {
		return false
	}

	hooks[event] = append(kept, desired)
	root["hooks"] = hooks
	return true
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
	for _, entry := range entries {
		if isGortex(entry) {
			if opts.Force {
				continue
			}
			for i, want := range desired {
				if !found[i] && codexHookEntryMatchesDesired(entry, want) {
					found[i] = true
					break
				}
			}
			// This is a Gortex-owned entry whose matcher is no longer one
			// of the desired entries (for example, its hook mode changed).
			// Reconcile it rather than preserving stale posture forever.
			if !codexHookEntryMatchesAnyDesired(entry, desired) {
				continue
			}
		}
		kept = append(kept, entry)
	}

	changed := false
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
	return codexHookEntryCommand(entry) == codexHookEntryCommand(desired)
}

func codexHookEntryMatchesAnyDesired(entry any, desired []map[string]any) bool {
	for _, want := range desired {
		if codexHookEntryMatchesDesired(entry, want) {
			return true
		}
	}
	return false
}

func codexHookEntryCommand(entry any) string {
	group, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	handlers, ok := codexHookList(group["hooks"])
	if !ok || len(handlers) != 1 {
		return ""
	}
	handler, ok := handlers[0].(map[string]any)
	if !ok {
		return ""
	}
	cmd, _ := handler["command"].(string)
	return cmd
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
		if cmd, _ := hm["command"].(string); cmd == codexSessionStartCommand {
			return true
		}
		if cmd, _ := hm["command_windows"].(string); cmd == codexSessionStartWindowsCommand {
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

func codexSessionStartHookEntry() map[string]any {
	return map[string]any{
		"matcher": codexSessionStartMatcher,
		"hooks": []any{
			map[string]any{
				"type":            "command",
				"command":         codexSessionStartCommand,
				"command_windows": codexSessionStartWindowsCommand,
				"timeout":         codexHookTimeoutSeconds,
				"statusMessage":   "Loading Gortex graph orientation...",
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
				"statusMessage": "Loading Gortex Bash guidance...",
			},
		},
	}
}

func codexMCPReadPreToolUseHookEntry(env agents.Env) map[string]any {
	return map[string]any{
		"matcher": codexMCPReadPreToolUseMatcher,
		"hooks": []any{
			map[string]any{
				"type":          "command",
				"command":       codexPreToolUseCommand(env),
				"timeout":       codexHookTimeoutSeconds,
				"statusMessage": "Loading Gortex read guidance...",
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
				"statusMessage": "Loading Gortex Bash output context...",
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

func codexPreToolUseCommand(env agents.Env) string {
	return codexHookCommand(env)
}

func codexHookCommand(env agents.Env) string {
	base := strings.TrimSpace(env.HookCommand)
	if base == "" {
		base = "gortex hook"
	}
	return base + " --agent=codex --mode=" + codexHookMode(env)
}

func codexHookMode(env agents.Env) string {
	switch strings.TrimSpace(env.CodexHookMode) {
	case "deny", "consult-unlock", "nudge", "adaptive-nudge":
		return strings.TrimSpace(env.CodexHookMode)
	default:
		return "enrich"
	}
}
