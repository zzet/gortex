package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// DecisionKind enumerates the outcomes the Grep-redirect probe can log.
type DecisionKind string

const (
	DecisionProbedHit        DecisionKind = "probed_hit"
	DecisionProbedMiss       DecisionKind = "probed_miss"
	DecisionSkippedNonSymbol DecisionKind = "skipped_non_symbol"
	DecisionTimedOut         DecisionKind = "timed_out"
	// DecisionNudged records that ModeAdaptiveNudge fired its
	// once-per-burst soft-deny after a streak of non-symbolic calls.
	DecisionNudged DecisionKind = "nudged"
)

type hookDecision struct {
	Timestamp  string       `json:"ts"`
	Tool       string       `json:"tool"`
	Pattern    string       `json:"pattern"`
	Decision   DecisionKind `json:"decision"`
	Hits       int          `json:"hits,omitempty"`
	DurationMS int64        `json:"duration_ms,omitempty"`
}

// codexHookEffect records one Codex lifecycle-hook invocation without
// retaining the prompt, command, paths, or source output.  Keeping this
// separate from hookDecision makes it possible to calculate an emitted-context
// rate per event and spot a regression before it becomes another large bucket
// of skipped probes.
type codexHookEffect struct {
	Timestamp           string `json:"ts"`
	Kind                string `json:"kind"`
	Event               string `json:"event"`
	Tool                string `json:"tool,omitempty"`
	EmittedContext      bool   `json:"emitted_context"`
	DaemonReachability  string `json:"daemon_reachability"`
	AlternationSegments int    `json:"alternation_segments,omitempty"`
	DurationMS          int64  `json:"duration_ms"`
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

// logHookDecision appends one JSONL record. Best-effort: errors are swallowed
// because telemetry must never block a hook.
func logHookDecision(tool, pattern string, decision DecisionKind, hits int, dur time.Duration) {
	path := hookDecisionsPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	rec := hookDecision{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Tool:       tool,
		Pattern:    pattern,
		Decision:   decision,
		Hits:       hits,
		DurationMS: dur.Milliseconds(),
	}
	line, err := json.Marshal(rec)
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

func logCodexHookEffect(event, tool string, emitted bool, reachability string, alternations int, dur time.Duration) {
	path := hookDecisionsPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if reachability == "" {
		reachability = "not_checked"
	}
	rec := codexHookEffect{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: "codex_hook_effectiveness",
		Event: event, Tool: tool, EmittedContext: emitted, DaemonReachability: reachability,
		AlternationSegments: alternations, DurationMS: dur.Milliseconds(),
	}
	line, err := json.Marshal(rec)
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
