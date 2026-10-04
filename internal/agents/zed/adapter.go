// Package zed implements the Gortex init integration for the Zed
// editor. Zed calls its MCP registry `context_servers` (not
// `mcpServers`) and stores it in a platform-specific settings.json:
//
//	macOS:   ~/Library/Application Support/Zed/settings.json
//	Linux:   ~/.config/zed/settings.json
//	Windows: %APPDATA%\Zed\settings.json
//
// The server entry shape:
//
//	{
//	  "context_servers": {
//	    "gortex": {
//	      "source": "custom",
//	      "command": "gortex",
//	      "args": ["mcp", "--index", ".", "--watch"]
//	    }
//	  }
//	}
//
// Docs: https://zed.dev/docs/ai/mcp
package zed

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "zed"
const DocsURL = "https://zed.dev/docs/ai/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// userSettingsPath returns the platform-specific Zed settings file.
// Returns "" when Home is unset or the OS is unsupported.
func userSettingsPath(home string) string {
	if home == "" {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Zed", "settings.json")
	case "linux":
		return filepath.Join(home, ".config", "zed", "settings.json")
	case "windows":
		// %APPDATA% is usually ~\AppData\Roaming on Windows.
		return filepath.Join(home, "AppData", "Roaming", "Zed", "settings.json")
	default:
		return ""
	}
}

// WritesCommunitiesRouting reports that this adapter merges the
// communities routing block into its instruction file (project mode).
func (a *Adapter) WritesCommunitiesRouting() bool { return true }

// CommunitiesRoutingPath reports the instruction file that carries the
// communities block in project mode.
func (a *Adapter) CommunitiesRoutingPath(env agents.Env) string {
	return filepath.Join(env.Root, ".rules")
}


// Detect checks for the zed CLI on PATH or the platform-specific
// settings.json directory.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("zed"); err == nil && p != "" {
		return true, nil
	}
	path := userSettingsPath(env.Home)
	if path == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Dir(path)); err == nil {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	if settings := userSettingsPath(env.Home); settings != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path:   settings,
			Action: agents.ActionWouldMerge,
			Keys:   []string{"context_servers"},
		})
	}
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path: filepath.Join(env.Root, ".rules"), Action: agents.ActionWouldMerge,
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
		internalutil.Logf(env.Stderr, "[gortex init] skip Zed setup (zed not detected)")
		return res, nil
	}
	path := userSettingsPath(env.Home)
	if path == "" {
		return res, fmt.Errorf("zed: no user settings path known for %s", runtime.GOOS)
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Zed integration...")

	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		servers, ok := root["context_servers"].(map[string]any)
		if !ok {
			servers = make(map[string]any)
		}
		if _, exists := servers["gortex"]; exists && !opts.Force {
			return false, nil
		}
		servers["gortex"] = map[string]any{
			"source":  "custom",
			"command": "gortex",
			"args":    []string{"mcp"},
		}
		root["context_servers"] = servers
		return true, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)

	// Zed's Agent panel reads `.rules` at the project root on every
	// turn. Write a marker-guarded community-routing block there
	// when skills were generated. Skipped in global mode (the file
	// is per-repo).
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		rulesPath := filepath.Join(env.Root, ".rules")
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
