package hooks

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// postHookInput is the PostToolUse payload Claude Code sends. It differs
// from PreToolUse only by carrying tool_response — the textual output
// the tool produced. We deliberately decode tool_response as `any` because
// each tool returns a different shape (Grep emits text with file:line
// matches, Glob emits a newline-separated file list, Read emits raw file
// content). The shape-specific handlers below normalise to a string.
type postHookInput struct {
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	ToolResponse  any            `json:"tool_response"`
	CWD           string         `json:"cwd"`
	// SessionID scopes the mutation briefing to this session's own edits.
	// Absent from Codex payloads, which therefore fall back to the labeled
	// whole-tree briefing.
	SessionID string `json:"session_id"`
}

// runPostToolUse parses the PostToolUse payload and appends graph
// enrichment as additionalContext when the tool was a Grep / Glob / Read
// that touched the indexed graph. Other tools fall through to a no-op —
// PostToolUse must never block the run.
//
// The handler is shape-aware:
//   - Grep: parse "path:line:text" lines, look up the enclosing symbol
//     in the graph for the first few hits, append name + kind so the
//     agent sees graph-grade follow-ups inline.
//   - Glob: count the matched files, look up symbol counts per file,
//     append a short summary so the agent can pick the right one without
//     a follow-up tool call.
//   - Read: look up the file's symbol count and importer count so the
//     agent knows where the file lives before deciding to act.
//
// Every graph lookup goes over the daemon's AF_UNIX MCP socket (see
// fileSummaryViaDaemon), the same channel the PreToolUse file-indexed
// probe uses. An earlier revision hit an HTTP :8765 /api/graph/* API that
// was removed when the web surface migrated to the daemon, so these
// lookups silently returned nothing regardless of configuration (#241).
func runPostToolUse(data []byte) {
	started := time.Now()
	var input postHookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "PostToolUse" {
		return
	}
	daemonUp := daemonReachableFn()
	emitted := false
	terminalObserved := false
	defer func() {
		if terminalObserved {
			localizationTerminalTelemetry("observed", emitted, started)
			return
		}
		logHookEffectiveness("PostToolUse", emitted, daemonUp, 0, time.Since(started))
	}()

	if terminal, observed := observeLocalizationTerminal(data); observed {
		terminalObserved = true
		emitted = emitLocalizationTerminalContext(terminal)
		return
	}

	// Daemon outage: the follow-ups below both need daemon answers and
	// instruct "Do not re-Read / re-Grep" — guidance that must not land
	// while the daemon cannot serve the tools it points at (#486). The
	// per-file probes would fail individually anyway; returning here keeps
	// a flapping daemon from emitting follow-ups mid-outage and skips up to
	// one failed dial per matched file. Terminal-receipt handling above is
	// local state and deliberately stays ahead of this gate.
	if !daemonUp {
		return
	}

	ctx := postToolContext(input)
	if ctx == "" {
		return
	}
	emitted = true
	emitPostToolContext(ctx, false)
}

func emitLocalizationTerminalContext(terminal localizationTerminalHookInput) bool {
	if !localizationTerminalIdentityCurrent(terminal.TerminalIdentity) {
		return false
	}
	emitPostToolContext(localizationTerminalAdditionalContext(terminal.TerminalReceipt), false)
	return true
}

func postToolContext(input postHookInput) string {
	switch input.ToolName {
	case "Grep":
		return postGrep(input)
	case "Glob":
		return postGlob(input)
	case "Read":
		return postRead(input)
	}
	return ""
}

func emitPostToolContext(ctx string, replace bool) {
	output := HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     "PostToolUse",
			AdditionalContext: ctx,
		},
	}
	if replace {
		// Codex currently implements output replacement through the supported
		// PostToolUse block decision. The nominal suppressOutput field is parsed
		// but rejected by current releases, so never emit it.
		output.Decision = "block"
		output.Reason = ctx
	}
	out, err := json.Marshal(output)
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

// grepHitLineRe matches the leading "<path>:<line>" of a ripgrep-style
// hit (Claude Code's Grep tool uses ripgrep underneath). Captures the
// path and line number; the rest of the line — the matched text — is
// discarded.
var grepHitLineRe = regexp.MustCompile(`^([^:]+):(\d+):`)

// postGrep parses ripgrep-style match lines from tool_response and adds
// "enclosing symbol" lookups for the first few hits so the agent doesn't
// have to follow up with find_usages / get_callers manually. Summaries
// are memoised per file so several hits in one file cost one socket call.
func postGrep(input postHookInput) string {
	body := responseText(input.ToolResponse)
	if body == "" {
		return ""
	}
	hits := parseGrepHits(body)
	if len(hits) == 0 {
		return ""
	}

	const maxLookup = 5
	// Cache summaries (including misses, cached as nil) so repeated hits
	// in the same file don't re-dial the daemon.
	cache := make(map[string]*hookFileSummary)
	enriched := make([]string, 0, maxLookup)
	for _, h := range hits {
		if len(enriched) >= maxLookup {
			break
		}
		summary, seen := cache[h.path]
		if !seen {
			summary, _ = fileSummaryFn(input.CWD, h.path)
			cache[h.path] = summary
		}
		if summary == nil {
			continue
		}
		n := enclosingNode(summary.Symbols, h.line)
		if n == nil {
			continue
		}
		label := n.Name
		if n.Kind != "" {
			label = n.Kind + " " + n.Name
		}
		enriched = append(enriched, fmt.Sprintf("  %s:%d → %s", h.path, h.line, label))
	}
	if len(enriched) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] Graph context for %d of %d Grep hit(s):\n", len(enriched), len(hits))
	for _, line := range enriched {
		b.WriteString(line + "\n")
	}
	b.WriteString("Follow-up: call `relations(operation:\"usages\", target:{symbol:\"<id>\"})`; choose operation `callers` when you need only callers. Do not re-Grep.\n")
	return b.String()
}

// postGlob counts the matched files and looks up symbol counts per file
// so the agent can rank them by relevance without another Read/Grep
// roundtrip. Files that aren't indexed are still counted but only
// reported in aggregate ("12 indexed / 3 unindexed").
func postGlob(input postHookInput) string {
	body := responseText(input.ToolResponse)
	if body == "" {
		return ""
	}
	paths := parseGlobPaths(body)
	if len(paths) == 0 {
		return ""
	}

	type fileSummary struct {
		path    string
		symbols int
	}
	const maxFiles = 8
	indexed := make([]fileSummary, 0, maxFiles)
	unindexed := 0
	for _, p := range paths {
		summary, ok := fileSummaryFn(input.CWD, p)
		if !ok || summary == nil || len(summary.Symbols) == 0 {
			unindexed++
			continue
		}
		if len(indexed) < maxFiles {
			indexed = append(indexed, fileSummary{path: p, symbols: len(summary.Symbols)})
		}
	}
	if len(indexed) == 0 {
		return ""
	}

	// Sort by symbol count desc — the largest files are usually the
	// most interesting for "where is logic concentrated?" queries.
	sort.Slice(indexed, func(i, j int) bool { return indexed[i].symbols > indexed[j].symbols })

	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] Indexed %d/%d Glob match(es):\n", len(paths)-unindexed, len(paths))
	for _, f := range indexed {
		fmt.Fprintf(&b, "  %s — %d symbol(s)\n", f.path, f.symbols)
	}
	if len(paths)-unindexed > len(indexed) {
		fmt.Fprintf(&b, "  ... and %d more indexed file(s)\n", (len(paths)-unindexed)-len(indexed))
	}
	b.WriteString("Follow-up: use `read(operation:\"summary\", target:{file:\"<path>\"})` for one file or `explore(operation:\"outline\")` for the workspace shape.\n")
	return b.String()
}

// postRead enriches a Read by reporting the file's graph footprint —
// symbol count and how many other files import it — so the agent sees
// where the file sits in the codebase. Files outside the graph are
// silently skipped.
func postRead(input postHookInput) string {
	filePath, _ := input.ToolInput["file_path"].(string)
	if filePath == "" {
		return ""
	}
	summary, ok := fileSummaryFn(input.CWD, filePath)
	if !ok || summary == nil || len(summary.Symbols) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] Graph footprint for %s:\n", filePath)
	fmt.Fprintf(&b, "  %d indexed symbol(s)\n", len(summary.Symbols))
	if summary.Dependents > 0 {
		fmt.Fprintf(&b, "  %d file(s) import this one\n", summary.Dependents)
	}
	b.WriteString("Follow-up: call `read(operation:\"summary\", target:{file:\"<path>\"})`; choose operation `editing_context` before editing. Do not re-Read.\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// Response normalisation
// ---------------------------------------------------------------------------

// responseText extracts a plain string from tool_response regardless of
// the shape Claude Code sent: bare string, {"content": "..."}, or
// {"output": "..."}. Unknown shapes return "" — the handler then no-ops.
func responseText(v any) string {
	switch r := v.(type) {
	case string:
		return r
	case map[string]any:
		for _, k := range []string{"content", "output", "stdout", "text"} {
			if s, ok := r[k].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// parseGrepHits scans the response for "<path>:<line>:" prefixes. Lines
// that don't match the shape are ignored — Claude Code's Grep tool can
// emit summary lines (e.g. "Found 4 files") that aren't hits.
type grepHit struct {
	path string
	line int
}

func parseGrepHits(body string) []grepHit {
	var out []grepHit
	seen := make(map[string]bool)
	for ln := range strings.SplitSeq(body, "\n") {
		m := grepHitLineRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		path := m[1]
		lineNum, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		key := path + ":" + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, grepHit{path: path, line: lineNum})
	}
	return out
}

// parseGlobPaths splits the response on newlines, trims whitespace, and
// keeps non-empty entries that look like file paths (skip summary
// strings). The Glob tool emits one path per line.
func parseGlobPaths(body string) []string {
	var out []string
	for ln := range strings.SplitSeq(body, "\n") {
		p := strings.TrimSpace(ln)
		if p == "" {
			continue
		}
		// Skip "(no matches)" / "Found N files" preambles.
		if strings.HasPrefix(p, "(") || strings.HasPrefix(p, "Found ") {
			continue
		}
		// Must look like a path (have an extension or a slash). This
		// is a conservative filter — anything else gets dropped so we
		// don't try to graph-lookup commentary lines.
		if !strings.Contains(p, "/") && !strings.Contains(p, ".") {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ---------------------------------------------------------------------------
// Graph lookups (over the daemon AF_UNIX MCP socket)
// ---------------------------------------------------------------------------

// summaryNode is the subset of a get_file_summary node the PostToolUse
// enrichment consumes: name/kind for the label, and the line span so the
// enclosing symbol for a grep hit can be resolved client-side.
type summaryNode struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// hookFileSummary is the parsed shape the PostToolUse handlers need from a
// get_file_summary response: the file's definition symbols plus a count of
// the files that import it.
type hookFileSummary struct {
	Symbols    []summaryNode
	Dependents int
}

// fileSummaryFn is the seam tests stub; production routes through the
// daemon's MCP socket (fileSummaryViaDaemon). A false / nil return is the
// "no signal" case — daemon unreachable, malformed response, or the file
// genuinely not indexed; callers treat all three the same (no enrichment).
var fileSummaryFn = fileSummaryViaDaemon

// fileSummaryViaDaemon fetches get_file_summary for filePath over the
// daemon socket and parses out the definition symbols + importer count.
// Shares daemonFileSummaryRaw (dial + path resolution + frame exchange)
// with the PreToolUse file-indexed probe so both stay on one transport.
func fileSummaryViaDaemon(cwd, filePath string) (*hookFileSummary, bool) {
	resp, ok := daemonFileSummaryRaw(cwd, filePath)
	if !ok {
		return nil, false
	}
	syms, dependents, ok := parseFileSummary(resp)
	if !ok {
		return nil, false
	}
	return &hookFileSummary{Symbols: syms, Dependents: dependents}, true
}

// parseFileSummary unwraps a get_file_summary tools/call response —
// JSON-RPC envelope → first content block (the JSON payload as text) →
// {nodes, dependents}. get_file_summary strips the file/import nodes, so
// nodes is the definition-symbol list; a not-indexed file comes back as a
// tool error / guidance text, which fails the parse → ok=false.
func parseFileSummary(resp []byte) (nodes []summaryNode, dependents int, ok bool) {
	text := parseToolCallText(resp)
	if text == "" {
		return nil, 0, false
	}
	var summary struct {
		Nodes      []summaryNode     `json:"nodes"`
		Dependents []json.RawMessage `json:"dependents"`
	}
	if err := json.Unmarshal([]byte(text), &summary); err != nil {
		return nil, 0, false
	}
	if len(summary.Nodes) == 0 {
		return nil, 0, false
	}
	return summary.Nodes, len(summary.Dependents), true
}

// enclosingNode returns the innermost definition node whose [start,end]
// line span contains line, or nil when no node covers it. Ties break to
// the smallest span so a method wins over the type/file that also spans
// the line. Nodes without an end line are treated as single-line spans.
func enclosingNode(nodes []summaryNode, line int) *summaryNode {
	var best *summaryNode
	bestSpan := int(^uint(0) >> 1) // max int
	for i := range nodes {
		n := &nodes[i]
		if n.StartLine <= 0 {
			continue
		}
		end := n.EndLine
		if end < n.StartLine {
			end = n.StartLine
		}
		if line < n.StartLine || line > end {
			continue
		}
		if span := end - n.StartLine; best == nil || span < bestSpan {
			best, bestSpan = n, span
		}
	}
	return best
}
