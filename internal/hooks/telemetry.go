package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
)

// DecisionKind enumerates the outcomes a PreToolUse redirect can log.
type DecisionKind string

const (
	DecisionProbedHit        DecisionKind = "probed_hit"
	DecisionProbedMiss       DecisionKind = "probed_miss"
	DecisionSkippedNonSymbol DecisionKind = "skipped_non_symbol"
	DecisionTimedOut         DecisionKind = "timed_out"
	// DecisionNudged records that ModeAdaptiveNudge fired its
	// once-per-burst soft-deny after a streak of non-symbolic calls.
	DecisionNudged DecisionKind = "nudged"
	// DecisionRedirectedWrite records that a shell command was recognised as
	// mutating indexed source and answered with the graph-aware edit path.
	// Without it a shell write leaves no trace of which door a change took.
	DecisionRedirectedWrite DecisionKind = "redirected_write"
	// DecisionViewFallback records that a path-scoped probe was answered from
	// a graph other than the probed path's own — the family primary standing
	// in for a working copy whose availability or removal grace window is
	// running. It changes no verdict; it exists so a fallback answer is
	// visible before it is ever given weight.
	DecisionViewFallback DecisionKind = "view_fallback"
)

type hookDecision struct {
	Timestamp  string       `json:"ts"`
	Tool       string       `json:"tool"`
	Decision   DecisionKind `json:"decision"`
	Hits       int          `json:"hits,omitempty"`
	DurationMS int64        `json:"duration_ms,omitempty"`
	// View names the graph that answered a path-scoped probe and why it stood
	// in. The probed path is deliberately absent: this log carries no content.
	View string `json:"view,omitempty"`
}

// hookEffectiveness records one hook invocation without source, prompt, path,
// symbol, or command content. Keeping it separate from hookDecision preserves
// the existing probe log while making skipped/no-output invocations visible;
// that denominator is what catches regressions such as "91% skipped".
type hookEffectiveness struct {
	Timestamp string `json:"ts"`
	Event     string `json:"event"`
	// Agent names the harness whose hook invoked us. Without it a machine
	// running two agents produces one indistinguishable stream, and "these
	// hooks never ran" — the finding that identifies an agent silently
	// skipping them — cannot be said about either one. Omitted (and read as
	// unattributed) in logs written before this field existed.
	Agent               string `json:"agent,omitempty"`
	EmittedContext      bool   `json:"emitted_context"`
	DaemonReachable     *bool  `json:"daemon_reachable,omitempty"`
	AlternationSegments int    `json:"alternation_segments"`
	DurationMS          int64  `json:"duration_ms"`
}

// AgentClaudeCode is the attribution for the default wire protocol: the
// `--agent` flag is empty for Claude Code, so the empty string has to be
// resolved to a name rather than recorded as "unknown".
const AgentClaudeCode = "claude-code"

// activeAgent is the harness this hook process is serving, set once from the
// command line before any handler runs. A package-level value rather than a
// parameter threaded through every logging call site: a hook process handles
// exactly one event for one agent and then exits.
var activeAgent = AgentClaudeCode

// SetAgent records which harness invoked this hook process. Empty resolves to
// Claude Code, matching the `--agent` flag's documented default — so no hook
// command string has to change to gain attribution. That matters beyond
// tidiness: Codex records trust against each hook command's hash and skips
// hooks whose definition changed, so rewriting one to add a flag would
// silently disable it until every user re-approved it in /hooks.
func SetAgent(name string) {
	name = strings.TrimSpace(strings.ToLower(name))
	switch name {
	case "", "claude", "claude-code", "claudecode":
		activeAgent = AgentClaudeCode
	default:
		activeAgent = name
	}
}

// ActiveAgent reports the harness this process is serving.
func ActiveAgent() string { return activeAgent }

var hookEffectivenessEvents = map[string]bool{
	"PostToolUse":                   true,
	"PreToolUse":                    true,
	"SessionStart":                  true,
	"UserPromptSubmit":              true,
	"PreCompact":                    true,
	"PostCompact":                   true,
	"Stop":                          true,
	"SubagentStart":                 true,
	"SubagentStop":                  true,
	"LocalizationTerminal.observed": true,
	"LocalizationTerminal.denied":   true,
	"LocalizationTerminal.claim_lock_release_failed": true,
	"LocalizationTerminal.cleared_prompt":            true,
	"LocalizationTerminal.cleared_session":           true,
}

// hookDecisionsPath returns the telemetry file path. Respects GORTEX_HOOK_LOG
// so tests can redirect writes. Defaults to ~/.gortex/cache (or the
// $XDG_CACHE_HOME equivalent when that variable is set).
func hookDecisionsPath() string {
	if p := os.Getenv("GORTEX_HOOK_LOG"); p != "" {
		return p
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v == "" || !filepath.IsAbs(v) {
		if _, err := os.UserHomeDir(); err != nil {
			return ""
		}
	}
	return filepath.Join(platform.CacheDir(), "hook-decisions.jsonl")
}

// hookEffectivenessPath is a privacy-safe, denominator-complete companion to
// hook-decisions.jsonl. Tests can redirect it independently so decision-log
// assertions remain stable.
func hookEffectivenessPath() string {
	if p := os.Getenv("GORTEX_HOOK_EFFECTIVENESS_LOG"); p != "" {
		return p
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v == "" || !filepath.IsAbs(v) {
		if _, err := os.UserHomeDir(); err != nil {
			return ""
		}
	}
	return filepath.Join(platform.CacheDir(), "hook-effectiveness.jsonl")
}

// logHookDecision appends one JSONL record. Best-effort: errors are swallowed
// because telemetry must never block a hook.
func logHookDecision(tool, _ string, decision DecisionKind, hits int, dur time.Duration) {
	path := hookDecisionsPath()
	if path == "" {
		return
	}
	rec := hookDecision{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Tool:       tool,
		Decision:   decision,
		Hits:       hits,
		DurationMS: dur.Milliseconds(),
	}
	appendHookJSONL(path, rec)
}

// logProbeViewFallback records a path-scoped answer that came from a graph
// other than the probed path's own. An exact answer is the ordinary case and
// writes nothing, so the log stays a record of degradations rather than a
// per-call trace.
//
// tool names the control verb that answered — both path-scoped probes
// (file_coverage and search_symbols) can be served by a stand-in graph, and a
// record that did not say which one produced it would read as coverage.
func logProbeViewFallback(tool string, view *daemon.ProbeView) {
	if view == nil || view.Exact {
		return
	}
	path := hookDecisionsPath()
	if path == "" {
		return
	}
	appendHookJSONL(path, hookDecision{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Tool:      tool,
		Decision:  DecisionViewFallback,
		View:      view.Kind + "/" + view.FallbackReason,
	})
}

// logHookEffectiveness appends one bounded observation. alternationSegments is
// capped to prevent an adversarial regex from becoming a high-cardinality
// metric; values above the probe ceiling share the same overflow bucket.
func logHookEffectiveness(event string, emitted, reachable bool, alternationSegments int, dur time.Duration) {
	logHookEffectivenessReachability(event, emitted, &reachable, alternationSegments, dur)
}

// logHookEffectivenessUnknown records hooks that deliberately perform no
// daemon I/O. Omitting reachability avoids turning local lifecycle/terminal
// decisions into false daemon-down samples.
func logHookEffectivenessUnknown(event string, emitted bool, alternationSegments int, dur time.Duration) {
	logHookEffectivenessReachability(event, emitted, nil, alternationSegments, dur)
}

func logHookEffectivenessReachability(event string, emitted bool, reachable *bool, alternationSegments int, dur time.Duration) {
	if !hookEffectivenessEvents[event] {
		return
	}
	if alternationSegments < 0 {
		alternationSegments = 0
	}
	if alternationSegments > maxAlternationProbes+1 {
		alternationSegments = maxAlternationProbes + 1
	}
	path := hookEffectivenessPath()
	if path == "" {
		return
	}
	appendHookJSONL(path, hookEffectiveness{
		Timestamp:           time.Now().UTC().Format(time.RFC3339Nano),
		Event:               event,
		Agent:               activeAgent,
		EmittedContext:      emitted,
		DaemonReachable:     reachable,
		AlternationSegments: alternationSegments,
		DurationMS:          dur.Milliseconds(),
	})
}

func appendHookJSONL(path string, record any) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}
