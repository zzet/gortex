// Package cline implements the Gortex init integration for Cline
// (formerly Claude Dev — extension ID saoudrizwan.claude-dev). Cline
// stores its MCP config under the host editor's globalStorage
// directory; we probe both the VS Code and Cursor variants on each
// OS. The Step 3 audit will add JetBrains support and verify whether
// Cline has renamed alwaysAllow → autoApprove.
package cline

import (
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "cline"
const DocsURL = "https://docs.cline.bot/mcp/mcp-overview"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// settingsPaths returns candidate cline_mcp_settings.json locations
// across OS/editor combos. Order doesn't matter — we write to every
// path whose parent directory exists.
func settingsPaths(home string) []string {
	var paths []string
	vscodeBases := []string{
		filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings"),
		filepath.Join(home, ".config", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings"),
	}
	cursorBases := []string{
		filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "saoudrizwan.claude-dev", "settings"),
		filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "saoudrizwan.claude-dev", "settings"),
	}
	for _, dir := range append(vscodeBases, cursorBases...) {
		paths = append(paths, filepath.Join(dir, "cline_mcp_settings.json"))
	}
	return paths
}

// alwaysAllow is Cline's coarse tool-level auto-approve list. It follows the
// compact MCP surface while leaving external review publishing unapproved.
var alwaysAllow = agents.CompactMCPAutoApproveTools()

// v060AlwaysAllow is the exact approval list shipped by gortex v0.60.0. It is
// only a migration fingerprint; customized lists are preserved. The concrete
// retirement gate is documented in docs/versioning.md.
var v060AlwaysAllow = []string{
	"graph_stats", "search_symbols", "winnow_symbols", "get_symbol", "get_file_summary",
	"get_editing_context", "get_dependencies", "get_dependents", "get_call_chain", "get_callers",
	"find_implementations", "find_usages", "get_cluster", "get_symbol_signature", "get_symbol_source", "batch_symbols",
	"find_import_path", "explain_change_impact", "get_recent_changes", "smart_context", "get_edit_plan", "get_test_targets", "suggest_pattern",
	"get_communities", "get_community", "get_processes", "get_process", "detect_changes", "index_repository", "reindex_repository",
	"verify_change", "check_guards", "prefetch_context", "find_dead_code", "find_hotspots", "find_cycles", "would_create_cycle",
	"diff_context", "index_health", "get_symbol_history", "scaffold", "batch_edit", "flow_between", "taint_paths", "find_clones",
}

// WritesCommunitiesRouting reports that this adapter merges the
// communities routing block into its instruction file (project mode).
func (a *Adapter) WritesCommunitiesRouting() bool { return true }

// CommunitiesRoutingPath reports the instruction file that carries the
// communities block in project mode.
func (a *Adapter) CommunitiesRoutingPath(env agents.Env) string {
	return filepath.Join(env.Root, ".clinerules", "gortex-communities.md")
}

func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if env.Home == "" {
		return false, nil
	}
	for _, p := range settingsPaths(env.Home) {
		if _, err := os.Stat(filepath.Dir(p)); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	if env.Home == "" {
		return p, nil
	}
	for _, path := range settingsPaths(env.Home) {
		if _, err := os.Stat(filepath.Dir(path)); err == nil {
			p.Files = append(p.Files, agents.FileAction{
				Path:   path,
				Action: agents.ActionWouldMerge,
				Keys:   []string{"mcpServers"},
			})
		}
	}
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path:   filepath.Join(env.Root, ".clinerules", "gortex-communities.md"),
			Action: agents.ActionWouldCreate,
			Keys:   []string{"communities-rule"},
		})
	}
	return p, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip Cline setup (Cline not detected)")
		return res, nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Cline integration...")

	for _, path := range settingsPaths(env.Home) {
		if _, err := os.Stat(filepath.Dir(path)); err != nil {
			continue
		}
		entry := agents.DefaultGortexMCPEntry()
		entry["alwaysAllow"] = alwaysAllow
		action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
			return agents.UpsertMCPServerApprovalList(root, "gortex", "alwaysAllow", alwaysAllow, entry, opts, v060AlwaysAllow), nil
		}, opts)
		if err != nil {
			internalutil.Warnf(env.Stderr, "could not configure Cline at %s: %v", path, err)
			continue
		}
		res.Files = append(res.Files, action)
	}
	// Cline reads .clinerules/*.md as project-scoped instructions
	// on every chat turn. The community-routing file is ours
	// end-to-end — regenerated each `gortex init` run so the
	// listing tracks the current graph. Skipped in global mode
	// (file is per-repo) and when --no-skills / no communities
	// qualify.
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		rulesPath := filepath.Join(env.Root, ".clinerules", "gortex-communities.md")
		body := agents.CommunitiesStartMarker + "\n" + env.SkillsRouting + "\n" + agents.CommunitiesEndMarker + "\n"
		ruleAction, err := agents.WriteOwnedFile(env.Stderr, rulesPath, body, opts)
		if err != nil {
			internalutil.Warnf(env.Stderr, "could not write Cline community rules at %s: %v", rulesPath, err)
		} else {
			res.Files = append(res.Files, ruleAction)
		}
	}

	res.Configured = len(res.Files) > 0
	return res, nil
}
