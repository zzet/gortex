package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// rrfK is the standard reciprocal-rank-fusion smoothing constant: the
// fused contribution of an item ranked r in one signal is 1/(k+r).
// k=60 keeps single-signal rank-1 hits from drowning out items that
// place moderately well across several signals.
const rrfK = 60.0

// bridgeSideEntry is one provider- or consumer-side participant of a
// contract bridge group: where the contract lives and which symbol
// carries it.
type bridgeSideEntry struct {
	ContractID string `json:"contract_id"`
	Repo       string `json:"repo,omitempty"`
	SymbolID   string `json:"symbol_id,omitempty"`
	FilePath   string `json:"file_path,omitempty"`
	Line       int    `json:"line,omitempty"`
}

// bridgeGroupResult is one ranked contract-bridge group: the bridge
// node plus its provider side, consumer side, and (in rank mode) the
// fused score with per-signal ranks.
type bridgeGroupResult struct {
	BridgeID      string            `json:"bridge_id"`
	CanonicalKey  string            `json:"canonical_key"`
	ContractType  string            `json:"contract_type"`
	Repos         []string          `json:"repos,omitempty"`
	CrossRepo     bool              `json:"cross_repo,omitempty"`
	ProviderCount int               `json:"provider_count"`
	ConsumerCount int               `json:"consumer_count"`
	Providers     []bridgeSideEntry `json:"providers"`
	Consumers     []bridgeSideEntry `json:"consumers"`
	FusedScore    float64           `json:"fused_score,omitempty"`
	SignalRanks   map[string]int    `json:"signal_ranks,omitempty"`
	// MatchedVia lists the anchor contract IDs that reached this
	// bridge in impact mode.
	MatchedVia []string `json:"matched_via,omitempty"`
}

// handleContractBridges serves `contracts action=bridge`: queries the
// persisted contract-bridge subgraph (KindContractBridge nodes +
// EdgeBridges fan-out materialised by the indexer's contract
// reconcile).
//
// Two modes:
//
//   - rank (default): order bridge groups by reciprocal rank fusion
//     over independent signal rankings — text match on the canonical
//     key/contract names, path+repo match, graph adjacency to the
//     given symbol, and bridge consumer degree. Pass `query` and/or
//     `symbol`.
//   - impact: given `symbol`, return every bridge reachable from the
//     symbol's contracts (its own and its file's) — the cross-service
//     blast radius of changing that symbol.
func (s *Server) handleContractBridges(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	mode := req.GetString("mode", "rank")
	query := strings.TrimSpace(req.GetString("query", ""))
	symbolID := strings.TrimSpace(req.GetString("symbol", ""))
	limit := req.GetInt("limit", 10)
	if limit <= 0 {
		limit = 10
	}

	allowed, err := s.resolveRepoFilter(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	groups := s.collectBridgeGroups(ctx, allowed)
	if len(groups) == 0 {
		return mcp.NewToolResultError("no contract bridges materialized — index repositories with matched provider/consumer contracts first"), nil
	}

	switch mode {
	case "impact":
		return s.bridgeImpact(ctx, req, groups, symbolID)
	case "rank", "":
		return s.bridgeRank(ctx, req, groups, query, symbolID, limit)
	default:
		return mcp.NewToolResultError("unknown bridge mode: " + mode + " (expected: rank or impact)"), nil
	}
}

// collectBridgeGroups materialises the queryable view of every
// persisted bridge node, resolving the provider/consumer sides
// through the live contract registry (with the contract node's own
// Meta as fallback so the view survives a daemon restart that hasn't
// rehydrated the registry yet). A non-nil allowed set scopes the
// result to bridges touching at least one allowed repo.
func (s *Server) collectBridgeGroups(ctx context.Context, allowed map[string]bool) []*bridgeGroupResult {
	registry := s.effectiveContractRegistry()
	g := s.readerFor(ctx)
	var out []*bridgeGroupResult
	for n := range g.NodesByKind(graph.KindContractBridge) {
		if n == nil || n.Meta == nil {
			continue
		}
		grp := &bridgeGroupResult{
			BridgeID:     n.ID,
			CanonicalKey: bridgeMetaString(n.Meta, "canonical_key"),
			ContractType: bridgeMetaString(n.Meta, "contract_type"),
			Repos:        bridgeMetaStringSlice(n.Meta, "repos"),
		}
		if v, ok := n.Meta["cross_repo"].(bool); ok {
			grp.CrossRepo = v
		}
		grp.ProviderCount = bridgeMetaInt(n.Meta, "provider_count")
		grp.ConsumerCount = bridgeMetaInt(n.Meta, "consumer_count")

		// The bridge node is pinned to one (workspace, project) match
		// boundary; the registry lookup below must be scoped to it so a
		// same-ID contract record in an unrelated workspace doesn't leak
		// into this bridge's participant list. WorkspaceID lives on the
		// node; project rides in Meta. Both default to the node's repo
		// prefix when unset (the same "missing → repo-name" rule the
		// matcher uses), so older nodes without the Meta keys still scope.
		bnd := bridgeBoundary{
			workspace: bridgeBoundarySlug(n.WorkspaceID, n.Meta, "workspace", n.RepoPrefix),
			project:   bridgeBoundarySlug("", n.Meta, "project", n.RepoPrefix),
		}

		for _, e := range g.GetOutEdges(n.ID) {
			if e.Kind != graph.EdgeBridges {
				continue
			}
			side := "provider"
			if e.Meta != nil {
				if v, _ := e.Meta["side"].(string); v != "" {
					side = v
				}
			}
			provs, cons := bridgeSideEntries(g, registry, e.To, side, bnd)
			grp.Providers = append(grp.Providers, provs...)
			grp.Consumers = append(grp.Consumers, cons...)
		}
		sortBridgeSide(grp.Providers)
		sortBridgeSide(grp.Consumers)

		if allowed != nil && !bridgeTouchesRepos(grp, allowed) {
			continue
		}
		out = append(out, grp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BridgeID < out[j].BridgeID })
	return out
}

// bridgeBoundary is the (workspace, project) the matcher paired a
// bridge's contracts inside. bridgeSideEntries scopes its registry
// lookup to it so a same-ID record in an unrelated workspace can't be
// listed as a participant of this bridge.
type bridgeBoundary struct {
	workspace string
	project   string
}

// matches reports whether a contract belongs to the boundary. An empty
// boundary slug (older bridge node without the Meta keys, or a
// genuinely empty workspace) matches everything so the read path stays
// backward-compatible.
func (b bridgeBoundary) matches(c contracts.Contract) bool {
	if b.workspace != "" && c.EffectiveWorkspace() != b.workspace {
		return false
	}
	if b.project != "" && c.EffectiveProject() != b.project {
		return false
	}
	return true
}

// bridgeBoundarySlug resolves a boundary slug from the bridge node:
// prefer the explicit value, then Meta[key], then the repo-prefix
// default the matcher falls back to when the slug is unset.
func bridgeBoundarySlug(explicit string, meta map[string]any, key, repoPrefix string) string {
	if explicit != "" {
		return explicit
	}
	if v := bridgeMetaString(meta, key); v != "" {
		return v
	}
	return repoPrefix
}

// bridgeSideEntries resolves the participant records for one
// EdgeBridges endpoint. side="both" expands to records on both roles
// (an exact-ID match collapses provider and consumer into one
// contract node). Records outside the bridge's match boundary are
// filtered out so a same-ID contract in an unrelated workspace is not
// listed as a participant.
func bridgeSideEntries(g graph.Reader, registry *contracts.Registry, contractID, side string, bnd bridgeBoundary) (provs, cons []bridgeSideEntry) {
	wantProv := side == "provider" || side == "both"
	wantCons := side == "consumer" || side == "both"

	if registry != nil {
		records := registry.ByID(contractID)
		for _, c := range records {
			if !bnd.matches(c) {
				continue
			}
			entry := bridgeSideEntry{
				ContractID: contractID,
				Repo:       c.RepoPrefix,
				SymbolID:   c.SymbolID,
				FilePath:   c.FilePath,
				Line:       c.Line,
			}
			switch {
			case c.Role == contracts.RoleProvider && wantProv:
				provs = append(provs, entry)
			case c.Role == contracts.RoleConsumer && wantCons:
				cons = append(cons, entry)
			}
		}
		if len(provs) > 0 || len(cons) > 0 {
			return provs, cons
		}
	}

	// Fallback: the contract node itself. It carries a single role's
	// Meta (same-ID records collapse), so this is best-effort — the
	// registry path above is authoritative whenever it has data.
	n := g.GetNode(contractID)
	if n == nil {
		return nil, nil
	}
	entry := bridgeSideEntry{
		ContractID: contractID,
		Repo:       n.RepoPrefix,
		FilePath:   n.FilePath,
	}
	if n.Meta != nil {
		entry.SymbolID = bridgeMetaString(n.Meta, "symbol_id")
		entry.Line = bridgeMetaInt(n.Meta, "line")
	}
	if wantProv {
		provs = append(provs, entry)
	}
	if wantCons {
		cons = append(cons, entry)
	}
	return provs, cons
}

// bridgeRank orders bridge groups by reciprocal rank fusion over the
// independent signal rankings that apply to the request.
func (s *Server) bridgeRank(ctx context.Context, req mcp.CallToolRequest, groups []*bridgeGroupResult, query, symbolID string, limit int) (*mcp.CallToolResult, error) {
	rankings := make(map[string][]string)
	byID := make(map[string]*bridgeGroupResult, len(groups))
	for _, g := range groups {
		byID[g.BridgeID] = g
	}

	if query != "" {
		tokens := bridgeQueryTokens(query)
		rankings["text"] = rankBridges(groups, func(g *bridgeGroupResult) float64 {
			return bridgeTextScore(g, tokens)
		})
		rankings["path_repo"] = rankBridges(groups, func(g *bridgeGroupResult) float64 {
			return bridgePathRepoScore(g, tokens)
		})
	}

	if symbolID != "" {
		symNode := s.readerFor(ctx).GetNode(symbolID)
		if symNode == nil {
			return mcp.NewToolResultError("symbol not found: " + symbolID), nil
		}
		anchors := s.bridgeAnchorContracts(ctx, symNode)
		rankings["adjacency"] = rankBridges(groups, func(g *bridgeGroupResult) float64 {
			return bridgeAdjacencyScore(g, symNode, anchors)
		})
	}

	// Degree always participates: a heavily-consumed contract group is
	// the more load-bearing answer at equal text/graph relevance.
	rankings["degree"] = rankBridges(groups, func(g *bridgeGroupResult) float64 {
		return float64(g.ConsumerCount)
	})

	fused, perSignal := reciprocalRankFusion(rankings, rrfK)

	ranked := make([]*bridgeGroupResult, 0, len(groups))
	for id, score := range fused {
		g := byID[id]
		if g == nil {
			continue
		}
		g.FusedScore = score
		g.SignalRanks = perSignal[id]
		ranked = append(ranked, g)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].FusedScore != ranked[j].FusedScore {
			return ranked[i].FusedScore > ranked[j].FusedScore
		}
		return ranked[i].BridgeID < ranked[j].BridgeID
	})
	total := len(ranked)
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "bridges: %d (showing %d)\n", total, len(ranked))
		for _, g := range ranked {
			fmt.Fprintf(&b, "  %.4f %s [%s] providers=%d consumers=%d repos=%s\n",
				g.FusedScore, g.CanonicalKey, g.ContractType,
				g.ProviderCount, g.ConsumerCount, strings.Join(g.Repos, ","))
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	payload := map[string]any{
		"mode":    "rank",
		"groups":  ranked,
		"total":   total,
		"signals": signalNames(rankings),
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// bridgeImpact returns every bridge reachable from the symbol's
// contract surface: contracts attached to the symbol itself plus the
// contracts declared in its file, expanded through the persisted
// EdgeBridges in-edges.
func (s *Server) bridgeImpact(ctx context.Context, req mcp.CallToolRequest, groups []*bridgeGroupResult, symbolID string) (*mcp.CallToolResult, error) {
	if symbolID == "" {
		return mcp.NewToolResultError("symbol is required for bridge impact mode"), nil
	}
	g := s.readerFor(ctx)
	symNode := g.GetNode(symbolID)
	if symNode == nil {
		return mcp.NewToolResultError("symbol not found: " + symbolID), nil
	}
	anchors := s.bridgeAnchorContracts(ctx, symNode)
	if len(anchors) == 0 {
		payload := map[string]any{
			"mode":   "impact",
			"symbol": symbolID,
			"groups": []*bridgeGroupResult{},
			"total":  0,
			"note":   "no contracts attached to this symbol or its file",
		}
		return s.respondJSONOrTOON(ctx, req, payload)
	}

	byID := make(map[string]*bridgeGroupResult, len(groups))
	for _, g := range groups {
		byID[g.BridgeID] = g
	}

	matchedVia := make(map[string][]string)
	for contractID := range anchors {
		for _, e := range g.GetInEdges(contractID) {
			if e.Kind != graph.EdgeBridges {
				continue
			}
			if _, ok := byID[e.From]; !ok {
				continue
			}
			matchedVia[e.From] = append(matchedVia[e.From], contractID)
		}
	}

	impacted := make([]*bridgeGroupResult, 0, len(matchedVia))
	for bridgeID, via := range matchedVia {
		g := byID[bridgeID]
		sort.Strings(via)
		g.MatchedVia = dedupeSortedStrings(via)
		impacted = append(impacted, g)
	}
	sort.Slice(impacted, func(i, j int) bool {
		if impacted[i].ConsumerCount != impacted[j].ConsumerCount {
			return impacted[i].ConsumerCount > impacted[j].ConsumerCount
		}
		return impacted[i].BridgeID < impacted[j].BridgeID
	})

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "impacted bridges: %d (symbol %s)\n", len(impacted), symbolID)
		for _, g := range impacted {
			fmt.Fprintf(&b, "  %s [%s] consumers=%d repos=%s via=%s\n",
				g.CanonicalKey, g.ContractType, g.ConsumerCount,
				strings.Join(g.Repos, ","), strings.Join(g.MatchedVia, ","))
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	payload := map[string]any{
		"mode":   "impact",
		"symbol": symbolID,
		"groups": impacted,
		"total":  len(impacted),
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// bridgeAnchorContracts returns the contract IDs anchored to a
// symbol: contracts attached to the symbol itself plus every contract
// declared in the symbol's file. This is the entry set a change to
// the symbol can reach without leaving its file.
func (s *Server) bridgeAnchorContracts(ctx context.Context, symNode *graph.Node) map[string]bool {
	anchors := make(map[string]bool)
	registry := s.effectiveContractRegistry()
	if registry != nil {
		for _, c := range registry.BySymbol(symNode.ID) {
			anchors[c.ID] = true
		}
		if symNode.FilePath != "" {
			for _, c := range registry.ByFile(symNode.FilePath) {
				anchors[c.ID] = true
			}
		}
	}
	// Graph fallback: provides/consumes out-edges land on contract
	// nodes directly.
	for _, e := range s.readerFor(ctx).GetOutEdges(symNode.ID) {
		if e.Kind == graph.EdgeProvides || e.Kind == graph.EdgeConsumes {
			anchors[e.To] = true
		}
	}
	return anchors
}

// reciprocalRankFusion fuses independent per-signal rankings into one
// score per item: fused(i) = Σ_s 1/(k + rank_s(i)) over the signals
// that ranked the item (1-based ranks). Items absent from a signal
// contribute nothing for it. Returns the fused scores plus each
// item's per-signal rank for explainability.
func reciprocalRankFusion(rankings map[string][]string, k float64) (map[string]float64, map[string]map[string]int) {
	fused := make(map[string]float64)
	perSignal := make(map[string]map[string]int)
	for signal, ids := range rankings {
		for i, id := range ids {
			rank := i + 1
			fused[id] += 1.0 / (k + float64(rank))
			if perSignal[id] == nil {
				perSignal[id] = make(map[string]int)
			}
			perSignal[id][signal] = rank
		}
	}
	return fused, perSignal
}

// rankBridges scores every group and returns the IDs of those with a
// positive score, best-first. Ties break on bridge ID so rankings are
// deterministic.
func rankBridges(groups []*bridgeGroupResult, score func(*bridgeGroupResult) float64) []string {
	type scored struct {
		id    string
		score float64
	}
	var hits []scored
	for _, g := range groups {
		if sc := score(g); sc > 0 {
			hits = append(hits, scored{g.BridgeID, sc})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].id < hits[j].id
	})
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	return ids
}

// bridgeQueryTokens lowercases and splits a free-text query into
// alphanumeric terms.
func bridgeQueryTokens(query string) []string {
	return strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
}

// bridgeTextScore matches query tokens against the bridge's canonical
// key, contract type, and the contract IDs on both sides (those embed
// the service/method/path/topic vocabulary). Token-boundary hits
// weigh double a bare substring hit.
func bridgeTextScore(g *bridgeGroupResult, tokens []string) float64 {
	var docParts []string
	docParts = append(docParts, g.CanonicalKey, g.ContractType)
	for _, e := range g.Providers {
		docParts = append(docParts, e.ContractID, e.SymbolID)
	}
	for _, e := range g.Consumers {
		docParts = append(docParts, e.ContractID, e.SymbolID)
	}
	doc := strings.ToLower(strings.Join(docParts, " "))
	docTokens := make(map[string]bool)
	for _, t := range bridgeQueryTokens(doc) {
		docTokens[t] = true
	}
	score := 0.0
	for _, tok := range tokens {
		switch {
		case docTokens[tok]:
			score += 2
		case strings.Contains(doc, tok):
			score++
		}
	}
	return score
}

// bridgePathRepoScore matches query tokens against the bridge's repo
// spread and the file paths of its participants.
func bridgePathRepoScore(g *bridgeGroupResult, tokens []string) float64 {
	var docParts []string
	docParts = append(docParts, g.Repos...)
	for _, e := range g.Providers {
		docParts = append(docParts, e.FilePath, e.Repo)
	}
	for _, e := range g.Consumers {
		docParts = append(docParts, e.FilePath, e.Repo)
	}
	doc := strings.ToLower(strings.Join(docParts, " "))
	docTokens := make(map[string]bool)
	for _, t := range bridgeQueryTokens(doc) {
		docTokens[t] = true
	}
	score := 0.0
	for _, tok := range tokens {
		switch {
		case docTokens[tok]:
			score += 2
		case strings.Contains(doc, tok):
			score++
		}
	}
	return score
}

// bridgeAdjacencyScore scores a bridge by graph proximity to the
// query symbol: bridges directly anchored to one of the symbol's
// contracts rank above same-file participants, which rank above
// same-directory, which rank above same-repo.
func bridgeAdjacencyScore(g *bridgeGroupResult, symNode *graph.Node, anchors map[string]bool) float64 {
	best := 0.0
	consider := func(e bridgeSideEntry) {
		score := 0.0
		switch {
		case anchors[e.ContractID]:
			score = 8
		case e.FilePath != "" && e.FilePath == symNode.FilePath:
			score = 4
		case e.FilePath != "" && symNode.FilePath != "" &&
			graphpath.Dir(e.FilePath) == graphpath.Dir(symNode.FilePath):
			score = 2
		case e.Repo != "" && e.Repo == symNode.RepoPrefix:
			score = 1
		}
		if score > best {
			best = score
		}
	}
	for _, e := range g.Providers {
		consider(e)
	}
	for _, e := range g.Consumers {
		consider(e)
	}
	return best
}

// bridgeTouchesRepos reports whether the bridge group involves at
// least one repo in the allowed set.
func bridgeTouchesRepos(g *bridgeGroupResult, allowed map[string]bool) bool {
	for _, r := range g.Repos {
		if allowed[r] {
			return true
		}
	}
	for _, e := range g.Providers {
		if allowed[e.Repo] {
			return true
		}
	}
	for _, e := range g.Consumers {
		if allowed[e.Repo] {
			return true
		}
	}
	return false
}

func sortBridgeSide(entries []bridgeSideEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ContractID != entries[j].ContractID {
			return entries[i].ContractID < entries[j].ContractID
		}
		if entries[i].Repo != entries[j].Repo {
			return entries[i].Repo < entries[j].Repo
		}
		if entries[i].FilePath != entries[j].FilePath {
			return entries[i].FilePath < entries[j].FilePath
		}
		return entries[i].Line < entries[j].Line
	})
}

func signalNames(rankings map[string][]string) []string {
	names := make([]string, 0, len(rankings))
	for name := range rankings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func dedupeSortedStrings(in []string) []string {
	out := in[:0]
	var prev string
	for i, s := range in {
		if i > 0 && s == prev {
			continue
		}
		out = append(out, s)
		prev = s
	}
	return out
}

// metaString / metaInt / metaStringSlice read loosely-typed Node.Meta
// values that may have round-tripped through a persistence backend
// (gob restores []string as-is; JSON paths may yield []any / float64).
func bridgeMetaString(meta map[string]any, key string) string {
	v, _ := meta[key].(string)
	return v
}

func bridgeMetaInt(meta map[string]any, key string) int {
	switch v := meta[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func bridgeMetaStringSlice(meta map[string]any, key string) []string {
	switch v := meta[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
