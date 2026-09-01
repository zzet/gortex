package lsp

import (
	"os"
	"strings"
)

// SweepEnv is the environment variable that overrides the configured
// per-file enrichment sweep mode. When set to a recognised value it wins
// over the provider's configured mode (and therefore over .gortex.yaml),
// so an operator can dial sweep intensity for one run without editing
// config.
const SweepEnv = "GORTEX_LSP_SWEEP"

// Per-file enrichment sweep modes. The sweep is the whole-repo hover /
// call-hierarchy phase that runs after the tier-deciding confirm and add
// passes: it stamps hover type strings and interrogates the server for
// call/type-hierarchy edges the AST extractor missed, file by file.
//
// On an already-resolved graph (a warm restart) that sweep is pure churn —
// it re-opens and re-hovers every file to confirm zero new edges. The mode
// gates how much of it runs:
//
//   - sweepModeDemand (DEFAULT): sweep a file when its declarations still
//     carry unresolved same-name call candidates (enrichment demand) OR it
//     carries a dispatch-relevant declaration — a callable taking part in
//     dynamic dispatch, or an interface / hierarchy-involved type (see
//     lspGraphView.typeIsDispatchRelevant). The dispatch disjunct is
//     load-bearing: a type never contributes call demand, yet the sweep is
//     the only path that recovers its cross-file / dynamic extends /
//     supertype edges, so gating on demand alone would silently drop them.
//     A bare data type with no hierarchy involvement carries none of those
//     edges, so it no longer admits its file. A file with neither signal is
//     skipped, so a warm restart pays no sweep for it while the
//     already-enriched declarations that are swept skip their redundant
//     hover.
//   - sweepModeFull: sweep every file of the language — the pre-knob
//     behaviour, kept for a cold index that wants maximal hover coverage.
//   - sweepModeOff: skip the per-file sweep entirely. The confirm / add /
//     interface passes still run, so tiers and recall are unaffected.
const (
	sweepModeDemand = "demand"
	sweepModeFull   = "full"
	sweepModeOff    = "off"
)

// normalizeSweepMode canonicalises a configured / env sweep-mode string.
// Case- and whitespace-insensitive; "none" is accepted as an alias for
// "off". An empty or unrecognised value returns "" so the caller can fall
// through to the next precedence source (env → config → default).
func normalizeSweepMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case sweepModeDemand:
		return sweepModeDemand
	case sweepModeFull:
		return sweepModeFull
	case sweepModeOff, "none":
		return sweepModeOff
	default:
		return ""
	}
}

// resolveSweepMode picks the effective per-file sweep mode by precedence:
// the GORTEX_LSP_SWEEP env override wins over the operator-configured value,
// which wins over the per-server spec default, which wins over the global
// demand-gated default. Only the two operator-set sources (env, config) can
// override a server's own default, so a server that needs the full sweep
// gets it out of the box while an operator retains the last word. An
// unrecognised value at any level is ignored (falls through) rather than
// failing the pass.
func resolveSweepMode(configured, specDefault string) string {
	if env := normalizeSweepMode(os.Getenv(SweepEnv)); env != "" {
		return env
	}
	if cfg := normalizeSweepMode(configured); cfg != "" {
		return cfg
	}
	if sd := normalizeSweepMode(specDefault); sd != "" {
		return sd
	}
	return sweepModeDemand
}

// effectiveSweepMode resolves the sweep mode for this provider, honouring
// the GORTEX_LSP_SWEEP env override over the router-configured field over
// the server spec's own DefaultSweepMode.
func (p *Provider) effectiveSweepMode() string {
	specDefault := ""
	if p.spec != nil {
		specDefault = p.spec.DefaultSweepMode
	}
	return resolveSweepMode(p.sweepMode, specDefault)
}

// sweepFile reports whether the per-file hover / call-hierarchy sweep should
// run for a file under mode, given its unresolved-demand count and whether it
// carries a dispatch-relevant declaration. Under the demand default a file is
// swept when at least one of its declarations still has unresolved same-name
// call candidates (demand > 0) OR it carries a dispatch-relevant declaration
// (dispatch): a callable taking part in dynamic dispatch, or an interface /
// hierarchy-involved type. The latter never surfaces as demand, so without
// this disjunct a hierarchy-carrying file would drop the extends / supertype
// edges only this sweep recovers. "full" always sweeps, "off" never does.
func sweepFile(mode string, demand int, dispatch bool) bool {
	switch mode {
	case sweepModeOff:
		return false
	case sweepModeFull:
		return true
	default: // sweepModeDemand and any unrecognised residue
		return demand > 0 || dispatch
	}
}

// OpenDocsEnv is the environment variable that overrides whether the
// enrichment pass sends the textDocument/didOpen / didClose document
// lifecycle before querying a file. "1" / "true" forces the lifecycle on
// even for a server whose spec opts out; "0" / "false" skips it for every
// server. Empty falls through to the spec's NoDidOpen.
const OpenDocsEnv = "GORTEX_LSP_OPEN_DOCS"

// normalizeOnOff canonicalises an on/off override value ("on" / "1" /
// "true", "off" / "0" / "false") — the shared vocabulary of the open-docs
// and heavy-requests overrides. An empty or unrecognised value returns ""
// so the caller falls through to the next precedence source.
func normalizeOnOff(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "1", "true":
		return "on"
	case "off", "0", "false":
		return "off"
	default:
		return ""
	}
}

// resolveOpensDocs reports whether the enrichment pass should send the
// didOpen / didClose lifecycle for this server, by precedence: the
// GORTEX_LSP_OPEN_DOCS env override wins over the operator-configured
// value (`semantic.lsp_open_docs`), which wins over the spec's NoDidOpen,
// which wins over the open-by-default fallback. An unrecognised value at
// any level is ignored (falls through) rather than failing the pass.
func resolveOpensDocs(configured string, spec *ServerSpec) bool {
	if env := normalizeOnOff(os.Getenv(OpenDocsEnv)); env != "" {
		return env == "on"
	}
	if cfg := normalizeOnOff(configured); cfg != "" {
		return cfg == "on"
	}
	if spec != nil && spec.NoDidOpen {
		return false
	}
	return true
}

// HeavyRequestsEnv is the environment variable that overrides the
// heavy-request opt-out (ServerSpec.NoHeavyRequests) in both directions:
// "on" / "1" / "true" restores textDocument/references and
// callHierarchy/incomingCalls for a server whose spec opts out — the
// operator runs a build without the FindReferences leak — while "off" /
// "0" / "false" disables them for every server. Empty falls through to
// the spec.
const HeavyRequestsEnv = "GORTEX_LSP_HEAVY"

// resolveNoHeavyRequests reports whether the enrichment pass must skip the
// heavy request classes for this server: the GORTEX_LSP_HEAVY env override
// wins over the spec's NoHeavyRequests, which wins over the allow-by-default
// fallback. Shares the on/off vocabulary of the open-docs override.
func resolveNoHeavyRequests(spec *ServerSpec) bool {
	if env := normalizeOnOff(os.Getenv(HeavyRequestsEnv)); env != "" {
		return env == "off"
	}
	return spec != nil && spec.NoHeavyRequests
}
