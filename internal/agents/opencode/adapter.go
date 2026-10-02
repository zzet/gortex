// Package opencode implements the Gortex init integration for
// OpenCode. OpenCode reads its project config from `opencode.json` or
// `opencode.jsonc` at the repo root (and `~/.config/opencode/` for the
// global config); it does NOT read `.opencode/config.json` — Gortex
// wrote there historically and OpenCode silently ignored it, so the MCP
// server was never registered. We now write the MCP stanza to a root
// `opencode.json` (or merge into an existing `opencode.json` /
// `opencode.jsonc`).
//
// OpenCode uses a different MCP schema than the canonical
// Claude / Cursor shape:
//
//	{
//	  "$schema": "https://opencode.ai/config.json",
//	  "mcp": {
//	    "gortex": {
//	      "type": "local",
//	      "command": ["gortex", "mcp", ...],
//	      "environment": {...},
//	      "enabled": true
//	    }
//	  }
//	}
//
// We preserve any existing $schema reference and add one if absent.
//
// # What lands where
//
//	ModeProject: <root>/opencode.json               (mcp stanza)
//	             <root>/AGENTS.md                   (community routing, --skills)
//	             <root>/.opencode/skills/<dir>/SKILL.md  (generated community skills)
//
//	ModeGlobal:  <home>/.config/opencode/skills/<id>/SKILL.md   (curated pack)
//	             <home>/.config/opencode/commands/<id>.md       (slash commands)
//	             <home>/.config/opencode/plugin/gortex.js       (enforcement bridge)
//
// skills.go explains the split (codebase-agnostic playbooks are a
// user-level concern, repo-derived skills are not) and plugin.go explains
// why the one executable artifact is user-level only.
package opencode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "opencode"
const DocsURL = "https://opencode.ai/docs/mcp"
const SchemaURL = "https://opencode.ai/config.json"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if _, err := os.Stat(filepath.Join(env.Root, ".opencode")); err == nil {
		return true, nil
	}
	if p, err := exec.LookPath("opencode"); err == nil && p != "" {
		return true, nil
	}
	if env.Home != "" {
		if _, err := os.Stat(filepath.Join(env.Home, ".config", "opencode")); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// projectConfigPath resolves the file the MCP stanza should be written
// into. OpenCode reads `opencode.json` / `opencode.jsonc` from the repo
// root, so we merge into an existing one (preferring `.jsonc`, since a
// hand-authored, comment-bearing config keeps its extension) and
// otherwise default to a fresh `opencode.json`.
func projectConfigPath(root string) string {
	for _, name := range []string{"opencode.jsonc", "opencode.json"} {
		p := filepath.Join(root, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(root, "opencode.json")
}

// GlobalConfigPath is the user-level config OpenCode reads for every
// project, `~/.config/opencode/opencode.json`. An existing `.jsonc` wins
// for the same reason it does per-repo: a hand-authored, comment-bearing
// config keeps its extension, and writing the sibling `.json` would
// leave the user with two configs and no clue which one OpenCode reads.
//
// $OPENCODE_CONFIG overrides the location entirely. It is honoured only
// when Home is the machine's real home, so the render fence and the
// adapter tests — which set a sandbox Home but inherit the process
// environment — cannot be steered into the developer's real config by an
// exported variable.
func GlobalConfigPath(home string) string {
	if override := strings.TrimSpace(os.Getenv("OPENCODE_CONFIG")); override != "" && isRealUserHome(home) {
		return override
	}
	dir := filepath.Join(home, ".config", "opencode")
	for _, name := range []string{"opencode.jsonc", "opencode.json"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(dir, "opencode.json")
}

// isRealUserHome reports whether home is the machine's actual home, so
// environment overrides apply to a real install but never to a sandbox.
func isRealUserHome(home string) bool {
	real, err := os.UserHomeDir()
	return err == nil && real != "" && real == home
}

// upsertMCPServer returns the mutation both scopes share. Project and
// user configs take the identical `mcp.gortex` entry, and OpenCode's
// schema differs enough from the canonical shape (a `command` array
// rather than command+args, `environment` rather than `env`) that a
// second copy of it would be a standing invitation to drift.
func upsertMCPServer(opts agents.ApplyOpts) func(map[string]any, bool) (bool, error) {
	return func(root map[string]any, _ bool) (bool, error) {
		mcpSection, ok := root["mcp"].(map[string]any)
		if !ok {
			mcpSection = make(map[string]any)
		}
		if _, exists := mcpSection["gortex"]; exists && !opts.Force {
			return false, nil
		}
		mcpSection["gortex"] = map[string]any{
			"type":    "local",
			"command": []string{"gortex", "mcp"},
			"enabled": true,
		}
		root["mcp"] = mcpSection
		if _, hasSchema := root["$schema"]; !hasSchema {
			root["$schema"] = SchemaURL
		}
		return true, nil
	}
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	if env.Mode == agents.ModeGlobal {
		if env.Home != "" {
			p.Files = append(p.Files, agents.FileAction{
				Path: GlobalConfigPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"mcp"},
			})
		}
	} else {
		p.Files = append(p.Files, agents.FileAction{
			Path: projectConfigPath(env.Root), Action: agents.ActionWouldMerge, Keys: []string{"mcp"},
		})
		if env.SkillsRouting != "" {
			p.Files = append(p.Files, agents.FileAction{
				Path: filepath.Join(env.Root, "AGENTS.md"), Action: agents.ActionWouldMerge,
				Keys: []string{"communities-block"},
			})
		}
	}
	p.Files = append(p.Files, planPlugin(env)...)
	skillFiles, err := planSkills(env)
	if err != nil {
		return nil, err
	}
	p.Files = append(p.Files, skillFiles...)
	return p, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip OpenCode setup (OpenCode not detected)")
		return res, nil
	}
	if env.Mode == agents.ModeGlobal {
		return res, a.applyGlobal(env, opts, res)
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up OpenCode integration...")

	path := projectConfigPath(env.Root)
	// Gortex used to write `.opencode/config.json`, which OpenCode does
	// not read. If that stale file is still around, point the user at
	// the config we're actually writing now.
	if legacy := filepath.Join(env.Root, ".opencode", "config.json"); legacy != path {
		if _, err := os.Stat(legacy); err == nil {
			internalutil.Logf(env.Stderr, "[gortex init] note: %s is no longer read by OpenCode; writing the MCP config to %s instead", legacy, filepath.Base(path))
		}
	}
	action, err := agents.MergeJSON(env.Stderr, path, upsertMCPServer(opts), opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)

	// AGENTS.md gets a marker-guarded community-routing block when
	// skills were generated (--skills, default on in `gortex init`).
	// The codex adapter targets the same file with the same markers
	// so a repo running both adapters converges on one block.
	if env.SkillsRouting != "" {
		agentsMdPath := filepath.Join(env.Root, "AGENTS.md")
		routingAction, err := agents.UpsertMarkedBlock(env.Stderr, agentsMdPath, env.SkillsRouting,
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, routingAction)
	}

	// Generated community skills describe this repo, so they are the one
	// skill set that belongs in the repo tree. skills.go documents the
	// flat layout and the kebab-case gate.
	skillActions, err := applySkills(env, opts)
	if err != nil {
		return res, fmt.Errorf("opencode skills: %w", err)
	}
	res.Files = append(res.Files, skillActions...)

	res.Configured = true
	return res, nil
}

// applyGlobal handles `gortex install`: the codebase-agnostic artifacts
// that belong to the user, not to any one repo — the MCP server, the
// curated playbook and slash-command packs, and the enforcement bridge.
//
// The MCP registration is the load-bearing one and it must stay here.
// This function also installs 21 skills and a plugin that tell the model
// to reach for the Gortex tools on every turn; without a server entry
// those tools are not mounted, so the user gets an OpenCode that has
// been taught to ask for something it cannot call. Registering per-repo
// from `gortex init` alone is not a substitute — a user who runs only
// `gortex install`, which is the documented machine-wide step, would
// never get a server at all.
func (a *Adapter) applyGlobal(env agents.Env, opts agents.ApplyOpts, res *agents.Result) error {
	if env.Home == "" {
		return fmt.Errorf("opencode: global mode requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex install] setting up OpenCode integration...")

	mcpAction, err := agents.MergeJSON(env.Stderr, GlobalConfigPath(env.Home), upsertMCPServer(opts), opts)
	if err != nil {
		return fmt.Errorf("opencode global config: %w", err)
	}
	res.Files = append(res.Files, mcpAction)

	pluginActions, err := applyPlugin(env, opts)
	if err != nil {
		return fmt.Errorf("opencode plugin: %w", err)
	}
	res.Files = append(res.Files, pluginActions...)

	skillActions, err := applySkills(env, opts)
	if err != nil {
		return fmt.Errorf("opencode skills: %w", err)
	}
	res.Files = append(res.Files, skillActions...)

	res.Configured = true
	return nil
}
