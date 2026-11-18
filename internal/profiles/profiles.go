// Package profiles defines the instruction profiles a machine can run
// Gortex under: named bundles of (a) an instructions body written to
// ~/.gortex/instructions/<name>.md and @-included from the agent's
// rules file, (b) a tool-surface preset for the MCP server, (c) a
// skills subset for skill-aware agents, and (d) a hook-verbosity tier.
//
// The four surfaces are generated from ONE table (Table) so they
// cannot drift: the localization preset's eager tool list is the same
// slice the instructions body renders its rule table from, and the
// hook tier rides on the same Profile row the installer materialises.
//
// The package is a leaf: internal/agents (install), internal/mcp
// (session tool policy), internal/hooks (verbosity) and cmd/gortex
// (the `gortex instructions` verb) all import it, never the reverse.
package profiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/platform"
)

// HookTier selects how much ambient text the lifecycle hooks inject
// per session / per turn. Lean keeps every positioning cue (the
// prefer-graph-tools rule, the deny-hook warning, the tool-name
// mappings) and trims only status prose around them.
type HookTier string

const (
	// HookTierStandard is today's hook output: full daemon readiness,
	// cwd coverage, rule preamble, prompt-derived symbol injection.
	HookTierStandard HookTier = "standard"
	// HookTierLean compresses status blocks to one line and caps the
	// per-turn prompt injection, keeping the rule cues intact.
	HookTierLean HookTier = "lean"
)

// DefaultName is the profile installed and active when the user never
// ran `gortex instructions switch`.
const DefaultName = "core"

// ActiveEnv overrides the on-disk active-profile state when set to a
// known profile name — the seam benchmarks and CI use to pin a profile
// per process tree without touching the user's machine state.
const ActiveEnv = "GORTEX_INSTRUCTIONS_PROFILE"

// Profile is one row of the single source-of-truth table.
type Profile struct {
	Name    string
	Summary string
	// ToolPreset is the MCP tool-surface preset sessions default to
	// while this profile is active. Empty keeps the server's existing
	// client-aware default (named MCP clients get the compact public
	// surface; uninitialized sessions keep the server's global preset).
	ToolPreset string
	// EagerTools, when non-nil, is the eager allow-set of the preset
	// named by ToolPreset. internal/mcp builds the preset from this
	// slice and the instructions body renders its rule rows from it —
	// one list, two surfaces.
	EagerTools []string
	// Skills is the subset of shipped gortex-* skills this profile
	// keeps installed. Nil means all shipped skills.
	Skills   []string
	HookTier HookTier

	body func() string
}

// localizationEagerTools is the lean "where is the code that does X"
// surface: orient, search, trace, read. It is the single source for
// both the `localization` tool preset (internal/mcp) and the rule
// table in the localization instructions body.
var localizationEagerTools = []string{
	// the one-shot localization opener
	"explore",
	// orient
	"smart_context", "index_health",
	// search
	"search_symbols", "search_text",
	// trace
	"find_usages", "get_callers", "find_implementations",
	// read — batch_symbols is load-bearing for turn economy: without a
	// multi-symbol read, follow-ups on a localization neighborhood cost
	// one turn per symbol (measured +1 median turn on the localization
	// benchmark when it was left out of this surface).
	"get_symbol_source", "batch_symbols", "get_file_summary", "read_file",
}

// Table returns the profile table. Callers must not mutate the
// returned slices.
func Table() []Profile {
	return []Profile{
		{
			Name:     "core",
			Summary:  "balanced default — full workflow guidance, client-aware tool surface",
			HookTier: HookTierStandard,
			body:     coreBody,
		},
		{
			Name:    "localization",
			Summary: "lean code-finding guidance — diet instructions body, proven tool surface",
			// ToolPreset is intentionally empty: the profile diets the
			// @-included body (the per-turn ambient) and keeps the
			// client-aware default tool surface. The deeper 14-tool
			// `localization` preset (built from EagerTools below) stays
			// available via GORTEX_TOOLS=localization — benchmarked, it
			// cut tools/list ~30% but cost file-hits on cap-adjacent
			// sessions, so it is opt-in rather than the profile default.
			EagerTools: localizationEagerTools,
			Skills:     []string{"gortex-explore", "gortex-guide", "gortex-debug"},
			HookTier:   HookTierStandard,
			body:       localizationBody,
		},
		{
			Name:       "full",
			Summary:    "maximum guidance — the whole documented dev-cycle surface eager",
			ToolPreset: "core",
			HookTier:   HookTierStandard,
			body:       fullBody,
		},
	}
}

// Names lists the profile names in table order.
func Names() []string {
	t := Table()
	out := make([]string, len(t))
	for i, p := range t {
		out[i] = p.Name
	}
	return out
}

// ByName resolves one profile row.
func ByName(name string) (Profile, bool) {
	for _, p := range Table() {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// LocalizationEagerTools returns the eager allow-set of the
// localization preset — the slice internal/mcp registers under the
// preset name so the tool surface and the instructions body cannot
// drift.
func LocalizationEagerTools() []string {
	return append([]string(nil), localizationEagerTools...)
}

// Body renders the profile's instructions markdown.
func (p Profile) Body() string {
	if p.body == nil {
		return ""
	}
	return p.body()
}

// DefaultDir is where profile files and the active state live:
// <gortex data dir>/instructions (default ~/.gortex/instructions).
func DefaultDir() string {
	return filepath.Join(platform.DataDir(), "instructions")
}

// ActiveFileName is the file agents @-include. It is always a byte
// copy of the active profile's <name>.md — never a symlink (Windows
// parity, and @-include resolution should never depend on link
// support).
const ActiveFileName = "active.md"

const stateFileName = "active.json"

type state struct {
	Name       string `json:"name"`
	SwitchedAt string `json:"switched_at,omitempty"`
}

// ActiveName reports which profile is active for dir: the ActiveEnv
// override when it names a known profile, else the recorded state,
// else DefaultName. Unknown or unreadable state degrades to
// DefaultName — a missing file must behave exactly like a machine
// that never switched.
func ActiveName(dir string) string {
	if v := strings.TrimSpace(os.Getenv(ActiveEnv)); v != "" {
		if _, ok := ByName(v); ok {
			return v
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		return DefaultName
	}
	var st state
	if json.Unmarshal(raw, &st) != nil {
		return DefaultName
	}
	if _, ok := ByName(st.Name); !ok {
		return DefaultName
	}
	return st.Name
}

// Active resolves the active profile row for dir.
func Active(dir string) Profile {
	p, _ := ByName(ActiveName(dir))
	return p
}

// ActiveToolPreset is the one-call convenience internal/mcp uses when
// resolving a session's default tool surface.
func ActiveToolPreset() string {
	return Active(DefaultDir()).ToolPreset
}

// ActiveHookTier is the one-call convenience internal/hooks uses.
func ActiveHookTier() HookTier {
	return Active(DefaultDir()).HookTier
}

// Generate materialises every profile's <name>.md under dir and
// refreshes ActiveFileName to a byte copy of the active profile.
// Idempotent: unchanged files are not rewritten (no spurious mtime
// bumps on re-install).
func Generate(dir string, binaryVersion ...string) error {
	_, err := rewriteGeneratedProfiles(dir, "", optionalBinaryVersion(binaryVersion), false)
	return err
}

// Switch changes only generated profiles and the stored selection. Production
// callers pass their canonical version; unversioned library callers retain the
// compatibility API without claiming a binary version they do not know.
func Switch(dir, name string, binaryVersion ...string) (Profile, error) {
	p, ok := ByName(name)
	if !ok {
		return Profile{}, fmt.Errorf("unknown instruction profile %q (known: %s)", name, strings.Join(Names(), ", "))
	}
	_, err := rewriteGeneratedProfiles(dir, name, optionalBinaryVersion(binaryVersion), false)
	return p, err
}

// writeIfChanged writes content to path unless it is already
// byte-identical.
func writeIfChanged(path string, content []byte) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(content) {
		return nil
	}
	return atomicWrite(path, content)
}

// atomicWrite is temp-file + rename in path's directory.
func atomicWrite(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := platform.ReplaceFile(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Remove deletes the generated instructions directory. Used by
// uninstall flows; missing dir is not an error.
func Remove(dir string) error {
	err := os.RemoveAll(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
