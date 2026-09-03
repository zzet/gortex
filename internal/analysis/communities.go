package analysis

import (
	"fmt"
	"math"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Community represents a discovered functional cluster in the codebase.
type Community struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Members  []string `json:"members"`  // node IDs
	Files    []string `json:"files"`    // unique file paths
	Size     int      `json:"size"`     // member count
	Cohesion float64  `json:"cohesion"` // internal edge density (0-1)
	// Hub is the in-cluster-highest-degree member's symbol name —
	// the function or type everything else in the cluster connects
	// through. Strong semantic disambiguator: "parser/languages ·
	// GoExtractor" tells you what the cluster does at a glance,
	// where a file-basename like "golang" leaves you guessing.
	Hub string `json:"hub,omitempty"`
	// ParentID points at the super-community this cluster belongs to
	// after the second Louvain pass. Sibling clusters under the same
	// parent are typically tightly related (e.g. three
	// parser/languages sub-clusters that each specialise around a
	// different AST primitive). Empty for top-level / singleton
	// communities that have no sibling at the same modularity level.
	ParentID string `json:"parent_id,omitempty"`
}

// CommunityResult is the output of community detection.
type CommunityResult struct {
	Communities []Community       `json:"communities"`
	NodeToComm  map[string]string `json:"node_to_community"` // nodeID → communityID
	Modularity  float64           `json:"modularity"`
}

// DetectCommunities runs community detection on the graph. As of
// the Leiden switchover this is a thin wrapper around
// DetectCommunitiesLeiden — the Leiden algorithm delivered 66%
// fewer communities, +25% modularity, and 61% less sibling
// fragmentation on the live gortex graph compared to the legacy
// Louvain implementation, at the cost of ~15% extra CPU time.
//
// The Louvain implementation is preserved as
// DetectCommunitiesLouvain so we can benchmark, A/B, or fall back
// without re-deriving the algorithm.
func DetectCommunities(g graph.Store) *CommunityResult {
	return DetectCommunitiesLeiden(g)
}

// DetectCommunitiesLouvain is the original Louvain implementation,
// retained for benchmarking and as a known-good fallback.
func DetectCommunitiesLouvain(g graph.Reader) *CommunityResult {
	nodes := g.AllNodes()
	edges := g.AllEdges()

	// Filter to symbol nodes only (skip file and import nodes, and
	// cross-daemon federation proxy nodes — they stand in for remote symbols
	// and must not form or join local communities).
	symbolNodes := make(map[string]bool)
	for _, n := range nodes {
		if graph.IsProxyNode(n) {
			continue
		}
		// Content sections are leaf knowledge, not code structure — they
		// must not form or join code communities (which seed the skills
		// router and architecture layers).
		if graph.IsContentNode(n) {
			continue
		}
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			symbolNodes[n.ID] = true
		}
	}

	// Build adjacency with weights for clustering-relevant edges
	type edgeKey struct{ a, b string }
	weights := make(map[edgeKey]float64)
	for _, e := range edges {
		if !symbolNodes[e.From] || !symbolNodes[e.To] {
			continue
		}
		w := edgeWeight(e.Kind)
		if w == 0 {
			continue
		}
		// Undirected: add both directions
		k1 := edgeKey{e.From, e.To}
		k2 := edgeKey{e.To, e.From}
		weights[k1] += w
		weights[k2] += w
	}

	// Build neighbor lists
	neighbors := make(map[string]map[string]float64)
	for k, w := range weights {
		if neighbors[k.a] == nil {
			neighbors[k.a] = make(map[string]float64)
		}
		neighbors[k.a][k.b] = w
	}

	// Total edge weight
	var totalWeight float64
	for _, w := range weights {
		totalWeight += w
	}
	totalWeight /= 2 // each edge counted twice

	if totalWeight == 0 {
		return &CommunityResult{NodeToComm: make(map[string]string)}
	}

	// Weighted degree per node
	degree := make(map[string]float64)
	for id := range symbolNodes {
		for _, w := range neighbors[id] {
			degree[id] += w
		}
	}

	// Louvain Phase 1: local moves over the raw symbol graph. Each
	// node starts in its own singleton community; we move nodes
	// greedily until no move improves modularity.
	commIDs := make([]string, 0, len(symbolNodes))
	for id := range symbolNodes {
		commIDs = append(commIDs, id)
	}
	sort.Strings(commIDs) // deterministic visitation
	comm, commNodes := louvainLocalMoves(commIDs, neighbors, degree, totalWeight)
	return finaliseCommunityPartition(nodes, comm, commNodes, neighbors, degree, totalWeight)
}

// disambiguateLabels makes every cluster label unique. The
// passes cascade from most-meaningful to last-resort:
//
//  1. Append the cluster's hub symbol — the highest-in-cluster-degree
//     member. "parser/languages · GoExtractor" describes what the
//     cluster centers on; "parser/languages · golang" (a file
//     basename) leaves you guessing. The hub is what code calls.
//
//  2. Append a file basename when the hub is missing or also
//     colliding. The first file (alphabetical) is the fallback.
//
//  3. Size suffix when files match too.
//
//  4. Ordinal tiebreaker for the pathological case where multiple
//     clusters truly share modal dir + hub + first file + size.
//
// Deterministic across reruns: the hub is the same when in-cluster
// degrees are stable, files are sorted, and Louvain produces
// communities in a stable order.
func disambiguateLabels(communities []Community) {
	appendChip := func(c *Community, chip string) {
		if chip == "" {
			return
		}
		c.Label = c.Label + " · " + chip
	}
	fileBasename := func(c *Community, idx int) string {
		if idx >= len(c.Files) {
			return ""
		}
		sample := filepath.Base(c.Files[idx])
		if dot := strings.LastIndex(sample, "."); dot > 0 {
			sample = sample[:dot]
		}
		return sample
	}

	// Stage 1: hub-symbol disambiguation.
	{
		counts := make(map[string]int)
		for _, c := range communities {
			counts[c.Label]++
		}
		for i := range communities {
			if counts[communities[i].Label] > 1 {
				appendChip(&communities[i], cleanHubName(communities[i].Hub))
			}
		}
	}

	// Stages 2a/2b: file-basename disambiguation (first then second
	// file) for any label still colliding after the hub pass.
	for pass := 0; pass < 2; pass++ {
		counts := make(map[string]int)
		for _, c := range communities {
			counts[c.Label]++
		}
		for i := range communities {
			if counts[communities[i].Label] > 1 {
				appendChip(&communities[i], fileBasename(&communities[i], pass))
			}
		}
	}

	// Stage 3: size suffix for any label that's still shared. Two
	// clusters of different sizes become distinguishable here.
	{
		counts := make(map[string]int)
		for _, c := range communities {
			counts[c.Label]++
		}
		for i := range communities {
			if counts[communities[i].Label] > 1 {
				communities[i].Label = fmt.Sprintf("%s (%d)", communities[i].Label, communities[i].Size)
			}
		}
	}

	// Stage 4: ordinal tiebreaker. Truly identical clusters
	// (same dir, same hub, same first file, same size) get a numeric
	// suffix so the UI never shows two cards with the same label.
	{
		counts := make(map[string]int)
		for _, c := range communities {
			counts[c.Label]++
		}
		seen := make(map[string]int)
		for i := range communities {
			lbl := communities[i].Label
			if counts[lbl] > 1 {
				seen[lbl]++
				communities[i].Label = fmt.Sprintf("%s #%d", lbl, seen[lbl])
			}
		}
	}
}

// findHub returns the symbol name of the member with the highest
// in-cluster weighted degree — the "centre" of the cluster.
// In-cluster degree (rather than total degree) matters because we
// want the symbol others in *this* cluster connect to, not the
// most-called function in the entire codebase.
func findHub(members []string, nodeMap map[string]*graph.Node, neighbors map[string]map[string]float64) string {
	if len(members) == 0 {
		return ""
	}
	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		memberSet[m] = true
	}
	var hubID string
	var hubDeg float64
	for _, m := range members {
		var deg float64
		for n, w := range neighbors[m] {
			if memberSet[n] {
				deg += w
			}
		}
		// Tie-break on lexicographic ID so the pick is deterministic
		// when several members share the top in-cluster degree.
		if deg > hubDeg || (deg == hubDeg && hubID == "") || (deg == hubDeg && m < hubID) {
			hubDeg = deg
			hubID = m
		}
	}
	if hubID == "" {
		return ""
	}
	n := nodeMap[hubID]
	if n == nil {
		return ""
	}
	return n.Name
}

// cleanHubName trims a symbol name down to a tag-friendly form.
// Strips Go method-receiver wrapping ("(*Foo).Bar" → "Foo.Bar") and
// caps length so chips don't blow out the card.
func cleanHubName(name string) string {
	if name == "" {
		return ""
	}
	// "(*Foo).Bar" → "Foo.Bar"
	if strings.HasPrefix(name, "(*") {
		if end := strings.Index(name, ")."); end > 2 {
			name = name[2:end] + name[end+1:]
		}
	}
	if strings.HasPrefix(name, "(") {
		if end := strings.Index(name, ")."); end > 1 {
			name = name[1:end] + name[end+1:]
		}
	}
	const max = 32
	if len(name) > max {
		name = name[:max-1] + "…"
	}
	return name
}

// louvainLocalMoves runs the inner loop of Louvain phase 1. Used by
// the raw-node pass and again by the phase-2 aggregation pass —
// they're algorithmically identical, only the graph differs.
//
// Inputs:
//   - nodeIDs:    deterministic visitation order
//   - neighbors:  adjacency with weights (undirected, both directions stored)
//   - degree:     weighted degree per node
//   - totalWeight: sum of all edge weights / 2 (each edge counted twice in neighbors)
//
// Returns:
//   - nodeID → communityID (just the surviving membership)
//   - communityID → list of member nodeIDs
//
// We seed each node into its own community and iterate up to ten
// passes, stopping early once no node finds a beneficial move.
func louvainLocalMoves(
	nodeIDs []string,
	neighbors map[string]map[string]float64,
	degree map[string]float64,
	totalWeight float64,
) (map[string]string, map[string][]string) {
	comm := make(map[string]string, len(nodeIDs))
	commNodes := make(map[string][]string, len(nodeIDs))
	sigmaIn := make(map[string]float64, len(nodeIDs))
	sigmaTot := make(map[string]float64, len(nodeIDs))
	for _, id := range nodeIDs {
		comm[id] = id
		commNodes[id] = []string{id}
		sigmaTot[id] = degree[id]
	}

	improved := true
	for pass := 0; pass < 10 && improved; pass++ {
		improved = false
		for _, id := range nodeIDs {
			currentComm := comm[id]
			bestComm := currentComm
			bestGain := 0.0

			commWeights := make(map[string]float64)
			for neighbor, w := range neighbors[id] {
				commWeights[comm[neighbor]] += w
			}

			ki := degree[id]
			kiIn := commWeights[currentComm]
			removeDelta := kiIn - sigmaTot[currentComm]*ki/(2*totalWeight)

			for c, wc := range commWeights {
				if c == currentComm {
					continue
				}
				gain := wc - sigmaTot[c]*ki/(2*totalWeight) - removeDelta
				if gain > bestGain {
					bestGain = gain
					bestComm = c
				}
			}

			if bestComm != currentComm {
				improved = true
				old := commNodes[currentComm]
				for i, nid := range old {
					if nid == id {
						commNodes[currentComm] = append(old[:i], old[i+1:]...)
						break
					}
				}
				sigmaIn[currentComm] -= 2 * kiIn
				sigmaTot[currentComm] -= ki

				comm[id] = bestComm
				commNodes[bestComm] = append(commNodes[bestComm], id)
				sigmaIn[bestComm] += 2 * commWeights[bestComm]
				sigmaTot[bestComm] += ki

				if len(commNodes[currentComm]) == 0 {
					delete(commNodes, currentComm)
					delete(sigmaIn, currentComm)
					delete(sigmaTot, currentComm)
				}
			}
		}
	}

	return comm, commNodes
}

// assignDirectoryParents groups peer communities that share their
// directory head (the substring before the first " ·" or " +N dirs"
// disambiguator). Clusters whose head matches no other cluster get
// no parent — they're already singular on the canvas.
//
// Parent ids are stable across reruns because they're derived from
// the head string itself, not from any incidental hash or counter.
func assignDirectoryParents(communities []Community) {
	headCount := make(map[string]int)
	for _, c := range communities {
		headCount[labelHead(c.Label)]++
	}
	for i := range communities {
		head := labelHead(communities[i].Label)
		if headCount[head] >= 2 {
			communities[i].ParentID = "group/" + head
		}
	}
}

// labelHead pulls the directory-prefix part out of a fully-formatted
// disambiguated label. We always insert " · " or " +N dirs" between
// the head and any disambiguator, so the head ends right before the
// first occurrence of either.
func labelHead(label string) string {
	// First " · " marks where the disambiguator chips start.
	if i := strings.Index(label, " · "); i > 0 {
		label = label[:i]
	}
	// " +N dirs" marks the "spread" annotation; the head is what's
	// before it.
	if i := strings.Index(label, " +"); i > 0 {
		label = label[:i]
	}
	// Trailing " (N)" size or " #N" ordinal disambiguators.
	if i := strings.Index(label, " ("); i > 0 {
		label = label[:i]
	}
	if i := strings.Index(label, " #"); i > 0 {
		label = label[:i]
	}
	return label
}

func edgeWeight(kind graph.EdgeKind) float64 {
	switch kind {
	case graph.EdgeCalls, graph.EdgeSpawns:
		return 3.0
	case graph.EdgeMemberOf, graph.EdgeParamOf:
		return 2.0
	case graph.EdgeReferences, graph.EdgeReturns, graph.EdgeTypedAs:
		return 1.5
	case graph.EdgeImplements, graph.EdgeExtends,
		graph.EdgeAliases, graph.EdgeComposes:
		return 2.0
	case graph.EdgeImports, graph.EdgeDependsOnModule:
		return 0.5
	case graph.EdgeInstantiates:
		return 1.0
	default:
		// Domain-specific edges (queries, config, flag toggles, emits,
		// owns, licensed_as, generated_by, …) deliberately do not
		// influence community formation — they pull symbols toward
		// per-domain hubs (the flag node, the table node) which is
		// noise for code-cluster detection.
		return 0
	}
}

func computeCohesion(members []string, neighbors map[string]map[string]float64) float64 {
	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		memberSet[m] = true
	}

	var internal, total float64
	for _, m := range members {
		for n, w := range neighbors[m] {
			total += w
			if memberSet[n] {
				internal += w
			}
		}
	}

	if total == 0 {
		return 0
	}
	return math.Round(internal/total*100) / 100
}

func computeModularity(comm map[string]string, neighbors map[string]map[string]float64, degree map[string]float64, totalWeight float64) float64 {
	if totalWeight == 0 {
		return 0
	}
	var q float64
	for i, ci := range comm {
		for j, w := range neighbors[i] {
			if comm[j] == ci {
				q += w - degree[i]*degree[j]/(2*totalWeight)
			}
		}
	}
	return math.Round(q/(2*totalWeight)*1000) / 1000
}

// inferCommunityLabel produces a human-meaningful name for a
// Louvain cluster.
//
// The earlier heuristic tallied the *basename* of each file's parent
// directory and picked the modal one. That collapsed structurally
// distinct clusters into duplicate labels — a cluster with 60 files
// scattered across parser/, graph/, dataflow/, mcp/ would still be
// called "languages" if a handful of files happened to live under
// .../parser/languages/. The dashboard then showed dozens of
// "languages" cards that looked identical at a glance.
//
// New strategy:
//
//  1. Find the longest directory prefix shared by every file in the
//     cluster. If that prefix is deeper than the repo head + a
//     well-known plumbing segment (internal/src/lib/pkg), the
//     cluster is "pure" and we name it by the trailing two segments
//     of that prefix (e.g. "parser/languages").
//
//  2. Otherwise the cluster spans multiple subdirectories. Pick the
//     directory holding the most files and label it
//     "<modalDir> +N dirs" so the reader can immediately tell this
//     is a wiring/mixed cluster — different from the pure case and
//     different from other mixed clusters as long as their modal
//     directory or spread differs.
//
//  3. Fall back to the shared-name-prefix heuristic only when the
//     file-based path produces nothing meaningful, and finally to a
//     numeric cluster id.
func inferCommunityLabel(members []string, nodeMap map[string]*graph.Node, files []string) string {
	if len(files) == 0 {
		return fmt.Sprintf("cluster-%d", len(members))
	}

	if pure := pureClusterLabel(files); pure != "" {
		return pure
	}

	if mixed := mixedClusterLabel(files); mixed != "" {
		return mixed
	}

	if np := namePrefixLabel(members, nodeMap); np != "" {
		return np
	}

	return path.Dir(filepath.ToSlash(files[0]))
}

// RelabelNarrowedCommunity recomputes a community's label from a member /
// file set that was narrowed AFTER detection ran — the shape a scope clamp
// produces when it drops the members a caller may not see.
//
// The detection-time label is derived from the whole partition cell: it is
// the modal directory across every file in it, so a cell that straddles a
// scope boundary is labelled with whichever side contributed more files.
// Reporting that label next to a narrowed member list is both a disclosure
// of the other side and a description of a cluster the caller was not
// shown. Recomputing over the surviving files keeps the label a description
// of what was actually returned.
//
// The name-prefix fallback needs hydrated nodes and is skipped here; a
// narrowed cell falls through to its first surviving file's directory
// instead, which needs no graph reads.
func RelabelNarrowedCommunity(members, files []string) string {
	return inferCommunityLabel(members, nil, files)
}

// pureClusterLabel returns a name for clusters whose files share a
// meaningful directory ancestor (deeper than repo/plumbing). Returns
// "" when no such ancestor exists, signalling a mixed cluster.
func pureClusterLabel(files []string) string {
	pfx := longestCommonDirPrefix(files)
	if pfx == "" {
		return ""
	}
	trimmed := stripPlumbingPrefix(pfx)
	if trimmed == "" {
		// The shared ancestor was just the repo head or a generic
		// plumbing wrapper — not informative.
		return ""
	}
	return trailingPathSegments(trimmed, 2)
}

// mixedClusterLabel names a cluster whose files spread across many
// directories. We surface the modal directory plus a spread count
// so two mixed clusters with different modes don't look identical.
func mixedClusterLabel(files []string) string {
	dirCount := make(map[string]int)
	for _, f := range files {
		// Graph file paths keep OS-native separators (see graphRelKey), so
		// normalise before taking the directory: the result is rendered into
		// the label and trimmed by helpers that split on "/".
		dirCount[path.Dir(filepath.ToSlash(f))]++
	}
	if len(dirCount) == 0 {
		return ""
	}
	var bestDir string
	var bestCount int
	for d, c := range dirCount {
		if c > bestCount || (c == bestCount && d < bestDir) {
			bestCount = c
			bestDir = d
		}
	}
	if bestDir == "" {
		return ""
	}
	trimmed := stripPlumbingPrefix(bestDir)
	if trimmed == "" {
		trimmed = bestDir
	}
	name := trailingPathSegments(trimmed, 2)
	if name == "" {
		name = trimmed
	}
	if len(dirCount) > 1 {
		return fmt.Sprintf("%s +%d dirs", name, len(dirCount)-1)
	}
	return name
}

// longestCommonDirPrefix returns the longest directory path shared
// by every file path. Returns "" when no shared ancestor exists
// (different repo heads, etc.).
func longestCommonDirPrefix(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	// Graph file paths keep OS-native separators (see graphRelKey), so
	// normalise with ToSlash first, then take the directory with path.Dir —
	// the trimming loop below cuts at "/", so a prefix carrying the OS
	// separator could never be shortened and the function would bail out with
	// "" on the first mismatch.
	pfx := path.Dir(filepath.ToSlash(paths[0]))
	for _, p := range paths[1:] {
		dir := path.Dir(filepath.ToSlash(p))
		for pfx != "" && !isPathPrefix(dir, pfx) {
			cut := strings.LastIndex(pfx, "/")
			if cut < 0 {
				pfx = ""
				break
			}
			pfx = pfx[:cut]
		}
		if pfx == "" {
			return ""
		}
	}
	return pfx
}

// isPathPrefix reports whether `pfx` is a directory ancestor of
// (or equal to) `p`, treating "/"-bounded segments to avoid the
// "foo" / "foobar" false positive.
func isPathPrefix(p, pfx string) bool {
	if p == pfx {
		return true
	}
	return strings.HasPrefix(p, pfx+"/")
}

// stripPlumbingPrefix drops the repo head segment and any well-known
// plumbing segment (internal/src/lib/pkg) that carries no signal.
// Returns "" when nothing meaningful remains.
func stripPlumbingPrefix(p string) string {
	if i := strings.Index(p, "/"); i >= 0 {
		p = p[i+1:]
	} else {
		return ""
	}
	for _, plumb := range []string{"internal/", "src/", "lib/", "pkg/"} {
		if strings.HasPrefix(p, plumb) {
			p = p[len(plumb):]
			break
		}
	}
	if p == "internal" || p == "src" || p == "lib" || p == "pkg" {
		return ""
	}
	return p
}

// trailingPathSegments returns the last n non-empty segments of a
// "/"-joined path.
func trailingPathSegments(p string, n int) string {
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) <= n {
		return strings.Join(out, "/")
	}
	return strings.Join(out[len(out)-n:], "/")
}

// namePrefixLabel preserves the legacy "shared identifier prefix"
// heuristic ("HandleUser", "HandleAuth" → "handle") used when the
// file-based paths don't yield anything useful.
func namePrefixLabel(members []string, nodeMap map[string]*graph.Node) string {
	prefixCount := make(map[string]int)
	for _, mid := range members {
		n := nodeMap[mid]
		if n == nil {
			continue
		}
		name := n.Name
		for i := 1; i < len(name); i++ {
			if name[i] >= 'A' && name[i] <= 'Z' {
				prefix := strings.ToLower(name[:i])
				if len(prefix) >= 3 {
					prefixCount[prefix]++
				}
				break
			}
		}
	}
	var bestPrefix string
	var bestPrefixCount int
	for p, c := range prefixCount {
		if c > bestPrefixCount && c >= 3 {
			bestPrefixCount = c
			bestPrefix = p
		}
	}
	return bestPrefix
}

// finaliseCommunityPartition converts a (nodeID → community label)
// partition into a fully-shaped CommunityResult: renumbered IDs,
// per-cluster files / cohesion / hub, label disambiguation, and
// sibling-group parent assignment. Shared by the in-process Louvain
// path (which builds the partition itself) and the backend-delegated
// path (DetectCommunitiesLouvainBackend, which takes the partition
// from graph.CommunityDetector).
//
// commNodes can be nil; when it is, the function inverts comm to
// recover the per-community member list (one extra pass — only used
// on the backend path where commNodes isn't pre-built).
func finaliseCommunityPartition(
	nodes []*graph.Node,
	comm map[string]string,
	commNodes map[string][]string,
	neighbors map[string]map[string]float64,
	degree map[string]float64,
	totalWeight float64,
) *CommunityResult {
	if commNodes == nil {
		commNodes = make(map[string][]string, len(comm))
		for nid, cid := range comm {
			commNodes[cid] = append(commNodes[cid], nid)
		}
	}

	nodeMap := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		nodeMap[n.ID] = n
	}

	result := &CommunityResult{
		NodeToComm: make(map[string]string),
	}

	// Renumber: keep clusters of size >= 2, sort old labels for
	// determinism, mint sequential "community-N" names.
	oldIDs := make([]string, 0, len(commNodes))
	for cid := range commNodes {
		if len(commNodes[cid]) >= 2 {
			oldIDs = append(oldIDs, cid)
		}
	}
	sort.Strings(oldIDs)
	commRemap := make(map[string]string, len(oldIDs))
	for i, cid := range oldIDs {
		commRemap[cid] = fmt.Sprintf("community-%d", i)
	}

	for nodeID, cid := range comm {
		if newID, ok := commRemap[cid]; ok {
			result.NodeToComm[nodeID] = newID
		}
	}

	for oldID, members := range commNodes {
		newID, ok := commRemap[oldID]
		if !ok {
			continue
		}
		fileSet := make(map[string]bool)
		for _, mid := range members {
			if n, ok := nodeMap[mid]; ok {
				fileSet[n.FilePath] = true
			}
		}
		files := make([]string, 0, len(fileSet))
		for f := range fileSet {
			files = append(files, f)
		}
		sort.Strings(files)

		c := Community{
			ID:       newID,
			Label:    inferCommunityLabel(members, nodeMap, files),
			Members:  members,
			Files:    files,
			Size:     len(members),
			Cohesion: computeCohesion(members, neighbors),
			Hub:      findHub(members, nodeMap, neighbors),
		}
		result.Communities = append(result.Communities, c)
	}

	disambiguateLabels(result.Communities)
	assignDirectoryParents(result.Communities)
	sort.Slice(result.Communities, func(i, j int) bool {
		return result.Communities[i].Size > result.Communities[j].Size
	})
	result.Modularity = computeModularity(comm, neighbors, degree, totalWeight)
	return result
}

// DetectCommunitiesLouvainBackend runs Louvain via the backend's
// engine-native implementation (graph.CommunityDetector) and threads
// the resulting partition through
// the same post-processing the in-process DetectCommunitiesLouvain
// uses. The output is shape-identical: every Community label,
// hub, cohesion, parent, and modularity field is populated from
// the partition, so downstream consumers (UI, rerank pipeline)
// can't tell which path produced it.
//
// Returns nil when the backend errors — callers should fall
// through to the in-process path rather than surface a half-done
// CommunityResult.
func DetectCommunitiesLouvainBackend(g graph.Reader, cd graph.CommunityDetector) *CommunityResult {
	if g == nil || cd == nil {
		return nil
	}
	hits, err := cd.Louvain(graph.CommunityOpts{})
	if err != nil || len(hits) == 0 {
		return nil
	}

	nodes := g.AllNodes()
	symbolNodes := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			symbolNodes[n.ID] = true
		}
	}

	// Rebuild the same weighted neighbor view DetectCommunitiesLouvain
	// uses — needed for cohesion / hub / modularity. The work is
	// O(V + E) per call; small relative to the engine-native
	// partitioning save.
	type edgeKey struct{ a, b string }
	weights := make(map[edgeKey]float64)
	for _, e := range g.AllEdges() {
		if !symbolNodes[e.From] || !symbolNodes[e.To] {
			continue
		}
		w := edgeWeight(e.Kind)
		if w == 0 {
			continue
		}
		weights[edgeKey{e.From, e.To}] += w
		weights[edgeKey{e.To, e.From}] += w
	}
	neighbors := make(map[string]map[string]float64)
	for k, w := range weights {
		if neighbors[k.a] == nil {
			neighbors[k.a] = make(map[string]float64)
		}
		neighbors[k.a][k.b] = w
	}
	var totalWeight float64
	for _, w := range weights {
		totalWeight += w
	}
	totalWeight /= 2
	degree := make(map[string]float64, len(symbolNodes))
	for id := range symbolNodes {
		for _, w := range neighbors[id] {
			degree[id] += w
		}
	}

	comm := make(map[string]string, len(hits))
	for _, h := range hits {
		if !symbolNodes[h.NodeID] {
			continue
		}
		comm[h.NodeID] = strconv.FormatInt(h.CommunityID, 10)
	}
	if len(comm) == 0 {
		return nil
	}
	return finaliseCommunityPartition(nodes, comm, nil, neighbors, degree, totalWeight)
}
