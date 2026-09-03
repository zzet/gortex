package review

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
)

// Anchor-method labels record which resolver tier located a snippet. They ride
// on Finding.Anchor so a caller can tell an exact hunk hit from a fuzzy or LLM
// re-location. An unresolved anchor carries Line == 0 so callers drop it.
const (
	AnchorExactHunk  = "exact_hunk" // matched a new-side hunk/context line
	AnchorOldSide    = "old_side"   // matched a removed (old-side) line
	AnchorFullFile   = "full_file"  // matched the post-change file on disk
	AnchorLLM        = "llm"        // re-located by the LLM fallback seam
	AnchorUnresolved = "unresolved" // nothing matched
)

const (
	sideRight = "RIGHT" // new-side
	sideLeft  = "LEFT"  // old-side
)

// DiffLine is one line carried out of a unified diff for snippet anchoring.
// For new-side lines NewLine is the post-change line number and Added marks an
// inserted line (vs a context line). For old-side lines NewLine is the line's
// position in the pre-change file and Added is false.
type DiffLine struct {
	NewLine int    `json:"new_line"`
	Text    string `json:"text"`
	Added   bool   `json:"added"`
}

// FileChange is the per-file view a reviewer grounds against: the new-side
// (post-change) lines a finding anchors to, plus the removed old-side lines so
// a snippet that survives only in the pre-change text can still be located.
type FileChange struct {
	Path     string     `json:"path"`
	Lines    []DiffLine `json:"lines"`               // new-side: added + context
	OldLines []DiffLine `json:"old_lines,omitempty"` // removed (old-side)
}

// ChangeView is the per-PR/diff view a reviewer grounds against: every changed
// file with its new-side and old-side lines, keyed by cleaned relative path.
type ChangeView struct {
	RepoRoot string
	ByFile   map[string]*FileChange
}

// Anchor is a resolved (file,line) location for a free-text snippet, plus the
// confidence of the match and the resolver tier (Method) that produced it. A
// Method of AnchorUnresolved carries Line == 0 so callers drop the finding.
type Anchor struct {
	File       string  `json:"file"`
	Line       int     `json:"line"`
	Confidence float64 `json:"confidence"`
	Method     string  `json:"method"`
	Side       string  `json:"side,omitempty"`
}

// BuildChangeView builds the diff view for a scope by reusing the landed
// new-side substrate (analysis.MapGitDiffWithLines) and parsing the removed
// old-side lines from the same diff. The graph supplies the changed-symbol
// spans inside MapGitDiffWithLines; it may be nil (then only the line text is
// available, which is all the resolver needs). repoPrefix anchors the node
// join in multi-repo mode (see analysis.MapGitDiff).
func BuildChangeView(g graph.Reader, repoRoot, repoPrefix, scope, baseRef string) (*ChangeView, error) {
	_, newLines, err := analysis.MapGitDiffWithLines(g, repoRoot, repoPrefix, scope, baseRef)
	if err != nil {
		return nil, err
	}

	view := &ChangeView{RepoRoot: repoRoot, ByFile: map[string]*FileChange{}}
	for file, hunkLines := range newLines {
		fc := view.fileChange(file)
		for _, hl := range hunkLines {
			fc.Lines = append(fc.Lines, DiffLine{
				NewLine: hl.NewLine,
				Text:    hl.Text,
				Added:   hl.Side == "+",
			})
		}
	}

	// The new-side substrate discards removed lines; recover them from the raw
	// diff so the old-side tier can match a deleted snippet.
	if raw, derr := rawDiff(repoRoot, scope, baseRef); derr == nil {
		applyOldSide(view, raw)
	}

	return view, nil
}

// ChangeViewFromDiff builds a ChangeView from raw unified-diff text — the
// pasted-diff (off-disk) path and the deterministic test seam. It parses both
// the new-side and the old-side lines locally.
func ChangeViewFromDiff(repoRoot, diffText string) *ChangeView {
	view := &ChangeView{RepoRoot: repoRoot, ByFile: map[string]*FileChange{}}
	applyNewSide(view, diffText)
	applyOldSide(view, diffText)
	return view
}

func (v *ChangeView) fileChange(file string) *FileChange {
	file = cleanPath(file)
	fc := v.ByFile[file]
	if fc == nil {
		fc = &FileChange{Path: file}
		v.ByFile[file] = fc
	}
	return fc
}

// LocateSnippet resolves a free-text snippet to an exact (file,line) using the
// deterministic tiers, stopping at the first that matches:
//
//  1. new-side hunk lines (reuses analysis.LineGrounder scoring)
//  2. removed old-side lines
//  3. the full post-change file on disk
//
// A miss returns Anchor{Method: AnchorUnresolved, Line: 0}. LocateSnippetWithLLM
// wraps this with the optional tier-4 LLM re-location fallback.
func LocateSnippet(view *ChangeView, file, snippet string) Anchor {
	if view == nil {
		return unresolved(file)
	}
	file = cleanPath(file)
	fc := view.ByFile[file]
	if fc == nil {
		return unresolved(file)
	}

	// Tier 1: new-side hunk lines, scored by the landed grounder.
	if hit, ok := scoreNewSide(fc, file, snippet); ok {
		return Anchor{File: file, Line: hit.line, Confidence: hit.conf, Method: AnchorExactHunk, Side: sideRight}
	}

	// Tier 2: removed old-side lines.
	if line, conf, ok := matchDiffLines(fc.OldLines, snippet); ok {
		return Anchor{File: file, Line: line, Confidence: conf, Method: AnchorOldSide, Side: sideLeft}
	}

	// Tier 3: the full post-change file on disk.
	if body, ok := view.readFile(file); ok {
		if line, conf, ok := matchFullFile(string(body), snippet); ok {
			return Anchor{File: file, Line: line, Confidence: conf, Method: AnchorFullFile, Side: sideRight}
		}
	}

	return unresolved(file)
}

// LocateSnippetWithLLM runs the deterministic LocateSnippet tiers and, on a
// miss, falls back to the optional LLM re-location seam (tier 4). A nil gen
// (disabled seam) simply yields the unresolved anchor — never an error.
func LocateSnippetWithLLM(ctx context.Context, gen LLMGen, view *ChangeView, file, snippet string) Anchor {
	a := LocateSnippet(view, file, snippet)
	if a.Method != AnchorUnresolved {
		return a
	}
	return ReLocateWithLLM(ctx, gen, view, file, snippet)
}

// ReLocateWithLLM is the tier-4 fallback: it asks the LLM to map a snippet to a
// new-side line number among the file's candidate lines. The seam is optional —
// a nil gen, an empty file, an LLM error, or an out-of-range answer all yield an
// unresolved anchor rather than an error.
func ReLocateWithLLM(ctx context.Context, gen LLMGen, view *ChangeView, file, snippet string) Anchor {
	if gen == nil || view == nil {
		return unresolved(file)
	}
	file = cleanPath(file)
	fc := view.ByFile[file]
	if fc == nil || len(fc.Lines) == 0 {
		return unresolved(file)
	}

	prompt := relocatePrompt(file, snippet, fc.Lines)
	out, err := gen(ctx, prompt, 16)
	if err != nil {
		return unresolved(file)
	}
	n, ok := parseLineAnswer(out)
	if !ok {
		return unresolved(file)
	}
	// Validate the answer against the candidate new-side lines.
	for _, dl := range fc.Lines {
		if dl.NewLine == n {
			return Anchor{File: file, Line: n, Confidence: 0.4, Method: AnchorLLM, Side: sideRight}
		}
	}
	return unresolved(file)
}

// LLMGen is the optional LLM re-location seam: a closure over Service.Generate
// (or any (ctx,prompt,maxTokens)->string). Keeping it a func leaves
// internal/review free of an llm/svc import and lets tests stub it. A nil seam
// means the LLM tier is disabled and yields AnchorUnresolved rather than erroring.
type LLMGen func(ctx context.Context, prompt string, maxTokens int) (string, error)

func unresolved(file string) Anchor {
	return Anchor{File: cleanPath(file), Line: 0, Confidence: 0, Method: AnchorUnresolved}
}

// Located reports whether the anchor resolved to a real line.
func (a Anchor) Located() bool { return a.Method != AnchorUnresolved && a.Line > 0 }

// Apply writes the resolved location onto a Finding: File, single-line
// Line/StartLine/EndLine, Side, and the tier in Anchor. The Confidence is set
// only when the anchor improves on a finding's existing confidence so an
// upstream rule confidence is not clobbered by a weaker anchor score.
func (a Anchor) Apply(f *Finding) {
	if f == nil {
		return
	}
	f.File = a.File
	f.Anchor = a.Method
	if !a.Located() {
		return
	}
	f.Line = a.Line
	f.StartLine = a.Line
	f.EndLine = a.Line
	f.Side = a.Side
	if f.Confidence == 0 {
		f.Confidence = a.Confidence
	}
}

// LocateFinding resolves a finding's snippet (from its Message/Body via the
// caller) onto an exact location and writes it back. It is the convenience entry
// the review flow uses: deterministic tiers plus the optional LLM fallback.
func LocateFinding(ctx context.Context, gen LLMGen, view *ChangeView, f *Finding, snippet string) Anchor {
	if f == nil {
		return unresolved("")
	}
	a := LocateSnippetWithLLM(ctx, gen, view, f.File, snippet)
	a.Apply(f)
	return a
}

// --- tier-1 grounder reuse ---

type lineHit struct {
	line int
	conf float64
}

// scoreNewSide resolves a snippet against the file's new-side lines using the
// landed analysis.LineGrounder scoring (exact / whitespace-normalised /
// token-overlap). ok is false when the file has no new-side lines or nothing
// matches. An empty snippet anchors to the first added line (grounder default).
func scoreNewSide(fc *FileChange, file, snippet string) (lineHit, bool) {
	if fc == nil || len(fc.Lines) == 0 {
		return lineHit{}, false
	}
	hunkLines := make([]analysis.HunkLine, 0, len(fc.Lines))
	for _, dl := range fc.Lines {
		side := " "
		if dl.Added {
			side = "+"
		}
		hunkLines = append(hunkLines, analysis.HunkLine{NewLine: dl.NewLine, Side: side, Text: dl.Text})
	}
	// nil graph: GroundFinding with an empty symbol id scans every file line and
	// never needs a symbol-span lookup.
	grounder := analysis.NewLineGrounder(nil, map[string][]analysis.HunkLine{file: hunkLines})
	hit, ok := grounder.GroundFinding(file, "", snippet)
	if !ok {
		return lineHit{}, false
	}
	return lineHit{line: hit.Line, conf: hit.Confidence}, true
}

// --- old-side / full-file deterministic matching ---

// matchDiffLines does exact then whitespace-normalised substring matching over a
// set of diff lines, returning the matched line number and a confidence.
func matchDiffLines(lines []DiffLine, snippet string) (int, float64, bool) {
	snip := strings.TrimSpace(snippet)
	if snip == "" || len(lines) == 0 {
		return 0, 0, false
	}
	for _, dl := range lines {
		if strings.Contains(dl.Text, snip) {
			return dl.NewLine, 1.0, true
		}
	}
	normSnip := normalizeWS(snip)
	if normSnip != "" {
		for _, dl := range lines {
			if strings.Contains(normalizeWS(dl.Text), normSnip) {
				return dl.NewLine, 0.7, true
			}
		}
	}
	return 0, 0, false
}

// matchFullFile does exact then whitespace-normalised substring matching over
// the post-change file, returning a 1-based line number.
func matchFullFile(body, snippet string) (int, float64, bool) {
	snip := strings.TrimSpace(snippet)
	if snip == "" {
		return 0, 0, false
	}
	fileLines := strings.Split(body, "\n")
	for i, l := range fileLines {
		if strings.Contains(l, snip) {
			return i + 1, 1.0, true
		}
	}
	normSnip := normalizeWS(snip)
	if normSnip != "" {
		for i, l := range fileLines {
			if strings.Contains(normalizeWS(l), normSnip) {
				return i + 1, 0.7, true
			}
		}
	}
	return 0, 0, false
}

func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// --- LLM prompt + answer parsing ---

func relocatePrompt(file, snippet string, lines []DiffLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Map the snippet to the single best matching line number in %s.\n", file)
	fmt.Fprintf(&b, "Snippet:\n%s\n\nCandidate lines (number: text):\n", snippet)
	for _, dl := range lines {
		fmt.Fprintf(&b, "%d: %s\n", dl.NewLine, dl.Text)
	}
	b.WriteString("\nRespond with only the integer line number.")
	return b.String()
}

// parseLineAnswer extracts the first integer from a model reply.
func parseLineAnswer(out string) (int, bool) {
	out = strings.TrimSpace(out)
	if out == "" {
		return 0, false
	}
	var digits strings.Builder
	started := false
	for _, r := range out {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
			started = true
			continue
		}
		if started {
			break
		}
	}
	if digits.Len() == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(digits.String())
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// cleanPath normalises a path the same way the diff parser keys files, so a
// caller-supplied path joins the ChangeView map.
func cleanPath(p string) string {
	p = strings.TrimPrefix(p, "./")
	return filepath.Clean(p)
}

// --- diff parsing (local, covers both sides) ---

// applyNewSide parses added + context lines (new-side) from unified-diff text.
func applyNewSide(view *ChangeView, diffText string) {
	var file string
	var newLine int
	sc := bufio.NewScanner(strings.NewReader(diffText))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if f, ok := parseFileHeaderNew(line); ok {
			file, newLine = f, 0
			continue
		}
		if isDiffHeader(line) {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			if file == "" {
				continue
			}
			if start, ok := parseNewStart(line); ok {
				newLine = start
			}
			continue
		}
		if file == "" || newLine == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			view.fileChange(file).Lines = append(view.fileChange(file).Lines, DiffLine{NewLine: newLine, Text: line[1:], Added: true})
			newLine++
		case strings.HasPrefix(line, "-"):
			// removed — handled by applyOldSide
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file"
		default:
			text := line
			if strings.HasPrefix(line, " ") {
				text = line[1:]
			}
			view.fileChange(file).Lines = append(view.fileChange(file).Lines, DiffLine{NewLine: newLine, Text: text, Added: false})
			newLine++
		}
	}
}

// applyOldSide parses removed lines (old-side) from unified-diff text, tagging
// each with its position in the pre-change file.
func applyOldSide(view *ChangeView, diffText string) {
	var file string
	var oldLine int
	sc := bufio.NewScanner(strings.NewReader(diffText))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if f, ok := parseFileHeaderNew(line); ok {
			file, oldLine = f, 0
			continue
		}
		if isDiffHeader(line) {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			if file == "" {
				continue
			}
			if start, ok := parseOldStart(line); ok {
				oldLine = start
			}
			continue
		}
		if file == "" || oldLine == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(line, "-"):
			view.fileChange(file).OldLines = append(view.fileChange(file).OldLines, DiffLine{NewLine: oldLine, Text: line[1:], Added: false})
			oldLine++
		case strings.HasPrefix(line, "+"):
			// added — no old-side position
		case strings.HasPrefix(line, "\\"):
		default:
			// context advances the old-side counter too
			oldLine++
		}
	}
}

func parseFileHeaderNew(line string) (string, bool) {
	if strings.HasPrefix(line, "+++ b/") {
		return cleanPath(strings.TrimPrefix(line, "+++ b/")), true
	}
	if strings.HasPrefix(line, "+++ /dev/null") {
		return "", true
	}
	return "", false
}

func isDiffHeader(line string) bool {
	return strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "diff ") ||
		strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "new file") ||
		strings.HasPrefix(line, "deleted file") || strings.HasPrefix(line, "rename ") ||
		strings.HasPrefix(line, "similarity ") || strings.HasPrefix(line, "old mode") ||
		strings.HasPrefix(line, "new mode") || strings.HasPrefix(line, "copy ")
}

func parseNewStart(line string) (int, bool) { return parseHunkStart(line, "+") }
func parseOldStart(line string) (int, bool) { return parseHunkStart(line, "-") }

// parseHunkStart extracts the starting line for the requested side ("+"/"-")
// from a "@@ -a,b +c,d @@" header.
func parseHunkStart(line, sidePrefix string) (int, bool) {
	parts := strings.SplitN(line, "@@", 3)
	if len(parts) < 2 {
		return 0, false
	}
	for _, f := range strings.Fields(strings.TrimSpace(parts[1])) {
		if !strings.HasPrefix(f, sidePrefix) {
			continue
		}
		f = strings.TrimPrefix(f, sidePrefix)
		rangeP := strings.SplitN(f, ",", 2)
		start, err := strconv.Atoi(rangeP[0])
		if err != nil {
			return 0, false
		}
		return start, true
	}
	return 0, false
}

// rawDiff runs the same context-bearing diff MapGitDiffWithLines uses so the
// old-side lines can be recovered. Routed through gitcmd (no ad-hoc exec).
// analysis.GitDiffArgs pins the a/ b/ header prefixes the parsers anchor on.
func rawDiff(repoRoot, scope, baseRef string) (string, error) {
	return gitcmd.Output(context.Background(), repoRoot, analysis.GitDiffArgs(scope, baseRef, 3)...)
}

// readFile returns the post-change file content from disk under RepoRoot.
func (v *ChangeView) readFile(file string) ([]byte, bool) {
	if v.RepoRoot == "" {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(v.RepoRoot, file))
	if err != nil {
		return nil, false
	}
	return b, true
}
