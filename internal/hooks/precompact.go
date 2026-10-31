package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// PreCompactInput is the JSON structure Claude Code sends to PreCompact hooks.
// Fields that aren't used here are still included so future logic can branch
// on the trigger ("auto" vs "manual") or respect custom_instructions.
type PreCompactInput struct {
	HookEventName      string `json:"hook_event_name"`
	SessionID          string `json:"session_id"`
	TranscriptPath     string `json:"transcript_path"`
	CWD                string `json:"cwd"`
	Trigger            string `json:"trigger"`
	CustomInstructions string `json:"custom_instructions"`
}

// compactionReinjectionAdvisory is the load-bearing half of the post-compaction
// block. Claude Code rebuilds the window from the summary, the most recent
// exchanges, and up to five recently-read files, which it re-reads from disk and
// re-injects as system-reminder blocks. That path calls the Read tool directly,
// so no PreToolUse hook sees it and none of Gortex's redirects apply: the agent
// wakes up holding raw whole-file content it never asked for. Telling it so is
// what stops it paying for the same bytes twice.
const compactionReinjectionAdvisory = "This session was just compacted. Claude Code rebuilt the context window from " +
	"the summary, the most recent exchanges, and up to five recently-read files — re-read from disk and " +
	"re-injected as `system-reminder` blocks. That content is already in context and did not pass through " +
	"Gortex, so re-reading those files buys nothing and pays for the same bytes twice.\n\n" +
	"Continue with Gortex: call `explore` for the task, then inspect with `search`, `read`, `relations`, " +
	"or `trace`. Do not re-read indexed files.\n\n"

// runPreCompact handles a PreCompact hook invocation given the raw stdin bytes
// already read by the dispatcher.
//
// It deliberately emits nothing. PreCompact has no context-injection contract in
// Claude Code: its documented output is a top-level `decision: "block"` (or exit
// code 2), both of which *block compaction* — and `hookSpecificOutput.additionalContext`
// is honoured only for SessionStart, Setup, SubagentStart, the tool events, Stop
// and SubagentStop. PreCompact is not on that list, and its stdout is not one of
// the three events (UserPromptSubmit, UserPromptExpansion, SessionStart) whose
// plain stdout becomes context. A briefing emitted here therefore never reaches
// the model, whatever shape it takes.
//
// The orientation snapshot is delivered instead by SessionStart with
// source="compact", which does honour additionalContext and fires once the new
// context window has been built. See buildCompactionBriefing and runSessionStart.
//
// The event is still handled so hook-effectiveness telemetry records the
// compaction, and so there is a place to hang a future PreCompact decision.
// It never blocks compaction.
func runPreCompact(data []byte, _ int) {
	started := time.Now()
	var input PreCompactInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "PreCompact" {
		return
	}
	logHookEffectiveness("PreCompact", false, daemonReachableFn(), 0, time.Since(started))
}

// buildCompactionBriefing renders the post-compaction orientation block that
// SessionStart(source="compact") injects.
//
// Unlike the rest of the briefing surface it always returns something: the
// re-injection advisory costs no daemon call and is the most valuable half of
// the block, so an unreachable bridge degrades to advisory-only rather than to
// silence.
func buildCompactionBriefing(port int) string {
	var sb strings.Builder
	sb.WriteString("\n## Gortex Post-Compaction Snapshot\n\n")
	sb.WriteString(compactionReinjectionAdvisory)

	stats := callServerTool(port, "graph_stats", nil)
	if stats == "" {
		// No bridge => the advisory above is the whole value.
		return sb.String()
	}

	if summary := renderStatsSummary(stats); summary != "" {
		sb.WriteString("**Index:** ")
		sb.WriteString(summary)
		sb.WriteString("\n\n")
	}

	if churn := renderSymbolHistory(port); churn != "" {
		// Not session-scoped: get_symbol_history reads the process-global
		// Server.symHistory, which every client of a shared daemon writes to.
		sb.WriteString("### Recently Modified Symbols (Gortex-tracked edits on this daemon, all clients)\n\n")
		sb.WriteString(churn)
		sb.WriteString("\n")
	}

	if hot := renderHotspots(port); hot != "" {
		sb.WriteString("### Top Hotspots (highest fan-in/out — likely load-bearing)\n\n")
		sb.WriteString(hot)
		sb.WriteString("\n")
	}

	if fb := renderFeedback(port); fb != "" {
		sb.WriteString("### Feedback-Ranked Symbols (most useful in past tasks)\n\n")
		sb.WriteString(fb)
		sb.WriteString("\n")
	}

	sb.WriteString("_End of Gortex snapshot._\n")
	return sb.String()
}

// renderStatsSummary produces one-line "nodes=X edges=Y languages=a,b,c".
func renderStatsSummary(raw string) string {
	var stats struct {
		TotalNodes int            `json:"total_nodes"`
		TotalEdges int            `json:"total_edges"`
		ByLanguage map[string]int `json:"by_language"`
	}
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		return ""
	}

	// Pick the 3 most-represented languages.
	type langCount struct {
		name string
		n    int
	}
	var langs []langCount
	for k, v := range stats.ByLanguage {
		langs = append(langs, langCount{k, v})
	}
	sort.Slice(langs, func(i, j int) bool { return langs[i].n > langs[j].n })
	if len(langs) > 3 {
		langs = langs[:3]
	}
	var names []string
	for _, l := range langs {
		names = append(names, l.name)
	}

	return fmt.Sprintf("%d nodes, %d edges, languages: %s",
		stats.TotalNodes, stats.TotalEdges, strings.Join(names, ", "))
}

// renderSymbolHistory lists the top modified symbols (compact text).
func renderSymbolHistory(port int) string {
	raw := callServerTool(port, "get_symbol_history", map[string]any{"compact": true})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return cappedLines(raw, 10)
}

// renderHotspots returns the top hotspots from the analyze tool.
func renderHotspots(port int) string {
	raw := callServerTool(port, "analyze", map[string]any{
		"kind":    "hotspots",
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return cappedLines(raw, 8)
}

// renderFeedback returns the most-useful symbols from past sessions.
func renderFeedback(port int) string {
	raw := callServerTool(port, "feedback", map[string]any{
		"action":  "query",
		"top_n":   5,
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return cappedLines(raw, 8)
}

// cappedLines returns the first `max` lines of s, joined with newlines.
func cappedLines(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > max {
		lines = lines[:max]
	}
	return strings.Join(lines, "\n") + "\n"
}

// cappedText caps s by line count first and then by bytes, so a single
// enormous line — a JSON payload from a tool that ignored `compact` — cannot
// blow the briefing budget. cappedLines alone counts newlines, which bounds
// nothing when the whole payload is one line. The byte cut lands on a line
// boundary when one exists inside the budget.
func cappedText(s string, maxLines, maxBytes int) string {
	out := cappedLines(s, maxLines)
	if maxBytes <= 0 || len(out) <= maxBytes {
		return out
	}
	cut := out[:maxBytes]
	if nl := strings.LastIndexByte(cut, '\n'); nl > 0 {
		cut = cut[:nl]
	}
	return cut + "\n… truncated …\n"
}

// callServerTool resolves a Gortex tool call for a hook handler. Live hook
// invocations carry a CWD and use the daemon's default AF_UNIX socket first;
// the optional HTTP REST surface is a compatibility fallback for older
// integrations and focused tests. Returns "" on any error so callers degrade
// silently.
//
// The socket fallback is gated on a set hookCWD (see setHookCWD): the pure-HTTP
// unit tests never set it, so they keep their existing "no bridge" semantics,
// while a live hook process — which sets hookCWD from the payload — reaches a
// normally-running daemon that only listens on the socket.
func callServerTool(port int, name string, args map[string]any) string {
	cwd := loadHookCWD()
	if cwd != "" {
		if raw := callServerToolDaemonFn(cwd, name, args); raw != "" {
			return raw
		}
	}
	return callServerToolHTTP(port, name, args)
}

// callServerToolWithin is callServerTool bounded by timeout. answered is false
// when the call did not return in time: the bridge is reachable but busy,
// which a briefing reports instead of folding it into the silence of an
// unreachable bridge. The abandoned call ends with the hook process.
func callServerToolWithin(port int, name string, args map[string]any, timeout time.Duration) (raw string, answered bool) {
	done := make(chan string, 1)
	go func() { done <- callServerTool(port, name, args) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case raw = <-done:
		return raw, true
	case <-timer.C:
		return "", false
	}
}

// callServerToolHTTP issues a POST /v1/tools/{name} against the server and
// returns the text content from the first response block. Returns empty string
// on any error (server unreachable, non-200, missing content).
func callServerToolHTTP(port int, name string, args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"arguments": args})
	if err != nil {
		return ""
	}

	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://localhost:%d/v1/tools/%s", port, name)
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ""
	}
	if parsed.IsError || len(parsed.Content) == 0 {
		return ""
	}
	return parsed.Content[0].Text
}
