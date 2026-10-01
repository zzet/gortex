package opencode

// remove.go is the counterpart to applyGlobal for `gortex uninstall
// --global`.
//
// # Ownership, not location
//
// Every deletion is gated on evidence Gortex authored the thing being
// deleted, never on the path it sits at. `~/.config/opencode/skills` and
// `.../commands` are SHARED trees — a user's own skills and slash commands
// live there beside ours — so removing either directory would delete their
// work. The predicate is the one skills.go writes with: a shipped id whose
// file is still byte-identical to the body we render. A file that differs
// is KEPT with a warning, the same never-clobber posture writeCuratedFile
// takes on the way in.
//
// The bridge plugin is the one file Gortex owns end-to-end, and even it is
// checked: `plugin/gortex.js` is deleted only when it carries PluginMarker,
// so a same-named plugin somebody else wrote is left alone.
//
// The user-level `mcp.gortex` entry is stripped from
// ~/.config/opencode/opencode.json, leaving every other server and every
// other top-level key untouched. The file itself is never deleted: it is
// the user's config, and we only ever added one key to it.
//
// # Stop-line: no instructions block at user scope
//
// applyGlobal writes none, so RemoveGlobal removes none. OpenCode's
// community routing block lives in the repo's AGENTS.md and comes out
// with the repo-level half of `gortex uninstall`.

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

// RemoveGlobal strips the user-level OpenCode footprint `gortex install`
// wrote. Returns the number of artifacts removed-or-cleaned and any
// per-artifact failures — a partial clean still reports rather than
// aborting. Signature mirrors claudecode.Adapter.RemoveGlobal so `gortex
// uninstall` can call every host through the same shape.
func (a *Adapter) RemoveGlobal(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	if env.Home == "" {
		return 0, []string{"opencode: global cleanup requires a resolved home directory"}
	}

	// 1. ~/.config/opencode/plugin/gortex.js — the enforcement bridge.
	pluginRemoved, pluginFailures := removePlugin(env, opts)
	removed += pluginRemoved
	failures = append(failures, pluginFailures...)

	// 2. ~/.config/opencode/{skills,commands}/ — the curated packs.
	packRemoved, packFailures := removeCuratedPack(env, opts)
	removed += packRemoved
	failures = append(failures, packFailures...)

	// 3. The user-level mcp.gortex entry.
	mcpRemoved, mcpFailures := removeGlobalMCPServer(env, opts)
	removed += mcpRemoved
	failures = append(failures, mcpFailures...)

	return removed, failures
}

// removeGlobalMCPServer strips `mcp.gortex` from the user config. The
// `mcp` map itself is dropped only when our entry was the last one in it,
// so a user who also runs other servers keeps their section — and the
// config file is never deleted, because everything else in it is theirs.
//
// agents.RemoveMCPServer cannot be reused: it is hardcoded to the
// canonical `mcpServers` key, and OpenCode spells the section `mcp`.
func removeGlobalMCPServer(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	path := GlobalConfigPath(env.Home)
	if _, err := os.Stat(path); err != nil {
		return 0, nil
	}
	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		section, ok := root["mcp"].(map[string]any)
		if !ok {
			return false, nil
		}
		if _, exists := section["gortex"]; !exists {
			return false, nil
		}
		delete(section, "gortex")
		if len(section) == 0 {
			delete(root, "mcp")
		} else {
			root["mcp"] = section
		}
		return true, nil
	}, opts)
	if err != nil {
		return 0, []string{fmt.Sprintf("opencode: %s: %v", path, err)}
	}
	if action.Action == agents.ActionSkip {
		return 0, nil
	}
	return 1, nil
}

// GlobalArtifacts lists the user-level OpenCode paths that currently carry
// a Gortex footprint, sorted. It applies the SAME ownership tests
// RemoveGlobal does — a customised skill is absent from this list exactly
// because removal will keep it — so the uninstall wizard's preview can
// never promise a deletion that will not happen, nor stay silent about one
// that will.
func GlobalArtifacts(home string) []string {
	if home == "" {
		return nil
	}
	var present []string

	if path := PluginPath(home); opencodeFileContains(path, PluginMarker) {
		present = append(present, path)
	}
	if path := V1PluginPath(home); opencodeFileContains(path, PluginMarker) {
		present = append(present, path)
	}
	// The config is listed only when our server entry is actually in it,
	// so the preview matches what removal will do rather than naming a
	// file that will be left byte-identical.
	if path := GlobalConfigPath(home); hasGortexMCPServer(path) {
		present = append(present, path)
	}
	for path, shipped := range ownedPackFiles(home) {
		if isShippedOpenCodeFile(path, shipped) {
			present = append(present, path)
		}
	}

	sort.Strings(present)
	return present
}

// removePlugin deletes the bridge, but only when the file on disk is
// actually ours. PluginMarker is the argv element that makes the file a
// Gortex bridge; matching on it rather than on the file name is what keeps
// a same-named plugin somebody else wrote out of the blast radius.
func removePlugin(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	// Remove V2 plugin
	v2Path := PluginPath(env.Home)
	data, err := os.ReadFile(v2Path)
	if errors.Is(err, os.ErrNotExist) {
		// V2 not present, continue to check V1
	} else if err != nil {
		return 0, []string{fmt.Sprintf("%s: %v", v2Path, err)}
	} else if strings.Contains(string(data), PluginMarker) {
		if opts.DryRun {
			// Will count V2 + package.json + V1 below
		} else {
			if err := os.Remove(v2Path); err != nil {
				return 0, []string{fmt.Sprintf("%s: %v", v2Path, err)}
			}
			internalutil.Logf(env.Stderr, "[gortex uninstall] removed %s", v2Path)

			// Also remove the package.json
			pkgPath := PluginPkgPath(env.Home)
			if err := os.Remove(pkgPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 1, []string{fmt.Sprintf("%s: %v", pkgPath, err)}
			}
			internalutil.Logf(env.Stderr, "[gortex uninstall] removed %s", pkgPath)

			// Prune the plugin directory and its parent if empty
			pruneEmptyDir(filepath.Dir(v2Path))
			pruneEmptyDir(filepath.Dir(filepath.Dir(v2Path)))
		}
		removed++
	}

	// Remove V1 plugin
	v1Path := V1PluginPath(env.Home)
	data, err = os.ReadFile(v1Path)
	if errors.Is(err, os.ErrNotExist) {
		// V1 not present
	} else if err != nil {
		return removed, []string{fmt.Sprintf("%s: %v", v1Path, err)}
	} else if strings.Contains(string(data), PluginMarker) {
		if !opts.DryRun {
			if err := os.Remove(v1Path); err != nil {
				return removed, []string{fmt.Sprintf("%s: %v", v1Path, err)}
			}
			internalutil.Logf(env.Stderr, "[gortex uninstall] removed %s", v1Path)
			pruneEmptyDir(filepath.Dir(v1Path))
		}
		removed++
	}

	if opts.DryRun {
		// Dry run: count what would be removed.
		// Only count files that exist AND contain PluginMarker (i.e., are ours).
		// V2 plugin file was already counted above if it had PluginMarker.
		// Package.json is only ours if the V2 plugin file is ours (same dir).
		v2Path := PluginPath(env.Home)
		if v2Present := opencodeFileContains(v2Path, PluginMarker); v2Present {
			// Count package.json only if V2 plugin is ours
			if _, err := os.Stat(PluginPkgPath(env.Home)); err == nil {
				removed++
			}
		}
		// Count V1 plugin only if it has PluginMarker
		v1Path := V1PluginPath(env.Home)
		if opencodeFileContains(v1Path, PluginMarker) {
			removed++
		}
		return removed, nil
	}

	return removed, nil
}

// ownedPackFiles maps every curated skill / command path to the body
// Gortex ships there. One map so the remover and the preview enumerate
// exactly the same set. A corpus that fails to parse yields nothing —
// the package's own tests turn that into a red build, and reporting an
// empty footprint beats guessing at one.
func ownedPackFiles(home string) map[string]string {
	out := make(map[string]string)
	if skills, err := Skills(); err == nil {
		root := globalSkillsDir(home)
		for _, s := range skills {
			out[filepath.Join(root, s.ID, skillFileName)] = renderSkill(s)
		}
	}
	if commands, err := Commands(); err == nil {
		root := globalCommandsDir(home)
		for _, c := range commands {
			out[filepath.Join(root, c.ID+commandFileExt)] = renderCommand(c)
		}
	}
	return out
}

// removeCuratedPack deletes the shipped skills and commands whose bytes
// are still ours, prunes each skill's own directory when it empties, and
// then the two roots. A user's skill directory in the same root is never
// looked at, and the roots survive as long as it does.
func removeCuratedPack(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	owned := ownedPackFiles(env.Home)
	paths := make([]string, 0, len(owned))
	for path := range owned {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		n, f := removeOwnedFile(env.Stderr, path, owned[path], opts)
		removed += n
		failures = append(failures, f...)
		if n > 0 && !opts.DryRun && filepath.Base(path) == skillFileName {
			// Skills are <id>/SKILL.md; commands are flat <id>.md files.
			pruneEmptyDir(filepath.Dir(path))
		}
	}
	if removed > 0 && !opts.DryRun {
		pruneEmptyDir(globalSkillsDir(env.Home))
		pruneEmptyDir(globalCommandsDir(env.Home))
	}
	return removed, failures
}

// removeOwnedFile deletes path only when its bytes are still exactly what
// Gortex shipped. Anything else is the user's edit and is kept with a
// warning, mirroring writeCuratedFile's never-clobber posture on the way
// in.
func removeOwnedFile(w io.Writer, path, shipped string, opts agents.ApplyOpts) (removed int, failures []string) {
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	if string(existing) != shipped {
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

func isShippedOpenCodeFile(path, shipped string) bool {
	data, err := os.ReadFile(path)
	return err == nil && string(data) == shipped
}

// pruneEmptyDir removes dir only when it holds nothing. os.Remove refuses
// a non-empty directory, which is exactly the guard we want: a user file
// sitting next to one we deleted keeps its directory alive.
func pruneEmptyDir(dir string) {
	_ = os.Remove(dir)
}

func opencodeFileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}

// hasGortexMCPServer reports whether the config at path actually carries
// our server entry. It parses rather than substring-matching: "gortex"
// appears in a user's config for plenty of innocent reasons, and the
// uninstall preview must not claim a file it will then leave alone.
// An unreadable or malformed config reads as "nothing of ours", which is
// the safe direction — removal will skip it too.
func hasGortexMCPServer(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(agents.StripJSONComments(data), &root); err != nil {
		return false
	}
	section, ok := root["mcp"].(map[string]any)
	if !ok {
		return false
	}
	_, exists := section["gortex"]
	return exists
}
