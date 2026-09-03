package wiki

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// mermaidID converts a Gortex node ID to a Mermaid-safe identifier.
// Mermaid IDs disallow ::, /, ., space, parentheses, and angle
// brackets. We replace each with underscore so the result is a
// single token even for method receivers like "(*Foo).Bar".
func mermaidID(id string) string {
	r := strings.NewReplacer(
		"::", "_",
		"/", "_",
		".", "_",
		"-", "_",
		" ", "_",
		"<", "_",
		">", "_",
		"(", "_",
		")", "_",
		"*", "_",
		"#", "_",
		"@", "_",
		":", "_",
	)
	return r.Replace(id)
}

// mermaidEscape escapes characters that break Mermaid labels. Only
// the double-quote needs special handling inside `["..."]` labels.
func mermaidEscape(s string) string {
	return strings.ReplaceAll(s, `"`, `#quot;`)
}

// RenderCommunityGraph emits a Mermaid flowchart of communities and
// the cross-community calls between them. Each node is a community;
// edge weights are the number of calls flowing across the boundary.
// Used both on the index page and as the wiki/<repo>/_assets file.
func RenderCommunityGraph(g graph.Reader, communities *analysis.CommunityResult, opts CommunityGraphOpts) string {
	if communities == nil || len(communities.Communities) == 0 {
		return "graph LR\n  empty[\"No communities detected\"]\n"
	}

	// Filter communities by size and cap count.
	type sized struct {
		id    string
		label string
		size  int
	}
	var keep []sized
	for _, c := range communities.Communities {
		if c.Size < opts.MinSize {
			continue
		}
		label := c.Label
		if label == "" {
			label = c.ID
		}
		keep = append(keep, sized{id: c.ID, label: label, size: c.Size})
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].size > keep[j].size })
	if opts.Max > 0 && len(keep) > opts.Max {
		keep = keep[:opts.Max]
	}
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k.id] = true
	}

	// Aggregate cross-community calls.
	type edge struct {
		from, to string
		count    int
	}
	edgeMap := make(map[string]*edge)
	if g != nil {
		for _, e := range g.AllEdges() {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			from := communities.NodeToComm[e.From]
			to := communities.NodeToComm[e.To]
			if from == "" || to == "" || from == to {
				continue
			}
			if !keepSet[from] || !keepSet[to] {
				continue
			}
			key := from + "→" + to
			if x, ok := edgeMap[key]; ok {
				x.count++
			} else {
				edgeMap[key] = &edge{from: from, to: to, count: 1}
			}
		}
	}

	var b strings.Builder
	b.WriteString("graph LR\n")
	for _, k := range keep {
		label := fmt.Sprintf("%s\\n%d symbols", k.label, k.size)
		fmt.Fprintf(&b, "  %s[\"%s\"]\n", mermaidID(k.id), mermaidEscape(label))
	}
	b.WriteString("\n")
	// Sort edges so the output is deterministic.
	keys := make([]string, 0, len(edgeMap))
	for k := range edgeMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ed := edgeMap[k]
		fmt.Fprintf(&b, "  %s -->|%d| %s\n", mermaidID(ed.from), ed.count, mermaidID(ed.to))
	}
	return b.String()
}

// CommunityGraphOpts narrows the community-graph diagram.
type CommunityGraphOpts struct {
	MinSize int
	Max     int
}

// RenderProcessSequence emits a Mermaid sequenceDiagram for one
// Process. Participants are unique communities (or files when a node
// has no community assignment) touched by the process; messages are
// the EdgeCalls transitions in DFS preorder. Each transition is
// labelled with the callee symbol name.
//
// The first iteration emits a flat sequence: we deliberately do not
// emit `loop` or `alt` blocks because the DFS preorder of a call
// graph doesn't carry enough information to reconstruct those control
// structures correctly. A faithful flat sequence is better than a
// confidently-wrong control-flow rendering.
func RenderProcessSequence(p analysis.Process, nodeByID map[string]*graph.Node, commLabelByNode map[string]string) string {
	if len(p.Steps) == 0 {
		return "sequenceDiagram\n  Note over Empty: process has no steps\n"
	}

	// Identify participants in first-seen order so the diagram reads
	// left-to-right in the order communities first appear.
	type participant struct {
		id    string
		label string
	}
	var parts []participant
	seen := make(map[string]bool)

	pickPartID := func(nodeID string) (string, string) {
		if label, ok := commLabelByNode[nodeID]; ok && label != "" {
			return mermaidID("c_" + label), label
		}
		if n := nodeByID[nodeID]; n != nil && n.FilePath != "" {
			return mermaidID("f_" + n.FilePath), n.FilePath
		}
		return mermaidID("n_" + nodeID), nodeID
	}

	addPart := func(nodeID string) string {
		pid, label := pickPartID(nodeID)
		if !seen[pid] {
			seen[pid] = true
			parts = append(parts, participant{id: pid, label: label})
		}
		return pid
	}

	// Walk steps in order and pre-register participants, building
	// the parent-of relation from depth so the messages line up
	// caller→callee.
	parentStack := make([]int, 0, len(p.Steps))
	parents := make([]int, len(p.Steps)) // index of parent step, -1 for root
	for i, step := range p.Steps {
		// Pop stack until top's depth < this step's depth.
		for len(parentStack) > 0 {
			top := parentStack[len(parentStack)-1]
			if p.Steps[top].Depth < step.Depth {
				break
			}
			parentStack = parentStack[:len(parentStack)-1]
		}
		if len(parentStack) == 0 {
			parents[i] = -1
		} else {
			parents[i] = parentStack[len(parentStack)-1]
		}
		parentStack = append(parentStack, i)
		addPart(step.ID)
	}

	var b strings.Builder
	b.WriteString("sequenceDiagram\n")
	b.WriteString("  autonumber\n")
	for _, pt := range parts {
		fmt.Fprintf(&b, "  participant %s as %s\n", pt.id, mermaidEscape(pt.label))
	}
	b.WriteString("\n")
	// Emit one message per non-root step: caller → callee with the
	// callee's symbol name as the message label.
	for i, step := range p.Steps {
		if parents[i] < 0 {
			// Root: emit a note so the diagram has an entry-point
			// anchor without a message arrow.
			fromID, _ := pickPartID(step.ID)
			label := stepLabel(step.ID, nodeByID)
			fmt.Fprintf(&b, "  Note over %s: entry → %s\n", fromID, mermaidEscape(label))
			continue
		}
		fromID, _ := pickPartID(p.Steps[parents[i]].ID)
		toID, _ := pickPartID(step.ID)
		label := stepLabel(step.ID, nodeByID)
		fmt.Fprintf(&b, "  %s->>%s: %s\n", fromID, toID, mermaidEscape(label))
	}
	return b.String()
}

// stepLabel returns a short human-readable name for a step.
func stepLabel(id string, nodeByID map[string]*graph.Node) string {
	if n, ok := nodeByID[id]; ok && n != nil && n.Name != "" {
		return n.Name
	}
	// Fall back to the trailing segment of the ID.
	if idx := strings.LastIndex(id, "::"); idx >= 0 {
		return id[idx+2:]
	}
	return id
}

// RenderArchitecture emits a Mermaid flowchart showing communities
// grouped by parent (when present) plus cross-community arrows.
// Mirrors the architecture overview page.
func RenderArchitecture(g graph.Reader, communities *analysis.CommunityResult, opts CommunityGraphOpts) string {
	if communities == nil || len(communities.Communities) == 0 {
		return "graph TB\n  empty[\"No communities detected\"]\n"
	}

	// Filter and cap.
	type sized struct {
		id     string
		label  string
		size   int
		parent string
	}
	var keep []sized
	for _, c := range communities.Communities {
		if c.Size < opts.MinSize {
			continue
		}
		label := c.Label
		if label == "" {
			label = c.ID
		}
		keep = append(keep, sized{id: c.ID, label: label, size: c.Size, parent: c.ParentID})
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].size > keep[j].size })
	if opts.Max > 0 && len(keep) > opts.Max {
		keep = keep[:opts.Max]
	}
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k.id] = true
	}

	// Group by parent.
	groups := make(map[string][]sized) // parent → children (parent="" → singleton)
	var parentOrder []string
	for _, k := range keep {
		parent := k.parent
		if _, ok := groups[parent]; !ok {
			parentOrder = append(parentOrder, parent)
		}
		groups[parent] = append(groups[parent], k)
	}
	sort.Strings(parentOrder)

	var b strings.Builder
	b.WriteString("graph TB\n")
	for _, parent := range parentOrder {
		members := groups[parent]
		if parent != "" {
			fmt.Fprintf(&b, "  subgraph %s [%s]\n", mermaidID(parent), mermaidEscape(parent))
		}
		for _, k := range members {
			label := fmt.Sprintf("%s\\n%d symbols", k.label, k.size)
			indent := "  "
			if parent != "" {
				indent = "    "
			}
			fmt.Fprintf(&b, "%s%s[\"%s\"]\n", indent, mermaidID(k.id), mermaidEscape(label))
		}
		if parent != "" {
			b.WriteString("  end\n")
		}
	}
	b.WriteString("\n")

	// Cross-community calls.
	type edge struct {
		from, to string
		count    int
	}
	edgeMap := make(map[string]*edge)
	if g != nil {
		for _, e := range g.AllEdges() {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			from := communities.NodeToComm[e.From]
			to := communities.NodeToComm[e.To]
			if from == "" || to == "" || from == to {
				continue
			}
			if !keepSet[from] || !keepSet[to] {
				continue
			}
			key := from + "→" + to
			if x, ok := edgeMap[key]; ok {
				x.count++
			} else {
				edgeMap[key] = &edge{from: from, to: to, count: 1}
			}
		}
	}
	keys := make([]string, 0, len(edgeMap))
	for k := range edgeMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ed := edgeMap[k]
		fmt.Fprintf(&b, "  %s -->|%d| %s\n", mermaidID(ed.from), ed.count, mermaidID(ed.to))
	}
	return b.String()
}
