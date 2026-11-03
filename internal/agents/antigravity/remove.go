package antigravity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

// RemoveGlobal removes the user-level Antigravity footprint written by
// `gortex install`. It cleans native MCP registrations, hooks, rules,
// plugin manifest, curated skills, and Knowledge Item artifacts.
// Preserves any user-customized files and non-Gortex config keys.
func (a *Adapter) RemoveGlobal(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	if env.Home == "" {
		return 0, []string{"antigravity: global cleanup requires a resolved home directory"}
	}
	w := env.Stderr

	count := func(action agents.FileAction, err error, label string) {
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", label, err))
			return
		}
		if action.Action != agents.ActionSkip {
			removed++
		}
	}

	// 1. Remove "gortex" server from all MCP config files.
	for _, mcpPath := range mcpConfigPaths(env.Home) {
		mcpAction, err := agents.MergeJSONC(w, mcpPath, func(root map[string]any, _ bool) (bool, error) {
			return agents.RemoveMCPServer(root, "gortex"), nil
		}, opts)
		count(mcpAction, err, mcpPath)
	}

	// 2. Remove Gortex hooks from hooks.json.
	hookPath := HookConfigPath(env.Home)
	hookAction, err := removeAntigravityHooks(env, opts)
	count(hookAction, err, hookPath)

	// 3. Remove global instructions rule block.
	insPath := GlobalInstructionsPath(env.Home)
	insAction, err := removeGlobalInstructions(env, opts)
	count(insAction, err, insPath)

	// 4. Remove plugin.json manifest if owned.
	manifestPath := GlobalPluginManifestPath(env.Home)
	manifestAction, err := removePluginManifest(env, opts)
	count(manifestAction, err, manifestPath)

	// 5. Remove Knowledge Item artifacts.
	kiRemoved, kiFailures := removeKnowledgeItems(env, opts)
	removed += kiRemoved
	failures = append(failures, kiFailures...)

	// 6. Remove curated skills.
	skillsRemoved, skillsFailures := removeAntigravityOwnedSkills(env, opts)
	removed += skillsRemoved
	failures = append(failures, skillsFailures...)

	return removed, failures
}

// GlobalArtifacts lists all user-level Antigravity files that currently
// carry a Gortex footprint.
func GlobalArtifacts(home string) []string {
	if home == "" {
		return nil
	}
	var present []string

	for _, mcpPath := range mcpConfigPaths(home) {
		if mcpConfigHasGortexServer(mcpPath) {
			present = append(present, mcpPath)
		}
	}

	hookPath := HookConfigPath(home)
	if hookConfigHasGortexEntry(hookPath) {
		present = append(present, hookPath)
	}

	insPath := GlobalInstructionsPath(home)
	if fileContainsString(insPath, agents.GlobalRulesStartMarker) {
		present = append(present, insPath)
	}

	manifestPath := GlobalPluginManifestPath(home)
	if isOwnedPluginManifest(manifestPath) {
		present = append(present, manifestPath)
	}

	kiBase := filepath.Join(home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	for _, rel := range []string{"metadata.json", filepath.Join("artifacts", "gortex-instructions.md")} {
		p := filepath.Join(kiBase, rel)
		if isShippedKI(p) {
			present = append(present, p)
		}
	}

	for _, id := range CuratedSkillNames() {
		p := curatedSkillPath(home, id)
		if isShippedSkill(p, id) {
			present = append(present, p)
		}
	}

	sort.Strings(present)
	return present
}

func removeAntigravityHooks(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, error) {
	path := HookConfigPath(env.Home)
	action := agents.FileAction{Path: path, Action: agents.ActionSkip}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		action.Reason = "absent"
		return action, nil
	}
	if err != nil {
		return agents.FileAction{}, fmt.Errorf("read %s: %w", path, err)
	}

	var root map[string]any
	if json.Unmarshal(agents.StripJSONComments(data), &root) != nil {
		action.Reason = "unparsable"
		return action, nil
	}

	changed := false
	if _, exists := root[hookPluginName]; exists {
		delete(root, hookPluginName)
		changed = true
	}

	if !changed {
		action.Reason = "no-gortex-hooks"
		return action, nil
	}

	if len(root) == 0 {
		if opts.DryRun {
			return agents.FileAction{Path: path, Action: agents.ActionWouldDelete}, nil
		}
		if err := os.Remove(path); err != nil {
			return agents.FileAction{}, fmt.Errorf("remove %s: %w", path, err)
		}
		internalutil.Logf(env.Stderr, "[gortex uninstall] removed %s", path)
		pruneEmptyDir(filepath.Dir(path))
		return agents.FileAction{Path: path, Action: agents.ActionDelete}, nil
	}

	if opts.DryRun {
		return agents.FileAction{Path: path, Action: agents.ActionWouldMerge, Keys: []string{"hooks"}}, nil
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return agents.FileAction{}, fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := agents.AtomicWriteFile(path, out, 0o644); err != nil {
		return agents.FileAction{}, err
	}
	internalutil.Logf(env.Stderr, "[gortex uninstall] removed the gortex hook entries from %s", path)
	return agents.FileAction{Path: path, Action: agents.ActionMerge, Keys: []string{"hooks"}}, nil
}

func removeGlobalInstructions(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, error) {
	path := GlobalInstructionsPath(env.Home)
	action, err := agents.UpsertMarkedBlock(env.Stderr, path, "",
		agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, opts)
	if err != nil {
		return action, err
	}
	if opts.DryRun {
		return action, nil
	}

	// If the file is now empty or missing, delete it and prune rules directory.
	if data, err := os.ReadFile(path); err == nil {
		if strings.TrimSpace(string(data)) == "" {
			_ = os.Remove(path)
			pruneEmptyDir(filepath.Dir(path))
			return agents.FileAction{Path: path, Action: agents.ActionDelete}, nil
		}
	}
	return action, nil
}

func removePluginManifest(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, error) {
	path := GlobalPluginManifestPath(env.Home)
	if !isOwnedPluginManifest(path) {
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "not-owned"}, nil
	}
	if opts.DryRun {
		return agents.FileAction{Path: path, Action: agents.ActionWouldDelete}, nil
	}
	if err := os.Remove(path); err != nil {
		return agents.FileAction{}, err
	}
	internalutil.Logf(env.Stderr, "[gortex uninstall] removed %s", path)
	pruneEmptyDir(filepath.Dir(path))               // plugins/gortex
	pruneEmptyDir(filepath.Dir(filepath.Dir(path))) // plugins
	return agents.FileAction{Path: path, Action: agents.ActionDelete}, nil
}

func isOwnedPluginManifest(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	name, _ := m["name"].(string)
	return strings.TrimSpace(name) == "gortex"
}

func removeKnowledgeItems(env agents.Env, opts agents.ApplyOpts) (int, []string) {
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	metaPath := filepath.Join(kiBase, "metadata.json")
	instrPath := filepath.Join(kiBase, "artifacts", "gortex-instructions.md")

	removed := 0
	var failures []string

	for path, allowed := range map[string][]string{
		metaPath:  {Metadata, v060Metadata},
		instrPath: {Instructions, v060Instructions},
	} {
		n, f := removeOwnedOneOf(env.Stderr, path, allowed, opts)
		removed += n
		failures = append(failures, f...)
	}

	if removed > 0 && !opts.DryRun {
		pruneEmptyDir(filepath.Join(kiBase, "artifacts"))
		pruneEmptyDir(kiBase)
		pruneEmptyDir(filepath.Dir(kiBase)) // knowledge
	}
	return removed, failures
}

func removeOwnedOneOf(w io.Writer, path string, allowedBodies []string, opts agents.ApplyOpts) (int, []string) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	content := string(data)
	matched := false
	for _, allowed := range allowedBodies {
		if content == allowed {
			matched = true
			break
		}
	}
	if !matched {
		internalutil.Warnf(w, "keeping customised %s", path)
		return 0, nil
	}
	if opts.DryRun {
		return 1, nil
	}
	if err := os.Remove(path); err != nil {
		return 0, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	internalutil.Logf(w, "[gortex uninstall] removed %s", path)
	return 1, nil
}

func isShippedKI(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	s := string(data)
	return s == Metadata || s == v060Metadata || s == Instructions || s == v060Instructions
}

func removeAntigravityOwnedSkills(env agents.Env, opts agents.ApplyOpts) (int, []string) {
	removed := 0
	var failures []string

	for _, id := range CuratedSkillNames() {
		path := curatedSkillPath(env.Home, id)
		if !isShippedSkill(path, id) {
			continue
		}
		if opts.DryRun {
			removed++
			continue
		}
		if err := os.Remove(path); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		removed++
		internalutil.Logf(env.Stderr, "[gortex uninstall] removed %s", path)
		pruneEmptyDir(filepath.Dir(path)) // skill dir
	}

	if removed > 0 && !opts.DryRun {
		pruneEmptyDir(curatedSkillsRoot(env.Home)) // skills
	}
	return removed, failures
}

func isShippedSkill(path, id string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	shipped, ok := CuratedSkills()[id]
	return ok && string(data) == shipped
}

func mcpConfigHasGortexServer(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(agents.StripJSONComments(data), &root) != nil {
		return false
	}
	_, ok := root.MCPServers["gortex"]
	return ok
}

func hookConfigHasGortexEntry(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root map[string]any
	if json.Unmarshal(agents.StripJSONComments(data), &root) != nil {
		return false
	}
	for _, specAny := range root {
		spec, ok := specAny.(map[string]any)
		if !ok {
			continue
		}
		for _, canonicalEvent := range HookEvents {
			onDiskEvent := antigravityHookEventNames[canonicalEvent]
			if countGortexEventHandlers(onDiskEvent, spec[onDiskEvent]) > 0 {
				return true
			}
		}
	}
	return false
}

func fileContainsString(path, needle string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}

func pruneEmptyDir(dir string) {
	_ = os.Remove(dir)
}
