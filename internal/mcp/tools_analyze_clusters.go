package mcp

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// handleAnalyzeClusters returns the cached community-detection
// result as clusters with richer per-cluster stats than the raw
// `get_communities` surface: density (intra-cluster edges /
// possible-pairs), file-spread (distinct files touched), language
// mix, and a hub identifier.
//
// "Offline clustering" in the spec refers to k-means / DBSCAN over
// embeddings. Per-node embeddings aren't a public API of the indexer
// today, so the analyzer clusters the call graph instead — these ARE
// offline clusters with strong topology-grounded labels, and they
// serve the downstream "show me the conceptual areas of this
// codebase" use case the L12 spec listed.
//
// The `algorithm` argument selects the mechanism: leiden (default,
// the cached modularity communities), louvain (the legacy modularity
// detector), or spectral (recursive Fiedler-vector bisection — pairs
// better with similarity edges where modularity's resolution limit
// blurs boundaries). The wire response echoes the algorithm used.
func (s *Server) handleAnalyzeClusters(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	minSize := max(req.GetInt("min_size", 3), 1)
	limit := max(req.GetInt("limit", 50), 1)
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))
	algorithm := strings.ToLower(strings.TrimSpace(req.GetString("algorithm", "leiden")))
	// resolution (γ) tunes the Leiden modularity granularity. Absent or
	// non-positive falls back to 1.0 (standard modularity). Higher γ →
	// smaller, denser clusters; lower γ → fewer, larger ones.
	resolution := req.GetFloat("resolution", 1.0)
	if resolution <= 0 {
		resolution = 1.0
	}

	var cr *analysis.CommunityResult
	fullGraphBurst := false
	// incrStats is populated only on the Leiden path; it records
	// whether the partition was recomputed incrementally (only the
	// packages that changed since the last call) or in full.
	var incrStats analysis.IncrementalCommunityStats
	switch algorithm {
	case "", "leiden":
		algorithm = "leiden"
		// The incremental partition cache is keyed to the default
		// resolution. A non-default γ recomputes the partition fully so
		// the cache (γ = 1.0) is never poisoned; γ = 1.0 keeps the fast
		// cached path and stays byte-identical to the pre-resolution
		// output.
		if resolution == 1.0 {
			cr, incrStats = s.incrementalCommunities()
			fullGraphBurst = !incrStats.Incremental
		} else {
			// Leiden reads the whole corpus through the store's kind-scoped
			// edge projection, which a request view does not provide, so the
			// partition is computed over the base corpus even when the call
			// carries an overlay or a routed view.
			cr = analysis.DetectCommunitiesLeidenWith(s.graph, analysis.LeidenOptions{Resolution: resolution})
			fullGraphBurst = true
		}
		// Both Leiden arms partition the base corpus — the cached one is a
		// per-server partition shared by every session, the uncached one runs
		// over the store directly. Under a view that is a base-scoped answer.
		annotateBaseScoped(ctx, graphview.CapSyntaxGraph)
	case "louvain":
		cr = analysis.DetectCommunitiesLouvain(s.readerFor(ctx))
		fullGraphBurst = true
	case "spectral":
		cr = analysis.SpectralClusters(s.readerFor(ctx))
		fullGraphBurst = true
	default:
		return mcp.NewToolResultError("analyze clusters: unknown algorithm " + algorithm +
			" (expected: leiden, louvain, spectral)"), nil
	}
	if fullGraphBurst {
		defer scheduleOSMemoryReleaseAfterBurst(s.logger, "analyze_clusters")
	}

	// Clamp the global partition to the session workspace so a
	// workspace-bound caller never sees clusters whose members live in a
	// sibling workspace (community detection runs over the whole index).
	cr = s.communitiesInSessionScope(ctx, cr)

	if cr == nil || len(cr.Communities) == 0 {
		empty := map[string]any{
			"clusters":  []map[string]any{},
			"total":     0,
			"algorithm": algorithm,
			"note":      "no communities detected in the session workspace",
		}
		if blk := s.workspaceScopeBlock(ctx, req, "clusters"); blk != nil {
			empty["scope"] = blk
		}
		return s.respondJSONOrTOON(ctx, req, empty)
	}

	type clusterRow struct {
		ID           string         `json:"id"`
		Label        string         `json:"label"`
		Hub          string         `json:"hub,omitempty"`
		Size         int            `json:"size"`
		Files        int            `json:"files"`
		FileSpread   float64        `json:"file_spread"`
		Density      float64        `json:"density"`
		Languages    map[string]int `json:"languages"`
		TopFiles     []string       `json:"top_files,omitempty"`
		MemberSample []string       `json:"member_sample,omitempty"`
	}

	// First pass: keep only the clusters that survive size + path-prefix
	// gates, then sort + truncate to the requested limit. The density,
	// language-mix, and top-files work below is bounded by the truncated
	// row count instead of every community in the partition — important
	// on a disk backend where each member touches the graph store.
	type pending struct {
		c   *analysis.Community
		row clusterRow
	}
	survivors := make([]pending, 0, len(cr.Communities))
	for i := range cr.Communities {
		c := &cr.Communities[i]
		if c.Size < minSize {
			continue
		}
		if pathPrefix != "" {
			match := false
			for _, f := range c.Files {
				if strings.HasPrefix(f, pathPrefix) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		row := clusterRow{
			ID: c.ID, Label: c.Label, Hub: c.Hub, Size: c.Size,
			Files:     len(c.Files),
			Languages: map[string]int{},
		}
		if c.Size > 0 {
			row.FileSpread = roundScore(float64(len(c.Files)) / float64(c.Size))
		}
		survivors = append(survivors, pending{c: c, row: row})
	}
	sort.Slice(survivors, func(i, j int) bool {
		if survivors[i].c.Size != survivors[j].c.Size {
			return survivors[i].c.Size > survivors[j].c.Size
		}
		return survivors[i].c.ID < survivors[j].c.ID
	})
	truncated := false
	if len(survivors) > limit {
		survivors = survivors[:limit]
		truncated = true
	}

	// Batch every surviving cluster's member ids and pull their nodes +
	// outgoing edges in two calls — one round-trip each on
	// a disk backend, against the per-member GetNode / GetOutEdges loop the
	// previous shape ran (N members × 2 round-trips). Members from
	// communities that didn't survive the truncate above never reach
	// the store.
	//
	// Per-cluster member cap: communities can hold thousands of nodes
	// each. On a disk backend, fetching tens of thousands of nodes + edges per
	// call is several seconds of cost — the rendered response only
	// uses these to compute density / language mix / top files, all of
	// which converge on a representative sample long before they need
	// every member. With a default 50-cluster limit and ~200 sampled
	// members per cluster, the IN-list stays under 10k IDs and the
	// rendering stays sub-second. The exact `size` field still reflects
	// the true cluster size because it comes from c.Size, not from the
	// sampled set.
	const sampleCap = 200
	sampleMemberIDs := make([]string, 0, len(survivors)*sampleCap)
	sampleSets := make([]map[string]bool, 0, len(survivors))
	for _, p := range survivors {
		members := p.c.Members
		if len(members) > sampleCap {
			members = members[:sampleCap]
		}
		set := make(map[string]bool, len(members))
		for _, m := range members {
			set[m] = true
		}
		sampleSets = append(sampleSets, set)
		sampleMemberIDs = append(sampleMemberIDs, members...)
	}
	clusterReader := s.readerFor(ctx)
	memberNodes := clusterReader.GetNodesByIDs(sampleMemberIDs)
	memberOutEdges := clusterReader.GetOutEdgesByNodeIDs(sampleMemberIDs)

	rows := make([]clusterRow, 0, len(survivors))
	for i, p := range survivors {
		c := p.c
		row := p.row
		memberSet := sampleSets[i]
		sampleSize := len(memberSet)

		// Density on the sample, normalised against (sampleSize ·
		// (sampleSize-1)) to keep the ratio meaningful when only part
		// of the cluster was inspected. Intra-sample edges restricted
		// to the call / reference kinds the clusterer cares about.
		intra := 0
		for m := range memberSet {
			for _, e := range memberOutEdges[m] {
				if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
					continue
				}
				if memberSet[e.To] {
					intra++
				}
			}
		}
		if sampleSize > 1 {
			possible := sampleSize * (sampleSize - 1)
			row.Density = roundScore(float64(intra) / float64(possible))
		}

		fileCounts := map[string]int{}
		for m := range memberSet {
			n := memberNodes[m]
			if n == nil {
				continue
			}
			if n.Language != "" {
				row.Languages[n.Language]++
			}
			if n.FilePath != "" {
				fileCounts[n.FilePath]++
			}
		}
		row.TopFiles = topN(fileCounts, 3)
		row.MemberSample = sliceFirstN(c.Members, 5)

		rows = append(rows, row)
	}

	resp := map[string]any{
		"clusters":  rows,
		"total":     len(rows),
		"truncated": truncated,
		"algorithm": algorithm,
	}
	// On the Leiden path, report whether the partition was recomputed
	// incrementally (only the changed packages) or in full, so a
	// caller can see the cache working.
	if algorithm == "leiden" {
		recompute := "incremental"
		if !incrStats.Incremental {
			recompute = "full"
		}
		detection := map[string]any{
			"recompute":           recompute,
			"changed_packages":    incrStats.ChangedPackages,
			"total_packages":      incrStats.TotalPackages,
			"repartitioned_nodes": incrStats.RepartitionedNodes,
		}
		if incrStats.FullRecomputeReason != "" {
			detection["full_recompute_reason"] = incrStats.FullRecomputeReason
		}
		detection["resolution"] = resolution
		resp["detection"] = detection
	}
	if blk := s.workspaceScopeBlock(ctx, req, "clusters"); blk != nil {
		resp["scope"] = blk
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// handleAnalyzeConcepts labels each cluster with a human-readable
// theme via the wired LLM service. Falls back to deterministic
// path-prefix-based naming when no LLM is available so the tool
// still produces useful output offline.
func (s *Server) handleAnalyzeConcepts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	minSize := max(req.GetInt("min_size", 3), 1)
	limit := max(req.GetInt("limit", 30), 1)
	maxTokens := max(req.GetInt("max_tokens", 80), 16)
	useLLM := requestBoolDefault(req, "use_llm", s.llmService != nil && s.llmService.Enabled())

	cr := s.getCommunities()
	// Clamp to the session workspace (community detection is global).
	cr = s.communitiesInSessionScope(ctx, cr)
	if cr == nil || len(cr.Communities) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"concepts": []map[string]any{},
			"total":    0,
			"source":   "communities-empty",
		})
	}

	type conceptRow struct {
		ClusterID  string   `json:"cluster_id"`
		Theme      string   `json:"theme"`
		Files      []string `json:"files,omitempty"`
		MemberSize int      `json:"member_size"`
		Source     string   `json:"source"` // "llm" or "heuristic"
	}

	clusters := append([]analysis.Community(nil), cr.Communities...)
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].Size > clusters[j].Size })

	out := make([]conceptRow, 0, len(clusters))
	for _, c := range clusters {
		if c.Size < minSize {
			continue
		}
		if len(out) >= limit {
			break
		}
		theme, source := s.labelConcept(ctx, c, useLLM, maxTokens)
		out = append(out, conceptRow{
			ClusterID:  c.ID,
			Theme:      theme,
			Files:      sliceFirstN(c.Files, 3),
			MemberSize: c.Size,
			Source:     source,
		})
	}

	resp := map[string]any{
		"concepts": out,
		"total":    len(out),
	}
	if blk := s.workspaceScopeBlock(ctx, req, "concepts"); blk != nil {
		resp["scope"] = blk
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// labelConcept returns a theme label for the cluster + the source
// the label came from ("llm" or "heuristic"). LLM failures fall
// back to the heuristic — the tool never errors on a single
// cluster's label.
func (s *Server) labelConcept(ctx context.Context, c analysis.Community, useLLM bool, maxTokens int) (string, string) {
	if useLLM && s.llmService != nil && s.llmService.Enabled() {
		prompt := buildConceptPrompt(c)
		ans, err := s.llmService.Generate(ctx, prompt, maxTokens)
		if err == nil {
			t := strings.TrimSpace(ans)
			if t != "" {
				return shortenLabel(t), "llm"
			}
		}
	}
	return heuristicConceptLabel(c), "heuristic"
}

// buildConceptPrompt asks the LLM for a 3-6 word theme covering
// every member file. Anchored to specific evidence so the label
// stays grounded.
func buildConceptPrompt(c analysis.Community) string {
	files := sliceFirstN(c.Files, 8)
	hub := c.Hub
	if hub == "" {
		hub = "(none)"
	}
	return fmt.Sprintf(
		"Name this code cluster with a 3-6 word topic label. Hub: %s. Files: %s. "+
			"Respond with the label only, no quotes, no punctuation.",
		hub, strings.Join(files, ", "))
}

// heuristicConceptLabel returns a deterministic label derived from
// the cluster's hub + common file-path prefix. Used when LLM is
// disabled or fails.
func heuristicConceptLabel(c analysis.Community) string {
	if c.Label != "" {
		return c.Label
	}
	prefix := commonFilePrefix(c.Files)
	if prefix == "" && c.Hub != "" {
		return c.Hub
	}
	if prefix != "" && c.Hub != "" {
		return prefix + " · " + c.Hub
	}
	if prefix != "" {
		return prefix
	}
	return "cluster-" + c.ID
}

// commonFilePrefix returns the longest leading directory shared by
// every file in the list — typically "internal/auth" or similar.
// Empty when files diverge at the root.
func commonFilePrefix(files []string) string {
	if len(files) == 0 {
		return ""
	}
	// path.Dir, not filepath.Dir: the containment test below joins with "/",
	// so a prefix cleaned to the OS separator could never match it on
	// Windows and every cluster would report an empty prefix.
	prefix := path.Dir(filepath.ToSlash(files[0]))
	for _, f := range files[1:] {
		dir := path.Dir(filepath.ToSlash(f))
		for !strings.HasPrefix(dir+"/", prefix+"/") && prefix != "." && prefix != "/" {
			prefix = path.Dir(prefix)
		}
		if prefix == "." || prefix == "/" {
			return ""
		}
	}
	if prefix == "." {
		return ""
	}
	return prefix
}

// shortenLabel trims an LLM response to a single line + ≤80 chars
// so a verbose model doesn't produce a paragraph in the theme field.
func shortenLabel(s string) string {
	s = strings.SplitN(s, "\n", 2)[0]
	s = strings.Trim(s, `"'.`)
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// topN returns the n highest-count keys from the map, sorted by
// count DESC then key ASC. Used for top_files in cluster rows.
func topN(counts map[string]int, n int) []string {
	type kv struct {
		k string
		v int
	}
	all := make([]kv, 0, len(counts))
	for k, v := range counts {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	if len(all) > n {
		all = all[:n]
	}
	out := make([]string, 0, len(all))
	for _, kvp := range all {
		out = append(out, kvp.k)
	}
	return out
}

// sliceFirstN returns the first n elements of s, or s itself if
// shorter. Returns a nil slice for empty input.
func sliceFirstN(s []string, n int) []string {
	if len(s) == 0 {
		return nil
	}
	if len(s) <= n {
		return append([]string(nil), s...)
	}
	return append([]string(nil), s[:n]...)
}
