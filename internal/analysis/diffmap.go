package analysis

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
)

// DiffHunk represents a changed range in a file.
//
// StartLine/EndLine are new-side (post-change) line numbers, except when
// Deleted is set: a deleted file has no new side at all, so its range is the
// old-side span the file occupied before it was removed. That is the span
// that joins the still-indexed pre-delete symbols.
type DiffHunk struct {
	FilePath  string `json:"file_path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Deleted   bool   `json:"deleted,omitempty"`
}

// FileChangeKind classifies one file entry in a diff.
type FileChangeKind string

const (
	FileAdded    FileChangeKind = "added"
	FileModified FileChangeKind = "modified"
	FileDeleted  FileChangeKind = "deleted"
	FileRenamed  FileChangeKind = "renamed"
)

// FileChange is one file-level entry in a diff, carried independently of the
// hunks. Two common change kinds carry no usable hunk at all — a deleted file
// (new side is /dev/null) and a 100%-similar rename (no @@ header is emitted) —
// so a hunk-only view of a diff reports them as no change whatsoever.
// Path is the file's post-change path, or its pre-change path when the change
// removed it; PreviousPath is set only for a rename.
type FileChange struct {
	Path         string         `json:"path"`
	PreviousPath string         `json:"previous_path,omitempty"`
	Kind         FileChangeKind `json:"change_kind"`
}

// ChangedSymbol is a symbol affected by a git diff hunk.
type ChangedSymbol struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
	Line     int    `json:"start_line"`
}

// DiffResult is the output of git diff → symbol mapping.
//
// FileChanges is the file-granular view and is authoritative for *what* the
// diff touched: ChangedSymbols is empty for any file the graph does not index
// (docs, manifests, fixtures) and for deletes/renames the graph still indexes
// under the old path, so an empty ChangedSymbols never means "nothing changed".
type DiffResult struct {
	Hunks          []DiffHunk      `json:"hunks"`
	ChangedSymbols []ChangedSymbol `json:"changed_symbols"`
	ChangedFiles   []string        `json:"changed_files"`
	FileChanges    []FileChange    `json:"file_changes"`
}

// MapGitDiff parses git diff output and maps changed lines to symbols in the graph.
// scope: "unstaged", "staged", "all", "compare"
// baseRef: used when scope is "compare" (e.g., "main")
// repoRoot: absolute path to the repository root
// repoPrefix: the graph repo prefix anchoring repoRoot's indexed nodes.
// The daemon keys every file path as "<prefix>/<rel>" while git emits
// repo-relative paths; empty only for the standalone Indexer, which
// mints unprefixed paths.
func MapGitDiff(g graph.Store, repoRoot, repoPrefix, scope, baseRef string) (*DiffResult, error) {
	if err := gitcmd.ValidateRef(baseRef); err != nil {
		return nil, err
	}
	args := buildDiffArgs(scope, baseRef)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// gitcmd runs `git -C repoRoot args...`; use Run (raw stdout, no trailing
	// trim) so the parsed diff stays byte-identical to the pre-gitcmd output.
	output, err := gitcmd.Run(ctx, repoRoot, args...)
	if err != nil {
		// A failed `git diff` is not an empty diff. Swallowing the error
		// whenever stdout happened to be empty is how an unknown base ref, a
		// repo root that is not a work tree, or a git that never ran at all
		// became a confident "no changes" answer.
		return nil, fmt.Errorf("git diff failed: %w", err)
	}

	hunks, files := parseDiffFiles(string(output))
	return joinHunksToSymbols(g, repoPrefix, hunks, files), nil
}

// PathDomain names the vocabulary a file path is spelled in. The two overlap,
// so the domain has to travel with the path rather than be recovered from it:
// in a repo whose tree carries a top-level directory named like the repo
// prefix, "repo-a/pkg/widget.go" is a well-formed path in BOTH domains — a
// git-relative path naming the nested file, and the graph key of the entirely
// different top-level "pkg/widget.go".
type PathDomain int

const (
	// RepoRelativePath is git's and a forge's spelling, relative to the work
	// tree. Every changed-file list in this package is this domain.
	RepoRelativePath PathDomain = iota
	// GraphKeyedPath is the graph's own node key: "<prefix>/<rel>" in
	// multi-repo mode, the bare relative path when no prefix applies.
	GraphKeyedPath
)

// GraphKey renders path as the graph keys it. A repo-relative path is prefixed
// unconditionally — never conditionally on how it happens to be spelled — so a
// path that merely looks prefixed cannot be resolved against the wrong file.
func GraphKey(repoPrefix, path string, domain PathDomain) string {
	if domain == GraphKeyedPath {
		return path
	}
	// A repo-relative path is always converted, prefix or not. The key's shape
	// is "<prefix>/" + the remainder in the indexing machine's native
	// separators (see internal/graphpath); with no prefix the remainder IS the
	// key, and a '/'-spelled git or forge path still misses a key stored with
	// native separators on Windows. FromSlash is the identity on POSIX.
	key := filepath.FromSlash(path)
	if repoPrefix == "" {
		return key
	}
	return repoPrefix + "/" + key
}

// RepoRelPath is GraphKey's inverse: the repo-relative '/' spelling that
// CODEOWNERS rules, forge comment APIs and report rows speak. Stripping the
// prefix is safe here and only here, because a GraphKeyedPath carries it by
// construction — the same strip applied to a repo-relative path is the
// prefix-shadow bug.
func RepoRelPath(repoPrefix, path string, domain PathDomain) string {
	rel := filepath.ToSlash(path)
	if domain == GraphKeyedPath && repoPrefix != "" {
		rel = strings.TrimPrefix(rel, repoPrefix+"/")
	}
	return rel
}

// JoinFileNodes maps one changed-file path to the graph nodes its file
// defines. domain states which vocabulary path is in; see PathDomain for why
// it cannot be inferred.
//
// The previous contract tried the raw key first and the prefixed key second,
// which resolved a legitimate git-relative "<prefix>/<rel>" against the
// same-named top-level file instead — and returned nil when that shadow did
// not exist, never trying the real key.
func JoinFileNodes(g graph.Store, repoPrefix, path string, domain PathDomain) []*graph.Node {
	return g.GetFileNodes(GraphKey(repoPrefix, path, domain))
}

// joinHunksToSymbols builds the hunk→symbol/file join shared by MapGitDiff
// and MapGitDiffWithLines: every symbol whose line range overlaps a changed
// hunk in its file, deduped, plus the changed-file set. ChangedFiles keeps
// the diff-relative paths (callers re-join them with git pathspecs); only
// the node lookup is prefix-aware.
func joinHunksToSymbols(g graph.Store, repoPrefix string, hunks []DiffHunk, files []FileChange) *DiffResult {
	result := &DiffResult{Hunks: hunks, FileChanges: files}

	fileSet := make(map[string]bool)
	symbolSeen := make(map[string]bool)

	addSymbol := func(n *graph.Node) {
		if n.Kind == graph.KindFile || symbolSeen[n.ID] {
			return
		}
		symbolSeen[n.ID] = true
		result.ChangedSymbols = append(result.ChangedSymbols, ChangedSymbol{
			ID:       n.ID,
			Name:     n.Name,
			Kind:     string(n.Kind),
			FilePath: n.FilePath,
			Line:     n.StartLine,
		})
	}

	for _, hunk := range hunks {
		fileSet[hunk.FilePath] = true

		// Find symbols whose line range overlaps the hunk
		for _, n := range JoinFileNodes(g, repoPrefix, hunk.FilePath, RepoRelativePath) {
			// Check if symbol's line range overlaps with the hunk
			if n.StartLine <= hunk.EndLine && n.EndLine >= hunk.StartLine {
				addSymbol(n)
			}
		}
	}

	// A file record reaches ChangedFiles even when it carried no hunk, which
	// is how a delete of an empty file or a 100%-similar rename stays visible.
	// Removing or moving a file affects every symbol the graph still indexes
	// under its old path, and no line range in the diff describes that — so
	// those symbols are taken whole rather than by overlap.
	for _, fc := range files {
		if fc.Path != "" {
			fileSet[fc.Path] = true
		}
		vanished := fc.PreviousPath
		if fc.Kind == FileDeleted {
			vanished = fc.Path
		}
		if vanished == "" {
			continue
		}
		fileSet[vanished] = true
		for _, n := range JoinFileNodes(g, repoPrefix, vanished, RepoRelativePath) {
			addSymbol(n)
		}
	}

	for f := range fileSet {
		result.ChangedFiles = append(result.ChangedFiles, f)
	}
	sort.Strings(result.ChangedFiles)

	return result
}

// GitDiffArgs builds the `git diff` argv for a scope ("unstaged", "staged",
// "all", "compare") with the given context width. The -c overrides pin the
// diff header prefixes to git's standard a/ b/ form: parseDiffHunks and
// parseDiffLines anchor on "+++ b/", which a developer's diff.mnemonicPrefix
// (c/ w/ headers on worktree-side diffs) or diff.noprefix config would
// otherwise silently zero out — every hunk drops, every diff-driven tool
// reports an empty changeset.
func GitDiffArgs(scope, baseRef string, unified int) []string {
	args := []string{
		"-c", "diff.mnemonicPrefix=false",
		"-c", "diff.noprefix=false",
		"diff",
	}
	switch scope {
	case "staged":
		args = append(args, "--cached")
	case "all":
		args = append(args, "HEAD")
	case "compare":
		// A hostile base degrades to the documented default rather than
		// reaching argv: the token is baseRef+"...HEAD", so a value
		// starting with "-" is still an option to git, and GitDiffArgs
		// has no error return to surface it through. MapGitDiff rejects
		// it loudly before calling here; this is the backstop for the
		// other callers that build args directly.
		baseRef = gitcmd.SafeRef(baseRef)
		if baseRef == "" {
			baseRef = "main"
		}
		args = append(args, baseRef+"...HEAD")
	default: // unstaged — bare `git diff`
	}
	return append(args, fmt.Sprintf("--unified=%d", unified))
}

func buildDiffArgs(scope, baseRef string) []string {
	return GitDiffArgs(scope, baseRef, 0)
}

// buildDiffArgsWithContext mirrors buildDiffArgs but emits a context window so
// the new-side line text survives into the hunk body for snippet grounding.
func buildDiffArgsWithContext(scope, baseRef string) []string {
	return GitDiffArgs(scope, baseRef, 3)
}

// HunkLine is a single new-side line carried out of a unified diff: added lines
// (Side "+") and context lines (Side " "), each tagged with its line number in
// the post-change file. Removed lines never appear (they have no new-side line).
type HunkLine struct {
	NewLine int    `json:"new_line"`
	Side    string `json:"side"`
	Text    string `json:"text"`
}

// ParseDiffHunks parses unified git-diff output into per-file changed ranges.
// It is the exported entry point over the same parser MapGitDiff uses and
// returns results identical to the internal parser.
func ParseDiffHunks(output string) []DiffHunk {
	return parseDiffHunks(output)
}

// parseDiffLines walks unified diff output and returns, per new-side file path,
// the added and context lines with their post-change line numbers. Removed
// lines are skipped; the new-side line counter only advances on added/context.
// Keys match DiffHunk.FilePath (cleaned, relative) so a hunk and its lines join.
func parseDiffLines(output string) map[string][]HunkLine {
	lines := make(map[string][]HunkLine)
	var currentFile string
	var newLine int

	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "+++ b/") {
			currentFile = filepath.Clean(strings.TrimPrefix(line, "+++ b/"))
			newLine = 0
			continue
		}
		if strings.HasPrefix(line, "+++ /dev/null") {
			currentFile = ""
			newLine = 0
			continue
		}
		// Skip the remaining diff-header lines so their leading +/-/space never
		// leaks into the hunk body.
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "diff ") ||
			strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "new file") ||
			strings.HasPrefix(line, "deleted file") || strings.HasPrefix(line, "rename ") ||
			strings.HasPrefix(line, "similarity ") || strings.HasPrefix(line, "old mode") ||
			strings.HasPrefix(line, "new mode") || strings.HasPrefix(line, "copy ") {
			continue
		}

		if strings.HasPrefix(line, "@@") {
			if currentFile == "" {
				continue
			}
			start, ok := parseNewStart(line)
			if !ok {
				continue
			}
			newLine = start
			continue
		}

		if currentFile == "" || newLine == 0 {
			continue
		}

		switch {
		case strings.HasPrefix(line, "+"):
			lines[currentFile] = append(lines[currentFile], HunkLine{
				NewLine: newLine,
				Side:    "+",
				Text:    line[1:],
			})
			newLine++
		case strings.HasPrefix(line, "-"):
			// Removed line — no new-side position, do not advance.
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" marker — not a content line.
		default:
			// Context line (leading space), or a bare blank context line.
			text := line
			if strings.HasPrefix(line, " ") {
				text = line[1:]
			}
			lines[currentFile] = append(lines[currentFile], HunkLine{
				NewLine: newLine,
				Side:    " ",
				Text:    text,
			})
			newLine++
		}
	}

	return lines
}

// parseNewStart extracts the new-side starting line from a "@@ -a,b +c,d @@"
// hunk header.
func parseNewStart(line string) (int, bool) {
	parts := strings.SplitN(line, "@@", 3)
	if len(parts) < 2 {
		return 0, false
	}
	for _, f := range strings.Fields(strings.TrimSpace(parts[1])) {
		if !strings.HasPrefix(f, "+") {
			continue
		}
		f = strings.TrimPrefix(f, "+")
		rangeP := strings.SplitN(f, ",", 2)
		start, err := strconv.Atoi(rangeP[0])
		if err != nil {
			return 0, false
		}
		return start, true
	}
	return 0, false
}

// MapGitDiffWithLines mirrors MapGitDiff but uses a context-bearing diff so it
// can additionally return, per file, the new-side lines (added + context) with
// their post-change line numbers — the substrate snippet grounding anchors on.
// The returned *DiffResult is computed with the same logic as MapGitDiff (only
// the diff's context width differs), so symbol overlap is unaffected.
// repoPrefix anchors the node join exactly as in MapGitDiff.
func MapGitDiffWithLines(g graph.Store, repoRoot, repoPrefix, scope, baseRef string) (*DiffResult, map[string][]HunkLine, error) {
	if err := gitcmd.ValidateRef(baseRef); err != nil {
		return nil, nil, err
	}
	args := buildDiffArgsWithContext(scope, baseRef)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Run (raw stdout) keeps the trailing newline so the final hunk line is
	// never dropped; gitcmd injects `-C repoRoot` for us.
	output, err := gitcmd.Run(ctx, repoRoot, args...)
	if err != nil {
		// See MapGitDiff: an error here means the diff was never observed,
		// which is not the same answer as "the diff was empty".
		return nil, nil, fmt.Errorf("git diff failed: %w", err)
	}

	text := string(output)
	hunks, files := parseDiffFiles(text)
	lines := parseDiffLines(text)

	return joinHunksToSymbols(g, repoPrefix, hunks, files), lines, nil
}

// parseDiffHunks returns only the changed line ranges. Callers that need to
// know a file changed at all — including the deletes and renames that carry no
// hunk — want parseDiffFiles instead.
func parseDiffHunks(output string) []DiffHunk {
	hunks, _ := parseDiffFiles(output)
	return hunks
}

// fileDiffState accumulates the header facts parseDiffFiles has seen for the
// diff entry it is currently inside.
type fileDiffState struct {
	oldPath string
	newPath string
	added   bool
	deleted bool
	renamed bool
}

// change projects the accumulated header facts onto a FileChange. Rename is
// checked first: a rename carries both an old and a new path, and describing
// it as a delete of one plus an add of the other loses the link between them.
func (s fileDiffState) change() FileChange {
	switch {
	case s.renamed:
		return FileChange{Path: s.newPath, PreviousPath: s.oldPath, Kind: FileRenamed}
	case s.deleted:
		return FileChange{Path: s.oldPath, Kind: FileDeleted}
	case s.added:
		return FileChange{Path: s.newPath, Kind: FileAdded}
	default:
		return FileChange{Path: s.newPath, Kind: FileModified}
	}
}

// parseDiffFiles walks unified diff output and returns the changed line ranges
// together with a file-level record for every entry in the diff — including
// the entries that carry no hunk at all.
//
// Anchoring only on "+++ b/" and "@@" — the pre-fix parser — silently drops
// two change kinds that are squarely inside every scope this package serves:
//
//   - a deleted file, whose new side is "+++ /dev/null", so clearing the
//     current path there also skips the "@@" that follows it;
//   - a 100%-similar rename, which git reports with "rename from"/"rename to"
//     headers and no "@@" header whatsoever.
//
// Both produced zero hunks, so the file never reached ChangedFiles and its
// symbols never reached ChangedSymbols: a delete-only or rename-only change
// read as an empty diff. Deletes are therefore parsed off the old side and
// renames off their rename headers.
func parseDiffFiles(output string) ([]DiffHunk, []FileChange) {
	var (
		hunks   []DiffHunk
		changes []FileChange
		cur     fileDiffState
		inEntry bool
	)

	flush := func() {
		if !inEntry {
			return
		}
		if change := cur.change(); change.Path != "" {
			changes = append(changes, change)
		}
	}

	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = fileDiffState{}
			inEntry = true
			// Only a same-path header parses unambiguously (see
			// parseDiffGitPaths). Every other entry shape carries an
			// explicit ---/+++ or rename header below, which overrides this.
			cur.oldPath = parseDiffGitPaths(line)
			cur.newPath = cur.oldPath
			continue
		case strings.HasPrefix(line, "deleted file mode "):
			cur.deleted = true
			continue
		case strings.HasPrefix(line, "new file mode "):
			cur.added = true
			continue
		case strings.HasPrefix(line, "rename from "):
			cur.renamed = true
			cur.oldPath = cleanDiffPath(strings.TrimPrefix(line, "rename from "))
			continue
		case strings.HasPrefix(line, "rename to "):
			cur.renamed = true
			cur.newPath = cleanDiffPath(strings.TrimPrefix(line, "rename to "))
			continue
		case strings.HasPrefix(line, "--- a/"):
			cur.oldPath = cleanDiffPath(strings.TrimPrefix(line, "--- a/"))
			continue
		case strings.HasPrefix(line, "--- /dev/null"):
			cur.added = true
			continue
		case strings.HasPrefix(line, "+++ b/"):
			cur.newPath = cleanDiffPath(strings.TrimPrefix(line, "+++ b/"))
			continue
		case strings.HasPrefix(line, "+++ /dev/null"):
			cur.deleted = true
			continue
		case !strings.HasPrefix(line, "@@"):
			continue
		}

		// A deleted file has no new side, so its only line numbers are
		// old-side — and they span exactly the lines the file used to hold,
		// which is what joins the symbols the graph still indexes for it.
		if cur.deleted && !cur.renamed {
			if hunk := parseHunkHeader(line, cur.oldPath, "-"); hunk != nil {
				hunk.Deleted = true
				hunks = append(hunks, *hunk)
			}
			continue
		}
		if hunk := parseHunkHeader(line, cur.newPath, "+"); hunk != nil {
			hunks = append(hunks, *hunk)
		}
	}
	flush()

	return hunks, changes
}

// cleanDiffPath normalizes a path lifted out of a diff header the same way
// DiffHunk.FilePath and parseDiffLines do, so a hunk and its file record join.
func cleanDiffPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

// parseDiffGitPaths recovers the path from a "diff --git a/P b/P" header. It is
// the only path source for an entry that carries no ---/+++ and no rename
// header — a mode-only change (chmod) is the common case.
//
// The header is ambiguous in general: git separates the two paths with a bare
// space, which a path may itself contain. When both sides name the same file
// the line is exactly "a/P b/P", so P is recoverable by length regardless of
// its spaces. When they differ, the entry is a rename or a copy and carries
// explicit rename headers, so this returns "" and lets those win.
func parseDiffGitPaths(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	// Git quotes paths containing control or non-ASCII bytes; decoding that
	// quoting here would be guesswork, and every quoted entry that matters
	// also carries an explicit header below.
	if !strings.HasPrefix(rest, "a/") || strings.Contains(rest, `"`) {
		return ""
	}
	// len("a/") + len(P) + len(" ") + len("b/") + len(P)
	pathLen := (len(rest) - 5) / 2
	if pathLen <= 0 || len(rest) != 2*pathLen+5 {
		return ""
	}
	old, next := rest[2:2+pathLen], rest[2+pathLen:]
	if !strings.HasPrefix(next, " b/") || next[3:] != old {
		return ""
	}
	return cleanDiffPath(old)
}

// parseHunkHeader reads one "@@ -old,count +new,count @@" header and returns
// the range on the requested side ("+" for the new side, "-" for the old side
// of a deleted file), attributed to filePath.
func parseHunkHeader(line, filePath, side string) *DiffHunk {
	if filePath == "" {
		return nil
	}
	// Format: @@ -old,count +new,count @@
	parts := strings.SplitN(line, "@@", 3)
	if len(parts) < 2 {
		return nil
	}
	ranges := strings.TrimSpace(parts[1])
	fields := strings.Fields(ranges)

	for _, f := range fields {
		if strings.HasPrefix(f, side) {
			f = strings.TrimPrefix(f, side)
			rangeP := strings.SplitN(f, ",", 2)
			start, err := strconv.Atoi(rangeP[0])
			if err != nil {
				continue
			}
			count := 1
			if len(rangeP) > 1 {
				count, _ = strconv.Atoi(rangeP[1])
			}
			if count == 0 {
				count = 1
			}

			// Normalize file path to be relative
			relPath := filepath.Clean(filePath)

			return &DiffHunk{
				FilePath:  relPath,
				StartLine: start,
				EndLine:   start + count - 1,
			}
		}
	}
	return nil
}
