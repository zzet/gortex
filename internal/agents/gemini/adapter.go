// Package gemini implements the Gortex init integration for the
// Gemini CLI. Gemini reads ~/.gemini/settings.json (user-level) and
// .gemini/settings.json (project-level); both accept the standard
// {"mcpServers": {...}} shape.
//
// Note: this adapter is distinct from the antigravity adapter —
// Gemini CLI and Google Antigravity are different products that
// happen to share the ~/.gemini/ root directory.
//
// Docs: https://geminicli.com/docs/tools/mcp-server/
package gemini

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "gemini"
const DocsURL = "https://geminicli.com/docs/tools/mcp-server/"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// WritesCommunitiesRouting reports that this adapter merges the
// communities routing block into its instruction file (project mode).
func (a *Adapter) WritesCommunitiesRouting() bool { return true }

// CommunitiesRoutingPath reports the instruction file that carries the
// communities block in project mode.
func (a *Adapter) CommunitiesRoutingPath(env agents.Env) string {
	return filepath.Join(env.Root, "GEMINI.md")
}

// Detect checks for the gemini CLI on PATH or an existing user-level
// settings.json. We avoid colliding with the antigravity adapter's
// detection by looking at ~/.gemini/settings.json specifically
// rather than the whole ~/.gemini/ tree.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("gemini"); err == nil && p != "" {
		return true, nil
	}
	if env.Home == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".gemini", "settings.json")); err == nil {
		return true, nil
	}
	return false, nil
}

// configPath returns the right settings.json for the current Env.
// Project mode writes .gemini/settings.json; global mode writes
// ~/.gemini/settings.json.
func configPath(env agents.Env) string {
	if env.Mode == agents.ModeGlobal && env.Home != "" {
		return filepath.Join(env.Home, ".gemini", "settings.json")
	}
	return filepath.Join(env.Root, ".gemini", "settings.json")
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{Files: []agents.FileAction{
		{Path: configPath(env), Action: agents.ActionWouldMerge, Keys: []string{"mcpServers", "hooks"}},
	}}
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path: filepath.Join(env.Root, "GEMINI.md"), Action: agents.ActionWouldMerge,
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
		internalutil.Logf(env.Stderr, "[gortex init] skip Gemini CLI setup (gemini not detected)")
		return res, nil
	}
	if env.Home == "" {
		return res, fmt.Errorf("gemini: requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Gemini CLI integration...")

	action, err := agents.MergeJSON(env.Stderr, configPath(env), func(root map[string]any, _ bool) (bool, error) {
		mcp := agents.UpsertMCPServer(root, "gortex", agents.DefaultGortexMCPEntry(), opts)
		// Install SessionStart + AfterTool lifecycle hooks alongside the
		// MCP registration so Gemini gets the same orientation and
		// stale-index hints Claude does.
		hk := agents.UpsertGeminiHooks(root, Name, opts)
		return mcp || hk, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)

	// GEMINI.md gets a marker-guarded community-routing block when
	// skills were generated (--skills, default on in `gortex init`).
	// Skipped in global mode (the file is per-repo) and when no
	// communities met the min-size threshold.
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		geminiMdPath := filepath.Join(env.Root, "GEMINI.md")
		routingAction, err := agents.UpsertMarkedBlock(env.Stderr, geminiMdPath, env.SkillsRouting,
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, routingAction)
	}

	res.Configured = true
	return res, nil
}
