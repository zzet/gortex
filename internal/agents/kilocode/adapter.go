// Package kilocode implements the Gortex init integration for the
// Kilo Code VS Code extension. Kilo Code stores MCP servers at the
// VS Code extension's globalStorage directory:
//
//	macOS/Linux: ~/.config/Code/User/globalStorage/kilocode.kilo/mcp_settings.json
//	(variants exist under Library/Application Support on macOS)
//
// The schema is the canonical {"mcpServers": {<name>: {...}}}
// plus an `alwaysAllow` list that mirrors the Cline pattern.
// Project-level config is also supported at .kilocode/mcp.json.
//
// Docs: https://kilo.ai/docs/features/mcp/using-mcp-in-kilo-code
package kilocode

import (
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "kilocode"
const DocsURL = "https://kilo.ai/docs/features/mcp/using-mcp-in-kilo-code"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// alwaysAllow is Kilo Code's coarse tool-level auto-approve list. Kilo is a
// Cline fork and uses the same field and compact-tool policy.
var alwaysAllow = agents.CompactMCPAutoApproveTools()

// v060AlwaysAllow fingerprints the exact approval list shipped by gortex
// v0.60.0. User customizations, including deliberately narrower lists, are
// never widened. The concrete retirement gate is in docs/versioning.md.
var v060AlwaysAllow = []string{
	"graph_stats", "search_symbols", "winnow_symbols", "get_symbol", "get_file_summary",
	"get_editing_context", "get_dependencies", "get_dependents", "get_call_chain", "get_callers",
	"find_implementations", "find_usages", "get_cluster", "get_symbol_source", "batch_symbols",
	"find_import_path", "explain_change_impact", "get_recent_changes", "smart_context", "get_edit_plan", "get_test_targets", "suggest_pattern",
	"get_communities", "get_processes", "detect_changes", "index_repository", "reindex_repository",
	"verify_change", "check_guards", "prefetch_context", "analyze", "diff_context", "index_health", "get_symbol_history",
	"scaffold", "batch_edit", "contracts", "feedback", "flow_between", "taint_paths",
}

// globalStoragePaths returns candidate paths for Kilo Code's MCP
// settings across VS Code install flavours (VS Code stable,
// VS Code Insiders, VSCodium, Cursor). We write to every candidate
// whose parent globalStorage directory exists.
func globalStoragePaths(home string) []string {
	bases := []string{
		filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "kilocode.kilo", "settings"),
		filepath.Join(home, ".config", "Code", "User", "globalStorage", "kilocode.kilo", "settings"),
		filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "kilocode.kilo", "settings"),
		filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "kilocode.kilo", "settings"),
	}
	paths := make([]string, 0, len(bases)+1)
	for _, b := range bases {
		paths = append(paths, filepath.Join(b, "mcp_settings.json"))
	}
	return paths
}

// WritesCommunitiesRouting reports that this adapter merges the
// communities routing block into its instruction file (project mode).
func (a *Adapter) WritesCommunitiesRouting() bool { return true }

// CommunitiesRoutingPath reports the instruction file that carries the
// communities block in project mode.
func (a *Adapter) CommunitiesRoutingPath(env agents.Env) string {
	return filepath.Join(env.Root, ".kilocoderules")
}


func (a *Adapter) Detect(env agents.Env) (bool, error) {
	// Project-level hint: .kilocode/ in the repo.
	if _, err := os.Stat(filepath.Join(env.Root, ".kilocode")); err == nil {
		return true, nil
	}
	if env.Home == "" {
		return false, nil
	}
	for _, p := range globalStoragePaths(env.Home) {
		if _, err := os.Stat(filepath.Dir(p)); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	// Project-level .kilocode/mcp.json when we're in project mode.
	if env.Mode != agents.ModeGlobal {
		p.Files = append(p.Files, agents.FileAction{
			Path:   filepath.Join(env.Root, ".kilocode", "mcp.json"),
			Action: agents.ActionWouldMerge,
			Keys:   []string{"mcpServers"},
		})
		if env.SkillsRouting != "" {
			p.Files = append(p.Files, agents.FileAction{
				Path:   filepath.Join(env.Root, ".kilocoderules"),
				Action: agents.ActionWouldMerge,
				Keys:   []string{"communities-block"},
			})
		}
	}
	if env.Home != "" {
		for _, path := range globalStoragePaths(env.Home) {
			if _, err := os.Stat(filepath.Dir(path)); err == nil {
				p.Files = append(p.Files, agents.FileAction{
					Path:   path,
					Action: agents.ActionWouldMerge,
					Keys:   []string{"mcpServers"},
				})
			}
		}
	}
	return p, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip Kilo Code setup (kilo-code not detected)")
		return res, nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Kilo Code integration...")

	entry := agents.DefaultGortexMCPEntry()
	entry["alwaysAllow"] = alwaysAllow

	merge := func(root map[string]any, _ bool) (bool, error) {
		return agents.UpsertMCPServerApprovalList(root, "gortex", "alwaysAllow", alwaysAllow, entry, opts, v060AlwaysAllow), nil
	}

	// Project-level first, if a .kilocode/ dir already exists.
	if env.Mode != agents.ModeGlobal {
		projectPath := filepath.Join(env.Root, ".kilocode", "mcp.json")
		if _, err := os.Stat(filepath.Join(env.Root, ".kilocode")); err == nil {
			action, err := agents.MergeJSON(env.Stderr, projectPath, merge, opts)
			if err != nil {
				return res, err
			}
			res.Files = append(res.Files, action)
		}
	}

	// Global-storage paths — write to every one whose parent exists.
	if env.Home != "" {
		for _, path := range globalStoragePaths(env.Home) {
			if _, err := os.Stat(filepath.Dir(path)); err != nil {
				continue
			}
			action, err := agents.MergeJSON(env.Stderr, path, merge, opts)
			if err != nil {
				internalutil.Warnf(env.Stderr, "could not configure Kilo Code at %s: %v", path, err)
				continue
			}
			res.Files = append(res.Files, action)
		}
	}

	// .kilocoderules is the project-scoped instructions file Kilo
	// Code reads on every task (inherited from the Cline lineage).
	// Write a marker-guarded community-routing block when skills
	// were generated. Skipped in global mode and when --no-skills /
	// no communities qualify.
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		rulesPath := filepath.Join(env.Root, ".kilocoderules")
		routingAction, err := agents.UpsertMarkedBlock(env.Stderr, rulesPath, env.SkillsRouting,
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			internalutil.Warnf(env.Stderr, "could not update %s: %v", rulesPath, err)
		} else {
			res.Files = append(res.Files, routingAction)
		}
	}

	res.Configured = len(res.Files) > 0
	return res, nil
}
