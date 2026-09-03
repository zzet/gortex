package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// QRT-1: the graph teaches you its own architecture. Rather than hand-writing
// layer globs, suggest_boundaries seeds an architecture: block from the
// detected Leiden communities — each community becomes a candidate layer, and
// the observed cross-community call edges become its starter allow list. The
// output is a ready-to-paste .gortex.yaml block the change_contract
// architecture family then enforces.

type suggestedLayer struct {
	Name  string   `json:"name"`
	Paths []string `json:"paths"`
	Allow []string `json:"allow"`
	Size  int      `json:"size"`
}

func (s *Server) handleSuggestBoundaries(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	comms := s.getCommunities()
	// Clamp to the session workspace (community detection is global) so
	// boundaries are seeded only from in-workspace communities and never
	// leak a sibling workspace's layers.
	comms = s.communitiesInSessionScope(ctx, comms)
	if comms == nil || len(comms.Communities) == 0 {
		return mcp.NewToolResultError("no communities detected — run `analyze kind=clusters` (or reindex) first so boundaries can be seeded"), nil
	}
	minSize := req.GetInt("min_size", 3)
	limit := req.GetInt("limit", 12)

	// Select the largest communities that have a usable common path prefix.
	type cand struct {
		comm   int // index into comms.Communities
		prefix string
		name   string
		size   int
	}
	sorted := make([]int, 0, len(comms.Communities))
	for i := range comms.Communities {
		if comms.Communities[i].Size >= minSize {
			sorted = append(sorted, i)
		}
	}
	sort.Slice(sorted, func(a, b int) bool {
		return comms.Communities[sorted[a]].Size > comms.Communities[sorted[b]].Size
	})

	usedNames := make(map[string]bool)
	commToLayer := make(map[string]string) // community ID -> layer name
	var cands []cand
	for _, ci := range sorted {
		if len(cands) >= limit {
			break
		}
		c := comms.Communities[ci]
		prefix := commonDirPrefix(c.Files)
		if prefix == "" {
			continue
		}
		name := uniqueLayerName(prefix, c.Label, usedNames)
		usedNames[name] = true
		commToLayer[c.ID] = name
		cands = append(cands, cand{comm: ci, prefix: prefix, name: name, size: c.Size})
	}
	if len(cands) == 0 {
		return mcp.NewToolResultError("detected communities have no coherent path prefix to seed layers from — boundaries cannot be suggested for this graph"), nil
	}

	// Observed cross-layer dependencies become the allow lists.
	g := s.readerFor(ctx)
	allow := make(map[string]map[string]bool)
	for _, cd := range cands {
		c := comms.Communities[cd.comm]
		for _, memberID := range c.Members {
			for _, e := range g.GetOutEdges(memberID) {
				if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
					continue
				}
				targetComm, ok := comms.NodeToComm[e.To]
				if !ok {
					continue
				}
				toLayer, ok := commToLayer[targetComm]
				if !ok || toLayer == cd.name {
					continue
				}
				if allow[cd.name] == nil {
					allow[cd.name] = make(map[string]bool)
				}
				allow[cd.name][toLayer] = true
			}
		}
	}

	layers := make([]suggestedLayer, 0, len(cands))
	for _, cd := range cands {
		deps := make([]string, 0, len(allow[cd.name]))
		for d := range allow[cd.name] {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		layers = append(layers, suggestedLayer{
			Name:  cd.name,
			Paths: []string{cd.prefix + "/**"},
			Allow: deps,
			Size:  cd.size,
		})
	}

	resp := map[string]any{
		"suggested_layers": layers,
		"yaml":             renderArchitectureYAML(layers),
		"community_count":  len(comms.Communities),
		"modularity":       comms.Modularity,
		"note":             "Starter architecture block seeded from detected communities. Review the layer names and allow lists, then paste into .gortex.yaml; change_contract's architecture family enforces it (set architecture.severity: error to make breaks refuse).",
	}
	if blk := s.workspaceScopeBlock(ctx, req, "suggest_boundaries"); blk != nil {
		resp["scope"] = blk
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// commonDirPrefix returns the longest shared leading directory segments across
// the files, or "" when they share none.
func commonDirPrefix(files []string) string {
	if len(files) == 0 {
		return ""
	}
	var segLists [][]string
	for _, f := range files {
		dir := ""
		if i := strings.LastIndex(f, "/"); i >= 0 {
			dir = f[:i]
		}
		segLists = append(segLists, strings.Split(dir, "/"))
	}
	common := segLists[0]
	for _, segs := range segLists[1:] {
		n := len(common)
		if len(segs) < n {
			n = len(segs)
		}
		k := 0
		for k < n && common[k] == segs[k] {
			k++
		}
		common = common[:k]
		if len(common) == 0 {
			return ""
		}
	}
	if len(common) == 1 && common[0] == "" {
		return ""
	}
	return strings.Join(common, "/")
}

// uniqueLayerName derives a yaml-key-safe layer name from a path prefix (or
// the community label), disambiguating against names already used.
func uniqueLayerName(prefix, label string, used map[string]bool) string {
	base := sanitizeLayerName(prefix)
	if base == "" {
		base = sanitizeLayerName(label)
	}
	if base == "" {
		base = "layer"
	}
	name := base
	for i := 2; used[name]; i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	return name
}

func sanitizeLayerName(s string) string {
	// Keep the last two path segments for a terse but distinctive name.
	segs := strings.Split(s, "/")
	if len(segs) > 2 {
		segs = segs[len(segs)-2:]
	}
	joined := strings.Join(segs, "_")
	var b strings.Builder
	for _, r := range joined {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == '-' || r == '.' || r == ' ':
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func renderArchitectureYAML(layers []suggestedLayer) string {
	var b strings.Builder
	b.WriteString("architecture:\n  layers:\n")
	for _, l := range layers {
		fmt.Fprintf(&b, "    %s:\n", l.Name)
		fmt.Fprintf(&b, "      paths: [%q]\n", l.Paths[0])
		if len(l.Allow) == 0 {
			b.WriteString("      allow: []\n")
			continue
		}
		quoted := make([]string, len(l.Allow))
		for i, a := range l.Allow {
			quoted[i] = fmt.Sprintf("%q", a)
		}
		fmt.Fprintf(&b, "      allow: [%s]\n", strings.Join(quoted, ", "))
	}
	return b.String()
}
