package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/respbudget"
)

// federationReadTools is the allowlist of read traversal tools eligible
// for remote fan-out. Anything not here (and every effectful tool) is never
// federated.
var federationReadTools = map[string]bool{
	"find_usages":          true,
	"get_callers":          true,
	"get_call_chain":       true,
	"find_implementations": true,
	"get_dependents":       true,
	"search_symbols":       true,
	"smart_context":        true,
}

// localSchemaMajor is this daemon's graph-schema major version. A remote
// advertising an incompatible major is refused (never federated). Kept in
// sync with the value /v1/health advertises (server.SchemaVersion).
const localSchemaMajor = 1

// FederationConfig carries the tunable knobs (from .gortex.yaml's
// federation: block). Zero values fall back to sane defaults.
type FederationConfig struct {
	PerRemoteTimeout  time.Duration
	Budget            time.Duration
	BreakerThreshold  int
	BreakerCooldown   time.Duration
	HealthTTL         time.Duration
	NameKeyedFallback bool
}

func (c FederationConfig) withDefaults() FederationConfig {
	if c.PerRemoteTimeout <= 0 {
		c.PerRemoteTimeout = 2 * time.Second
	}
	if c.Budget <= 0 {
		c.Budget = 3 * time.Second
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = 3
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = 30 * time.Second
	}
	if c.HealthTTL <= 0 {
		c.HealthTTL = 30 * time.Second
	}
	return c
}

// Federator fans an allowlisted read tool out to enabled remotes after
// the local result is in hand, and merges the responses with provenance.
// It NEVER mutates a stored *graph.Node — it works on serialized bytes,
// so json.Unmarshal already yields detached copies; provenance lives only
// in the response-side origins map, never on a node.
type Federator struct {
	cfg       FederationConfig
	clientFor func(ServerEntry) (*ServerClient, error)
	breaker   *circuitBreaker
	health    *healthCache
	logger    *zap.Logger
}

// NewFederator builds a Federator. clientFor reuses the router's client
// cache so connections are shared.
func NewFederator(cfg FederationConfig, clientFor func(ServerEntry) (*ServerClient, error), logger *zap.Logger) *Federator {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Federator{
		cfg:       cfg,
		clientFor: clientFor,
		breaker:   newCircuitBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown),
		health:    newHealthCache(cfg.HealthTTL),
		logger:    logger,
	}
}

// FederationMeta is the response-side provenance block (JSON-only in v1).
type FederationMeta struct {
	RemotesQueried    []string          `json:"remotes_queried"`
	RemotesFailed     []RemoteFailure   `json:"remotes_failed,omitempty"`
	Degraded          bool              `json:"degraded"`
	NamespaceRewrites []string          `json:"namespace_rewrites,omitempty"`
	Origins           map[string]string `json:"origins,omitempty"`
	Note              string            `json:"note,omitempty"`
}

// RemoteFailure records why a remote was not merged.
type RemoteFailure struct {
	Slug   string `json:"slug"`
	Reason string `json:"reason"`
}

type remoteResult struct {
	slug     string
	toolJSON []byte
}

// Augment runs AFTER the local tool result is in hand. It fans the same
// tool+args out to each enabled remote under a bounded deadline, merges
// the responses by per-tool shape, and returns the merged bytes carrying
// a sibling federation{} block + origins map. It NEVER blocks past the
// budget and never lets one remote's failure drop another's results or
// the local result.
func (f *Federator) Augment(ctx context.Context, tool string, body, localResult []byte, remotes []ServerEntry) []byte {
	// Gate: only an allowlisted read tool with at least one enabled
	// remote is federated. With no enabled remotes the local result is
	// returned byte-for-byte — a pure-local install (or an all-disabled
	// roster) is unaffected; the federation{} block + origins map are
	// the additive superset that appears only when there is something to
	// federate.
	if !federationReadTools[tool] || IsEffectful(tool) || len(remotes) == 0 {
		return localResult
	}

	localTool, wrapped := unwrapToolJSON(localResult)

	// Renderings that cannot round-trip as JSON (compact, gcx, toon,
	// mermaid, dot) have no merge adapter: fanning out would silently
	// discard every remote row behind a local-only body. Skip the
	// fan-out entirely and make the partiality explicit instead —
	// merging the canonical graph before rendering is the deeper fix,
	// but until then a labeled local answer beats an unlabeled one.
	probe := bytes.TrimLeft(localTool, " \t\r\n")
	if len(probe) == 0 || (probe[0] != '{' && probe[0] != '[') || !json.Valid(probe) {
		return annotateLocalOnlyFormat(localResult, wrapped, len(remotes),
			respbudget.EffectiveFromArgs(argsMapFromBody(body)))
	}

	results, meta := f.fanOut(ctx, tool, body, remotes)

	merged, origins := f.merge(tool, body, localTool, results)
	meta.Origins = origins

	// Opt-in name-keyed fallback (OFF by default): a bare-name
	// search on each remote, rendered in a SEPARATE name_hits section
	// tagged text_matched — never merged into the primary id-keyed
	// results (name hits have different native ids that ID-dedup cannot
	// collapse). Rarity/length-gated so stdlib/builtin or too-short
	// names don't surface plausible-but-wrong cross-repo hits.
	if f.cfg.NameKeyedFallback && idKeyedTools[tool] {
		if name := bareNameFromBody(body); nameEligible(name) {
			if hits := f.nameKeyedFan(ctx, name, remotes); len(hits) > 0 {
				merged = injectField(merged, "name_hits", hits)
			}
		}
	}

	if len(meta.RemotesFailed) > 0 && meta.Note == "" {
		meta.Note = fmt.Sprintf("%d remote(s) did not answer; results are local%s.",
			len(meta.RemotesFailed), remoteOnlyOrPartial(meta))
	}

	// The caller's byte/token budget binds the FINAL representation:
	// each daemon budgeted only its own page, so a multi-peer merge can
	// overshoot every source's cap combined. Same arithmetic and
	// truncation meta as the per-daemon budget layer (respbudget), so
	// a trimmed merge is legible the same way a trimmed page is.
	mergedWithMeta := attachFederation(merged, meta)
	mergedWithMeta = budgetMergedJSON(mergedWithMeta, respbudget.EffectiveFromArgs(argsMapFromBody(body)))
	if !wrapped {
		return mergedWithMeta
	}
	return rewrapToolJSON(localResult, mergedWithMeta)
}

// budgetMergedJSON applies the caller's effective budget to the merged
// representation with the same structural trim (and truncation meta)
// the per-daemon budget layer uses. The generic trim bounds the
// top-level lists; the origins map scales with the rows — a coupling
// the shape-agnostic trim cannot know — so origins are re-pruned to
// the rows that survived.
func budgetMergedJSON(raw []byte, budget int) []byte {
	if budget <= 0 || len(raw) <= budget {
		return raw
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return raw // non-JSON body: some other layer's contract
	}
	// One trim, one prune: a non-fitting Apply empties every top-level
	// list, so re-running it after the prune could never cut further —
	// pruning the row-keyed annotations is the only shrink left.
	trimmed, _ := respbudget.Apply(generic, budget)
	m, ok := trimmed.(map[string]any)
	if !ok {
		return raw
	}
	pruneOriginsToRows(m)
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// pruneOriginsToRows drops origins entries whose row the budget trim
// removed. An origin annotates a returned row, and each merge shape
// keys rows differently — SubGraph nodes and keyed lists by "id",
// grouped usages by the nested rows' "symbol_id" — so the keep-set is
// the union of ids across every top-level list the payload actually
// carries (one nesting level down included, for the grouped shape).
func pruneOriginsToRows(m map[string]any) {
	origins, ok := m["origins"].(map[string]any)
	if !ok {
		return
	}
	keep := make(map[string]bool)
	sawList := false
	var collect func(items []any, depth int)
	collect = func(items []any, depth int) {
		for _, it := range items {
			im, ok := it.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"id", "symbol_id"} {
				if id, ok := im[key].(string); ok && id != "" {
					keep[id] = true
				}
			}
			if depth == 0 {
				for _, v := range im {
					if nested, ok := v.([]any); ok {
						collect(nested, 1)
					}
				}
			}
		}
	}
	for k, v := range m {
		arr, ok := v.([]any)
		if !ok || k == "origins" {
			continue
		}
		sawList = true
		collect(arr, 0)
	}
	if !sawList {
		return
	}
	for id := range origins {
		if !keep[id] {
			delete(origins, id)
		}
	}
}

// argsMapFromBody extracts the tool arguments map from the routed
// request body's production envelope ({"arguments":{...}}, the shape
// subGraphArgsFromBody and bareNameFromBody parse). Nil when absent.
func argsMapFromBody(body []byte) map[string]any {
	var env struct {
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	return env.Arguments
}

// localOnlyNote is the visible signal that a non-mergeable-format
// response is local-only. One definition, because its byte length is
// load-bearing twice: ReserveLocalNoteBudget subtracts it from the
// budget forwarded to the local handler, and annotateLocalOnlyFormat
// fits it into what that handler left.
func localOnlyNote(peerCount int) string {
	return fmt.Sprintf("note: local results only — %d enabled federation peer(s) were not queried because this response format cannot be merged; use the JSON format for federated results.", peerCount)
}

// nonMergeableFormatRequested mirrors the format routing the local
// handler will take, from the request args alone: these renderings
// cannot round-trip as JSON, so their federation path is the local-only
// note. A session-DEFAULT toon/gcx rendering is invisible here — that
// request gets no reservation and falls back to the fit-or-drop rule
// in annotateLocalOnlyFormat.
func nonMergeableFormatRequested(args map[string]any) bool {
	if compact, _ := args["compact"].(bool); compact {
		return true
	}
	switch f, _ := args["format"].(string); strings.ToLower(strings.TrimSpace(f)) {
	case "compact", "gcx", "toon", "mermaid", "dot":
		return true
	}
	return false
}

// ReserveLocalNoteBudget rewrites a to-be-dispatched local request so
// the local handler leaves headroom for the local-only note: the
// caller's effective cap minus the note's size, injected as max_bytes
// (min-merged by the handler with any caller axis, so it binds on
// both). Reserve-first is the token-budget decoration's rule — the
// note must never be the bytes that break the cap, and dropping it
// instead would silence the only "peers were not queried" signal on
// exactly the saturating responses. Requests that keep the JSON merge
// path, opt out of budgeting, or cap below the note's own size pass
// through untouched.
func (f *Federator) ReserveLocalNoteBudget(tool string, body []byte, peerCount int) []byte {
	if !federationReadTools[tool] || IsEffectful(tool) || peerCount == 0 {
		return body
	}
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		return body
	}
	args, _ := env["arguments"].(map[string]any)
	if !nonMergeableFormatRequested(args) {
		return body
	}
	budget := respbudget.EffectiveFromArgs(args)
	reserve := len(localOnlyNote(peerCount))
	if budget <= 0 || budget <= reserve {
		return body
	}
	// The reserved cap min-merges below any caller token axis, so it
	// binds regardless of which axis was the tighter one.
	args["max_bytes"] = budget - reserve
	env["arguments"] = args
	out, err := json.Marshal(env)
	if err != nil {
		return body
	}
	return out
}

// annotateLocalOnlyFormat appends an explicit local-only note to a
// non-mergeable-format response as a second content item, leaving the
// primary rendering byte-for-byte intact. Error results and unwrapped
// bodies pass through unchanged — an error is already explicit, and a
// bare body has no envelope to carry the note.
//
// The caller's byte/token budget binds the COMPLETE response.
// ReserveLocalNoteBudget normally pre-shrank the local rendering so
// the note has room by construction; when it could not (session-
// default formats, caps below the note's own size), a note that
// does not fit the remaining headroom is dropped — never the bytes
// that push the response past the cap it was budgeted to.
func annotateLocalOnlyFormat(localResult []byte, wrapped bool, peerCount, budget int) []byte {
	if !wrapped {
		return localResult
	}
	var m map[string]any
	if err := json.Unmarshal(localResult, &m); err != nil {
		return localResult
	}
	if m["isError"] == true || m["is_error"] == true {
		return localResult
	}
	content, ok := m["content"].([]any)
	if !ok {
		return localResult
	}
	note := localOnlyNote(peerCount)
	if budget > 0 {
		spent := 0
		for _, c := range content {
			if cm, ok := c.(map[string]any); ok {
				if text, ok := cm["text"].(string); ok {
					spent += len(text)
				}
			}
		}
		if spent+len(note) > budget {
			return localResult
		}
	}
	m["content"] = append(content, map[string]any{"type": "text", "text": note})
	out, err := json.Marshal(m)
	if err != nil {
		return localResult
	}
	return out
}

func remoteOnlyOrPartial(meta FederationMeta) string {
	if len(meta.RemotesQueried) > len(meta.RemotesFailed) {
		return " plus the remotes that answered"
	}
	return ""
}

// fanOut queries each enabled remote concurrently under a per-remote
// deadline and a global budget. A plain WaitGroup (not errgroup) is used
// so one remote's error never cancels the others.
func (f *Federator) fanOut(ctx context.Context, tool string, body []byte, remotes []ServerEntry) ([]remoteResult, FederationMeta) {
	meta := FederationMeta{RemotesQueried: []string{}}
	if len(remotes) == 0 {
		return nil, meta
	}
	budgetCtx, cancel := context.WithTimeout(ctx, f.cfg.Budget)
	defer cancel()

	var (
		mu      sync.Mutex
		results []remoteResult
		wg      sync.WaitGroup
	)
	fail := func(slug, reason string) {
		mu.Lock()
		meta.RemotesFailed = append(meta.RemotesFailed, RemoteFailure{Slug: slug, Reason: reason})
		meta.Degraded = true
		mu.Unlock()
		// Name the failing remote in the logs, not only the JSON block,
		// so a degraded fan-out is visible to operators.
		f.logger.Warn("federation: remote degraded",
			zap.String("tool", tool),
			zap.String("target_slug", slug),
			zap.String("reason", reason))
	}

	audit := auditInfoFrom(ctx)
	for _, rem := range remotes {
		rem := rem
		mu.Lock()
		meta.RemotesQueried = append(meta.RemotesQueried, rem.Slug)
		mu.Unlock()
		// Audit every remote-routed fan-out call (cross-daemon access
		// record), carrying the same {session_id, cwd, tool, target_slug}
		// tuple as the single-remote proxy-routing audit line.
		f.logger.Info("federation: remote-routed call",
			zap.String("tool", tool),
			zap.String("target_slug", rem.Slug),
			zap.String("cwd", audit.Cwd),
			zap.String("session_id", audit.SessionID),
			zap.String("via", "fan-out"))

		if f.breaker.isOpen(rem.Slug) {
			fail(rem.Slug, "circuit_open")
			continue
		}
		cli, err := f.clientFor(rem)
		if err != nil {
			fail(rem.Slug, "client_error")
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			// Capability + readiness negotiation (cached), inside the
			// goroutine so remotes negotiate concurrently. An
			// incompatible major schema is refused; a still-warming
			// remote is bucketed rather than counted as empty success.
			if h, herr := f.health.get(budgetCtx, cli, f.cfg.PerRemoteTimeout); herr == nil {
				if h.SchemaVersion != 0 && h.SchemaVersion != localSchemaMajor {
					fail(rem.Slug, "incompatible_schema")
					return
				}
				if !h.Indexed {
					fail(rem.Slug, "warming")
					return
				}
			}

			rctx, rcancel := context.WithTimeout(budgetCtx, f.cfg.PerRemoteTimeout)
			defer rcancel()
			out, status, err := cli.ProxyToolCtx(rctx, tool, body)
			if err != nil {
				f.breaker.fail(rem.Slug)
				fail(rem.Slug, "unreachable")
				return
			}
			if status >= 400 {
				f.breaker.fail(rem.Slug)
				fail(rem.Slug, fmt.Sprintf("status_%d", status))
				return
			}
			f.breaker.success(rem.Slug)
			toolJSON, _ := unwrapToolJSON(out)
			mu.Lock()
			results = append(results, remoteResult{slug: rem.Slug, toolJSON: toolJSON})
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results, meta
}

// merge dispatches to the per-tool adapter and returns the merged tool
// JSON plus the origins map.
func (f *Federator) merge(tool string, body, local []byte, remotes []remoteResult) ([]byte, map[string]string) {
	// Fan-out results arrive in goroutine completion order; sort by slug
	// so the merged result — and any post-merge cap cut from it — is
	// deterministic across runs.
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].slug < remotes[j].slug })
	switch tool {
	case "find_usages", "get_callers", "get_call_chain", "get_dependents":
		// group_by:"file" renders a grouped shape, not a SubGraph —
		// round-tripping it through the flat merge would discard every
		// grouped field and answer with an empty subgraph.
		if tool == "find_usages" && groupByFileRequested(body) {
			return mergeGroupedUsages(body, local, remotes)
		}
		return mergeSubGraph(tool, body, local, remotes)
	case "search_symbols":
		return mergeKeyedList(local, remotes, "results")
	case "find_implementations":
		return mergeKeyedList(local, remotes, "implementations")
	case "smart_context":
		return mergeSmartContext(local, remotes)
	default:
		return local, map[string]string{}
	}
}

// subGraphMergeArgs are the request args the post-merge recap needs:
// the queried node (kept through node pruning) and the caller's limit.
type subGraphMergeArgs struct {
	ID    string `json:"id"`
	Limit *int   `json:"limit"`
}

// subGraphArgsFromBody extracts the tool args from the routed request
// body: production stdio and streamable MCP dispatch nest them under
// "arguments", the one envelope every federation body reader parses
// (bareNameFromBody reads the same shape).
func subGraphArgsFromBody(body []byte) subGraphMergeArgs {
	var env struct {
		Arguments *subGraphMergeArgs `json:"arguments"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Arguments != nil {
		return *env.Arguments
	}
	return subGraphMergeArgs{}
}

// groupByFileRequested mirrors the handler's group_by:"file" gate on
// the routed body, so the merge dispatch and the render agree on when
// the grouped shape is in play.
func groupByFileRequested(body []byte) bool {
	gb, _ := argsMapFromBody(body)["group_by"].(string)
	return strings.EqualFold(strings.TrimSpace(gb), "file")
}

// The wire envelope of a group_by:"file" find_usages response. The
// group and row shapes are the shared query types the renderer
// serializes, so the merge round trip can never strip a field the
// renderer added.
type groupedUsagesWire struct {
	GroupedBy  string                  `json:"grouped_by"`
	FileCount  int                     `json:"file_count"`
	TotalUses  int                     `json:"total_uses"`
	Groups     []*query.UsageFileGroup `json:"groups"`
	Truncated  bool                    `json:"truncated"`
	TotalEdges int                     `json:"total_edges,omitempty"`
}

// mergeGroupedUsages merges group_by:"file" find_usages responses
// group-wise: rows dedupe on (file, line, kind, symbol), counts and
// totals are recomputed over the union, the caller's limit is
// reapplied once, globally, and every surviving row is attributed to
// the daemon that contributed it — mirroring the flat merge's
// contract.
func mergeGroupedUsages(body, local []byte, remotes []remoteResult) ([]byte, map[string]string) {
	origins := map[string]string{}
	var lg groupedUsagesWire
	if err := json.Unmarshal(local, &lg); err != nil || lg.GroupedBy == "" {
		return local, origins
	}
	byFile := make(map[string]*query.UsageFileGroup, len(lg.Groups))
	rowSeen := map[string]bool{}
	// The key carries the name and context columns too: a row whose
	// enclosing symbol was pruned (empty symbol_id) must not collapse
	// into a different peer's row at the same site.
	rowKey := func(file string, u query.UsageGroupItem) string {
		return file + "\x00" + strconv.Itoa(u.Line) + "\x00" + u.EdgeKind + "\x00" + u.SymbolID + "\x00" + u.SymbolName + "\x00" + u.Context
	}
	addRows := func(g *query.UsageFileGroup, source string) {
		dst := byFile[g.File]
		if dst == nil {
			dst = &query.UsageFileGroup{File: g.File}
			byFile[g.File] = dst
		}
		for _, u := range g.Uses {
			k := rowKey(g.File, u)
			if rowSeen[k] {
				continue
			}
			rowSeen[k] = true
			dst.Uses = append(dst.Uses, u)
			if u.SymbolID != "" {
				if _, exists := origins[u.SymbolID]; !exists { // local wins
					origins[u.SymbolID] = source
				}
			}
		}
	}
	for _, g := range lg.Groups {
		if g != nil {
			addRows(g, "local")
		}
	}
	// A source that already capped or trimmed its page makes the merged
	// totals a floor; total_edges falls back to total_uses for sources
	// that answered complete (they omit the explicit floor).
	anyTruncated := lg.Truncated || respbudget.Trimmed(local)
	totalEdgesFloor := max(lg.TotalEdges, lg.TotalUses)
	for _, rr := range remotes {
		var rg groupedUsagesWire
		if err := json.Unmarshal(rr.toolJSON, &rg); err != nil || rg.GroupedBy == "" {
			continue
		}
		anyTruncated = anyTruncated || rg.Truncated || respbudget.Trimmed(rr.toolJSON)
		totalEdgesFloor = max(totalEdgesFloor, max(rg.TotalEdges, rg.TotalUses))
		for _, g := range rg.Groups {
			if g != nil {
				addRows(g, "remote:"+rr.slug)
			}
		}
	}
	// One deterministic global order before any cut: rows within a
	// group by (line, kind, symbol), groups by count desc then path —
	// the same order the local renderer emits. (Map iteration order
	// upstream cannot leak through: both sorts are total orders.)
	groups := make([]*query.UsageFileGroup, 0, len(byFile))
	mergedRows := 0
	for _, g := range byFile {
		// The comparator is a total order over every field of the dedup
		// identity (line, kind, symbol, name, context): two rows the
		// dedup keeps apart must never compare equal, or the limit cut
		// below picks its survivor by source arrival order.
		sort.Slice(g.Uses, func(i, j int) bool {
			a, b := g.Uses[i], g.Uses[j]
			if a.Line != b.Line {
				return a.Line < b.Line
			}
			if a.EdgeKind != b.EdgeKind {
				return a.EdgeKind < b.EdgeKind
			}
			if a.SymbolID != b.SymbolID {
				return a.SymbolID < b.SymbolID
			}
			if a.SymbolName != b.SymbolName {
				return a.SymbolName < b.SymbolName
			}
			return a.Context < b.Context
		})
		g.Count = len(g.Uses)
		mergedRows += g.Count
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return groups[i].File < groups[j].File
	})
	totalEdgesFloor = max(totalEdgesFloor, mergedRows)

	args := subGraphArgsFromBody(body)
	limit := 50
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit > 0 && mergedRows > limit {
		remaining := limit
		kept := groups[:0]
		for _, g := range groups {
			if remaining == 0 {
				break
			}
			if len(g.Uses) > remaining {
				g.Uses = g.Uses[:remaining]
				g.Count = remaining
			}
			remaining -= g.Count
			kept = append(kept, g)
		}
		groups = kept
		anyTruncated = true
		mergedRows = limit
		// Origins annotate returned rows; re-derive from the cut page.
		keep := map[string]bool{}
		for _, g := range groups {
			for _, u := range g.Uses {
				if u.SymbolID != "" {
					keep[u.SymbolID] = true
				}
			}
		}
		for id := range origins {
			if !keep[id] {
				delete(origins, id)
			}
		}
	}

	out := groupedUsagesWire{
		GroupedBy: "file",
		FileCount: len(groups),
		TotalUses: mergedRows,
		Groups:    groups,
		Truncated: anyTruncated,
	}
	if anyTruncated {
		out.TotalEdges = totalEdgesFloor
	}
	b, err := json.Marshal(out)
	if err != nil {
		return local, origins
	}
	return b, origins
}

// mergeSubGraph merges query.SubGraph responses: nodes deduped by string
// ID (local wins), edges by (From,To,Kind,FilePath,Line) so distinct
// call sites of the same pair stay distinct rows. Origins keys each
// node ID to "local" or "remote:<slug>".
//
// Each daemon applied the caller's limit to its own page, so the merged
// row set must honor the contract once, globally: the limit is
// reapplied after a deterministic merge, truncation from any source
// propagates, and — because a source that discarded its tail makes the
// exact deduplicated total unknowable — the merged totals are an
// explicit floor (lower_bound) in that case.
func mergeSubGraph(tool string, body, local []byte, remotes []remoteResult) ([]byte, map[string]string) {
	origins := map[string]string{}
	// A JSON body outside the flat nodes/edges contract (a grouped
	// variant this dispatch does not yet route) must pass through
	// local-only: unmarshaling an unknown shape into a zero-valued
	// SubGraph would replace a correct local answer with an empty one.
	var shape struct {
		GroupedBy string `json:"grouped_by"`
	}
	if json.Unmarshal(local, &shape) == nil && shape.GroupedBy != "" {
		return local, origins
	}
	args := subGraphArgsFromBody(body)
	var sg query.SubGraph
	if err := json.Unmarshal(local, &sg); err != nil {
		return local, origins
	}
	// A source page the budget layer structurally trimmed is incomplete
	// even when its own `truncated` flag is false: the trim's
	// `_truncated_by_budget` marker is a map key, not a SubGraph field,
	// so it must be read off the raw source bytes before the typed
	// round trip discards it.
	anySourceTruncated := sg.Truncated || respbudget.Trimmed(local)
	totalEdgesFloor := sg.TotalEdges
	totalNodesFloor := sg.TotalNodes
	var sourceSummaries []*query.UsageSummary
	if sg.UsageSummary != nil {
		sourceSummaries = append(sourceSummaries, sg.UsageSummary)
	}
	// Zero-edge caveats are judged per SOURCE, each tagged with whether
	// that source resolved the queried node — the gate below needs the
	// distinction in both orientations, local and remote alike.
	type sourceCaveat struct {
		caveat   *graph.ZeroEdgeCaveat
		resolved bool
	}
	// Read the local orientation BEFORE the merge loop: sg.Nodes gains
	// remote rows below, after which this containment check would read
	// "anyone resolved".
	localResolved := nodeListContains(sg.Nodes, args.ID)
	depthUnknown := false
	var caveats []sourceCaveat
	if sg.Caveat != nil {
		caveats = append(caveats, sourceCaveat{sg.Caveat, localResolved})
	}
	seen := make(map[string]bool, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n != nil {
			seen[n.ID] = true
			origins[n.ID] = "local"
		}
	}
	edgeSeen := make(map[string]bool, len(sg.Edges))
	for _, e := range sg.Edges {
		if e != nil {
			edgeSeen[edgeKey(e)] = true
		}
	}
	for _, rr := range remotes {
		var rsg query.SubGraph
		if err := json.Unmarshal(rr.toolJSON, &rsg); err != nil {
			continue
		}
		if rsg.Truncated || respbudget.Trimmed(rr.toolJSON) {
			anySourceTruncated = true
		}
		totalEdgesFloor = max(totalEdgesFloor, rsg.TotalEdges)
		totalNodesFloor = max(totalNodesFloor, rsg.TotalNodes)
		// Remote completeness metadata: a peer's floor makes the merged
		// result a floor; its epistemic boundaries and caller notes are
		// evidence about rows now in the merged set.
		sg.LowerBound = sg.LowerBound || rsg.LowerBound
		sg.Boundaries = appendBoundaries(sg.Boundaries, rsg.Boundaries)
		sg.DynamicBoundaries = appendDynamicBoundaries(sg.DynamicBoundaries, rsg.DynamicBoundaries)
		// Budgeted-walk and freshness metadata: any source's early stop
		// makes the merged result at least as incomplete, the merged
		// depth guarantee is the weakest source's — unknowable when a
		// source stopped on budget without reporting how deep it got —
		// and the merged freshness is the stalest source's.
		sg.BudgetHit = sg.BudgetHit || rsg.BudgetHit
		if rsg.BudgetHit && rsg.StoppedAtDepth == 0 {
			depthUnknown = true
		}
		if rsg.StoppedAtDepth > 0 && (sg.StoppedAtDepth == 0 || rsg.StoppedAtDepth < sg.StoppedAtDepth) {
			sg.StoppedAtDepth = rsg.StoppedAtDepth
		}
		if rsg.LastSynced != nil && (sg.LastSynced == nil || rsg.LastSynced.Before(*sg.LastSynced)) {
			sg.LastSynced = rsg.LastSynced
		}
		if rsg.UsageSummary != nil {
			sourceSummaries = append(sourceSummaries, rsg.UsageSummary)
		}
		// Uncertainty metadata: a peer's caveat, suppression counters,
		// and tier_filtered marker make the merged answer exactly as
		// uncertain as that peer's own answer was. Counts floor at the
		// largest single source — the deduplicated union is unknowable.
		if rsg.Caveat != nil {
			caveats = append(caveats, sourceCaveat{rsg.Caveat, nodeListContains(rsg.Nodes, args.ID)})
		}
		sg.TextMatchedSuppressed = max(sg.TextMatchedSuppressed, rsg.TextMatchedSuppressed)
		sg.NameOnlyCandidates = max(sg.NameOnlyCandidates, rsg.NameOnlyCandidates)
		if sg.SuppressionCaveat == "" {
			sg.SuppressionCaveat = rsg.SuppressionCaveat
		}
		sg.TierFiltered = mergeTierFiltered(sg.TierFiltered, rsg.TierFiltered)
		if len(rsg.CallerNotes) > 0 {
			if sg.CallerNotes == nil {
				sg.CallerNotes = make(map[string]*graph.ConcurrencyAnnotation, len(rsg.CallerNotes))
			}
			for id, note := range rsg.CallerNotes {
				if _, exists := sg.CallerNotes[id]; !exists { // local wins
					sg.CallerNotes[id] = note
				}
			}
		}
		for _, n := range rsg.Nodes {
			if n == nil || seen[n.ID] {
				continue // local wins on collision
			}
			seen[n.ID] = true
			origins[n.ID] = "remote:" + rr.slug
			sg.Nodes = append(sg.Nodes, n)
		}
		for _, e := range rsg.Edges {
			if e == nil {
				continue
			}
			k := edgeKey(e)
			if edgeSeen[k] {
				continue
			}
			edgeSeen[k] = true
			sg.Edges = append(sg.Edges, e)
		}
	}
	if depthUnknown {
		sg.StoppedAtDepth = 0
	}
	// Totals: exact deduplicated counts when every source was complete;
	// otherwise the best available floor — never smaller than any single
	// source's own full count or the merged set itself.
	sg.TotalEdges = max(totalEdgesFloor, len(sg.Edges))
	sg.TotalNodes = max(totalNodesFloor, len(sg.Nodes))
	sg.Truncated = anySourceTruncated
	sg.LowerBound = sg.LowerBound || anySourceTruncated
	// Merged rows can invalidate the local zero-edge caveat: a symbol
	// unused locally but used on a peer must not answer "appears unused"
	// above rows that prove otherwise. Re-judge from the merged edges.
	// With zero merged rows every source answered empty, and the most
	// conservative source caveat survives — a local "likely_unused"
	// must not out-rank a peer's "coverage_incomplete". Two gates,
	// applied to local and remote sources alike, with the class
	// semantics owned by the graph package beside the class producer:
	//
	//   - A source that resolved nothing answers an own-graph-only
	//     caveat about its own graph, not the union: it cannot
	//     displace a CLASSIFICATION from a source that resolved the
	//     node. A resolving source that carried no caveat classified
	//     nothing, so it displaces nothing — without a resolving
	//     classification the gap caveat is the honest answer and
	//     survives, never a bare, confident "0 usages".
	//   - A tier_filtered marker suppresses only the classes it
	//     refutes: it names edges that exist below the requested tier.
	//     A different source's coverage UNCERTAINTY rides alongside
	//     the merged filter marker. (Within one response the handler
	//     keeps the markers exclusive; cross-source they are
	//     independent.)
	if len(sg.Edges) > 0 {
		if graph.WeakUsageEvidenceOnly(sg.Edges) {
			sg.Caveat = graph.CaveatForWeakUsageEvidence()
		} else {
			sg.Caveat = nil
		}
	} else {
		resolvedClassified := false
		for _, sc := range caveats {
			if sc.resolved {
				resolvedClassified = true
			}
		}
		sg.Caveat = nil
		for _, sc := range caveats {
			if graph.ZeroEdgeClassDescribesOwnGraphOnly(sc.caveat.Class) && !sc.resolved && resolvedClassified {
				continue
			}
			if sg.TierFiltered != nil && graph.ZeroEdgeClassRefutedByTierFilter(sc.caveat.Class) {
				continue
			}
			if sg.Caveat == nil || graph.ZeroEdgeClassConservatism(sc.caveat.Class) > graph.ZeroEdgeClassConservatism(sg.Caveat.Class) {
				sg.Caveat = sc.caveat
			}
		}
	}
	// The summary is a whole-set rollup. Recompute it over the merged
	// deduplicated rows with a merged-node getter (the owner hop for
	// child nodes resolves against nodes the merge carried), then floor
	// element-wise against every source's own rollup — a source's counts
	// describe its full set even when only its capped page reached the
	// merge. Attached only when a source carried one (find_usages).
	if len(sourceSummaries) > 0 {
		nodeByID := make(mapNodeGetter, len(sg.Nodes))
		for _, n := range sg.Nodes {
			if n != nil {
				nodeByID[n.ID] = n
			}
		}
		merged := query.UsageSummaryOf(&sg, nodeByID)
		if merged == nil {
			merged = &query.UsageSummary{}
		}
		for _, src := range sourceSummaries {
			merged.NRefs = max(merged.NRefs, src.NRefs)
			merged.NFiles = max(merged.NFiles, src.NFiles)
			merged.NTestRefs = max(merged.NTestRefs, src.NTestRefs)
		}
		sg.UsageSummary = merged
	}

	limit := 50
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit > 0 {
		switch tool {
		case "find_usages":
			// Usage rows page by edges: one stable global order (the
			// same the local row cap consumes), then the cap, once.
			query.SortEdgesForPage(sg.Edges)
			if len(sg.Edges) > limit {
				sg.Edges = sg.Edges[:limit]
				sg.Truncated = true
				keep := map[string]bool{args.ID: true}
				for _, e := range sg.Edges {
					keep[e.From] = true
					keep[e.To] = true
				}
				pruneMergedNodes(&sg, origins, keep)
			}
		default:
			// BFS-shaped tools cap nodes; the merge order (local first,
			// then slug-sorted remotes) is the deterministic page order.
			if len(sg.Nodes) > limit {
				keep := make(map[string]bool, limit)
				sg.Nodes = sg.Nodes[:limit]
				for _, n := range sg.Nodes {
					keep[n.ID] = true
				}
				kept := sg.Edges[:0]
				for _, e := range sg.Edges {
					if keep[e.From] && keep[e.To] {
						kept = append(kept, e)
					}
				}
				sg.Edges = kept
				sg.Truncated = true
				pruneMergedNodes(&sg, origins, keep)
			}
		}
	}
	out, err := json.Marshal(sg)
	if err != nil {
		return local, origins
	}
	return out, origins
}

// mergeTierFiltered floors the tier_filtered marker across sources:
// the counter keeps the largest single source's floor and the
// available tier keeps the strongest tier any source still holds, so
// a min_tier-emptied peer stays legible as "filtered", never as "no
// usages on that peer".
func mergeTierFiltered(local, remote *graph.TierFilteredCaveat) *graph.TierFilteredCaveat {
	if remote == nil {
		return local
	}
	if local == nil {
		c := *remote
		return &c
	}
	local.EdgesBelowMinTier = max(local.EdgesBelowMinTier, remote.EdgesBelowMinTier)
	if graph.OriginRank(remote.MaxAvailableTier) > graph.OriginRank(local.MaxAvailableTier) {
		local.MaxAvailableTier = remote.MaxAvailableTier
	}
	return local
}

// nodeListContains reports whether the queried id is among nodes.
func nodeListContains(nodes []*graph.Node, id string) bool {
	if id == "" {
		return false
	}
	for _, n := range nodes {
		if n != nil && n.ID == id {
			return true
		}
	}
	return false
}

// mapNodeGetter serves graph.NodeGetter lookups from the merged node
// set, so the summary's owner-hop classification resolves against the
// nodes the merge actually carried.
type mapNodeGetter map[string]*graph.Node

func (m mapNodeGetter) GetNode(id string) *graph.Node { return m[id] }

// mergedBoundaryCap bounds each merged boundary section, matching the
// 50-row cap the BFS walk applies to its own boundary recording.
const mergedBoundaryCap = 50

// appendBoundaries appends remote epistemic boundaries, deduplicated by
// (seed, target) — the same key the BFS walk dedups on — and capped so
// a many-peer merge cannot grow the section without bound.
func appendBoundaries(dst, src []graph.EpistemicBoundary) []graph.EpistemicBoundary {
	seen := make(map[string]bool, len(dst))
	for _, b := range dst {
		seen[b.SeedID+"\x00"+b.Target] = true
	}
	for _, b := range src {
		if len(dst) >= mergedBoundaryCap {
			break
		}
		k := b.SeedID + "\x00" + b.Target
		if seen[k] {
			continue
		}
		seen[k] = true
		dst = append(dst, b)
	}
	return dst
}

// appendDynamicBoundaries mirrors appendBoundaries for the dynamic
// section, keyed by the dispatch site tuple.
func appendDynamicBoundaries(dst, src []graph.DynamicBoundary) []graph.DynamicBoundary {
	seen := make(map[string]bool, len(dst))
	for _, b := range dst {
		seen[b.Site+"\x00"+b.Form+"\x00"+b.Key] = true
	}
	for _, b := range src {
		if len(dst) >= mergedBoundaryCap {
			break
		}
		k := b.Site + "\x00" + b.Form + "\x00" + b.Key
		if seen[k] {
			continue
		}
		seen[k] = true
		dst = append(dst, b)
	}
	return dst
}

// pruneMergedNodes drops nodes (and their origins entries) that the
// post-merge cap removed from the response.
func pruneMergedNodes(sg *query.SubGraph, origins map[string]string, keep map[string]bool) {
	nodes := sg.Nodes[:0]
	for _, n := range sg.Nodes {
		if n != nil && keep[n.ID] {
			nodes = append(nodes, n)
		}
	}
	sg.Nodes = nodes
	for id := range origins {
		if !keep[id] {
			delete(origins, id)
		}
	}
	// Notes annotate returned rows; one keyed to a pruned node is noise.
	for id := range sg.CallerNotes {
		if !keep[id] {
			delete(sg.CallerNotes, id)
		}
	}
}

func edgeKey(e *graph.Edge) string {
	return e.From + "\x00" + e.To + "\x00" + string(e.Kind) +
		"\x00" + e.FilePath + "\x00" + strconv.Itoa(e.Line)
}

// idKeyedTools are the SubGraph traversals whose primary query keys on a
// node id; only these get the optional bare-name fallback fan.
var idKeyedTools = map[string]bool{
	"find_usages":    true,
	"get_callers":    true,
	"get_call_chain": true,
}

// commonNames are too-frequent identifiers a bare-name fan would
// mis-match across repos; the fallback skips them even when enabled.
var commonNames = map[string]bool{
	"len": true, "set": true, "get": true, "new": true, "string": true,
	"error": true, "close": true, "read": true, "write": true, "run": true,
	"stop": true, "start": true, "init": true, "name": true, "value": true,
	"key": true, "data": true, "result": true, "next": true, "size": true,
}

// bareNameFromBody extracts the symbol name from the {"arguments":{"id":...}}
// body, stripping the repo/file prefix and the :: separator.
func bareNameFromBody(body []byte) string {
	var b struct {
		Arguments struct {
			ID string `json:"id"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return ""
	}
	id := b.Arguments.ID
	if i := strings.LastIndex(id, "::"); i >= 0 {
		id = id[i+2:]
	}
	return id
}

// nameEligible gates the bare-name fallback on rarity/length so a common
// or short identifier never fans out.
func nameEligible(name string) bool {
	if len(name) < 4 {
		return false
	}
	return !commonNames[strings.ToLower(name)]
}

// nameKeyedFan issues a per-remote search_symbols query for the bare
// name, capping each remote's contribution and tagging every hit with
// its origin + the text_matched confidence tier.
func (f *Federator) nameKeyedFan(ctx context.Context, name string, remotes []ServerEntry) []any {
	const perRemoteCap = 5
	body, _ := json.Marshal(map[string]any{
		"arguments": map[string]any{"query": name, "limit": perRemoteCap, "format": "json"},
	})
	budgetCtx, cancel := context.WithTimeout(ctx, f.cfg.Budget)
	defer cancel()
	var (
		mu   sync.Mutex
		hits []any
		wg   sync.WaitGroup
	)
	for _, rem := range remotes {
		rem := rem
		if f.breaker.isOpen(rem.Slug) {
			continue
		}
		cli, err := f.clientFor(rem)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rctx, rcancel := context.WithTimeout(budgetCtx, f.cfg.PerRemoteTimeout)
			defer rcancel()
			out, status, err := cli.ProxyToolCtx(rctx, "search_symbols", body)
			if err != nil || status >= 400 {
				return
			}
			tj, _ := unwrapToolJSON(out)
			var rp map[string]any
			if json.Unmarshal(tj, &rp) != nil {
				return
			}
			results, _ := rp["results"].([]any)
			mu.Lock()
			for i, r := range results {
				if i >= perRemoteCap {
					break
				}
				if m, ok := r.(map[string]any); ok {
					m["origin"] = "remote:" + rem.Slug
					m["confidence"] = "text_matched"
					hits = append(hits, m)
				}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return hits
}

// injectField adds a top-level key to a JSON object payload.
func injectField(b []byte, key string, value any) []byte {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return b
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	return out
}

// mergeKeyedList merges a {<key>:[{id,...}], total} payload (search_symbols
// results / find_implementations implementations): concat, dedup by native
// id (local wins), sum total.
func mergeKeyedList(local []byte, remotes []remoteResult, key string) ([]byte, map[string]string) {
	origins := map[string]string{}
	var payload map[string]any
	if err := json.Unmarshal(local, &payload); err != nil {
		return local, origins
	}
	items, _ := payload[key].([]any)
	seen := map[string]bool{}
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if id, ok := m["id"].(string); ok && id != "" {
				seen[id] = true
				origins[id] = "local"
			}
		}
	}
	total := toInt(payload["total"])
	for _, rr := range remotes {
		var rp map[string]any
		if err := json.Unmarshal(rr.toolJSON, &rp); err != nil {
			continue
		}
		ritems, _ := rp[key].([]any)
		for _, it := range ritems {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			if id != "" && seen[id] {
				continue
			}
			if id != "" {
				seen[id] = true
				origins[id] = "remote:" + rr.slug
			}
			items = append(items, m)
		}
		total += toInt(rp["total"])
	}
	payload[key] = items
	payload["total"] = total
	out, err := json.Marshal(payload)
	if err != nil {
		return local, origins
	}
	return out, origins
}

// mergeSmartContext keeps the local manifest authoritative and folds each
// remote's contribution into a SEPARATE remote_context section under its
// slug. The edit_plan is NEVER federated (edits are local-only).
func mergeSmartContext(local []byte, remotes []remoteResult) ([]byte, map[string]string) {
	origins := map[string]string{}
	var payload map[string]any
	if err := json.Unmarshal(local, &payload); err != nil {
		return local, origins
	}
	var sections []any
	for _, rr := range remotes {
		var rp map[string]any
		if err := json.Unmarshal(rr.toolJSON, &rp); err != nil {
			continue
		}
		section := map[string]any{"slug": rr.slug}
		// Carry the remote's relevant symbols / manifest, never its
		// edit_plan.
		for _, k := range []string{"relevant_symbols", "context_manifest", "symbols"} {
			if v, ok := rp[k]; ok {
				section[k] = v
			}
		}
		sections = append(sections, section)
	}
	if len(sections) > 0 {
		payload["remote_context"] = sections
	}
	delete(payload, "")
	out, err := json.Marshal(payload)
	if err != nil {
		return local, origins
	}
	return out, origins
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// attachFederation adds the federation{} block + origins map as siblings
// on a JSON-object tool response. A non-object payload is returned
// unchanged (no place to attach).
func attachFederation(toolJSON []byte, meta FederationMeta) []byte {
	var m map[string]any
	if err := json.Unmarshal(toolJSON, &m); err != nil {
		return toolJSON
	}
	fed := map[string]any{
		"remotes_queried": meta.RemotesQueried,
		"degraded":        meta.Degraded,
	}
	if len(meta.RemotesFailed) > 0 {
		fed["remotes_failed"] = meta.RemotesFailed
	}
	if len(meta.NamespaceRewrites) > 0 {
		fed["namespace_rewrites"] = meta.NamespaceRewrites
	}
	if meta.Note != "" {
		fed["note"] = meta.Note
	}
	m["federation"] = fed
	if len(meta.Origins) > 0 {
		m["origins"] = meta.Origins
	}
	out, err := json.Marshal(m)
	if err != nil {
		return toolJSON
	}
	return out
}

// unwrapToolJSON extracts the tool's JSON payload from the MCP result
// envelope ({content:[{type:text,text:<json>}]}). When the bytes are not
// that envelope, they are returned as-is with wrapped=false.
func unwrapToolJSON(b []byte) (toolJSON []byte, wrapped bool) {
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &env); err != nil || len(env.Content) == 0 {
		return b, false
	}
	if env.Content[0].Type != "text" || env.Content[0].Text == "" {
		return b, false
	}
	return []byte(env.Content[0].Text), true
}

// rewrapToolJSON replaces the text payload of an MCP result envelope with
// newToolJSON, preserving the envelope's other fields (e.g. is_error).
func rewrapToolJSON(envelope, newToolJSON []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(envelope, &m); err != nil {
		return newToolJSON
	}
	content, ok := m["content"].([]any)
	if !ok || len(content) == 0 {
		return newToolJSON
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		return newToolJSON
	}
	first["text"] = string(newToolJSON)
	content[0] = first
	m["content"] = content
	out, err := json.Marshal(m)
	if err != nil {
		return newToolJSON
	}
	return out
}
