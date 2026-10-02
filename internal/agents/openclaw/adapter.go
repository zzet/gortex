// Package openclaw implements the Gortex init integration for
// OpenClaw. OpenClaw's config lives at ~/.openclaw/openclaw.json
// in JSON5 format; MCP servers go under the nested "mcp.servers"
// table:
//
//	{
//	  "mcp": {
//	    "servers": {
//	      "gortex": {"command": "gortex", "args": [...], "env": {...}}
//	    }
//	  }
//	}
//
// We emit plain JSON — OpenClaw's JSON5 parser accepts vanilla
// JSON, and writing strict JSON sidesteps dependency on a JSON5
// encoder. Users can hand-edit the file afterwards and JSON5
// features (comments, trailing commas) won't be stripped because
// we never read back; we always overwrite the gortex entry only.
//
// Docs: https://docs.openclaw.ai/gateway/configuration-reference.md
package openclaw

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "openclaw"
const DocsURL = "https://docs.openclaw.ai/cli/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("openclaw"); err == nil && p != "" {
		return true, nil
	}
	if env.Home == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".openclaw")); err == nil {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	if env.Home == "" {
		return &agents.Plan{}, nil
	}
	return &agents.Plan{Files: []agents.FileAction{{
		Path:   filepath.Join(env.Home, ".openclaw", "openclaw.json"),
		Action: agents.ActionWouldMerge,
		Keys:   []string{"mcp"},
	}}}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip OpenClaw setup (openclaw not detected)")
		return res, nil
	}
	if env.Home == "" {
		return res, fmt.Errorf("openclaw: requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up OpenClaw integration...")

	path := filepath.Join(env.Home, ".openclaw", "openclaw.json")
	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		mcp, ok := root["mcp"].(map[string]any)
		if !ok {
			mcp = make(map[string]any)
		}
		servers, ok := mcp["servers"].(map[string]any)
		if !ok {
			servers = make(map[string]any)
		}
		if _, exists := servers["gortex"]; exists && !opts.Force {
			return false, nil
		}
		servers["gortex"] = map[string]any{
			"command": "gortex",
			"args":    []string{"mcp"},
		}
		mcp["servers"] = servers
		root["mcp"] = mcp
		return true, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	res.Configured = true
	return res, nil
}
