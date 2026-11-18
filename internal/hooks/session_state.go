package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// sessionState is the small per-session record the PreToolUse hook
// persists across individual tool calls. Claude Code invokes the hook
// as a fresh process per tool call, so any cross-call signal (has the
// agent consulted the graph yet? how long is the non-symbolic streak?)
// has to round-trip through disk keyed by session_id.
//
// Every field must be safe to read as its zero value — a missing or
// corrupt state file degrades to "fresh session", never an error.
type sessionState struct {
	// GraphConsulted records that the agent has invoked at least one
	// Gortex MCP tool this session. ModeConsultUnlock keys the
	// deny→additionalContext downgrade on it.
	GraphConsulted bool `json:"graph_consulted,omitempty"`
	// MutationRefusals is the legacy-daemon fallback, keyed by exact checkout
	// root. Each timestamp expires after ten minutes; modern ready/read_only
	// coverage wins without rewriting this record in PreToolUse.
	MutationRefusals map[string]int64 `json:"mutation_refusals,omitempty"`
	// Retain the original identity too: filename sanitization may map distinct
	// host session IDs to one spelling; that must never share an allowance.
	MutationRefusalSession string `json:"mutation_refusal_session,omitempty"`
	// NonSymbolicStreak counts consecutive non-symbolic fallback tool
	// calls (Read / Grep / Glob) since the last symbolic call or nudge.
	// ModeAdaptiveNudge fires a soft-deny when it crosses the threshold.
	NonSymbolicStreak int `json:"non_symbolic_streak,omitempty"`
	// DaemonDownNotified records that this session was already told the
	// daemon is unreachable and per-call enforcement is standing down, so
	// the degradation notice fires once per session rather than once per
	// tool call (see daemonDownNoticeOnce).
	DaemonDownNotified bool `json:"daemon_down_notified,omitempty"`
	// WrittenPaths is the set of file paths (and graph symbol IDs, whose
	// path part is extracted at match time) this session's tool calls were
	// about to rewrite. It is what lets a Stop-hook briefing tell this
	// session's edits apart from a sibling session's on a shared checkout.
	//
	// Recorded at PreToolUse, i.e. before the write executes, so a denied
	// or failed write leaves an entry here that never happened. That is
	// safe: ownership is only ever tested against files already dirty in
	// git, and a write that never landed leaves its file clean, so the
	// stale entry cannot match anything.
	WrittenPaths []string `json:"written_paths,omitempty"`
	// WrittenPathsTruncated records that WrittenPaths hit its cap and
	// stopped accepting new entries, so a briefing can admit that some of
	// this session's own edits may be missing from the attributed set.
	WrittenPathsTruncated bool `json:"written_paths_truncated,omitempty"`
	// UpdatedUnixNano stamps the last write so the janitor can age the
	// file out even on filesystems with coarse mtimes.
	UpdatedUnixNano int64 `json:"updated_unix_nano,omitempty"`
}

const (
	// sessionWrittenPathsCap bounds the attribution set per session.
	// Matches localizationTerminalPruneLimit so both hook state stores
	// read the same.
	sessionWrittenPathsCap = 256
	// sessionWritePathsPerCall bounds what one tool call may contribute
	// (cf. bashWriteProbeLimit for the shell-command probe).
	sessionWritePathsPerCall = 8
	// sessionStateTTL / sessionStateHardCap bound the state directory,
	// mirroring the localization store's constants.
	sessionStateTTL     = 24 * time.Hour
	sessionStateHardCap = 128
)

// hookSessionDirEnvVar lets tests redirect the per-session state
// directory, parallel to GORTEX_HOOK_LOG for telemetry.
const hookSessionDirEnvVar = "GORTEX_HOOK_SESSION_DIR"

// sessionStateDir returns the directory holding per-session state
// files. Honors GORTEX_HOOK_SESSION_DIR so tests can point it at a
// t.TempDir(). An absolute $XDG_CACHE_HOME is honoured; otherwise the
// directory stays under os.UserCacheDir() as before. Returns "" when no
// base directory can be resolved — all callers treat "" as "state
// disabled" and degrade gracefully.
func sessionStateDir() string {
	if p := strings.TrimSpace(os.Getenv(hookSessionDirEnvVar)); p != "" {
		return p
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v == "" || !filepath.IsAbs(v) {
		if cacheDir, err := os.UserCacheDir(); err != nil || cacheDir == "" {
			return ""
		}
	}
	return filepath.Join(platform.OSCacheDir(), "sessions")
}

// sanitizeSessionID reduces an arbitrary session_id to a safe single
// path segment: only [A-Za-z0-9._-] survive, everything else becomes
// '_'. Guards against path traversal ("../") and separators in a
// session_id that originates from the untrusted hook payload.
func sanitizeSessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	// A name that sanitizes to all dots ("." / "..") is still unsafe
	// as a path segment — neutralize it.
	if out == "" || strings.Trim(out, ".") == "" {
		return ""
	}
	return out
}

// sessionStatePath returns the JSON file path for a session, or "" when
// the session ID is empty/unusable or no base directory is available.
func sessionStatePath(sessionID string) string {
	safe := sanitizeSessionID(sessionID)
	if safe == "" {
		return ""
	}
	dir := sessionStateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, safe+".json")
}

// loadSessionState reads the per-session record. Best-effort: an empty
// session ID, a missing file, or any read/decode error all yield a
// zero-value sessionState. The hook must never block on state I/O.
func loadSessionState(sessionID string) sessionState {
	path := sessionStatePath(sessionID)
	if path == "" {
		return sessionState{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sessionState{}
	}
	var st sessionState
	if err := json.Unmarshal(data, &st); err != nil {
		return sessionState{}
	}
	return st
}

// saveSessionState writes the per-session record. Best-effort, mirroring
// telemetry.go: every error is swallowed so a read-only cache dir or a
// full disk can never stop a tool call from proceeding.
func saveSessionState(sessionID string, st sessionState) {
	path := sessionStatePath(sessionID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	st.UpdatedUnixNano = time.Now().UnixNano()
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if os.WriteFile(path, data, 0o644) != nil {
		return
	}
	// Until write-target capture shipped, nothing called this in the default
	// posture, so the directory could never grow. It can now, so bound it.
	trimStateDir(filepath.Dir(path), path, sessionStateTTL, sessionStateHardCap)
}
