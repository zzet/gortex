// Edge-driven analyzers built on top of the coverage parser
// extensions: channel send/recv, goroutine spawns, field writes,
// annotations, config readers, event emitters, error throws. Each
// walks one (kind -> edge kind) family and groups call sites by
// target so producer/consumer mismatches and hotspots become a
// graph query rather than a grep run.
//
// Conventions match the older analyzers in tools_enhancements.go:
//   - typed `<kind>Row` struct, JSON output by default,
//   - optional `compact: true` for one-line text,
//   - optional `format: "gcx"` for the GCX1 wire format,
//   - stable sort orders so diffs are predictable across runs.
//
// Filters are intentionally narrow: each analyzer surfaces the
// dimensions the parser actually emits in meta. Adding new filters
// means new meta keys upstream; we don't fabricate dimensions
// here.
package mcp

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// ---------------------------------------------------------------------------
// channel_ops — list channels with their senders/receivers.
// ---------------------------------------------------------------------------

// handleAnalyzeChannelOps walks every EdgeSends and EdgeRecvs edge
// and groups by channel target. The resulting rows surface
// producer/consumer mismatches (channels with sends but no
// receivers, or vice-versa) and concurrency hotspots (channels with
// many senders or receivers fanning across the codebase).
//
// Channels in v1 are extracted as `unresolved::<name>` targets
// because Go's tree-sitter pass doesn't propagate channel typing.
// The analyzer therefore reports the synthetic target id, not a
// fully-resolved channel symbol. That's the same fidelity
// `find_usages` would give a caller today and lets the rows be
// diff-able across runs.
//
// Filters:
//
//   - path_prefix: scope to operations originating in a directory
//     subtree. Useful for "channel discipline in package X".
func (s *Server) handleAnalyzeChannelOps(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pathPrefix := strings.TrimSpace(stringArg(req.GetArguments(), "path_prefix"))

	type channelRow struct {
		Channel   string   `json:"channel"`
		Sends     int      `json:"sends"`
		Recvs     int      `json:"recvs"`
		Senders   []string `json:"senders,omitempty"`
		Receivers []string `json:"receivers,omitempty"`
	}
	byChannel := map[string]*channelRow{}
	get := func(target string) *channelRow {
		row, ok := byChannel[target]
		if !ok {
			row = &channelRow{Channel: target}
			byChannel[target] = row
		}
		return row
	}

	// One scan over Sends+Recvs only — replaces the legacy AllEdges()
	// walk that pulled every edge over cgo just to keep two kinds. The
	// reader is hoisted so an overlay-active request walks the pushed
	// buffers for both the scan and the node lookups below.
	g := s.readerFor(ctx)
	for e := range edgesByKinds(g, graph.EdgeSends, graph.EdgeRecvs) {
		if !graphpath.HasPrefix(e.FilePath, pathPrefix) {
			continue
		}
		row := get(e.To)
		if e.Kind == graph.EdgeSends {
			row.Sends++
			row.Senders = appendUnique(row.Senders, e.From)
		} else {
			row.Recvs++
			row.Receivers = appendUnique(row.Receivers, e.From)
		}
	}

	rows := make([]*channelRow, 0, len(byChannel))
	for _, r := range byChannel {
		sort.Strings(r.Senders)
		sort.Strings(r.Receivers)
		rows = append(rows, r)
	}
	// Scope filter: keep a channel row iff its target node is visible to
	// the request (session workspace ceiling + optional repo allow-set),
	// and prune sender/receiver lists to visible nodes only. No-op for an
	// unbound, un-narrowed request. Sends/Recvs are edge counts, left as-is.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*channelRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, g.GetNode(r.Channel)) {
				continue
			}
			senders := make([]string, 0, len(r.Senders))
			for _, id := range r.Senders {
				if s.analyzeNodeVisible(ctx, g.GetNode(id)) {
					senders = append(senders, id)
				}
			}
			r.Senders = senders
			receivers := make([]string, 0, len(r.Receivers))
			for _, id := range r.Receivers {
				if s.analyzeNodeVisible(ctx, g.GetNode(id)) {
					receivers = append(receivers, id)
				}
			}
			r.Receivers = receivers
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		// Total op count desc; tie-break by channel id for stability.
		ti := rows[i].Sends + rows[i].Recvs
		tj := rows[j].Sends + rows[j].Recvs
		if ti != tj {
			return ti > tj
		}
		return rows[i].Channel < rows[j].Channel
	})

	if s.isGCX(ctx, req) {
		items := make([]channelOpItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, channelOpItem{
				Channel:   r.Channel,
				Sends:     r.Sends,
				Recvs:     r.Recvs,
				Senders:   strings.Join(r.Senders, ","),
				Receivers: strings.Join(r.Receivers, ","),
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("channel_ops", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "send=%d recv=%d  %s\n", r.Sends, r.Recvs, r.Channel)
		}
		if len(rows) == 0 {
			b.WriteString("no channel ops\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"channels": rows,
		"total":    len(rows),
	})
}

// ---------------------------------------------------------------------------
// goroutine_spawns — list spawn sites grouped by spawned target.
// ---------------------------------------------------------------------------

// handleAnalyzeGoroutineSpawns walks every EdgeSpawns edge and
// groups by spawned target. The mode meta (goroutine / async /
// promise / worker_pool) is surfaced verbatim so cross-language
// concurrency hotspots stay separable. Useful for spotting leaks
// (a single function with many spawn sites), unowned background
// work, and codebase-wide concurrency hygiene reviews.
func (s *Server) handleAnalyzeGoroutineSpawns(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Concurrency carries the shared sync_guarded / cross_concurrent
	// classification of the spawned target: sync_guarded reports
	// whether the goroutine body is a method on a lock-holding type,
	// and cross_concurrent confirms the target is reached across a
	// concurrency boundary. Omitted (nil) when neither flag is set.
	type spawnRow struct {
		Target      string                       `json:"target"`
		Mode        string                       `json:"mode,omitempty"`
		Spawns      int                          `json:"spawns"`
		Spawners    []string                     `json:"spawners,omitempty"`
		Concurrency *graph.ConcurrencyAnnotation `json:"concurrency,omitempty"`
	}
	byTarget := map[string]*spawnRow{}

	g := s.readerFor(ctx)
	for e := range edgesByKinds(g, graph.EdgeSpawns) {
		mode, _ := e.Meta["mode"].(string)
		key := e.To + "|" + mode
		row, ok := byTarget[key]
		if !ok {
			row = &spawnRow{Target: e.To, Mode: mode}
			byTarget[key] = row
		}
		row.Spawns++
		row.Spawners = appendUnique(row.Spawners, e.From)
	}

	rows := make([]*spawnRow, 0, len(byTarget))
	for _, r := range byTarget {
		sort.Strings(r.Spawners)
		if ann := graph.ClassifyConcurrency(g, r.Target); ann.Any() {
			a := ann
			r.Concurrency = &a
		}
		rows = append(rows, r)
	}
	// Scope filter: keep a spawn row iff its spawned target is visible,
	// and prune spawners to visible nodes only. No-op when unbound.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*spawnRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, g.GetNode(r.Target)) {
				continue
			}
			spawners := make([]string, 0, len(r.Spawners))
			for _, id := range r.Spawners {
				if s.analyzeNodeVisible(ctx, g.GetNode(id)) {
					spawners = append(spawners, id)
				}
			}
			r.Spawners = spawners
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Spawns != rows[j].Spawns {
			return rows[i].Spawns > rows[j].Spawns
		}
		if rows[i].Target != rows[j].Target {
			return rows[i].Target < rows[j].Target
		}
		return rows[i].Mode < rows[j].Mode
	})

	if s.isGCX(ctx, req) {
		items := make([]spawnItem, 0, len(rows))
		for _, r := range rows {
			it := spawnItem{
				Target:   r.Target,
				Mode:     r.Mode,
				Spawns:   r.Spawns,
				Spawners: strings.Join(r.Spawners, ","),
			}
			if r.Concurrency != nil {
				it.SyncGuarded = r.Concurrency.SyncGuarded
				it.SyncGuardedWhy = r.Concurrency.SyncGuardedWhy
				it.CrossConcurrent = r.Concurrency.CrossConcurrent
				it.CrossConcurrentWhy = r.Concurrency.CrossConcurrentWhy
			}
			items = append(items, it)
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("goroutine_spawns", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			mode := r.Mode
			if mode == "" {
				mode = "?"
			}
			fmt.Fprintf(&b, "%-3d [%s] %s", r.Spawns, mode, r.Target)
			if r.Concurrency != nil {
				if r.Concurrency.SyncGuarded {
					b.WriteString(" sync_guarded")
				}
				if r.Concurrency.CrossConcurrent {
					b.WriteString(" cross_concurrent")
				}
			}
			b.WriteByte('\n')
		}
		if len(rows) == 0 {
			b.WriteString("no spawn sites\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"spawns": rows,
		"total":  len(rows),
	})
}

// ---------------------------------------------------------------------------
// field_writers — for a given field id (or top-N most-written
// fields when no id given), list writer functions.
// ---------------------------------------------------------------------------

// handleAnalyzeFieldWriters walks EdgeWrites edges with a field
// target and groups by field. With `id` the analyzer scopes to one
// field — useful for "who mutates Server.config?". Without `id` it
// surfaces the top-N most-written fields, the mutability hotspot
// view.
//
// Filters:
//
//   - id: a specific field node id. When set, only that field is
//     reported. Useful for targeted review of a single field's
//     write surface.
//   - limit: max rows when no id is set (default 20).
func (s *Server) handleAnalyzeFieldWriters(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	idFilter := strings.TrimSpace(stringArg(args, "id"))
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	type writerRow struct {
		Field   string   `json:"field"`
		Writes  int      `json:"writes"`
		Writers []string `json:"writers,omitempty"`
	}
	byField := map[string]*writerRow{}

	g := s.readerFor(ctx)
	for e := range edgesByKinds(g, graph.EdgeWrites) {
		if idFilter != "" && e.To != idFilter {
			continue
		}
		// Only count writes whose target resolves to a field node.
		// Pre-resolution edges land on `unresolved::*.foo` and would
		// muddy the per-field rollup; the resolver post-pass
		// rewrites the To, so any unresolved edges left at query
		// time are a different problem.
		if idFilter == "" {
			target := g.GetNode(e.To)
			if target == nil || target.Kind != graph.KindField {
				continue
			}
		}
		row, ok := byField[e.To]
		if !ok {
			row = &writerRow{Field: e.To}
			byField[e.To] = row
		}
		row.Writes++
		row.Writers = appendUnique(row.Writers, e.From)
	}

	rows := make([]*writerRow, 0, len(byField))
	for _, r := range byField {
		sort.Strings(r.Writers)
		rows = append(rows, r)
	}
	// Scope filter: keep a field row iff the field node is visible, and
	// prune writers to visible nodes only. No-op when unbound.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*writerRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, g.GetNode(r.Field)) {
				continue
			}
			writers := make([]string, 0, len(r.Writers))
			for _, id := range r.Writers {
				if s.analyzeNodeVisible(ctx, g.GetNode(id)) {
					writers = append(writers, id)
				}
			}
			r.Writers = writers
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Writes != rows[j].Writes {
			return rows[i].Writes > rows[j].Writes
		}
		return rows[i].Field < rows[j].Field
	})
	truncated := false
	if idFilter == "" && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		items := make([]fieldWriterItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, fieldWriterItem{
				Field:   r.Field,
				Writes:  r.Writes,
				Writers: strings.Join(r.Writers, ","),
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("field_writers", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d  %s\n", r.Writes, r.Field)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated to %d\n", limit)
		}
		if len(rows) == 0 {
			b.WriteString("no field writes\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	resp := map[string]any{
		"fields":    rows,
		"total":     len(rows),
		"truncated": truncated,
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// handleAnalyzeIndirectMutations lists fields mutated *indirectly* — via a
// method call on the field (s.counter.Increment()) or a sibling call on the
// receiver (s.helper()) — surfacing the `via` method for each. These are the
// accesses_field edges tagged Meta["indirect"]=true synthesized by the
// receiver-mutation fixpoint. With `id` it scopes to one field.
func (s *Server) handleAnalyzeIndirectMutations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	idFilter := strings.TrimSpace(stringArg(args, "id"))
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	type mutator struct {
		Function string `json:"function"`
		Via      string `json:"via,omitempty"`
		File     string `json:"file,omitempty"`
		Line     int    `json:"line,omitempty"`
	}
	type fieldRow struct {
		Field     string    `json:"field"`
		Mutations int       `json:"mutations"`
		Mutators  []mutator `json:"mutators,omitempty"`
	}
	byField := map[string]*fieldRow{}

	g := s.readerFor(ctx)
	for e := range edgesByKinds(g, graph.EdgeAccessesField) {
		if e.Meta == nil {
			continue
		}
		if ind, _ := e.Meta["indirect"].(bool); !ind {
			continue
		}
		if idFilter != "" && e.To != idFilter {
			continue
		}
		via, _ := e.Meta["via"].(string)
		row, ok := byField[e.To]
		if !ok {
			row = &fieldRow{Field: e.To}
			byField[e.To] = row
		}
		row.Mutations++
		row.Mutators = append(row.Mutators, mutator{Function: e.From, Via: via, File: e.FilePath, Line: e.Line})
	}

	rows := make([]*fieldRow, 0, len(byField))
	for _, r := range byField {
		sort.Slice(r.Mutators, func(i, j int) bool { return r.Mutators[i].Function < r.Mutators[j].Function })
		rows = append(rows, r)
	}
	// Scope filter: keep a field row iff the field node is visible, and
	// prune mutators to those whose function node is visible. The `via`
	// method name is not a node ID, so it is left intact. No-op when unbound.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*fieldRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, g.GetNode(r.Field)) {
				continue
			}
			mutators := make([]mutator, 0, len(r.Mutators))
			for _, m := range r.Mutators {
				if s.analyzeNodeVisible(ctx, g.GetNode(m.Function)) {
					mutators = append(mutators, m)
				}
			}
			r.Mutators = mutators
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Mutations != rows[j].Mutations {
			return rows[i].Mutations > rows[j].Mutations
		}
		return rows[i].Field < rows[j].Field
	})
	truncated := false
	if idFilter == "" && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"fields":    rows,
		"total":     len(rows),
		"truncated": truncated,
	})
}

// handleAnalyzeSpeculative is the audit surface for opt-in speculative
// dynamic-dispatch edges: it groups the best-guess call edges (Meta
// speculative=true) by their shape (computed_member / getattr / …) with a
// candidate-count histogram and samples, so an agent can review what the
// best-guess synthesizer produced before trusting any of it.
func (s *Server) handleAnalyzeSpeculative(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type sample struct {
		From           string `json:"from"`
		To             string `json:"to"`
		CandidateCount int    `json:"candidate_count,omitempty"`
		Confidence     string `json:"confidence,omitempty"`
	}
	type shapeRow struct {
		Shape   string   `json:"shape"`
		Edges   int      `json:"edges"`
		Samples []sample `json:"samples,omitempty"`
	}
	byShape := map[string]*shapeRow{}
	total := 0
	scoped := s.scopeFiltersActive(ctx)
	g := s.readerFor(ctx)
	for e := range edgesByKinds(g, graph.EdgeCalls) {
		if !e.IsSpeculative() {
			continue
		}
		// Scope filter: a speculative edge is a from->to peer pair; drop it
		// unless both endpoints are visible. Gating the loop recomputes
		// Edges, total, and Samples together. No-op when unbound.
		if scoped && (!s.analyzeNodeVisible(ctx, g.GetNode(e.From)) || !s.analyzeNodeVisible(ctx, g.GetNode(e.To))) {
			continue
		}
		total++
		shape, _ := e.Meta["via"].(string)
		shape = strings.TrimPrefix(shape, "speculative.")
		if shape == "" {
			shape = "unknown"
		}
		row, ok := byShape[shape]
		if !ok {
			row = &shapeRow{Shape: shape}
			byShape[shape] = row
		}
		row.Edges++
		if len(row.Samples) < 10 {
			cc, _ := e.Meta["candidate_count"].(int)
			row.Samples = append(row.Samples, sample{From: e.From, To: e.To, CandidateCount: cc, Confidence: e.ConfidenceLabel})
		}
	}
	rows := make([]*shapeRow, 0, len(byShape))
	for _, r := range byShape {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Edges != rows[j].Edges {
			return rows[i].Edges > rows[j].Edges
		}
		return rows[i].Shape < rows[j].Shape
	})
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"shapes": rows,
		"total":  total,
		"note":   "speculative edges are best-guess fan-outs, hidden from default queries; pass include_speculative:true to surface them",
	})
}

// handleAnalyzeRefFacts surfaces resolved-reference facts — each reference edge
// that resolved to a concrete target, with the provenance tier that resolved
// it. The durable form is the backend's ref_facts sidecar; this read derives
// the same facts from the live graph (backend-agnostic). With `id` it scopes to
// references originating in one file.
func (s *Server) handleAnalyzeRefFacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	fileFilter := strings.TrimSpace(stringArg(args, "id"))
	if fileFilter == "" {
		fileFilter = strings.TrimSpace(stringArg(args, "path"))
	}
	limit := 200
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	type fact struct {
		From    string `json:"from"`
		To      string `json:"to"`
		Kind    string `json:"kind"`
		RefName string `json:"ref_name,omitempty"`
		Origin  string `json:"origin,omitempty"`
		Tier    string `json:"tier,omitempty"`
		File    string `json:"file,omitempty"`
	}

	g := s.readerFor(ctx)
	var nodes []*graph.Node
	if fileFilter != "" {
		nodes = g.GetFileNodes(fileFilter)
	} else {
		for _, k := range []graph.NodeKind{graph.KindFunction, graph.KindMethod} {
			for n := range g.NodesByKind(k) {
				nodes = append(nodes, n)
			}
		}
	}

	var facts []fact
	truncated := false
	scoped := s.scopeFiltersActive(ctx)
	for _, n := range nodes {
		if n == nil {
			continue
		}
		// Scope filter: drop reference facts whose origin node is out of
		// scope. No-op when unbound.
		if scoped && !s.analyzeNodeVisible(ctx, n) {
			continue
		}
		for _, e := range g.GetOutEdges(n.ID) {
			if e == nil || !graph.IsResolvableRefEdge(e.Kind) {
				continue
			}
			if e.To == "" || graph.IsUnresolvedTarget(e.To) || graph.IsStub(e.To) {
				continue
			}
			// Scope filter: also drop facts whose resolved target is out
			// of scope, so no cross-workspace target id leaks into a row.
			if scoped && !s.analyzeNodeVisible(ctx, g.GetNode(e.To)) {
				continue
			}
			refName := ""
			if t := g.GetNode(e.To); t != nil {
				refName = t.Name
			}
			origin := e.Origin
			if origin == "" {
				sem, _ := e.Meta["semantic_source"].(string)
				origin = graph.DefaultOriginFor(e.Kind, e.Confidence, sem)
			}
			facts = append(facts, fact{
				From: e.From, To: e.To, Kind: string(e.Kind), RefName: refName,
				Origin: origin, Tier: graph.ResolvedBy(origin), File: n.FilePath,
			})
			if len(facts) >= limit {
				truncated = true
				break
			}
		}
		if truncated {
			break
		}
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"facts":     facts,
		"total":     len(facts),
		"truncated": truncated,
	})
}

// ---------------------------------------------------------------------------
// annotation_users — for an annotation node id (or list all when no
// id), surface annotated symbols.
// ---------------------------------------------------------------------------

// handleAnalyzeAnnotationUsers walks EdgeAnnotated edges. With `id`
// the analyzer scopes to one annotation target — "every symbol
// annotated `@Deprecated`". Without `id` it lists every distinct
// annotation found, with annotated count, so the agent can decide
// which one to dive into. The `name` filter narrows the
// annotation-name match without requiring a synthetic id.
//
// Filters:
//
//   - id: annotation node id (e.g. `annotation::java::Deprecated`).
//     Returns one row per annotated symbol.
//   - name: annotation bare name (case-insensitive). Returns one
//     row per matching annotation grouped by id.
func (s *Server) handleAnalyzeAnnotationUsers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	idFilter := strings.TrimSpace(stringArg(args, "id"))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))
	g := s.readerFor(ctx)

	if idFilter != "" {
		type annotatedRow struct {
			Symbol string `json:"symbol"`
			File   string `json:"file"`
			Line   int    `json:"line"`
			Args   string `json:"args,omitempty"`
		}
		var rows []annotatedRow
		for e := range edgesByKinds(g, graph.EdgeAnnotated) {
			if e.To != idFilter {
				continue
			}
			argsStr, _ := e.Meta["args"].(string)
			rows = append(rows, annotatedRow{
				Symbol: e.From,
				File:   e.FilePath,
				Line:   e.Line,
				Args:   argsStr,
			})
		}
		// Scope filter: keep an annotated-symbol row iff the annotated
		// symbol node is visible. No-op when unbound.
		if s.scopeFiltersActive(ctx) {
			kept := make([]annotatedRow, 0, len(rows))
			for _, r := range rows {
				if s.analyzeNodeVisible(ctx, g.GetNode(r.Symbol)) {
					kept = append(kept, r)
				}
			}
			rows = kept
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].File != rows[j].File {
				return rows[i].File < rows[j].File
			}
			return rows[i].Line < rows[j].Line
		})

		if s.isGCX(ctx, req) {
			items := make([]annotatedItem, 0, len(rows))
			for _, r := range rows {
				items = append(items, annotatedItem(r))
			}
			return s.gcxResponseWithBudget(req)(encodeAnalyze("annotation_users", items))
		}
		if isCompact(req) {
			var b strings.Builder
			for _, r := range rows {
				if r.Args != "" {
					fmt.Fprintf(&b, "%s:%d  %s  (%s)\n", r.File, r.Line, r.Symbol, r.Args)
				} else {
					fmt.Fprintf(&b, "%s:%d  %s\n", r.File, r.Line, r.Symbol)
				}
			}
			if len(rows) == 0 {
				b.WriteString("no users for that annotation\n")
			}
			return mcp.NewToolResultText(b.String()), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"annotation": idFilter,
			"users":      rows,
			"total":      len(rows),
		})
	}

	// No id — group annotations by id, count usages.
	type annoRow struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Users int    `json:"users"`
	}
	byID := map[string]*annoRow{}
	for e := range edgesByKinds(g, graph.EdgeAnnotated) {
		row, ok := byID[e.To]
		if !ok {
			n := g.GetNode(e.To)
			name := ""
			if n != nil {
				name = n.Name
			}
			if nameFilter != "" && strings.ToLower(name) != nameFilter {
				continue
			}
			row = &annoRow{ID: e.To, Name: name}
			byID[e.To] = row
		}
		row.Users++
	}
	rows := make([]*annoRow, 0, len(byID))
	for _, r := range byID {
		rows = append(rows, r)
	}
	// Scope filter: keep an annotation row iff the annotation node is
	// visible. No-op when unbound. Users is a count, left as-is.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*annoRow, 0, len(rows))
		for _, r := range rows {
			if s.analyzeNodeVisible(ctx, g.GetNode(r.ID)) {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Users != rows[j].Users {
			return rows[i].Users > rows[j].Users
		}
		return rows[i].ID < rows[j].ID
	})

	if s.isGCX(ctx, req) {
		items := make([]annotationItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, annotationItem{
				ID:    r.ID,
				Name:  r.Name,
				Users: r.Users,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("annotation_users.list", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-4d  %s  (%s)\n", r.Users, r.Name, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no annotations\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"annotations": rows,
		"total":       len(rows),
	})
}

// ---------------------------------------------------------------------------
// config_readers — list config_key nodes with their readers.
// ---------------------------------------------------------------------------

// handleAnalyzeConfigReaders walks EdgeReadsConfig edges and
// groups by config-key target. Each row carries the config-key id,
// its surface name, the source (env/viper/etc — kept verbatim from
// node meta), and the reader symbol list. Useful for tracing
// configuration drift ("which functions read DATABASE_URL?") and
// finding hot config keys with many readers.
//
// Filters:
//
//   - name: config-key bare name (case-insensitive). Returns the
//     readers of that single key.
//   - limit: max rows when no name filter is set (default 20).
func (s *Server) handleAnalyzeConfigReaders(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	type configRow struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Source  string   `json:"source,omitempty"`
		Readers []string `json:"readers,omitempty"`
		Reads   int      `json:"reads"`
	}
	byKey := map[string]*configRow{}
	g := s.readerFor(ctx)
	for e := range edgesByKinds(g, graph.EdgeReadsConfig) {
		row, ok := byKey[e.To]
		if !ok {
			n := g.GetNode(e.To)
			name := ""
			source := ""
			if n != nil {
				name = n.Name
				source, _ = n.Meta["source"].(string)
			}
			if nameFilter != "" && strings.ToLower(name) != nameFilter {
				continue
			}
			row = &configRow{ID: e.To, Name: name, Source: source}
			byKey[e.To] = row
		}
		row.Reads++
		row.Readers = appendUnique(row.Readers, e.From)
	}

	rows := make([]*configRow, 0, len(byKey))
	for _, r := range byKey {
		sort.Strings(r.Readers)
		rows = append(rows, r)
	}
	// Scope filter: keep a config-key row iff the key node is visible, and
	// prune readers to visible nodes only. No-op when unbound.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*configRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, g.GetNode(r.ID)) {
				continue
			}
			readers := make([]string, 0, len(r.Readers))
			for _, id := range r.Readers {
				if s.analyzeNodeVisible(ctx, g.GetNode(id)) {
					readers = append(readers, id)
				}
			}
			r.Readers = readers
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Reads != rows[j].Reads {
			return rows[i].Reads > rows[j].Reads
		}
		return rows[i].ID < rows[j].ID
	})
	truncated := false
	if nameFilter == "" && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		items := make([]configReaderItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, configReaderItem{
				ID:      r.ID,
				Name:    r.Name,
				Source:  r.Source,
				Reads:   r.Reads,
				Readers: strings.Join(r.Readers, ","),
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("config_readers", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			source := r.Source
			if source == "" {
				source = "?"
			}
			fmt.Fprintf(&b, "%-3d [%s] %s\n", r.Reads, source, r.Name)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated to %d\n", limit)
		}
		if len(rows) == 0 {
			b.WriteString("no config readers\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"config_keys": rows,
		"total":       len(rows),
		"truncated":   truncated,
	})
}

// isEnvConfigKey reports whether a config-key node represents an
// environment variable (as opposed to a viper key, a K8s ConfigMap
// entry, a struct-tag default, …).
func isEnvConfigKey(n *graph.Node) bool {
	if n == nil || n.Kind != graph.KindConfigKey {
		return false
	}
	if src, _ := n.Meta["source"].(string); src == "env" {
		return true
	}
	return strings.HasPrefix(n.ID, "env::") || strings.HasPrefix(n.ID, "cfg::env::")
}

// handleAnalyzeEnvVarUsers walks EdgeReadsConfig edges restricted to
// environment-variable config keys and groups by the variable. Each
// row carries the env var name and the symbols that read it — the
// answer to "which functions depend on DATABASE_URL?" and which env
// vars are the most load-bearing.
//
// Filters: name (env var, case-insensitive), limit.
func (s *Server) handleAnalyzeEnvVarUsers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	type envRow struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Readers []string `json:"readers,omitempty"`
		Reads   int      `json:"reads"`
	}
	byKey := map[string]*envRow{}
	g := s.readerFor(ctx)
	for e := range edgesByKinds(g, graph.EdgeReadsConfig) {
		row, ok := byKey[e.To]
		if !ok {
			n := g.GetNode(e.To)
			if !isEnvConfigKey(n) {
				continue
			}
			if nameFilter != "" && strings.ToLower(n.Name) != nameFilter {
				continue
			}
			row = &envRow{ID: e.To, Name: n.Name}
			byKey[e.To] = row
		}
		row.Reads++
		row.Readers = appendUnique(row.Readers, e.From)
	}

	rows := make([]*envRow, 0, len(byKey))
	for _, r := range byKey {
		sort.Strings(r.Readers)
		rows = append(rows, r)
	}
	// Scope filter: keep an env-var row iff the key node is visible, and
	// prune readers to visible nodes only. No-op when unbound.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*envRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, g.GetNode(r.ID)) {
				continue
			}
			readers := make([]string, 0, len(r.Readers))
			for _, id := range r.Readers {
				if s.analyzeNodeVisible(ctx, g.GetNode(id)) {
					readers = append(readers, id)
				}
			}
			r.Readers = readers
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Reads != rows[j].Reads {
			return rows[i].Reads > rows[j].Reads
		}
		return rows[i].Name < rows[j].Name
	})
	truncated := false
	if nameFilter == "" && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("env_var_users", rows))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d %s\n", r.Reads, r.Name)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated to %d\n", limit)
		}
		if len(rows) == 0 {
			b.WriteString("no env var users\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"env_vars":  rows,
		"total":     len(rows),
		"truncated": truncated,
	})
}

// ---------------------------------------------------------------------------
// event_emitters — list event nodes with their emitters.
// ---------------------------------------------------------------------------

// handleAnalyzeEventEmitters walks EdgeEmits edges and groups by
// event target. The `level` filter narrows by log level when meta
// carries it (error/warn/info/debug); without a level filter every
// event is included. Useful for "every site that logs an error" or
// "what does this metric get emitted from".
//
// Filters:
//
//   - level: log/event level — error, warn, warning, info, debug,
//     trace, fatal. Case-insensitive. The matcher checks both
//     `level` and the `event_kind`/`method` meta keys so it works
//     across the parsers that use different conventions.
//   - name: event name (case-insensitive). Returns the emitters
//     for that single event.
func (s *Server) handleAnalyzeEventEmitters(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	levelFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "level")))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))

	type eventRow struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Kind     string   `json:"event_kind,omitempty"`
		Emits    int      `json:"emits"`
		Emitters []string `json:"emitters,omitempty"`
	}
	byEvent := map[string]*eventRow{}
	g := s.readerFor(ctx)
	for e := range edgesByKinds(g, graph.EdgeEmits) {
		// Level filter: an emit edge stores the method on the edge
		// (e.g. "Errorf"); the event node may carry an event_kind.
		// We accept either source so both per-event and per-call
		// taxonomies match.
		if levelFilter != "" {
			method, _ := e.Meta["method"].(string)
			if !levelMatches(levelFilter, method) {
				n := g.GetNode(e.To)
				if n == nil {
					continue
				}
				kind, _ := n.Meta["event_kind"].(string)
				if !levelMatches(levelFilter, kind) {
					continue
				}
			}
		}
		row, ok := byEvent[e.To]
		if !ok {
			n := g.GetNode(e.To)
			name := ""
			kind := ""
			if n != nil {
				name = n.Name
				kind, _ = n.Meta["event_kind"].(string)
			}
			if nameFilter != "" && strings.ToLower(name) != nameFilter {
				continue
			}
			row = &eventRow{ID: e.To, Name: name, Kind: kind}
			byEvent[e.To] = row
		}
		row.Emits++
		row.Emitters = appendUnique(row.Emitters, e.From)
	}
	rows := make([]*eventRow, 0, len(byEvent))
	for _, r := range byEvent {
		sort.Strings(r.Emitters)
		rows = append(rows, r)
	}
	// Scope filter: keep an event row iff the event node is visible, and
	// prune emitters to visible nodes only. No-op when unbound.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*eventRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, g.GetNode(r.ID)) {
				continue
			}
			emitters := make([]string, 0, len(r.Emitters))
			for _, id := range r.Emitters {
				if s.analyzeNodeVisible(ctx, g.GetNode(id)) {
					emitters = append(emitters, id)
				}
			}
			r.Emitters = emitters
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Emits != rows[j].Emits {
			return rows[i].Emits > rows[j].Emits
		}
		return rows[i].ID < rows[j].ID
	})

	if s.isGCX(ctx, req) {
		items := make([]eventEmitterItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, eventEmitterItem{
				ID:       r.ID,
				Name:     r.Name,
				Kind:     r.Kind,
				Emits:    r.Emits,
				Emitters: strings.Join(r.Emitters, ","),
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("event_emitters", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			kind := r.Kind
			if kind == "" {
				kind = "?"
			}
			fmt.Fprintf(&b, "%-3d [%s] %s\n", r.Emits, kind, r.Name)
		}
		if len(rows) == 0 {
			b.WriteString("no event emitters\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"events": rows,
		"total":  len(rows),
	})
}

// ---------------------------------------------------------------------------
// pubsub — event pub/sub topics with their publishers and subscribers.
// ---------------------------------------------------------------------------

// handleAnalyzePubsub walks the EdgeEmits (publish) and EdgeListensOn
// (subscribe) edges of the event pub/sub layer and groups them by
// topic node. Each row is one KindEvent topic (Meta["event_kind"]=
// "pubsub"): its transport (nats|kafka|rabbitmq|redis|socketio|
// eventemitter|unknown), the count + symbol list of publishers, and
// the count + symbol list of subscribers. Lets agents answer "who
// publishes order.created and who listens for it" with one graph walk,
// and spot orphan topics — published with no subscriber, or subscribed
// with no publisher.
//
// Filters: `transport` (one broker label), `name` (exact topic name),
// `role` (publish | subscribe — keep only topics with at least one
// edge of that side).
func (s *Server) handleAnalyzePubsub(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	transportFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "transport")))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))
	roleFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "role")))

	type pubsubRow struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Transport   string   `json:"transport,omitempty"`
		Publishes   int      `json:"publishes"`
		Subscribes  int      `json:"subscribes"`
		Publishers  []string `json:"publishers,omitempty"`
		Subscribers []string `json:"subscribers,omitempty"`
	}
	byTopic := map[string]*pubsubRow{}
	// rejected caches topic nodes that failed the transport/name
	// filter (or aren't pub/sub events) so a topic with many edges
	// isn't re-checked per edge.
	rejected := map[string]struct{}{}

	g := s.readerFor(ctx)
	ensureRow := func(nodeID string) *pubsubRow {
		if row, ok := byTopic[nodeID]; ok {
			return row
		}
		if _, ok := rejected[nodeID]; ok {
			return nil
		}
		n := g.GetNode(nodeID)
		if n == nil || n.Meta == nil {
			rejected[nodeID] = struct{}{}
			return nil
		}
		if kind, _ := n.Meta["event_kind"].(string); kind != "pubsub" {
			rejected[nodeID] = struct{}{}
			return nil
		}
		transport, _ := n.Meta["transport"].(string)
		if transportFilter != "" && strings.ToLower(transport) != transportFilter {
			rejected[nodeID] = struct{}{}
			return nil
		}
		if nameFilter != "" && strings.ToLower(n.Name) != nameFilter {
			rejected[nodeID] = struct{}{}
			return nil
		}
		row := &pubsubRow{ID: nodeID, Name: n.Name, Transport: transport}
		byTopic[nodeID] = row
		return row
	}

	for e := range edgesByKinds(g, graph.EdgeEmits, graph.EdgeListensOn) {
		switch e.Kind {
		case graph.EdgeEmits:
			row := ensureRow(e.To)
			if row == nil {
				continue
			}
			row.Publishes++
			row.Publishers = appendUnique(row.Publishers, e.From)
		case graph.EdgeListensOn:
			row := ensureRow(e.To)
			if row == nil {
				continue
			}
			row.Subscribes++
			row.Subscribers = appendUnique(row.Subscribers, e.From)
		}
	}

	rows := make([]*pubsubRow, 0, len(byTopic))
	for _, r := range byTopic {
		switch roleFilter {
		case "publish":
			if r.Publishes == 0 {
				continue
			}
		case "subscribe":
			if r.Subscribes == 0 {
				continue
			}
		}
		sort.Strings(r.Publishers)
		sort.Strings(r.Subscribers)
		rows = append(rows, r)
	}
	// Scope filter: keep a topic row iff the topic node is visible, and
	// prune publisher/subscriber lists to visible nodes only. No-op when
	// unbound. Publishes/Subscribes are edge counts, left as-is.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*pubsubRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, g.GetNode(r.ID)) {
				continue
			}
			publishers := make([]string, 0, len(r.Publishers))
			for _, id := range r.Publishers {
				if s.analyzeNodeVisible(ctx, g.GetNode(id)) {
					publishers = append(publishers, id)
				}
			}
			r.Publishers = publishers
			subscribers := make([]string, 0, len(r.Subscribers))
			for _, id := range r.Subscribers {
				if s.analyzeNodeVisible(ctx, g.GetNode(id)) {
					subscribers = append(subscribers, id)
				}
			}
			r.Subscribers = subscribers
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		ti := rows[i].Publishes + rows[i].Subscribes
		tj := rows[j].Publishes + rows[j].Subscribes
		if ti != tj {
			return ti > tj
		}
		return rows[i].ID < rows[j].ID
	})

	if s.isGCX(ctx, req) {
		items := make([]pubsubItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, pubsubItem{
				ID:          r.ID,
				Name:        r.Name,
				Transport:   r.Transport,
				Publishes:   r.Publishes,
				Subscribes:  r.Subscribes,
				Publishers:  strings.Join(r.Publishers, ","),
				Subscribers: strings.Join(r.Subscribers, ","),
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("pubsub", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			transport := r.Transport
			if transport == "" {
				transport = "?"
			}
			fmt.Fprintf(&b, "%dP/%dS [%s] %s\n", r.Publishes, r.Subscribes, transport, r.Name)
		}
		if len(rows) == 0 {
			b.WriteString("no pub/sub topics\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"topics": rows,
		"total":  len(rows),
	})
}

// ---------------------------------------------------------------------------
// error_surface — function/method nodes with their EdgeThrows
// targets.
// ---------------------------------------------------------------------------

// handleAnalyzeErrorSurface walks EdgeThrows edges and groups by
// thrower (the From side — the function/method that returns or
// raises the error). Each row lists the error types it can produce.
// Useful for sketching the error surface of a package, for finding
// functions that throw too many distinct error kinds, and for
// confirming that a refactor didn't widen the error surface
// inadvertently.
//
// Filters:
//
//   - path_prefix: scope to throwers under a directory subtree.
func (s *Server) handleAnalyzeErrorSurface(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pathPrefix := strings.TrimSpace(stringArg(req.GetArguments(), "path_prefix"))

	type throwerRow struct {
		Symbol    string   `json:"symbol"`
		File      string   `json:"file"`
		Line      int      `json:"line"`
		Throws    int      `json:"throws"`
		Errors    []string `json:"errors"`
		ErrorMsgs []string `json:"error_msgs,omitempty"`
	}
	rows := make([]*throwerRow, 0)
	// The surfacer probe runs on the request reader: an overlay view does
	// not implement the capability, so an overlay-active request falls
	// through to the edge walk below and reads the pushed buffers.
	g := s.readerFor(ctx)
	if surfacer, ok := g.(graph.ThrowerErrorSurfacer); ok {
		// Server-side path: one server-side aggregate for the per-thrower
		// throws+targets dedup, one for the per-thrower error-msg
		// attachment. No per-thrower GetOutEdges fanout.
		for _, r := range surfacer.ThrowerErrorSurface(pathPrefix) {
			row := &throwerRow{
				Symbol:    r.ThrowerID,
				File:      r.FilePath,
				Line:      r.Line,
				Throws:    r.Throws,
				Errors:    append([]string(nil), r.ErrorTargets...),
				ErrorMsgs: append([]string(nil), r.ErrorMsgs...),
			}
			sort.Strings(row.Errors)
			sort.Strings(row.ErrorMsgs)
			rows = append(rows, row)
		}
	} else {
		byThrower := map[string]*throwerRow{}
		for e := range edgesByKinds(g, graph.EdgeThrows) {
			if !graphpath.HasPrefix(e.FilePath, pathPrefix) {
				continue
			}
			row, ok := byThrower[e.From]
			if !ok {
				n := g.GetNode(e.From)
				file := e.FilePath
				line := e.Line
				if n != nil {
					if file == "" {
						file = n.FilePath
					}
					if line == 0 {
						line = n.StartLine
					}
				}
				row = &throwerRow{Symbol: e.From, File: file, Line: line}
				byThrower[e.From] = row
			}
			row.Throws++
			row.Errors = appendUnique(row.Errors, e.To)
		}
		// For every thrower, also surface the error_msg KindString
		// literals it emits. EdgeThrows targets error types; the
		// data-side companion (errors.New("…") → string::error_msg::…)
		// carries the literal message.
		for thrower, row := range byThrower {
			for _, e := range g.GetOutEdges(thrower) {
				if e == nil || e.Kind != graph.EdgeEmits {
					continue
				}
				n := g.GetNode(e.To)
				if n == nil || n.Kind != graph.KindString {
					continue
				}
				ctxLabel, _ := n.Meta["context"].(string)
				if ctxLabel != "error_msg" {
					continue
				}
				row.ErrorMsgs = appendUnique(row.ErrorMsgs, n.Name)
			}
		}
		for _, r := range byThrower {
			sort.Strings(r.Errors)
			sort.Strings(r.ErrorMsgs)
			rows = append(rows, r)
		}
	}
	// Scope filter: keep a thrower row iff the throwing symbol is visible,
	// and prune the error-target list to visible nodes only. Error message
	// literals (ErrorMsgs) are string values, not node ids, left intact.
	// Applies to both the ThrowerErrorSurfacer and fallback build paths.
	// No-op when unbound.
	if s.scopeFiltersActive(ctx) {
		kept := make([]*throwerRow, 0, len(rows))
		for _, r := range rows {
			if !s.analyzeNodeVisible(ctx, g.GetNode(r.Symbol)) {
				continue
			}
			errs := make([]string, 0, len(r.Errors))
			for _, id := range r.Errors {
				if s.analyzeNodeVisible(ctx, g.GetNode(id)) {
					errs = append(errs, id)
				}
			}
			r.Errors = errs
			kept = append(kept, r)
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		// Throwers with the most distinct error targets surface
		// first — those are the highest-leverage refactor candidates.
		ai, aj := len(rows[i].Errors), len(rows[j].Errors)
		if ai != aj {
			return ai > aj
		}
		if rows[i].Throws != rows[j].Throws {
			return rows[i].Throws > rows[j].Throws
		}
		return rows[i].Symbol < rows[j].Symbol
	})

	// Cap response size — large repos blow past the MCP token cap on
	// the JSON shape. Default 200 keeps the response useful while
	// callers that need everything pass an explicit limit.
	limit := 200
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	totalRows := len(rows)
	rowsTruncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		rowsTruncated = true
	}

	if s.isGCX(ctx, req) {
		items := make([]errorSurfaceItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, errorSurfaceItem{
				Symbol:    r.Symbol,
				File:      r.File,
				Line:      r.Line,
				Throws:    r.Throws,
				Errors:    strings.Join(r.Errors, ","),
				ErrorMsgs: strings.Join(r.ErrorMsgs, "\x1f"),
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("error_surface", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d  %s:%d  %s  -> %s\n",
				len(r.Errors), r.File, r.Line, r.Symbol, strings.Join(r.Errors, ","))
		}
		if len(rows) == 0 {
			b.WriteString("no error surface\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	resp := map[string]any{
		"throwers":  rows,
		"total":     totalRows,
		"truncated": rowsTruncated,
	}
	if rowsTruncated {
		resp["limit"] = limit
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// ---------------------------------------------------------------------------
// cross_repo — list repo-boundary-crossing call / type-hierarchy edges.
// ---------------------------------------------------------------------------

// handleAnalyzeCrossRepo walks the cross_repo_* edge layer (M3) and
// groups edges by (source repo, target repo, base relation). Each row
// is one directed dependency between two repos — "repo A's code calls
// into repo B", "a type in repo A implements an interface in repo B" —
// with a count and a capped sample of the underlying edges.
//
// The cross_repo_* edges are materialised by the resolver's
// DetectCrossRepoEdges pass; this analyzer is a pure read over that
// graph layer, so it stays correct under incremental reindex without
// re-deriving the repo-boundary test here.
//
// Filters:
//
//   - path_prefix: scope to edges anchored in a directory subtree
//     (matches the call/declaration site's file path).
//   - repo: scope to dependencies that touch a repo on either side
//     (source or target).
//   - kind: scope to one base relation — "calls", "implements", or
//     "extends".
func (s *Server) handleAnalyzeCrossRepo(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	repoFilter := strings.TrimSpace(stringArg(args, "repo"))
	// The `kind` arg is consumed by the analyze dispatcher to route
	// here; an optional base-relation filter rides on `base_kind`
	// (one of calls / implements / extends).
	kindFilter := strings.TrimSpace(stringArg(args, "base_kind"))

	type crossRepoRow struct {
		FromRepo string   `json:"from_repo"`
		ToRepo   string   `json:"to_repo"`
		Kind     string   `json:"kind"`
		Count    int      `json:"count"`
		Samples  []string `json:"samples,omitempty"`
	}
	byKey := map[string]*crossRepoRow{}

	g := s.readerFor(ctx)
	// repoOf prefers the live node's RepoPrefix and falls back to the
	// edge Meta stamped at materialisation time — Meta is dropped by
	// snapshot round-trips, so the node lookup is the durable path.
	repoOf := func(id, metaKey string, e *graph.Edge) string {
		if n := g.GetNode(id); n != nil && n.RepoPrefix != "" {
			return n.RepoPrefix
		}
		if e.Meta != nil {
			if r, _ := e.Meta[metaKey].(string); r != "" {
				return r
			}
		}
		return ""
	}

	for e := range edgesByKinds(g,
		graph.EdgeCrossRepoCalls,
		graph.EdgeCrossRepoImplements,
		graph.EdgeCrossRepoExtends,
	) {
		base, ok := graph.BaseKindForCrossRepo(e.Kind)
		if !ok {
			continue
		}
		if !graphpath.HasPrefix(e.FilePath, pathPrefix) {
			continue
		}
		if kindFilter != "" && string(base) != kindFilter {
			continue
		}
		fromRepo := repoOf(e.From, "source_repo", e)
		toRepo := repoOf(e.To, "target_repo", e)
		if repoFilter != "" && fromRepo != repoFilter && toRepo != repoFilter {
			continue
		}
		key := fromRepo + "\x00" + toRepo + "\x00" + string(base)
		row, ok := byKey[key]
		if !ok {
			row = &crossRepoRow{FromRepo: fromRepo, ToRepo: toRepo, Kind: string(base)}
			byKey[key] = row
		}
		row.Count++
		// Cap the per-row sample list — a hot repo pair can have
		// thousands of edges and the samples are only an orientation
		// aid, not the payload.
		if len(row.Samples) < 20 {
			row.Samples = appendUnique(row.Samples, e.From+" -> "+e.To)
		}
	}

	rows := make([]*crossRepoRow, 0, len(byKey))
	for _, r := range byKey {
		sort.Strings(r.Samples)
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if rows[i].FromRepo != rows[j].FromRepo {
			return rows[i].FromRepo < rows[j].FromRepo
		}
		if rows[i].ToRepo != rows[j].ToRepo {
			return rows[i].ToRepo < rows[j].ToRepo
		}
		return rows[i].Kind < rows[j].Kind
	})

	limit := 200
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	totalRows := len(rows)
	rowsTruncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		rowsTruncated = true
	}

	if s.isGCX(ctx, req) {
		items := make([]crossRepoItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, crossRepoItem{
				FromRepo: r.FromRepo,
				ToRepo:   r.ToRepo,
				Kind:     r.Kind,
				Count:    r.Count,
				Samples:  strings.Join(r.Samples, ","),
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("cross_repo", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-4d  %s -> %s  (%s)\n", r.Count, r.FromRepo, r.ToRepo, r.Kind)
		}
		if len(rows) == 0 {
			b.WriteString("no cross-repo edges\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	resp := map[string]any{
		"dependencies": rows,
		"total":        totalRows,
		"truncated":    rowsTruncated,
	}
	if rowsTruncated {
		resp["limit"] = limit
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// edgesByKinds streams every edge whose Kind is in the supplied set
// using the EdgesByKindsScanner capability when the backend
// implements it (one round-trip with a `kind IN (…)` filter), or
// falls back to per-kind EdgesByKind iteration otherwise.
//
// The edge-driven analyzers below use it instead of `for _, e := range
// s.graph.AllEdges() { switch e.Kind … }` so the disk backends stop
// materialising the full edge table over cgo for a handful of kinds.
// Pass each kind as a separate argument — kinds typed inline as a
// variadic so call sites read as `edgesByKinds(g, EdgeEmits,
// EdgeListensOn)` rather than constructing a slice each time.
//
// Empty kinds yields nothing — matches both the capability contract
// and the original semantics (no kinds requested means no rows).
func edgesByKinds(g graph.Reader, kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	if len(kinds) == 0 {
		return func(yield func(*graph.Edge) bool) {}
	}
	if scanner, ok := g.(graph.EdgesByKindsScanner); ok {
		return scanner.EdgesByKinds(kinds)
	}
	return func(yield func(*graph.Edge) bool) {
		for _, k := range kinds {
			if k == "" {
				continue
			}
			for e := range g.EdgesByKind(k) {
				if !yield(e) {
					return
				}
			}
		}
	}
}

// appendUnique returns dst with v added if not already present.
// Used by every analyzer above to dedupe the From-side caller list
// without falling back to a map (the lists are small per row, so a
// linear scan is cheaper than building a set per group).
func appendUnique(dst []string, v string) []string {
	if v == "" {
		return dst
	}
	if slices.Contains(dst, v) {
		return dst
	}
	return append(dst, v)
}

// levelMatches returns true when the requested level matches the
// candidate string. The match is case-insensitive and accepts
// common aliases (warn/warning, debug/trace) so callers can pass
// either the canonical level or the parser's verbatim method/kind
// without ceremony.
func levelMatches(want, candidate string) bool {
	candidate = strings.ToLower(candidate)
	if candidate == "" {
		return false
	}
	if strings.Contains(candidate, want) {
		return true
	}
	switch want {
	case "warn", "warning":
		return strings.Contains(candidate, "warn")
	case "info":
		return strings.Contains(candidate, "info")
	case "debug", "trace":
		return strings.Contains(candidate, "debug") || strings.Contains(candidate, "trace")
	case "error":
		return strings.Contains(candidate, "error") || strings.Contains(candidate, "err")
	case "fatal":
		return strings.Contains(candidate, "fatal") || strings.Contains(candidate, "panic")
	}
	return false
}
