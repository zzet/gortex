package mcp

import (
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// hotEagerTools is the closed list of tool names eagerly registered at
// session start. Everything else lands in the deferred catalog behind
// the tools_search discovery tool — see lazyToolRegistry below.
//
// Selection rules:
//   - keep "load-bearing" navigation / read / edit primitives the agent
//     reaches for on almost every task
//   - keep the dispatcher tools that fan out to many sub-analyses
//     (analyze, smart_context) so the agent doesn't have to know the
//     downstream names
//   - keep the orientation tools the agent needs in the first turn
//     (graph_stats, get_repo_outline, index_repository)
//
// Anything specialised (clones, surprises, simulate, overlay,
// subscribe/unsubscribe pairs, notebook, memories editing, scaffold,
// generate_*) is deferred — the bytes saved compound across every
// session, every reconnect, and every prompt-cache miss.
//
// NOTE: this is a registration / eager-publish set, NOT a write-
// authorization list. Its editing entries must not be confused with
// the authoritative mutating-tool set; for "does this tool mutate
// state" consult daemon.MutatingTools (internal/daemon/mutating.go).
var hotEagerTools = map[string]bool{
	"search_symbols":       true,
	"find_files":           true,
	"find_usages":          true,
	"find_implementations": true,
	"find_overrides":       true,
	"get_callers":          true,
	"get_call_chain":       true,
	"get_dependencies":     true,
	"get_dependents":       true,

	"get_symbol":          true,
	"get_symbol_source":   true,
	"get_file_summary":    true,
	"get_editing_context": true,
	"smart_context":       true,

	"get_repo_outline": true,
	"graph_stats":      true,
	"index_repository": true,

	"analyze":         true,
	"check_guards":    true,
	"get_diagnostics": true,

	"read_file":     true,
	"write_file":    true,
	"edit_file":     true,
	"edit_symbol":   true,
	"rename_symbol": true,
	"batch_edit":    true,
	"move_symbol":   true,
	"inline_symbol": true,

	// Review engine — eager because a reviewing agent reaches for these on
	// the first turn of a review task; a discovery round-trip would regress
	// that hot path. Registered as a group by registerReviewTools.
	"sibling_diff_context": true,
	"review":               true,
	"review_pack":          true,
	"suppress_finding":     true,
	"post_review":          true,
}

// LazyToolsSearchName is the well-known name of the discovery tool the
// server registers eagerly so deferred tools can be probed by name or
// keyword.
const LazyToolsSearchName = "tools_search"

// lazyToolsEnv toggles the lazy / pass-through split. Default is
// disabled (every tool ships eagerly) because the dominant stdio MCP
// clients in the wild — Claude Code among them — do not refresh
// tools/list on `notifications/tools/list_changed`, which makes the
// post-promotion surface unreachable. Set to a truthy value
// (1 / true / yes / on / enable / enabled) to opt back in on
// lazy-aware clients (HTTP transport, IDE plugins that follow the
// notification).
const lazyToolsEnv = "GORTEX_LAZY_TOOLS"

// deferredTool holds the tool definition + handler we stashed away so
// the discovery tool can return the schema and (optionally) promote it
// into the live MCP server.
type deferredTool struct {
	tool     mcp.Tool
	handler  server.ToolHandlerFunc
	keywords map[string]struct{}
}

// lazyToolRegistry owns the cold half of the tool surface — tools
// whose schemas are withheld from the initial tools/list payload and
// returned by the tools_search discovery tool on demand. Once a tool
// is promoted it migrates to the live MCP server and stops being
// returned by future tools_search calls; the registry is the source
// of truth for "what's still hidden".
type lazyToolRegistry struct {
	mu       sync.RWMutex
	deferred map[string]*deferredTool
	promoted map[string]struct{}
	// promote is wired by Server.attachLazyRegistry once mcpServer is
	// available; it AddTools the tool into the live server (which
	// fires notifications/tools/list_changed).
	promote func(*deferredTool)
	enabled bool
	// eager, when non-nil, replaces the hotEagerTools membership test
	// in IsDeferred — a tool ships eagerly iff eager(name) is true.
	// Set by a defer-mode tool preset (SetEagerPredicate) so the
	// eager surface follows the preset's allow-set instead of the
	// hard-coded hot set. Set once at construction before the register
	// sweep, so no mutex is needed.
	eager func(string) bool
}

func newLazyToolRegistry(enabled bool) *lazyToolRegistry {
	return &lazyToolRegistry{
		deferred: make(map[string]*deferredTool),
		promoted: make(map[string]struct{}),
		enabled:  enabled,
	}
}

// lazyEnabledFromEnv reads the opt-in env var. Default is disabled —
// the lazy split only works when the client honours
// notifications/tools/list_changed and re-fetches tools/list mid
// session. Claude Code (and most stdio MCP hosts) do not, so a
// promoted tool stays unreachable for them. Truthy value opts in.
func lazyEnabledFromEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(lazyToolsEnv)))
	switch v {
	case "1", "true", "yes", "on", "enable", "enabled":
		return true
	}
	return false
}

// Enabled reports whether the registry is active. When false, Register
// is a no-op and IsDeferred always returns false — every tool stays
// eager.
func (r *lazyToolRegistry) Enabled() bool {
	if r == nil {
		return false
	}
	return r.enabled
}

// IsDeferred reports whether a tool name should be deferred when
// registered. Hot tools always go eager; the discovery tool itself
// always goes eager. Everything else goes lazy when the registry is
// enabled.
func (r *lazyToolRegistry) IsDeferred(name string) bool {
	if r == nil || !r.enabled {
		return false
	}
	if name == LazyToolsSearchName {
		return false
	}
	if r.eager != nil {
		return !r.eager(name)
	}
	return !hotEagerTools[name]
}

// SetEagerPredicate overrides the hotEagerTools membership test used by
// IsDeferred: once set, a tool is eager iff f(name) is true. A defer-
// mode tool preset wires this so the cold tools/list carries exactly
// the preset's allow-set. Call before any Register (construction time).
func (r *lazyToolRegistry) SetEagerPredicate(f func(string) bool) {
	if r == nil {
		return
	}
	r.eager = f
}

// Register stashes a tool in the deferred set. The caller has already
// established that IsDeferred(name) is true.
func (r *lazyToolRegistry) Register(tool mcp.Tool, handler server.ToolHandlerFunc) {
	kw := tokenizeForSearch(tool.Name, tool.Description)
	r.mu.Lock()
	r.deferred[tool.Name] = &deferredTool{tool: tool, handler: handler, keywords: kw}
	r.mu.Unlock()
}

// Query parses one of the supported query forms and returns up to max
// matching deferred tools.
//
// Supported forms (mirroring Anthropic Claude Code 2.1.108+'s
// ToolSearch surface so a familiar agent doesn't need to relearn):
//
//   - ""                       → browse: list deferred tools, name-sorted
//   - "select:a,b,c"           → exact lookup by name (max ignored)
//   - "+slack send"            → require "slack" in the name, rank by remainder
//   - "memories invariants"    → keyword search, ranked
func (r *lazyToolRegistry) Query(query string, max int) []*deferredTool {
	hits, _ := r.QueryWithTotal(query, max)
	return hits
}

// QueryWithTotal behaves like Query but also returns total — the number of
// deferred tools that matched before the max cap was applied. total greater
// than len(result) means the response was truncated, and the caller should
// advise the agent to narrow the query or fetch a known tool by exact name
// via "select:<name>".
func (r *lazyToolRegistry) QueryWithTotal(query string, max int) ([]*deferredTool, int) {
	if r == nil {
		return nil, 0
	}
	if max <= 0 {
		max = 10
	}
	query = strings.TrimSpace(query)

	if rest, ok := strings.CutPrefix(query, "select:"); ok {
		names := strings.Split(rest, ",")
		out := make([]*deferredTool, 0, len(names))
		r.mu.RLock()
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if dt, ok := r.deferred[n]; ok {
				if _, done := r.promoted[n]; done {
					// Already promoted — return the schema anyway so a
					// caller using select: is idempotent.
					out = append(out, dt)
					continue
				}
				out = append(out, dt)
			}
		}
		r.mu.RUnlock()
		return out, len(out)
	}

	var required []string
	tokens := tokenize(strings.ToLower(query))
	rest := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if stripped, ok := strings.CutPrefix(t, "+"); ok {
			required = append(required, stripped)
			continue
		}
		rest = append(rest, t)
	}
	// "+slack send" parses as +slack send because tokenize splits on
	// punctuation; the '+' lands at the start of the slack token only
	// when the caller wrote it adjacent. Handle the leading-'+' case
	// by re-scanning the raw query for plus-prefixed words.
	for raw := range strings.FieldsSeq(query) {
		if stripped, ok := strings.CutPrefix(raw, "+"); ok {
			required = append(required, strings.ToLower(stripped))
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	type hit struct {
		t     *deferredTool
		score int
	}
	hits := make([]hit, 0, len(r.deferred))
	for _, dt := range r.deferred {
		if _, done := r.promoted[dt.tool.Name]; done {
			continue
		}
		nameLower := strings.ToLower(dt.tool.Name)
		descLower := strings.ToLower(dt.tool.Description)

		ok := true
		for _, req := range required {
			if !strings.Contains(nameLower, req) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}

		score := 0
		if len(rest) == 0 && len(required) == 0 {
			score = 1 // browse mode — keep everything, rank alphabetically below
		} else {
			for _, term := range rest {
				if _, kw := dt.keywords[term]; kw {
					score += 5
				}
				if strings.Contains(nameLower, term) {
					score += 10
				}
				if strings.Contains(descLower, term) {
					score += 1
				}
			}
			for _, req := range required {
				if strings.Contains(nameLower, req) {
					score += 12
				}
			}
			// Whole-query name match: a query that is verbatim (a substring
			// of) the tool's name dominates, so a broad query that names the
			// tool reliably surfaces it ahead of the max cap rather than
			// relying on per-token scoring alone.
			if ql := strings.ToLower(query); len(ql) >= 3 && strings.Contains(nameLower, ql) {
				score += 25
			}
		}
		if score == 0 {
			continue
		}
		hits = append(hits, hit{t: dt, score: score})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].t.tool.Name < hits[j].t.tool.Name
	})
	total := len(hits)
	if len(hits) > max {
		hits = hits[:max]
	}
	out := make([]*deferredTool, len(hits))
	for i, h := range hits {
		out[i] = h.t
	}
	return out, total
}

// Promote registers each named tool with the live MCP server and
// marks it promoted so future Query calls skip it. Idempotent and
// atomic: the promoted mark and the live AddTool happen under the
// same lock, so a concurrent caller can never observe a tool marked
// promoted but not yet registered — it either sees the tool already
// live (GetTool succeeds) or transitions it itself. Returns the slice
// of names that actually transitioned to promoted state in THIS call;
// callers must treat a false return as "already promoted or absent"
// and re-check GetTool rather than concluding the tool is missing.
func (r *lazyToolRegistry) Promote(names ...string) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var newly []*deferredTool
	var promotedNames []string
	for _, name := range names {
		if _, done := r.promoted[name]; done {
			continue
		}
		dt, ok := r.deferred[name]
		if !ok {
			continue
		}
		r.promoted[name] = struct{}{}
		newly = append(newly, dt)
		promotedNames = append(promotedNames, name)
	}
	if r.promote != nil {
		for _, dt := range newly {
			r.promote(dt)
		}
	}
	return promotedNames
}

// CountDeferred returns the number of deferred tools still hidden.
func (r *lazyToolRegistry) CountDeferred() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for name := range r.deferred {
		if _, done := r.promoted[name]; done {
			continue
		}
		_ = name
		count++
	}
	return count
}

// DeferredNames returns the sorted list of names still hidden behind
// tools_search. Used by the discovery tool's browse mode and by tests.
func (r *lazyToolRegistry) DeferredNames() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.deferred))
	for name := range r.deferred {
		if _, done := r.promoted[name]; done {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DeferredTool returns the registered mcp.Tool for a deferred (or
// already-promoted) name and whether it was found. Used by the tool
// catalog to read a deferred tool's Description without promoting it.
func (r *lazyToolRegistry) DeferredTool(name string) (mcp.Tool, bool) {
	if r == nil {
		return mcp.Tool{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if dt, ok := r.deferred[name]; ok {
		return dt.tool, true
	}
	return mcp.Tool{}, false
}

// PromotedNames returns the sorted list of tool names already promoted
// out of the deferred catalog this session. Test-only helper.
func (r *lazyToolRegistry) PromotedNames() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.promoted))
	for name := range r.promoted {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// tokenize splits a string into lowercase alphanumeric tokens.
// snake_case (find_usages) yields ["find", "usages"]; camelCase
// (findUsages) yields the same; punctuation acts as a separator.
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			cur.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			cur.WriteRune(r - 'A' + 'a')
		default:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func tokenizeForSearch(name, desc string) map[string]struct{} {
	out := make(map[string]struct{}, 16)
	for _, t := range tokenize(name) {
		out[t] = struct{}{}
	}
	for _, t := range tokenize(strings.ToLower(desc)) {
		if len(t) < 3 {
			continue // drop short stopwords / glue
		}
		out[t] = struct{}{}
	}
	return out
}
