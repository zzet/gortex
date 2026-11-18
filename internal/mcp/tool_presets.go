package mcp

import (
	"context"
	"os"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/profiles"
)

// ToolPolicyConfig is the operator-facing description of a restricted
// tool surface: a named preset, per-tool allow/deny deltas, and a mode.
// It is the wire between the `mcp.tools` config block, the GORTEX_TOOLS
// / GORTEX_TOOLS_MODE env overrides, and the resolved in-memory
// toolPolicy. Zero value (empty preset, no deltas) means "no
// restriction" — the full surface.
type ToolPolicyConfig struct {
	Preset         string
	Mode           string // "hide" | "defer" — default hide
	Allow          []string
	Deny           []string
	OperatorPinned bool // explicit config provenance, including core/defer
}

const (
	// toolPolicyModeHide removes non-allowed tools from tools/list and
	// hard-blocks calls to them. The minimal, locked-down surface a
	// headless harness wants — works identically on every client.
	toolPolicyModeHide = "hide"
	// toolPolicyModeDefer keeps non-allowed tools out of the cold
	// tools/list but still reachable via the tools_search discovery
	// tool (which promotes on demand). Only effective on clients that
	// honour notifications/tools/list_changed.
	toolPolicyModeDefer = "defer"

	toolPresetEnv     = "GORTEX_TOOLS"
	toolPresetModeEnv = "GORTEX_TOOLS_MODE"
)

// corePresetTools is the curated "classic developer" surface published
// eagerly by default — the workhorses a regular dev reaches for across
// the whole cycle: orient, search/navigate, read, edit, verify/test,
// analyze, review, and the mandatory memory steps. It is the allow-set
// of the `core` preset, which is the server default (in defer mode):
// these ship in the cold tools/list, everything else is deferred behind
// tools_search. Sized to cover the documented mandatory workflow end to
// end so the common task never needs a discovery round-trip.
//
// tool_profile and tools_search are always kept on top of any preset
// (isAlwaysKeptTool), so they are intentionally absent here.
//
// NB: this is the DEFAULT-surface roster, distinct from
// lazy_tools.go::hotEagerTools (the GORTEX_LAZY_TOOLS=1 eager set) — the
// two answer different questions and are allowed to diverge.
var corePresetTools = []string{
	// orient — explore is the one-shot localization verb, the loud first
	// move for any task-shaped request (ranked neighborhood + source +
	// call paths in one call). index_health is the cheap liveness check
	// the workflow recommends, so it ships eagerly too (no discovery
	// round-trip for the documented first step). get_active_project stays
	// deferred: it is only registered in multi-repo mode, so it can't be
	// an unconditional core tool.
	"explore", "smart_context", "get_repo_outline", "graph_stats", "index_health",
	// search / navigate
	"search_symbols", "search_text", "find_files", "find_usages",
	"find_implementations", "get_callers", "get_call_chain",
	"get_dependencies", "get_dependents",
	// read
	"read_file", "get_symbol", "get_symbol_source", "get_file_summary",
	"get_editing_context",
	// edit
	"edit_file", "edit_symbol", "write_file", "batch_edit", "rename_symbol",
	// verify / test
	"verify_change", "get_diagnostics", "check_guards", "get_test_targets",
	// analyze (61-kind dispatcher — one name, broad coverage)
	"analyze",
	// review / commit
	"detect_changes", "diff_context", "review",
	// memory (the mandatory save/recall/surface workflow)
	"surface_memories", "save_note", "store_memory",
}

// agentFloorTools is the measured working set retained by the legacy `agent`
// compatibility preset. It covers a navigate → read → edit → verify
// cycle; everything else defers behind tools_search. Named MCP clients now use
// the compact surface unless a higher-precedence policy explicitly selects
// this preset. tool_profile / tools_search are always kept on top
// (isAlwaysKeptTool).
var agentFloorTools = []string{
	// orient — explore is the one-shot localization verb: the obvious
	// opening move for any task-shaped request. It returns the ranked
	// neighborhood (symbols + source + call paths + file map) in one
	// call, folding the granular search/read/callers loop the agent would
	// otherwise grind through into a single turn.
	"explore", "smart_context", "index_health",
	// search / navigate
	"search_symbols", "find_usages", "find_implementations",
	"get_callers", "get_call_chain", "get_dependencies", "get_dependents",
	// read — batch_symbols is the follow-up reader after explore: it
	// fetches many bodies in one call, so a residual read loop collapses
	// into a single turn.
	"get_symbol_source", "batch_symbols", "get_file_summary",
	"get_editing_context", "read_file",
	// edit / verify
	"edit_file", "write_file", "verify_change",
}

// agentTailTools is the workflow-mandated memory trio — the negotiable
// tail added on top of the floor ONLY when the byte budget holds. It does
// not: even under aggressive compaction the trio's rich descriptions push
// the cold tools/list past its ceiling, so the trio stays DEFERRED (still
// reachable by name via tools_search / promote-on-demand and surfaced by
// the related_tools cue). Cut from here first, never from the floor.
var agentTailTools = []string{
	"distill_session", "surface_memories", "store_memory",
}

// agentPresetTools is the eager surface of the `agent` preset: exactly the
// floor. The tail is deferred (see agentTailTools). Kept lean enough that
// the cold tools/list stays a few thousand tokens — see the byte-ceiling
// regression test (TestAgentPresetByteCeiling).
var agentPresetTools = append([]string{}, agentFloorTools...)

// facadePresetTools is the complete, static facade-v1 surface. All names are
// registered live and carry compact schemas; capabilities discovers operation
// details without promoting more tools into tools/list.
var facadePresetTools = facadeToolNames()

// editPresetTools is the minimal headless code-editing surface: orient,
// navigate, mutate, verify. Sized so an agent can edit code safely on a
// remote box without the full 170-tool catalogue. tool_profile and
// tools_search are always kept on top of any preset (isAlwaysKeptTool).
var editPresetTools = []string{
	// orient + read
	"smart_context", "get_editing_context", "read_file", "get_symbol_source",
	"get_file_summary", "get_symbol",
	// navigate
	"search_symbols", "search_text", "find_files", "find_usages", "get_callers",
	// mutate
	"edit_file", "edit_symbol", "write_file", "batch_edit", "rename_symbol",
	// verify
	"verify_change", "get_test_targets", "check_guards", "get_diagnostics",
	// orientation
	"graph_stats",
}

// navPresetTools is the read-only navigation / exploration surface — no
// editing tools at all.
var navPresetTools = []string{
	"explore",
	"smart_context", "get_editing_context", "read_file", "get_symbol_source",
	"get_file_summary", "get_symbol",
	"search_symbols", "search_text", "find_files", "find_usages",
	"find_implementations", "find_overrides", "get_callers", "get_call_chain",
	"get_dependencies", "get_dependents", "get_repo_outline", "graph_stats",
}

// localizationPresetTools is the eager surface of the `localization`
// preset — the lean "where is the code that does X" working set. The
// list lives in internal/profiles (the instruction-profile table) so
// the tool surface and the localization instructions body render from
// the same slice and cannot drift.
var localizationPresetTools = profiles.LocalizationEagerTools()

// builtinToolPresetSet resolves a preset name to its explicit allow-set.
// A nil set with denyMutating=false is the sentinel for "no explicit
// restriction" (the full surface); `readonly` carries denyMutating=true
// instead of an explicit list so it tracks the authoritative
// daemon.MutatingTools set as it evolves. known=false flags an
// unrecognised preset name.
func builtinToolPresetSet(name string) (set map[string]bool, denyMutating, known bool) {
	switch name {
	case "", "full", "all":
		return nil, false, true
	case "core", "default", "classic":
		return toToolSet(corePresetTools), false, true
	case "agent", "coding-agent":
		return toToolSet(agentPresetTools), false, true
	case FacadeSurfaceVersion, "compact", "facade", "agent-v2":
		return toToolSet(facadePresetTools), false, true
	case "readonly", "read-only", "read_only":
		return nil, true, true
	case "edit", "editor", "edit-harness":
		return toToolSet(editPresetTools), false, true
	case "nav", "navigate", "explore":
		return toToolSet(navPresetTools), false, true
	case "localization", "locate", "find":
		return toToolSet(localizationPresetTools), false, true
	default:
		return nil, false, false
	}
}

// builtinPresetNames lists the recognised preset names for diagnostics.
var builtinPresetNames = []string{"agent", FacadeSurfaceVersion, "core", "full", "readonly", "edit", "nav", "localization"}

// toolPolicy is the resolved, in-memory restriction applied to the tool
// surface by the lazy registry (defer mode) and toolSurfaceFilter /
// checkToolGate (hide mode). The zero/nil policy allows everything.
type toolPolicy struct {
	preset       string
	mode         string
	explicit     map[string]bool // non-nil => base surface is exactly this set
	denyMutating bool            // drop daemon.MutatingTools (the `readonly` preset)
	allow        map[string]bool // force-include (overrides the preset)
	deny         map[string]bool // force-exclude (overrides everything)
	active       bool
	// lean, set for the `agent` preset, applies an extra per-parameter
	// description compaction on this session's tools/list so the coding-
	// agent surface stays inside its byte ceiling. The full parameter prose
	// is always one tools_search / `full` preset away.
	lean bool
}

func toToolSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// normalizeToolMode maps a mode string to hide|defer (default hide).
func normalizeToolMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case toolPolicyModeDefer, "lazy", "search":
		return toolPolicyModeDefer
	default:
		return toolPolicyModeHide
	}
}

// newToolPolicy resolves a ToolPolicyConfig into a toolPolicy. An
// unrecognised preset name is logged and downgraded to the full surface
// (fail-open — a typo never silently strands an agent with no tools).
func newToolPolicy(cfg ToolPolicyConfig, logger *zap.Logger) *toolPolicy {
	rawPreset := strings.ToLower(strings.TrimSpace(cfg.Preset))
	allow := toToolSet(cfg.Allow)
	deny := toToolSet(cfg.Deny)

	var (
		explicit     map[string]bool
		denyMutating bool
		label        string
	)
	if rawPreset == "" {
		// No named preset. An explicit allow list (e.g. --tools
		// search_symbols,edit_file) IS the surface; otherwise the full
		// surface, minus any deny.
		if len(allow) > 0 {
			explicit = allow
			label = "custom"
		} else {
			label = "full"
		}
	} else if set, dm, known := builtinToolPresetSet(rawPreset); known {
		explicit = set
		denyMutating = dm
		label = rawPreset
		switch label {
		case "all":
			label = "full"
		case "default", "classic":
			label = "core"
		case "coding-agent":
			label = "agent"
		case "compact", "facade", "agent-v2":
			label = FacadeSurfaceVersion
		case "locate", "find":
			label = "localization"
		}
	} else {
		// A typo'd preset fails open to the full surface (never strands
		// an agent with no tools); allow deltas stay additive.
		if logger != nil {
			logger.Warn("unknown MCP tool preset; serving the full surface",
				zap.String("preset", cfg.Preset),
				zap.Strings("known", builtinPresetNames))
		}
		label = "full"
	}
	if label == FacadeSurfaceVersion && (len(allow) > 0 || len(deny) > 0) {
		// This is a closed protocol contract: tools/list and the hard call gate
		// must always agree on the same 21 names. Use a legacy/custom preset when
		// a per-tool surface is required.
		if logger != nil {
			logger.Warn("compact MCP surface ignores allow/deny deltas",
				zap.Strings("allow", cfg.Allow), zap.Strings("deny", cfg.Deny))
		}
		allow = nil
		deny = nil
	}
	active := explicit != nil || denyMutating || len(allow) > 0 || len(deny) > 0
	return &toolPolicy{
		preset:       label,
		mode:         normalizeToolMode(cfg.Mode),
		explicit:     explicit,
		denyMutating: denyMutating,
		allow:        allow,
		deny:         deny,
		active:       active,
		lean:         label == "agent" || label == "localization" || label == FacadeSurfaceVersion,
	}
}

// isAlwaysKeptTool: introspection (tool_profile) and discovery
// (tools_search) stay reachable under every preset so an agent can
// always see its surface and, in defer mode, discover more. An explicit
// deny still wins (checked before this in allows).
//
// The untracked-cwd repair tools (capabilities / workspace_admin) are
// deliberately NOT listed here: on the facade surface they are ordinary
// members of the published set, and this predicate's facade-v1 arm below
// returns false for everything it names — adding them would hide exactly
// the tools a blocked session needs.
func isAlwaysKeptTool(name string) bool {
	return name == "tool_profile" || name == LazyToolsSearchName
}

// allows reports whether name is part of this policy's allowed surface.
// A nil or inactive policy allows everything.
func (p *toolPolicy) allows(name string) bool {
	if !p.isActive() {
		return true
	}
	if p.deny[name] {
		return false
	}
	if isAlwaysKeptTool(name) {
		// capabilities replaces legacy discovery/introspection on the closed
		// facade-v1 surface. Keeping these two names would make tools/list and
		// the hard call gate disagree.
		if p.preset == FacadeSurfaceVersion {
			return false
		}
		return true
	}
	if p.allow[name] {
		return true
	}
	if p.explicit != nil {
		return p.explicit[name]
	}
	if p.denyMutating && daemon.IsMutating(name) {
		return false
	}
	return true
}

func (p *toolPolicy) isActive() bool  { return p != nil && p.active }
func (p *toolPolicy) hideMode() bool  { return p.isActive() && p.mode == toolPolicyModeHide }
func (p *toolPolicy) deferMode() bool { return p.isActive() && p.mode == toolPolicyModeDefer }

// toolPolicyConfigFromEnv reads GORTEX_TOOLS / GORTEX_TOOLS_MODE. The
// bool reports whether either var was set.
func toolPolicyConfigFromEnv() (ToolPolicyConfig, bool) {
	spec := strings.TrimSpace(os.Getenv(toolPresetEnv))
	mode := strings.TrimSpace(os.Getenv(toolPresetModeEnv))
	if spec == "" && mode == "" {
		return ToolPolicyConfig{}, false
	}
	cfg := parseToolSpec(spec)
	if mode != "" {
		cfg.Mode = mode
	}
	return cfg, true
}

// isKnownPresetName reports whether name is one of the built-in preset
// names (full / readonly / edit / nav + aliases).
func isKnownPresetName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	_, _, known := builtinToolPresetSet(name)
	return known
}

// parseToolSpec parses a spec into a ToolPolicyConfig. The grammar is:
//
//   - the first bare token that names a built-in preset is the preset
//     (full / readonly / edit / nav); any further bare tokens are added
//     to the allow set — so "edit,find_files" means the edit preset plus
//     find_files;
//   - if the first bare token is NOT a known preset, every bare token is
//     an explicit tool name — so "search_symbols,edit_file" means exactly
//     those two tools (an expert allow list, no preset);
//   - +name / -name are always allow / deny deltas.
func parseToolSpec(spec string) ToolPolicyConfig {
	var cfg ToolPolicyConfig
	presetTaken := false
	for _, tok := range strings.Split(spec, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		switch {
		case strings.HasPrefix(tok, "+"):
			cfg.Allow = append(cfg.Allow, strings.TrimPrefix(tok, "+"))
		case strings.HasPrefix(tok, "-"):
			cfg.Deny = append(cfg.Deny, strings.TrimPrefix(tok, "-"))
		case !presetTaken && isKnownPresetName(tok):
			cfg.Preset = tok
			presetTaken = true
		default:
			cfg.Allow = append(cfg.Allow, tok)
		}
	}
	return cfg
}

// ParseToolSpec parses a "preset,+tool,-tool" spec into its parts. The
// first bare token is the preset; +name / -name are allow / deny deltas.
// Exported for CLI flag folding (cmd/gortex).
func ParseToolSpec(spec string) (preset string, allow, deny []string) {
	cfg := parseToolSpec(spec)
	return cfg.Preset, cfg.Allow, cfg.Deny
}

// mergeToolPolicyEnv overlays GORTEX_TOOLS / GORTEX_TOOLS_MODE over a
// base (config-file / flag-folded) config: an env preset or mode
// overrides the base when set; allow/deny deltas append. Mirrors the
// repo-wide "GORTEX_* env overrides file config" convention.
func mergeToolPolicyEnv(base ToolPolicyConfig) ToolPolicyConfig {
	env, ok := toolPolicyConfigFromEnv()
	if !ok {
		return base
	}
	out := base
	if env.Preset != "" {
		out.Preset = env.Preset
	}
	if env.Mode != "" {
		out.Mode = env.Mode
	}
	out.Allow = append(append([]string{}, base.Allow...), env.Allow...)
	out.Deny = append(append([]string{}, base.Deny...), env.Deny...)
	return out
}

// resolveToolPolicy builds the policy from a base config (threaded from
// options / the config file) with the GORTEX_TOOLS / GORTEX_TOOLS_MODE
// env overrides applied on top.
func resolveToolPolicy(base ToolPolicyConfig, logger *zap.Logger) *toolPolicy {
	return newToolPolicy(mergeToolPolicyEnv(base), logger)
}

// ToolSurface is a resolved tool-visibility predicate usable outside the
// MCP server — the stdio proxy uses it to filter a daemon's tools/list
// and gate calls per connection, so a client can scope its own pipe
// (gortex mcp --tools / GORTEX_TOOLS) while the daemon stays full. Built
// from the same ToolPolicyConfig + GORTEX_TOOLS env as the server.
type ToolSurface struct{ p *toolPolicy }

// NewToolSurface resolves a config (with the GORTEX_TOOLS env overrides
// applied) into a queryable surface.
func NewToolSurface(base ToolPolicyConfig, logger *zap.Logger) *ToolSurface {
	return &ToolSurface{p: resolveToolPolicy(base, logger)}
}

// Active reports whether the surface restricts anything at all.
func (s *ToolSurface) Active() bool { return s != nil && s.p.isActive() }

// Allows reports whether a tool name is visible in this surface. A nil
// or inactive surface allows everything.
func (s *ToolSurface) Allows(name string) bool {
	if s == nil {
		return true
	}
	return s.p.allows(name)
}

// GateCalls reports whether disallowed tools should be blocked at call
// time. True only for an active surface in hide mode; defer mode keeps
// non-listed tools reachable (the proxy analogue of the server keeping
// deferred tools callable after a tools_search promotion). A nil or
// inactive surface gates nothing.
func (s *ToolSurface) GateCalls() bool {
	return s != nil && s.p.hideMode()
}

// Preset returns the resolved preset label (full / readonly / edit / nav
// / custom) for logging.
func (s *ToolSurface) Preset() string {
	if s == nil || s.p == nil {
		return "full"
	}
	return s.p.preset
}

// effectiveSessionPolicy resolves the tool-surface policy in force for the
// current request's session. A forwarded, operator-pinned, or active-profile
// selection wins; otherwise every MCP client connection gets the compact
// closed surface — whether or not its initialize carried a clientInfo.name,
// which is informational and routinely left empty by SDKs and proxies — and
// only an internal session-less caller keeps the server default. The result
// is cached on the session so it is derived once, not on every tools/list.
// Never nil.
//
// This is the single authoritative resolution point the diet relies on:
// wherever tools/list is answered on the daemon, the surface for THIS
// connection is decided here, so a client preset actually applies instead
// of being a no-op the proxy can only subtract from.
func (s *Server) effectiveSessionPolicy(ctx context.Context) *toolPolicy {
	if s == nil {
		return nil
	}
	sess := s.sessionFor(ctx)
	if sess == nil {
		return s.toolPolicy
	}
	connected := isClientConnection(ctx)
	sess.mu.Lock()
	// A fallback cached by an internal caller sharing this session must not
	// pin a client connection to the server default: re-resolve once. An
	// operator pin resolves to the same fallback, so it keeps the cache.
	stale := connected && sess.resolvedToolPolicy == nil && !s.toolPolicyOperatorPinned
	if sess.toolPolicyResolved && !stale {
		p := sess.resolvedToolPolicy
		sess.mu.Unlock()
		if p != nil {
			return p
		}
		return s.toolPolicy
	}
	spec, mode, client := sess.toolSpec, sess.toolMode, sess.clientName
	sess.mu.Unlock()

	p := s.resolveSessionPolicy(spec, mode, connected || strings.TrimSpace(client) != "")

	sess.mu.Lock()
	sess.resolvedToolPolicy = p
	sess.toolPolicyResolved = true
	sess.mu.Unlock()

	if p != nil {
		return p
	}
	return s.toolPolicy
}

// resolveSessionPolicy builds the effective policy from a client-forwarded
// spec + mode and whether the session is an MCP client connection, or
// returns nil to fall back to the server's global policy. A forwarded spec
// inherits the server's mode when the client did not pin one, so a bare
// `GORTEX_TOOLS=nav` keeps the daemon's defer semantics instead of silently
// switching to hide.
func (s *Server) resolveSessionPolicy(spec, mode string, identified bool) *toolPolicy {
	if strings.TrimSpace(spec) != "" {
		cfg := parseToolSpec(spec)
		switch {
		case strings.TrimSpace(mode) != "":
			cfg.Mode = mode
		case isFacadePreset(cfg.Preset):
			// facade-v1 is a closed, versioned contract. A bare forwarded
			// GORTEX_TOOLS=facade-v1 must not inherit the daemon's usual
			// core/defer mode and silently weaken its direct-call gate.
			cfg.Mode = toolPolicyModeHide
		case s.toolPolicy != nil:
			cfg.Mode = s.toolPolicy.mode
		}
		return newToolPolicy(cfg, s.logger)
	}
	// A deliberate server/operator policy outranks machine instruction
	// profiles and client-aware defaults. Returning nil makes
	// effectiveSessionPolicy fall back to the already-resolved global policy.
	if s.toolPolicyOperatorPinned {
		return nil
	}
	if p := s.instructionProfilePolicy(); p != nil {
		return p
	}
	if identified {
		return s.clientSurfacePolicy()
	}
	return nil
}

func isFacadePreset(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case FacadeSurfaceVersion, "compact", "facade", "agent-v2":
		return true
	default:
		return false
	}
}

// activeInstructionPreset reads the machine's active instruction
// profile and returns its tool preset ("" when the profile keeps the
// defaults). Package var so tests can stub the machine state.
var activeInstructionPreset = profiles.ActiveToolPreset

// instructionProfilePolicy applies the active instruction profile's
// tool preset to sessions that forwarded no explicit spec. Precedence:
// a forwarded spec wins (checked by the caller before this), an
// operator-pinned mcp.tools / GORTEX_TOOLS configuration wins, then
// the profile, then the client-aware default. The default `core`
// profile carries no preset, so machines that never ran
// `gortex instructions switch` resolve exactly as before.
func (s *Server) instructionProfilePolicy() *toolPolicy {
	if s == nil || s.toolPolicyOperatorPinned {
		return nil
	}
	preset := activeInstructionPreset()
	if strings.TrimSpace(preset) == "" {
		return nil
	}
	mode := toolPolicyModeDefer
	if s.toolPolicy != nil && s.toolPolicy.mode != "" {
		mode = s.toolPolicy.mode
	}
	return newToolPolicy(ToolPolicyConfig{Preset: preset, Mode: mode}, s.logger)
}

// operatorPinnedToolPolicy reports whether the base tool-policy config
// expresses a deliberate operator choice rather than the shipped
// default (`core` preset in defer mode, no deltas) — the active
// instruction profile only refines the shipped default, never an
// operator pin. GORTEX_TOOLS / GORTEX_TOOLS_MODE always pin.
func operatorPinnedToolPolicy(base ToolPolicyConfig) bool {
	if _, envSet := toolPolicyConfigFromEnv(); envSet {
		return true
	}
	if base.OperatorPinned {
		return true
	}
	if len(base.Allow) > 0 || len(base.Deny) > 0 {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(base.Preset)) {
	case "", "core", "default", "classic":
		// The shipped default preset. A mode is only a pin when it
		// deviates from the shipped defer default.
		return strings.TrimSpace(base.Mode) != "" && normalizeToolMode(base.Mode) != toolPolicyModeDefer
	}
	return true
}

// clientDefaultPolicy returns the client surface for a named MCP client. A
// name alone cannot tell an unnamed client from an internal caller, so an
// empty name returns nil here; effectiveSessionPolicy gives an unnamed
// client connection the same surface (see isClientConnection).
func (s *Server) clientDefaultPolicy(client string) *toolPolicy {
	if strings.TrimSpace(client) == "" {
		return nil
	}
	return s.clientSurfacePolicy()
}

// clientSurfacePolicy is the compact, closed coding surface every MCP client
// connection gets by default, independent of its clientInfo.name. Surface
// selection is intentionally independent of wire-format decoding: an unknown
// editor can use the JSON-safe compact tools, while only decoder-allowlisted
// clients default to GCX. Explicit forwarded, operator-pinned, and
// instruction-profile policies are resolved before this fallback.
func (s *Server) clientSurfacePolicy() *toolPolicy {
	return newToolPolicy(ToolPolicyConfig{Preset: FacadeSurfaceVersion, Mode: toolPolicyModeHide}, s.logger)
}

// toolPolicyBaseFromOptions extracts the config-supplied tool policy
// from the MultiRepoOptions, or the zero config when none was provided
// (the GORTEX_TOOLS env override still applies in resolveToolPolicy).
func toolPolicyBaseFromOptions(opts []MultiRepoOptions) ToolPolicyConfig {
	if len(opts) > 0 && opts[0].ToolPolicy != nil {
		return *opts[0].ToolPolicy
	}
	return ToolPolicyConfig{}
}
