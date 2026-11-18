package indexer

import (
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Measurement for a declaration-granular delta (no format change): for each
// changed file, the rows the view below holds there are diffed against the
// rows the delta wrote, grouped by the declaration that owns them, and the
// delta reports how many declarations changed, were added or removed, how
// many rows changed of how many, and whether the save would qualify for a
// row-level form (and, if not, why).

// editDeltaMeasureDeclarations turns the measurement on in a delta
// (GORTEX_EDIT_DELTA_DECLARATION_DIFF=1). TestDeclarationDiffReplay measures
// the same offline.
var editDeltaMeasureDeclarations = os.Getenv("GORTEX_EDIT_DELTA_DECLARATION_DIFF") == "1"

// editDeltaRowLevelThreshold is the share of a file's rows above which a
// row-level form would not pay (the path mask is cheaper).
const editDeltaRowLevelThreshold = 0.30

// editDeltaDeclarationDiff is one delta's declaration-level diff.
type editDeltaDeclarationDiff struct {
	Files        int  `json:"files"`
	Declarations int  `json:"declarations"`
	Changed      int  `json:"changed"`
	Added        int  `json:"added"`
	Removed      int  `json:"removed"`
	ChangedRows  int  `json:"changed_rows"`
	TotalRows    int  `json:"total_rows"`
	Qualifies    bool `json:"qualifies"`
	// Reason is the first of Reasons in declarationDiffReasonOrder; Reasons
	// is every reason the save falls back for, in that order.
	Reason  string   `json:"reason,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
}

// declarationDiffReasonOrder fixes which reason a save that falls back for
// several is reported under, so the same save always reports the same one.
var declarationDiffReasonOrder = []string{
	"file_rows", "unowned_rows", "type_declaration", "method_set", "package_level_declaration", "threshold",
}

// addReasons merges reasons into d in declarationDiffReasonOrder.
func (d *editDeltaDeclarationDiff) addReasons(reasons ...string) {
	seen := make(map[string]bool, len(d.Reasons)+len(reasons))
	for _, r := range append(d.Reasons, reasons...) {
		seen[r] = true
	}
	d.Reasons = d.Reasons[:0]
	for _, r := range declarationDiffReasonOrder {
		if seen[r] {
			d.Reasons = append(d.Reasons, r)
		}
	}
	d.Reason = ""
	if len(d.Reasons) > 0 {
		d.Reason = d.Reasons[0]
	}
}

// fileOwner is the owner key of rows that belong to the file, not to a
// declaration (the file node, imports, the package clause).
const fileOwner = "\x00file"

// unownedOwner is the owner key of rows no declaration span contains.
const unownedOwner = "\x00unowned"

func isOwningDeclaration(n *graph.Node) bool {
	if n == nil || !strings.Contains(n.ID, "::") || n.StartLine <= 0 {
		return false
	}
	switch n.Kind {
	case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface,
		graph.KindVariable, graph.KindConstant:
		return true
	}
	return false
}

// declarationOwners maps every node of one file's row set to its owning
// declaration: itself when it is one, else the innermost declaration whose
// span contains its start line, else the file.
type declarationOwners struct {
	decls []*graph.Node
	of    map[string]string
	kinds map[string]graph.NodeKind
}

func newDeclarationOwners(nodes []*graph.Node) *declarationOwners {
	o := &declarationOwners{of: map[string]string{}, kinds: map[string]graph.NodeKind{}}
	for _, n := range nodes {
		if isOwningDeclaration(n) {
			o.decls = append(o.decls, n)
			o.kinds[n.ID] = n.Kind
		}
	}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if isOwningDeclaration(n) {
			o.of[n.ID] = n.ID
			continue
		}
		if owner := o.byLine(n.StartLine); owner != "" {
			o.of[n.ID] = owner
		} else {
			o.of[n.ID] = fileOwner
		}
	}
	return o
}

// byLine is the innermost declaration whose span contains line.
func (o *declarationOwners) byLine(line int) string {
	best, bestSpan := "", 0
	for _, d := range o.decls {
		end := d.EndLine
		if end < d.StartLine {
			end = d.StartLine
		}
		if line >= d.StartLine && line <= end {
			if span := end - d.StartLine; best == "" || span < bestSpan {
				best, bestSpan = d.ID, span
			}
		}
	}
	return best
}

func (o *declarationOwners) edgeOwner(e *graph.Edge) string {
	if owner, ok := o.of[e.From]; ok {
		// The file's defines / contains edge to a declaration (or to one of
		// its children) is that declaration's: adding or removing a
		// declaration changes it, and nothing else of the file.
		if owner == fileOwner && (e.Kind == graph.EdgeDefines || e.Kind == graph.EdgeContains) {
			if target, ok := o.of[e.To]; ok && target != fileOwner {
				return target
			}
		}
		return owner
	}
	if owner := o.byLine(e.Line); owner != "" {
		return owner
	}
	return unownedOwner
}

// declarationNodeIdentity / declarationEdgeIdentity are a row's identity as
// the save states it: what the extractor and the resolver wrote. Metadata
// the later passes set (enrichment's confirmations and types, confidence,
// origin and tier, derived fingerprints, the file row's content hash) is
// not part of it: the view below holds a fully enriched generation and the
// delta is measured before its own enrichment, so comparing them would call
// every declaration changed.
//
// The file node's span is its line count: every save that adds or removes a
// line changes it, and it is rewritten with the file's own row on every
// save, so it is not part of the file node's identity.
func declarationNodeIdentity(n *graph.Node) string {
	if n.Kind == graph.KindFile {
		return fmt.Sprintf("n\x00%s\x00%s\x00%s\x00%s", n.ID, n.Kind, n.Name, n.FilePath)
	}
	return fmt.Sprintf("n\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d", n.ID, n.Kind, n.Name, n.FilePath, n.StartLine, n.EndLine)
}

func declarationEdgeIdentity(e *graph.Edge) string {
	return fmt.Sprintf("e\x00%s\x00%s\x00%s\x00%s\x00%d", e.From, e.To, e.Kind, e.FilePath, e.Line)
}

// ownerDigests hashes each owner's rows (nodes and recorded edges).
func ownerDigests(o *declarationOwners, nodes []*graph.Node, edges []*graph.Edge) (map[string]string, map[string]int) {
	rows := map[string][]string{}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		rows[o.of[n.ID]] = append(rows[o.of[n.ID]], declarationNodeIdentity(n))
	}
	for _, e := range edges {
		if e == nil {
			continue
		}
		owner := o.edgeOwner(e)
		rows[owner] = append(rows[owner], declarationEdgeIdentity(e))
	}
	digests := make(map[string]string, len(rows))
	counts := make(map[string]int, len(rows))
	for owner, r := range rows {
		sort.Strings(r)
		sum := sha256.Sum256([]byte(strings.Join(r, "\n")))
		digests[owner] = string(sum[:])
		counts[owner] = len(r)
	}
	return digests, counts
}

// diffDeclarations diffs one file's prior rows against its new rows.
func diffDeclarations(priorNodes []*graph.Node, priorEdges []*graph.Edge, newNodes []*graph.Node, newEdges []*graph.Edge) editDeltaDeclarationDiff {
	prior := newDeclarationOwners(priorNodes)
	next := newDeclarationOwners(newNodes)
	priorDigests, priorCounts := ownerDigests(prior, priorNodes, priorEdges)
	nextDigests, nextCounts := ownerDigests(next, newNodes, newEdges)
	var d editDeltaDeclarationDiff
	d.Files = 1
	d.Declarations = len(next.decls)
	for _, c := range nextCounts {
		d.TotalRows += c
	}
	var reasons []string
	note := func(r string) { reasons = append(reasons, r) }
	kindOf := func(owner string) graph.NodeKind {
		if k, ok := next.kinds[owner]; ok {
			return k
		}
		return prior.kinds[owner]
	}
	for owner, digest := range nextDigests {
		priorDigest, existed := priorDigests[owner]
		switch {
		case !existed:
			if owner != fileOwner && owner != unownedOwner {
				d.Added++
			}
			d.ChangedRows += nextCounts[owner]
		case priorDigest != digest:
			if owner != fileOwner && owner != unownedOwner {
				d.Changed++
			}
			d.ChangedRows += nextCounts[owner]
		default:
			continue
		}
		switch owner {
		case fileOwner:
			note("file_rows")
		case unownedOwner:
			note("unowned_rows")
		default:
			switch kindOf(owner) {
			case graph.KindType, graph.KindInterface:
				note("type_declaration")
			case graph.KindVariable, graph.KindConstant:
				note("package_level_declaration")
			}
			if !existed && kindOf(owner) == graph.KindMethod {
				note("method_set")
			}
		}
	}
	for owner := range priorDigests {
		if _, kept := nextDigests[owner]; kept {
			continue
		}
		if owner != fileOwner && owner != unownedOwner {
			d.Removed++
			switch kindOf(owner) {
			case graph.KindType, graph.KindInterface:
				note("type_declaration")
			case graph.KindMethod:
				note("method_set")
			}
		} else {
			note("file_rows")
		}
		d.ChangedRows += priorCounts[owner]
	}
	if d.TotalRows > 0 && float64(d.ChangedRows) > editDeltaRowLevelThreshold*float64(d.TotalRows) {
		note("threshold")
	}
	d.addReasons(reasons...)
	d.Qualifies = d.Reason == ""
	return d
}

// measureDeclarationDiff diffs every changed path of a delta: the view
// below's rows there against the delta's view of them.
func measureDeclarationDiff(dw *graph.DeltaWriter, paths []string) editDeltaDeclarationDiff {
	var total editDeltaDeclarationDiff
	below := dw.Below()
	belowRecorded, okBelow := graph.RecordedEdgesOf(below)
	nowRecorded, okNow := graph.RecordedEdgesOf(dw)
	if !okBelow || !okNow {
		total.Reason = "no_recorded_edges"
		return total
	}
	sort.Strings(paths)
	for _, p := range paths {
		d := diffDeclarations(below.GetFileNodes(p), belowRecorded.RecordedEdgesAt([]string{p}),
			dw.GetFileNodes(p), nowRecorded.RecordedEdgesAt([]string{p}))
		total.Files += d.Files
		total.Declarations += d.Declarations
		total.Changed += d.Changed
		total.Added += d.Added
		total.Removed += d.Removed
		total.ChangedRows += d.ChangedRows
		total.TotalRows += d.TotalRows
		total.addReasons(d.Reasons...)
	}
	total.Qualifies = total.Reason == "" && total.Files > 0
	return total
}
