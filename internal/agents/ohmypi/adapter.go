// Package ohmypi implements the Gortex init integration for
// Oh My Pi (omp). Oh My Pi stores project-level MCP servers
// at .omp/mcp.json using the canonical mcpServers schema:
//
//	{
//	  "mcpServers": {
//	    "gortex": {
//	      "command": "gortex",
//	      "args": ["mcp"],
//	      "env": {"GORTEX_INDEX_WORKERS": "8"}
//	    }
//	  }
//	}
//
// The MCP client name sent during the initialize handshake is
// "omp-coding-agent"; Gortex uses this to default the response
// format to GCX1.
package ohmypi

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "oh-my-pi"
const DocsURL = "https://github.com/can1357/oh-my-pi/blob/main/docs/mcp-config.md"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if env.Mode == agents.ModeGlobal {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".omp")); err == nil {
		return true, nil
	}
	if p, err := exec.LookPath("omp"); err == nil && p != "" {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	if env.Mode == agents.ModeGlobal {
		return &agents.Plan{}, nil
	}
	return &agents.Plan{Files: []agents.FileAction{
		{Path: filepath.Join(env.Root, ".omp", "mcp.json"), Action: agents.ActionWouldMerge, Keys: []string{"mcpServers"}},
	}}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	if env.Mode == agents.ModeGlobal {
		return res, nil
	}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip Oh My Pi setup (oh-my-pi not detected)")
		return res, nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Oh My Pi integration...")

	path := filepath.Join(env.Root, ".omp", "mcp.json")
	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		servers, ok := root["mcpServers"].(map[string]any)
		if !ok {
			servers = make(map[string]any)
		}
		if _, exists := servers["gortex"]; exists && !opts.Force {
			return false, nil
		}
		servers["gortex"] = map[string]any{
			"command": "gortex",
			"args":    []string{"mcp"},
			"env":     map[string]string{"GORTEX_INDEX_WORKERS": "8"},
		}
		root["mcpServers"] = servers
		return true, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	res.Configured = true
	return res, nil
}
