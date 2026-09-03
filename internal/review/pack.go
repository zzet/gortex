package review

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/tokens"
)

// Tier labels rank a PackEntry by how much of the symbol the reviewer needs to
// see. Tier 1 carries the actual +/- diff text, tier 2 the full caller source,
// tier 3 a signature-only outline of the broader neighbourhood.
const (
	TierChanged = "changed" // tier 1 — the change itself, as diff hunks
	TierCaller  = "caller"  // tier 2 — direct callers, full source
	TierOutline = "outline" // tier 3 — wider neighbourhood, signatures only
)

// PackEntry is one symbol/file in a tiered review pack. Exactly one of Diff
// (tier 1), Source (tier 2), or Signature (tier 3) is populated, matching Tier.
type PackEntry struct {
	ID        string `json:"id"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Tier      string `json:"tier"`
	Diff      string `json:"diff,omitempty"`
	Source    string `json:"source,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// ReviewPack is a tiered review context for a changeset: the changed symbols as
// diff hunks, their direct callers as full source, and the broader neighbourhood
// as a compact outline — all packed under one token budget. Truncated reports
// whether the budget demoted or dropped any entry.
type ReviewPack struct {
	Changed   []PackEntry `json:"changed"`
	Callers   []PackEntry `json:"callers"`
	Outline   []PackEntry `json:"outline"`
	Budget    int         `json:"token_budget"`
	Truncated bool        `json:"truncated"`
}

// BuildReviewPack assembles a tiered review context for a changeset.
//
//   - Tier 1 (Changed): every changed symbol rendered as its actual +/- diff
//     hunk text, sliced from the ChangeView by the symbol's [StartLine,EndLine]
//     span (graph node range), falling back to a unified diff of the symbol's
//     old/new source when the view carries no hunk lines for it.
//   - Tier 2 (Callers): the direct callers of the changed symbols
//     (impact.ByDepth[1]) rendered as full source, so the reviewer sees how the
//     change is used.
//   - Tier 3 (Outline): the broader neighbourhood (further callers /
//     containing-file siblings, impact.ByDepth[2..]) as a compact outline —
//     signatures only.
//
// The single token budget is filled tier 1 first, then tier 2, then tier 3. As
// it fills, the lower tiers are demoted/truncated first: tier 3 is dropped before
// tier 2, and tier 2 before tier 1, so the change itself always survives. A
// budget of <= 0 means "no budget" (every tier kept). Truncated is set when any
// entry was dropped.
func BuildReviewPack(g graph.Reader, view *ChangeView, diff *analysis.DiffResult, impact *analysis.ImpactResult, tokenBudget int) *ReviewPack {
	pack := &ReviewPack{Budget: tokenBudget}

	changed := buildChangedEntries(g, view, diff)
	callers, outline := buildImpactEntries(g, view, impact, changedIDSet(diff))

	if tokenBudget <= 0 {
		pack.Changed = changed
		pack.Callers = callers
		pack.Outline = outline
		return pack
	}

	// Fill tier 1 first; it is the change itself and is never demoted. A single
	// changed entry that on its own overruns the budget is still kept (the
	// reviewer must see the change) but marks the pack truncated.
	used := 0
	for _, e := range changed {
		pack.Changed = append(pack.Changed, e)
		used += entryTokens(e)
	}
	if used > tokenBudget {
		pack.Truncated = true
	}

	// Tier 2: direct callers, full source. Demoted before tier 1 but after
	// tier 3 — so fill it next, stopping when the budget is exhausted.
	for _, e := range callers {
		cost := entryTokens(e)
		if used+cost > tokenBudget {
			pack.Truncated = true
			continue
		}
		pack.Callers = append(pack.Callers, e)
		used += cost
	}

	// Tier 3: outline, demoted first. Fill last with whatever budget remains.
	for _, e := range outline {
		cost := entryTokens(e)
		if used+cost > tokenBudget {
			pack.Truncated = true
			continue
		}
		pack.Outline = append(pack.Outline, e)
		used += cost
	}

	return pack
}

// changedIDSet collects the changed-symbol ids so the impact tiers can exclude a
// changed symbol that is also (transitively) its own caller.
func changedIDSet(diff *analysis.DiffResult) map[string]bool {
	set := map[string]bool{}
	if diff == nil {
		return set
	}
	for _, cs := range diff.ChangedSymbols {
		set[cs.ID] = true
	}
	return set
}

// buildChangedEntries renders each changed symbol as its diff-hunk text (tier 1).
// Entries are sorted by id for a deterministic pack.
func buildChangedEntries(g graph.Reader, view *ChangeView, diff *analysis.DiffResult) []PackEntry {
	if diff == nil {
		return nil
	}
	entries := make([]PackEntry, 0, len(diff.ChangedSymbols))
	for _, cs := range diff.ChangedSymbols {
		entries = append(entries, PackEntry{
			ID:   cs.ID,
			File: cleanPath(cs.FilePath),
			Line: cs.Line,
			Tier: TierChanged,
			Diff: symbolHunk(g, view, cs),
		})
	}
	sortEntries(entries)
	return entries
}

// buildImpactEntries splits the impact blast radius into tier 2 (direct callers,
// full source) and tier 3 (the deeper neighbourhood, outline-only). A symbol
// that is itself a changed symbol is skipped from both tiers. Each tier is
// sorted by id for a deterministic pack.
func buildImpactEntries(g graph.Reader, view *ChangeView, impact *analysis.ImpactResult, changed map[string]bool) (callers, outline []PackEntry) {
	if impact == nil {
		return nil, nil
	}

	seen := map[string]bool{}

	for _, e := range impact.ByDepth[1] {
		if changed[e.ID] || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		callers = append(callers, PackEntry{
			ID:     e.ID,
			File:   cleanPath(e.FilePath),
			Line:   e.Line,
			Tier:   TierCaller,
			Source: fullSource(g, view, e.ID),
		})
	}

	// Tier 3: every deeper tier, outline-only. Walk depths in order so the
	// remainder is deterministic before the final id sort.
	depths := make([]int, 0, len(impact.ByDepth))
	for d := range impact.ByDepth {
		if d >= 2 {
			depths = append(depths, d)
		}
	}
	sort.Ints(depths)
	for _, d := range depths {
		for _, e := range impact.ByDepth[d] {
			if changed[e.ID] || seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			outline = append(outline, PackEntry{
				ID:        e.ID,
				File:      cleanPath(e.FilePath),
				Line:      e.Line,
				Tier:      TierOutline,
				Signature: signatureOf(g, e.ID, e.Name),
			})
		}
	}

	sortEntries(callers)
	sortEntries(outline)
	return callers, outline
}

// SymbolHunk renders the diff-hunk text for a single changed symbol — the
// exported entry point the packaged-review layer uses to ground a per-symbol
// classification on the change itself.
func SymbolHunk(g graph.Reader, view *ChangeView, sym analysis.ChangedSymbol) string {
	return symbolHunk(g, view, sym)
}

// symbolHunk renders the diff-hunk text for a changed symbol: the new-side and
// old-side lines from the ChangeView that fall inside the symbol's graph span
// [StartLine,EndLine], as a +/- block. When the view carries no lines for the
// symbol's range it falls back to a unified diff of the symbol's old/new source.
func symbolHunk(g graph.Reader, view *ChangeView, sym analysis.ChangedSymbol) string {
	file := cleanPath(sym.FilePath)
	start, end := symbolSpan(g, sym)

	if view != nil {
		if fc := view.ByFile[file]; fc != nil {
			if hunk := renderSpanHunk(fc, start, end); hunk != "" {
				return hunk
			}
		}
	}

	// Fallback: a unified diff of the symbol's old/new source. With no recorded
	// old text, treat the post-change source as a pure addition so the entry
	// still carries the change as +/- text.
	newSrc := symbolSource(g, view, sym.ID, start, end)
	if newSrc == "" {
		return ""
	}
	return symbolUnifiedDiff(file, "", newSrc)
}

// renderSpanHunk emits a +/- block from a FileChange limited to the lines whose
// new-side number falls inside [start,end]. Old-side (removed) lines that fall in
// the same range are emitted as `-` lines. Returns "" when no line falls in range.
func renderSpanHunk(fc *FileChange, start, end int) string {
	if fc == nil {
		return ""
	}
	type spanLine struct {
		line  int
		text  string
		mark  byte
		order int
	}
	var rows []spanLine
	for _, dl := range fc.Lines {
		if !inSpan(dl.NewLine, start, end) {
			continue
		}
		mark := byte(' ')
		if dl.Added {
			mark = '+'
		}
		rows = append(rows, spanLine{line: dl.NewLine, text: dl.Text, mark: mark, order: 0})
	}
	for _, dl := range fc.OldLines {
		if !inSpan(dl.NewLine, start, end) {
			continue
		}
		rows = append(rows, spanLine{line: dl.NewLine, text: dl.Text, mark: '-', order: 1})
	}
	if len(rows) == 0 {
		return ""
	}
	// Stable order: by line, then removed-after-context-at-the-same-line so the
	// rendered hunk is deterministic regardless of slice order.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].line != rows[j].line {
			return rows[i].line < rows[j].line
		}
		return rows[i].order < rows[j].order
	})
	var b strings.Builder
	for _, r := range rows {
		b.WriteByte(r.mark)
		b.WriteString(r.text)
		b.WriteByte('\n')
	}
	return b.String()
}

// inSpan reports whether line falls inside [start,end]. An unknown span (<=0)
// matches every line, so a symbol with no recorded range still gets its file's
// hunk lines.
func inSpan(line, start, end int) bool {
	if line <= 0 {
		return false
	}
	if start <= 0 || end <= 0 {
		return true
	}
	return line >= start && line <= end
}

// symbolSpan returns the [start,end] line range for a changed symbol, preferring
// the live graph node's range (more precise than the hunk's start line) and
// falling back to the diff's recorded line when the node is absent.
func symbolSpan(g graph.Reader, sym analysis.ChangedSymbol) (int, int) {
	if g != nil {
		if n := g.GetNode(sym.ID); n != nil && n.StartLine > 0 {
			end := n.EndLine
			if end < n.StartLine {
				end = n.StartLine
			}
			return n.StartLine, end
		}
	}
	if sym.Line > 0 {
		return sym.Line, sym.Line
	}
	return 0, 0
}

// fullSource renders a caller's complete source (tier 2). It reads the node's
// [StartLine,EndLine] span from disk under the ChangeView's RepoRoot.
func fullSource(g graph.Reader, view *ChangeView, id string) string {
	if g == nil {
		return ""
	}
	n := g.GetNode(id)
	if n == nil {
		return ""
	}
	return symbolSource(g, view, id, n.StartLine, n.EndLine)
}

// symbolSource reads the [start,end] source span for a symbol from disk under the
// ChangeView's RepoRoot. Returns "" when the range or repo root is unknown or the
// file can't be read (the synthetic-graph path with no on-disk file).
func symbolSource(g graph.Reader, view *ChangeView, id string, start, end int) string {
	if view == nil || view.RepoRoot == "" || start <= 0 {
		return ""
	}
	n := nodeForID(g, id)
	if n == nil {
		return ""
	}
	if end < start {
		end = start
	}
	data, err := os.ReadFile(filepath.Join(view.RepoRoot, cleanPath(n.FilePath)))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if start > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], "\n")
}

func nodeForID(g graph.Reader, id string) *graph.Node {
	if g == nil {
		return nil
	}
	return g.GetNode(id)
}

// signatureOf renders a tier-3 outline line for a symbol: its stamped
// Meta["signature"] when present, else a bare `name` fallback so the outline
// entry is never empty.
func signatureOf(g graph.Reader, id, name string) string {
	if g != nil {
		if n := g.GetNode(id); n != nil {
			if sig, ok := n.Meta["signature"].(string); ok && strings.TrimSpace(sig) != "" {
				return sig
			}
			if n.Name != "" {
				return n.Name
			}
		}
	}
	return name
}

// symbolUnifiedDiff renders a unified diff of a symbol's old/new source using the
// same go-difflib substrate the edit-preview tooling uses. Returns "" when the
// two sides are identical.
func symbolUnifiedDiff(path, oldSrc, newSrc string) string {
	if oldSrc == newSrc {
		return ""
	}
	out, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(oldSrc),
		B:        difflib.SplitLines(newSrc),
		FromFile: "a/" + path,
		ToFile:   "b/" + path,
		Context:  3,
	})
	if err != nil {
		return ""
	}
	return out
}

// entryTokens measures the token cost of an entry's payload (whichever tier
// field is populated) plus its one-line header — the same cl100k_base measure
// the rest of the budget machinery uses.
func entryTokens(e PackEntry) int {
	payload := e.Diff
	if payload == "" {
		payload = e.Source
	}
	if payload == "" {
		payload = e.Signature
	}
	// The rendered header (id/file:line) is a fixed per-entry overhead.
	header := fmt.Sprintf("%s %s:%d\n", e.ID, e.File, e.Line)
	return tokens.Count(header) + tokens.Count(payload)
}

func sortEntries(entries []PackEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].ID != entries[j].ID {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Line < entries[j].Line
	})
}

// Render produces a deterministic, human-readable rendering of the pack: a
// tier-1 changed block (diff hunks), a tier-2 callers block (full source), and a
// tier-3 outline block (signatures). The output is stable for a given pack —
// entries are pre-sorted by id — so it is safe to snapshot in a test.
func (p *ReviewPack) Render() string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# review pack (budget %d tokens, truncated=%t)\n", p.Budget, p.Truncated)

	b.WriteString("\n## changed (diff)\n")
	for _, e := range p.Changed {
		fmt.Fprintf(&b, "### %s — %s:%d\n", e.ID, e.File, e.Line)
		b.WriteString(strings.TrimRight(e.Diff, "\n"))
		b.WriteByte('\n')
	}

	b.WriteString("\n## callers (full source)\n")
	for _, e := range p.Callers {
		fmt.Fprintf(&b, "### %s — %s:%d\n", e.ID, e.File, e.Line)
		b.WriteString(strings.TrimRight(e.Source, "\n"))
		b.WriteByte('\n')
	}

	b.WriteString("\n## outline (signatures)\n")
	for _, e := range p.Outline {
		fmt.Fprintf(&b, "- %s — %s:%d  %s\n", e.ID, e.File, e.Line, e.Signature)
	}

	return b.String()
}
