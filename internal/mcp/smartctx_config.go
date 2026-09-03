package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/callpath"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/eval/quality"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/query"
)

// smartContextSections resolves which in-pack enrichment sections a
// smart_context call should attach. Per-call include_* params override the
// project's smart_context config; every section is off by default.
func (s *Server) smartContextSections(args map[string]any, relPath string) config.SmartContextSections {
	cfg := config.SmartContextConfig{}
	if s.configManager != nil {
		// Base read: the repo prefix keys a config lookup, and config is a
		// property of the indexed repo, not of a session's edits.
		cfg = s.configManager.GetRepoConfig(repoPrefixForPath(s.graph, relPath)).MCP.SmartContext
	}
	return cfg.Resolve(
		boolPtrArg(args, "include_call_paths"),
		boolPtrArg(args, "include_flows"),
		boolPtrArg(args, "include_confidence"),
	)
}

// boolPtrArg returns a *bool: the parsed value when the caller passed the key,
// nil when absent — so an unset flag inherits config rather than forcing false.
func boolPtrArg(args map[string]any, key string) *bool {
	if v, set := boolArg(args, key); set {
		return &v
	}
	return nil
}

// attachInPackSections records the opt-in in-pack enrichment sections on the
// assembled pack under result["in_pack"]. Only sections with content are
// written, so the default pack stays untouched; later passes attach the flow
// spine and confidence verdict to the same block.
func (s *Server) attachInPackSections(ctx context.Context, result map[string]any, sections config.SmartContextSections, symbols []*graph.Node) {
	block := map[string]any{}
	if sections.CallPaths {
		// The annotation follows the engine, not its output: "no call path
		// between these symbols" is a claim about the base corpus the
		// traversal ran over just as much as a path is.
		cp := s.inPackCallPaths(symbols)
		annotateBaseScoped(ctx, graphview.CapSyntaxGraph, graphview.CapResolutionLocal)
		if len(cp) > 0 {
			block["call_paths"] = cp
		}
	}
	if sections.Flows {
		if fl := s.inPackFlows(ctx, symbols); fl != nil {
			block["flows"] = fl
		}
	}
	if len(block) > 0 {
		result["in_pack"] = block
	}
}

// inPackFlows builds the flow section: a forward flow spine from the focus
// symbol (the first pack symbol) and the dynamic-dispatch boundaries that spine
// hits — call sites whose target the static graph cannot resolve, where the
// flow would continue at runtime. Returns nil when there is no multi-node spine
// and no boundary to announce.
func (s *Server) inPackFlows(ctx context.Context, symbols []*graph.Node) map[string]any {
	if s.readerFor(ctx) == nil || len(symbols) == 0 || symbols[0] == nil {
		return nil
	}
	budget := s.inPackBudget()
	spine, boundaries := s.flowSpine(ctx, symbols[0].ID, budget.FlowDepth)
	if len(boundaries) > budget.MaxBoundaries {
		boundaries = boundaries[:budget.MaxBoundaries]
	}
	out := map[string]any{}
	if len(spine) >= 2 {
		out["spine"] = spine
	}
	if len(boundaries) > 0 {
		out["boundaries"] = boundaries
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// packRecoveryKinds are the edge kinds the edge-recovery pass restores between
// pack symbols — the structural relations a many-rooted retrieval BFS leaves
// disconnected.
var packRecoveryKinds = map[graph.EdgeKind]bool{
	graph.EdgeCalls:      true,
	graph.EdgeExtends:    true,
	graph.EdgeImplements: true,
	graph.EdgeReferences: true,
	graph.EdgeOverrides:  true,
}

// recoverPackEdges returns the edges (calls/extends/implements/references/
// overrides) that exist between the pack's symbols — the internal connectivity
// a many-rooted retrieval leaves out. Returns nil when fewer than two symbols
// are in the pack or none are connected.
func (s *Server) recoverPackEdges(ctx context.Context, symbols []*graph.Node) []map[string]any {
	g := s.readerFor(ctx)
	if g == nil || len(symbols) < 2 {
		return nil
	}
	ids := make(map[string]bool, len(symbols))
	ordered := make([]string, 0, len(symbols))
	for _, n := range symbols {
		if n != nil && n.ID != "" && !ids[n.ID] {
			ids[n.ID] = true
			ordered = append(ordered, n.ID)
		}
	}
	sort.Strings(ordered)
	seen := map[string]bool{}
	var out []map[string]any
	for _, id := range ordered {
		for _, e := range g.GetOutEdges(id) {
			if e == nil || !packRecoveryKinds[e.Kind] || !ids[e.To] {
				continue
			}
			key := e.From + "\x00" + e.To + "\x00" + string(e.Kind)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, map[string]any{"from": e.From, "to": e.To, "kind": string(e.Kind)})
		}
	}
	return out
}

// packHierarchySiblings returns type symbols that share a supertype with a pack
// type — its siblings in the class hierarchy (e.g. a pack's InternalEngine and
// the sibling ReadOnlyEngine that both extend Engine). Already-packed types are
// excluded; the result is capped.
func (s *Server) packHierarchySiblings(ctx context.Context, symbols []*graph.Node) []map[string]any {
	g := s.readerFor(ctx)
	if g == nil {
		return nil
	}
	inPack := map[string]bool{}
	for _, n := range symbols {
		if n != nil {
			inPack[n.ID] = true
		}
	}
	parents := map[string]bool{}
	for _, n := range symbols {
		if n == nil || (n.Kind != graph.KindType && n.Kind != graph.KindInterface) {
			continue
		}
		for _, e := range g.GetOutEdges(n.ID) {
			if e != nil && (e.Kind == graph.EdgeExtends || e.Kind == graph.EdgeImplements) {
				parents[e.To] = true
			}
		}
	}
	if len(parents) == 0 {
		return nil
	}
	parentIDs := make([]string, 0, len(parents))
	for p := range parents {
		parentIDs = append(parentIDs, p)
	}
	sort.Strings(parentIDs)

	sibSeen := map[string]bool{}
	var out []map[string]any
	for _, parent := range parentIDs {
		for _, e := range g.GetInEdges(parent) {
			if e == nil || (e.Kind != graph.EdgeExtends && e.Kind != graph.EdgeImplements) {
				continue
			}
			sib := e.From
			if inPack[sib] || sibSeen[sib] {
				continue
			}
			n := g.GetNode(sib)
			if n == nil {
				continue
			}
			sibSeen[sib] = true
			out = append(out, map[string]any{"id": sib, "name": n.Name, "kind": string(n.Kind), "parent": parent})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return str(out[i]["id"]) < str(out[j]["id"]) })
	if len(out) > 10 {
		out = out[:10]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// addInPackSection records one section on result["in_pack"], creating the block
// if it does not exist yet.
func addInPackSection(result map[string]any, key string, val any) {
	inPack, _ := result["in_pack"].(map[string]any)
	if inPack == nil {
		inPack = map[string]any{}
	}
	inPack[key] = val
	result["in_pack"] = inPack
}

// inPackConfidence builds the retrieval-confidence verdict for a task: it runs a
// ranked search for the task and summarises the candidate-score distribution
// (how sharply the top result beats the rest) into a high/medium/low verdict.
// Returns nil when the search yields nothing.
func (s *Server) inPackConfidence(ctx context.Context, task string) map[string]any {
	eng := s.engineFor(ctx)
	if eng == nil || task == "" {
		return nil
	}
	cands := eng.SearchSymbolsRanked(task, 10, query.QueryOptions{}, nil)
	if len(cands) == 0 {
		return nil
	}
	scores := make([]float64, 0, len(cands))
	for _, c := range cands {
		if c != nil {
			scores = append(scores, c.Score)
		}
	}
	return buildConfidenceVerdict(quality.ConfidenceFromScores(task, scores))
}

// buildConfidenceVerdict reduces a confidence record to the in-pack verdict map.
// Returns nil for an empty record.
func buildConfidenceVerdict(rec quality.ConfidenceRecord) map[string]any {
	if rec.K == 0 {
		return nil
	}
	return map[string]any{
		"verdict":         confidenceVerdict(rec),
		"top1":            rec.Top1,
		"top2":            rec.Top2,
		"ratio_top1_top2": rec.Ratio12,
		"k":               rec.K,
		"std_dev":         rec.StdDev,
	}
}

// confidenceVerdict classifies a confidence record: a single candidate is
// "single", a sharp top-1 (≥2× the runner-up) is "high", a modest lead
// "medium", and a flat distribution "low" (the ranker is unsure).
func confidenceVerdict(rec quality.ConfidenceRecord) string {
	switch {
	case rec.K <= 1:
		return "single"
	case rec.Ratio12 >= 2.0:
		return "high"
	case rec.Ratio12 >= 1.25:
		return "medium"
	default:
		return "low"
	}
}

// lowConfidenceRetrievalNote returns the always-on compact retrieval note for
// a smart_context pack whose underlying retrieval looks untrustworthy. Unlike
// the opt-in verbose confidence block, this fires by default — but only when
// the signal is strong: a flat ranked-score distribution OR a head symbol
// anchored solely by speculative (best-guess dynamic-dispatch) edges. It is
// suppressed for single-symbol / distinctive-identifier lookups. Returns nil
// when the retrieval looks sound or there is nothing to assess.
func (s *Server) lowConfidenceRetrievalNote(ctx context.Context, task string, syms []*graph.Node) map[string]any {
	if strings.TrimSpace(task) == "" || len(syms) == 0 {
		return nil
	}
	eng := s.engineFor(ctx)
	if eng == nil {
		return nil
	}
	cands := eng.SearchSymbolsRanked(task, 10, query.QueryOptions{}, nil)
	scores := make([]float64, 0, len(cands))
	for _, c := range cands {
		if c != nil {
			scores = append(scores, c.Score)
		}
	}
	rec := quality.ConfidenceFromScores(task, scores)
	return retrievalNoteFor(task, rec, s.headAnchorsAllSpeculative(ctx, syms), likelyDirsFromSymbols(syms, 4))
}

// retrievalNoteFor is the pure decision behind lowConfidenceRetrievalNote: it
// turns the task, ranked-confidence record, head-anchor provenance, and
// candidate directories into the compact note (or nil). Kept side-effect-free
// so the trigger logic is unit-testable without a live engine.
func retrievalNoteFor(task string, rec quality.ConfidenceRecord, headSpeculative bool, dirs []string) map[string]any {
	// A single distinctive identifier is an exact lookup, not a fuzzy search —
	// never hedge it. A single ranked candidate is likewise confident.
	if isDistinctiveIdentifierQuery(task) || rec.K <= 1 {
		return nil
	}
	var reason string
	switch {
	case confidenceVerdict(rec) == "low":
		reason = fmt.Sprintf("ranked candidates are tightly clustered (top1/top2 ratio %.2f, std_dev %.3f) — the pack may be off-target",
			rec.Ratio12, rec.StdDev)
	case headSpeculative:
		// Provenance fusion: even a non-flat distribution is untrustworthy
		// when the head symbol is reachable only by speculative edges.
		reason = "the top result is anchored only by speculative (best-guess dynamic-dispatch) edges — treat the pack as a lead, not a fact"
	default:
		return nil
	}
	note := map[string]any{
		"verdict":         "low",
		"reason":          reason,
		"suggested_tools": []string{"find_usages", "search_text", "find_files"},
	}
	if len(dirs) > 0 {
		note["likely_dirs"] = dirs
	}
	return note
}

// isDistinctiveIdentifierQuery reports whether the task is a single token with
// the shape of a deliberately-typed code identifier (camelCase / snake_case /
// has digits) — an exact lookup that should never draw a low-confidence hedge.
func isDistinctiveIdentifierQuery(task string) bool {
	fields := strings.Fields(task)
	return len(fields) == 1 && hasIdentifierShape(fields[0])
}

// headAnchorsAllSpeculative reports whether the top pack symbol's semantic
// in-edges (calls / references / overrides / implements) exist and are ALL
// speculative — i.e. the symbol is reachable only by best-guess dispatch, so
// the retrieval that surfaced it rests on the weakest provenance tier.
// Structural containment edges are ignored; a symbol with no semantic in-edges
// is not treated as speculatively anchored.
func (s *Server) headAnchorsAllSpeculative(ctx context.Context, syms []*graph.Node) bool {
	if len(syms) == 0 || syms[0] == nil {
		return false
	}
	eng := s.engineFor(ctx)
	if eng == nil {
		return false
	}
	var sawSemantic bool
	for _, e := range eng.GetInEdges(syms[0].ID) {
		if e == nil {
			continue
		}
		switch e.Kind {
		case graph.EdgeCalls, graph.EdgeReferences, graph.EdgeOverrides, graph.EdgeImplements:
			sawSemantic = true
			if e.Origin != graph.OriginSpeculative {
				return false
			}
		}
	}
	return sawSemantic
}

// likelyDirsFromSymbols collects, in pack order, the distinct parent
// directories of the pack symbols' files (capped at max) — the dirs an agent
// should grep / list when the retrieval note flags low confidence.
func likelyDirsFromSymbols(syms []*graph.Node, max int) []string {
	seen := make(map[string]bool)
	dirs := make([]string, 0, max)
	for _, sym := range syms {
		if sym == nil || sym.FilePath == "" {
			continue
		}
		d := slashDir(sym.FilePath)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		dirs = append(dirs, d)
		if len(dirs) >= max {
			break
		}
	}
	return dirs
}

// slashDir returns the parent directory of a slash-or-OS path, slash-normalised
// and portable (filepath.Dir would mis-handle "/"-paths on Windows).
func slashDir(p string) string {
	p = filepath.ToSlash(p)
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return ""
}

// flowSpine greedily walks forward from the focus over resolved CALLS/REFERENCES
// edges (smallest target id first, for determinism), returning the chain of
// node ids it traverses and the dynamic-dispatch boundaries — out-edges to
// unresolved targets — encountered along the way.
func (s *Server) flowSpine(ctx context.Context, focus string, maxDepth int) (spine []string, boundaries []map[string]any) {
	g := s.readerFor(ctx)
	if g == nil {
		return nil, nil
	}
	visited := map[string]bool{focus: true}
	bseen := map[string]bool{}
	spine = []string{focus}
	cur := focus
	for depth := 0; depth < maxDepth; depth++ {
		next := ""
		for _, e := range g.GetOutEdges(cur) {
			if e == nil || (e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences) {
				continue
			}
			if graph.IsUnresolvedTarget(e.To) {
				key := e.From + "\x00" + e.To
				if !bseen[key] {
					bseen[key] = true
					boundaries = append(boundaries, map[string]any{
						"from":   e.From,
						"target": graph.UnresolvedName(e.To),
						"reason": "dynamic_dispatch",
					})
				}
				continue
			}
			if visited[e.To] {
				continue
			}
			if next == "" || e.To < next {
				next = e.To
			}
		}
		if next == "" {
			break
		}
		visited[next] = true
		spine = append(spine, next)
		cur = next
	}
	return spine, boundaries
}

// inPackCallPaths builds the anchored call-paths section: the focus symbol (the
// first pack symbol) is the anchor and the rest are roots, so each row shows how
// another pack symbol reaches the focus over the call graph. Returns nil when
// fewer than two symbols are in the pack or none reach the focus.
func (s *Server) inPackCallPaths(symbols []*graph.Node) []map[string]any {
	if s.graph == nil || len(symbols) < 2 {
		return nil
	}
	anchor := symbols[0].ID
	roots := make([]string, 0, len(symbols)-1)
	for _, n := range symbols[1:] {
		if n != nil && n.ID != "" {
			roots = append(roots, n.ID)
		}
	}
	// callpath.New builds its traversal state over a graph.Store, so these
	// paths are computed over the base corpus even when the request carries
	// an overlay or a routed view; the caller attaching the section annotates
	// the response for a view.
	anchored := callpath.New(s.graph).PathsToAnchor(roots, anchor, callpath.Options{MaxDepth: 8})
	if len(anchored) == 0 {
		return nil
	}
	if limit := s.inPackBudget().MaxCallPaths; len(anchored) > limit {
		anchored = anchored[:limit]
	}
	out := make([]map[string]any, 0, len(anchored))
	for _, ap := range anchored {
		out = append(out, map[string]any{
			"root":       ap.Root,
			"anchor":     anchor,
			"length":     ap.Path.Length,
			"confidence": ap.Path.Confidence,
			"nodes":      ap.Path.Nodes,
		})
	}
	return out
}
