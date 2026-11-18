package hooks

// Daemon-outage degradation (#486). When the daemon socket is unreachable,
// every per-call enforcement surface stands down: the PreToolUse denies and
// graph advisories, the PostToolUse follow-ups, and the Codex / Kimi / Pi /
// Hermes adapter mirrors. Guidance that mandates Gortex MCP tools is untrue
// the moment the daemon cannot serve them — an agent that follows it is
// instructed to use tools that cannot answer while being flagged for using
// the tools that can. The SessionStart briefing already announces rule-only
// enforcement to a session that starts mid-outage; the once-per-session
// notice below is the same signal for a session the outage begins in.
//
// Deliberately exempt: the localization terminal marker (it is local state
// that carries its own answer and mandates nothing the daemon serves), the
// consult-unlock / write-target markers (local bookkeeping), and the
// Stop / PreCompact / UserPromptSubmit briefings (their content is built
// entirely from daemon answers, so they already degrade to silence).

// daemonUnreachableStance is the one instruction both the SessionStart
// briefing and the per-call degradation notice give for an outage. Shared so
// the session-level and per-call layers cannot drift into contradicting each
// other about what an agent should do while the daemon is down.
const daemonUnreachableStance = "The graph is unavailable; continue natively for this step and report only the step that needs the graph. Never start a daemon."

// daemonDownNotice is the once-per-session PreToolUse notice for an outage
// that begins mid-session — the SessionStart briefing has already promised
// active enforcement, so going quiet without a word would read as a hook
// failure rather than a deliberate stand-down.
const daemonDownNotice = "[Gortex] Gortex daemon unreachable — per-call graph enforcement is standing down to rule-only guidance until the daemon is back. " +
	daemonUnreachableStance +
	" This notice fires once per session."

// daemonDownNoticeOnce returns the degradation notice the first time it is
// called for a session and "" on every later call, keyed through the same
// per-session state store the consult-unlock marker uses. Sessions without a
// usable state path (no session_id, no cache dir) stay quiet — without
// deduplication the notice would repeat on every call, and the SessionStart
// briefing already covers the outage once.
func daemonDownNoticeOnce(sessionID string) string {
	if sessionStatePath(sessionID) == "" {
		return ""
	}
	st := loadSessionState(sessionID)
	if st.DaemonDownNotified {
		return ""
	}
	st.DaemonDownNotified = true
	saveSessionState(sessionID, st)
	return daemonDownNotice
}
