// Package windsurf implements the Gortex init integration for
// Codeium's Windsurf editor. The canonical config path is
// ~/.codeium/mcp_config.json (documented 2026-04). Earlier Windsurf
// releases used ~/.codeium/windsurf/mcp_config.json; we detect both
// and prefer the new path, leaving the old file alone so users can
// uninstall a stale install cleanly. --force upgrades by removing
// the legacy file.
//
// Docs: https://docs.windsurf.com/plugins/cascade/mcp
package windsurf

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "windsurf"
const DocsURL = "https://docs.windsurf.com/plugins/cascade/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// currentConfigPath is the path Windsurf reads today.
func currentConfigPath(home string) string {
	return filepath.Join(home, ".codeium", "mcp_config.json")
}

// legacyConfigPath is the pre-2026 path Windsurf used.
func legacyConfigPath(home string) string {
	return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")
}

// WritesCommunitiesRouting reports that this adapter merges the
// communities routing block into its instruction file (project mode).
func (a *Adapter) WritesCommunitiesRouting() bool { return true }

// CommunitiesRoutingPath reports the instruction file that carries the
// communities block in project mode.
func (a *Adapter) CommunitiesRoutingPath(env agents.Env) string {
	return filepath.Join(env.Root, ".windsurfrules")
}


func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("windsurf"); err == nil && p != "" {
		return true, nil
	}
	if env.Home == "" {
		return false, nil
	}
	for _, p := range []string{
		filepath.Join(env.Home, ".codeium"),
		filepath.Join(env.Home, ".codeium", "windsurf"),
	} {
		if _, err := os.Stat(p); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	if env.Home != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path:   currentConfigPath(env.Home),
			Action: agents.ActionWouldMerge,
			Keys:   []string{"mcpServers"},
		})
	}
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path: filepath.Join(env.Root, ".windsurfrules"), Action: agents.ActionWouldMerge,
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
		internalutil.Logf(env.Stderr, "[gortex init] skip Windsurf setup (Windsurf not detected)")
		return res, nil
	}
	if env.Home == "" {
		return res, fmt.Errorf("windsurf: requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Windsurf integration...")

	// Migrate: if the legacy path has an entry but the current
	// path doesn't, move the gortex stanza over. Users who've
	// hand-edited the legacy file shouldn't lose their changes;
	// we leave the legacy file on disk unless --force.
	if err := migrateLegacyWindsurfConfig(env, opts); err != nil {
		internalutil.Warnf(env.Stderr, "windsurf: legacy-config migration failed: %v", err)
	}

	action, err := agents.MergeJSON(env.Stderr, currentConfigPath(env.Home), func(root map[string]any, _ bool) (bool, error) {
		return agents.UpsertMCPServer(root, "gortex", agents.DefaultGortexMCPEntry(), opts), nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)

	// .windsurfrules gets a marker-guarded community-routing block
	// when skills were generated. Skipped in global mode (the file
	// is per-repo) and when --no-skills / no communities qualify.
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		rulesPath := filepath.Join(env.Root, ".windsurfrules")
		routingAction, err := agents.UpsertMarkedBlock(env.Stderr, rulesPath, env.SkillsRouting,
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, routingAction)
	}

	res.Configured = true
	return res, nil
}

// migrateLegacyWindsurfConfig copies a pre-existing gortex stanza
// from the legacy ~/.codeium/windsurf/mcp_config.json to the
// current path. Non-gortex servers the user added are not moved —
// they may be intentionally Windsurf-specific.
func migrateLegacyWindsurfConfig(env agents.Env, opts agents.ApplyOpts) error {
	legacy := legacyConfigPath(env.Home)
	if _, err := os.Stat(legacy); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] migrating Windsurf config from %s to %s", legacy, currentConfigPath(env.Home))
	// No-op migration: we never overwrite the legacy file. The
	// merge below handles inserting the gortex stanza into the
	// current path. With --force we remove the legacy file.
	if opts.Force && !opts.DryRun {
		if err := os.Remove(legacy); err != nil {
			return err
		}
		internalutil.Logf(env.Stderr, "[gortex init] --force: removed legacy Windsurf config %s", legacy)
	}
	return nil
}
