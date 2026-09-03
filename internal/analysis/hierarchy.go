package analysis

import (
	"path"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// hierarchy.go computes a layered, multi-resolution view of the graph.
//
// The base graph's leaves are functions, methods, and types. An agent
// asking "what is the architecture of this codebase?" wants a coarse,
// rolled-up answer — not a traversal of thousands of function-leaf
// nodes. BuildHierarchy collapses the leaf graph into a chosen
// resolution tier: every leaf is mapped to the group it belongs to at
// that tier, and every leaf-level edge is aggregated into a weighted
// edge between the two groups it crosses.
//
// The rollup is computed on demand from the base graph. It allocates a
// fresh HierarchyView; the base graph is never mutated and no second
// persistent graph is kept. The output is deterministic — every list
// is sorted by a stable key — so two calls on the same graph produce
// byte-identical results.

// ResolutionLevel names a tier of the multi-resolution hierarchy,
// lowest (finest) to highest (coarsest).
type ResolutionLevel string

const (
	// LevelSymbol is the finest tier: one group per function / method
	// / type leaf. The rollup at this tier is the leaf graph itself,
	// minus non-leaf scaffolding (files, imports, packages).
	LevelSymbol ResolutionLevel = "symbol"
	// LevelFile groups leaves by their source file.
	LevelFile ResolutionLevel = "file"
	// LevelPackage groups leaves by their package — the directory
	// holding the source file. Directory grouping is used rather than
	// KindPackage nodes because most languages emit those sparsely (or
	// not at all), whereas every leaf has a file path.
	LevelPackage ResolutionLevel = "package"
	// LevelService groups leaves by the detected community they belong
	// to — the functional cluster Gortex's community detection
	// exposes. Leaves not assigned to any community fall back to their
	// package directory so the tier still partitions the whole graph.
	LevelService ResolutionLevel = "service"
	// LevelSystem is the coarsest tier: one group per repository.
	LevelSystem ResolutionLevel = "system"
)

// ValidResolutionLevel reports whether s names a known tier.
func ValidResolutionLevel(s ResolutionLevel) bool {
	switch s {
	case LevelSymbol, LevelFile, LevelPackage, LevelService, LevelSystem:
		return true
	}
	return false
}

// HierarchyNode is one rollup group at the requested resolution tier.
type HierarchyNode struct {
	// ID is the stable identifier of the group. Deterministic across
	// runs — derived from the file path / directory / community ID /
	// repo prefix that defines the group.
	ID string `json:"id"`
	// Label is a short human-readable name for the group.
	Label string `json:"label"`
	// Level is the resolution tier this node belongs to.
	Level ResolutionLevel `json:"level"`
	// LeafCount is the number of leaf symbols (functions / methods /
	// types) the group contains.
	LeafCount int `json:"leaf_count"`
}

// HierarchyEdge is an aggregated edge between two rollup groups. It
// exists only for pairs of distinct groups; intra-group edges are
// reported per group via HierarchyNode-keyed SelfLoops on the view,
// not as HierarchyEdge rows.
type HierarchyEdge struct {
	// From and To are HierarchyNode IDs.
	From string `json:"from"`
	To   string `json:"to"`
	// Weight is the number of underlying leaf-level edges (calls,
	// imports, references, and the other relation kinds) that cross
	// from a leaf in the From group to a leaf in the To group.
	Weight int `json:"weight"`
}

// HierarchyView is the rolled-up graph at one resolution tier.
type HierarchyView struct {
	// Level is the tier this view was built at.
	Level ResolutionLevel `json:"level"`
	// Nodes are the rollup groups, sorted by LeafCount descending then
	// ID ascending.
	Nodes []HierarchyNode `json:"nodes"`
	// Edges are the aggregated cross-group edges, sorted by Weight
	// descending then From / To ascending.
	Edges []HierarchyEdge `json:"edges"`
	// SelfLoops maps a HierarchyNode ID to the count of leaf-level
	// edges that stay inside that group (both endpoints in the same
	// group). Reported separately so the cross-group Edges list stays
	// a pure inter-group graph. Only groups with at least one
	// intra-group edge appear here.
	SelfLoops map[string]int `json:"self_loops,omitempty"`
	// LeafCount is the total number of leaf symbols rolled up.
	LeafCount int `json:"leaf_count"`
}

// hierarchyLeafKinds is the set of base-graph node kinds treated as
// leaves of the hierarchy — the symbols a rollup aggregates over.
// Scaffolding kinds (file, import, package, module, param, …) are not
// leaves: they are either the grouping axis itself or graph plumbing.
func hierarchyLeafKinds(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface:
		return true
	}
	return false
}

// BuildHierarchy rolls the base graph up to the requested resolution
// level and returns the layered view. communities supplies the
// detected functional clusters used by the service tier; it may be nil
// (the service tier then falls back to package grouping for every
// leaf, and the other tiers ignore it entirely).
//
// The base graph is read-only here — BuildHierarchy never mutates g
// and never persists a second graph. An unknown level yields an empty
// view carrying that level, so callers can surface a clean error.
func BuildHierarchy(g graph.Reader, level ResolutionLevel, communities *CommunityResult) *HierarchyView {
	view := &HierarchyView{Level: level, SelfLoops: map[string]int{}}
	if g == nil || !ValidResolutionLevel(level) {
		return view
	}

	// node-to-community lookup for the service tier.
	var nodeToComm map[string]string
	if communities != nil {
		nodeToComm = communities.NodeToComm
	}
	commLabel := communityLabelIndex(communities)

	// Pass 1: assign every leaf node to its group at this tier.
	// leafGroup maps leaf node ID → group ID; groups accumulates the
	// per-group rollup node (leaf count, label).
	leafGroup := make(map[string]string)
	groups := make(map[string]*HierarchyNode)
	for _, n := range g.AllNodes() {
		if n == nil || !hierarchyLeafKinds(n.Kind) {
			continue
		}
		gid, label := hierarchyGroupOf(n, level, nodeToComm, commLabel)
		if gid == "" {
			continue
		}
		leafGroup[n.ID] = gid
		grp := groups[gid]
		if grp == nil {
			grp = &HierarchyNode{ID: gid, Label: label, Level: level}
			groups[gid] = grp
		}
		grp.LeafCount++
		view.LeafCount++
	}

	// Pass 2: aggregate leaf-level edges into weighted group edges.
	// pairWeight counts edges that cross two distinct groups; a leaf
	// edge whose endpoints land in the same group bumps that group's
	// self-loop tally instead.
	type groupPair struct{ from, to string }
	pairWeight := make(map[groupPair]int)
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		fromG, okFrom := leafGroup[e.From]
		toG, okTo := leafGroup[e.To]
		if !okFrom || !okTo {
			// At least one endpoint is not a hierarchy leaf — skip
			// scaffolding edges (file→defines→symbol, etc.).
			continue
		}
		if fromG == toG {
			view.SelfLoops[fromG]++
			continue
		}
		pairWeight[groupPair{from: fromG, to: toG}]++
	}

	// Materialise the sorted node list.
	view.Nodes = make([]HierarchyNode, 0, len(groups))
	for _, grp := range groups {
		view.Nodes = append(view.Nodes, *grp)
	}
	sort.Slice(view.Nodes, func(i, j int) bool {
		if view.Nodes[i].LeafCount != view.Nodes[j].LeafCount {
			return view.Nodes[i].LeafCount > view.Nodes[j].LeafCount
		}
		return view.Nodes[i].ID < view.Nodes[j].ID
	})

	// Materialise the sorted edge list.
	view.Edges = make([]HierarchyEdge, 0, len(pairWeight))
	for pair, w := range pairWeight {
		view.Edges = append(view.Edges, HierarchyEdge{
			From: pair.from, To: pair.to, Weight: w,
		})
	}
	sort.Slice(view.Edges, func(i, j int) bool {
		if view.Edges[i].Weight != view.Edges[j].Weight {
			return view.Edges[i].Weight > view.Edges[j].Weight
		}
		if view.Edges[i].From != view.Edges[j].From {
			return view.Edges[i].From < view.Edges[j].From
		}
		return view.Edges[i].To < view.Edges[j].To
	})

	if len(view.SelfLoops) == 0 {
		view.SelfLoops = nil
	}
	return view
}

// hierarchyGroupOf maps a leaf node to its group ID and label at the
// requested resolution tier. Returns an empty group ID when the node
// carries no usable grouping key (e.g. a symbol tier node — every leaf
// is its own group there — is always groupable; a file/package tier
// node with an empty FilePath is not).
func hierarchyGroupOf(
	n *graph.Node,
	level ResolutionLevel,
	nodeToComm map[string]string,
	commLabel map[string]string,
) (id, label string) {
	switch level {
	case LevelSymbol:
		// Each leaf is its own group at the finest tier.
		return n.ID, n.Name
	case LevelFile:
		if n.FilePath == "" {
			return "", ""
		}
		return "file:" + n.FilePath, path.Base(n.FilePath)
	case LevelPackage:
		if n.FilePath == "" {
			return "", ""
		}
		dir := packageDirOf(n.FilePath)
		return "package:" + dir, dir
	case LevelService:
		if nodeToComm != nil {
			if cid, ok := nodeToComm[n.ID]; ok && cid != "" {
				lbl := commLabel[cid]
				if lbl == "" {
					lbl = cid
				}
				return "service:" + cid, lbl
			}
		}
		// Leaves outside every community fall back to their package
		// directory so the service tier still partitions the graph.
		if n.FilePath == "" {
			return "", ""
		}
		dir := packageDirOf(n.FilePath)
		return "service:dir:" + dir, dir
	case LevelSystem:
		repo := n.RepoPrefix
		if repo == "" {
			repo = "(root)"
		}
		return "system:" + repo, repo
	}
	return "", ""
}

// packageDirOf returns the directory portion of a file path — the
// hierarchy's notion of a package. A file at the repo root maps to the
// sentinel "(root)" so it still forms a single group.
func packageDirOf(filePath string) string {
	dir := path.Dir(strings.ReplaceAll(filePath, "\\", "/"))
	if dir == "" || dir == "." || dir == "/" {
		return "(root)"
	}
	return dir
}

// communityLabelIndex builds a community-ID → label lookup so the
// service tier can name its groups. Returns an empty (non-nil) map
// when communities is nil.
func communityLabelIndex(communities *CommunityResult) map[string]string {
	out := make(map[string]string)
	if communities == nil {
		return out
	}
	for _, c := range communities.Communities {
		out[c.ID] = c.Label
	}
	return out
}
