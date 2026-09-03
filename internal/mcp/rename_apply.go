package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/indexer"
)

// renameEdit is one planned single-line replacement in a coordinated rename.
// It is the unit both the preview and the on-disk apply operate on, so what a
// caller is shown is exactly what gets written.
type renameEdit struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	Confidence string `json:"confidence"`
	Reason     string `json:"reason"`
}

// unindexedRenameRecovery distinguishes an invalid symbol ID from an existing
// file that semantic rename cannot see. When the configured language extractor
// identifies exactly one requested declaration, it returns a guarded exact-line
// edit for that declaration only. References remain outside the recovery scope.
func (s *Server) unindexedRenameRecovery(ctx context.Context, id, newName string, dryRun bool) map[string]any {
	filePart, requestedSymbol, ok := strings.Cut(id, "::")
	if !ok || strings.TrimSpace(filePart) == "" || strings.TrimSpace(requestedSymbol) == "" {
		return nil
	}

	absPath, relPath, err := s.resolveFilePath(ctx, filePart)
	if err != nil {
		return nil
	}
	info, err := os.Lstat(absPath)
	if err != nil || !info.Mode().IsRegular() {
		// Refuse symlink leaves: an atomic edit of the link path would replace
		// the link object while leaving its target unchanged.
		return nil
	}

	file := s.graphPathSpelling(relPath)
	owner, relPath := s.indexerForRel(file)
	if owner == nil || relPath == "" {
		return nil
	}
	if indexed := s.engineFor(ctx).GetFileSymbols(file); indexed != nil && indexed.TotalNodes > 0 {
		return nil
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	extracted, err := owner.ExtractSource(ctx, relPath, content)
	if err != nil || extracted == nil {
		return nil
	}
	defer extracted.ReleaseTree()

	var declarationName string
	var declarationLine int
	for _, node := range extracted.Nodes {
		_, suffix, hasSuffix := strings.Cut(node.ID, "::")
		if !hasSuffix || suffix != requestedSymbol {
			continue
		}
		if declarationName != "" {
			return nil
		}
		declarationName = node.Name
		declarationLine = node.StartLine
	}
	if declarationName == "" || declarationLine <= 0 {
		return nil
	}

	oldLine, newLine, ok := s.verifiedDeclarationRename(
		ctx, owner, relPath, content, declarationLine, requestedSymbol, declarationName, newName,
	)
	if !ok {
		return nil
	}

	return map[string]any{
		"status":                   "refused",
		"symbol_id":                id,
		"requested_symbol":         requestedSymbol,
		"declaration_name":         declarationName,
		"new_name":                 newName,
		"file":                     file,
		"occurrences":              1,
		"semantic_rename_complete": false,
		"written":                  false,
		"dry_run":                  dryRun,
		"safe_fallback": map[string]any{
			"tool": "edit",
			"request": map[string]any{
				"operation":   "file",
				"target":      map[string]any{"file": file},
				"match":       oldLine,
				"replacement": newLine,
				"guard": map[string]any{
					"expected_occurrences": 1,
					"base_sha":             gitBlobSHA(content),
				},
			},
			"scope": "declaration_only",
			"guidance": []string{
				"Apply this exact contextual edit only after reviewing the declaration line.",
				"Update same-file and cross-file references separately; completeness is not proven.",
			},
		},
		"warning": "Only the parsed declaration is anchored. Same-file and cross-file references are not proven because this symbol is unavailable to the semantic graph. No text was changed.",
	}
}

const (
	maxUnindexedRenameLineBytes  = 16 << 10
	maxUnindexedRenameCandidates = 8
)

// verifiedDeclarationRename tries each whole-identifier occurrence on the
// declaration line and reparses the resulting file. It accepts exactly one
// candidate: the edit must remove the requested symbol ID and create the
// expected renamed ID on the same declaration line. This avoids relying on
// optional extractor columns, which may point at the start of the declaration
// rather than at its name.
func (s *Server) verifiedDeclarationRename(
	ctx context.Context,
	owner *indexer.Indexer,
	relPath string,
	content []byte,
	declarationLine int,
	requestedSymbol, declarationName, newName string,
) (string, string, bool) {
	if ctx.Err() != nil {
		return "", "", false
	}
	lines := splitLinesKeepEnds(string(content))
	if declarationLine > len(lines) {
		return "", "", false
	}
	oldLine := lines[declarationLine-1]
	body, term := splitLineTerminator(oldLine)
	if len(body) > maxUnindexedRenameLineBytes {
		return "", "", false
	}

	if !strings.HasSuffix(requestedSymbol, declarationName) {
		return "", "", false
	}
	expectedSymbol := strings.TrimSuffix(requestedSymbol, declarationName) + newName

	offsets := make([]int, 0, maxUnindexedRenameCandidates)
	for from := 0; ; {
		nameOffset := indexIdentifier(body, declarationName, from)
		if nameOffset < 0 {
			break
		}
		if len(offsets) == maxUnindexedRenameCandidates {
			return "", "", false
		}
		offsets = append(offsets, nameOffset)
		from = nameOffset + 1
	}

	var accepted string
	for _, nameOffset := range offsets {
		if ctx.Err() != nil {
			return "", "", false
		}
		candidateLine := body[:nameOffset] + newName + body[nameOffset+len(declarationName):] + term
		candidateLines := append([]string(nil), lines...)
		candidateLines[declarationLine-1] = candidateLine
		candidateContent := []byte(strings.Join(candidateLines, ""))

		candidate, err := owner.ExtractSource(ctx, relPath, candidateContent)
		if err == nil && candidate != nil {
			originalPresent := false
			expectedAtDeclaration := 0
			for _, node := range candidate.Nodes {
				_, suffix, hasSuffix := strings.Cut(node.ID, "::")
				if !hasSuffix {
					continue
				}
				if suffix == requestedSymbol {
					originalPresent = true
				}
				if suffix == expectedSymbol && node.StartLine == declarationLine {
					expectedAtDeclaration++
				}
			}
			candidate.ReleaseTree()
			if !originalPresent && expectedAtDeclaration == 1 {
				if accepted != "" {
					return "", "", false
				}
				accepted = candidateLine
			}
		}
	}
	if accepted == "" {
		return "", "", false
	}
	return oldLine, accepted, true
}

// indexIdentifier returns the byte offset of the first whole-identifier
// occurrence of name at or after from, or -1 when there is none. A candidate
// is whole only when neither neighbouring rune is an identifier rune, so
// searching for "Get" does not match inside "GetUser" or "doGet".
func indexIdentifier(line, name string, from int) int {
	if name == "" || from > len(line) {
		return -1
	}
	for i := from; i <= len(line)-len(name); {
		idx := strings.Index(line[i:], name)
		if idx < 0 {
			return -1
		}
		start := i + idx
		end := start + len(name)
		if identifierBoundary(line, start, end) {
			return start
		}
		// Rescan from one byte past this candidate's start rather than past
		// its end, so an overlapping whole match is not skipped.
		i = start + 1
	}
	return -1
}

// identifierBoundary reports whether line[start:end] stands alone as an
// identifier rather than sitting inside a longer one. Identifier runes come
// from isIdentRune — letters, digits, and '_'. Sigils such as PHP's '$' are
// deliberately excluded: they prefix a name without being part of it, and
// counting them would make "$foo" unrenamable.
func identifierBoundary(line string, start, end int) bool {
	if start > 0 {
		if r, _ := utf8.DecodeLastRuneInString(line[:start]); isIdentRune(r) {
			return false
		}
	}
	if end < len(line) {
		if r, _ := utf8.DecodeRuneInString(line[end:]); isIdentRune(r) {
			return false
		}
	}
	return true
}

// replaceIdentifierAll rewrites every whole-identifier occurrence of old and
// returns the new line plus the number of replacements. Replacing every
// occurrence (not just the first) matters on lines such as
// "x := Get(Get(y))", and the boundary check keeps "GetUser" intact.
func replaceIdentifierAll(line, old, replacement string) (string, int) {
	if old == "" {
		return line, 0
	}
	var b strings.Builder
	count := 0
	pos := 0
	for {
		start := indexIdentifier(line, old, pos)
		if start < 0 {
			b.WriteString(line[pos:])
			break
		}
		b.WriteString(line[pos:start])
		b.WriteString(replacement)
		count++
		pos = start + len(old)
	}
	if count == 0 {
		return line, 0
	}
	return b.String(), count
}

// splitLineTerminator splits a raw line into its content and trailing
// terminator ("", "\n", or "\r\n").
func splitLineTerminator(raw string) (body, term string) {
	switch {
	case strings.HasSuffix(raw, "\r\n"):
		return raw[:len(raw)-2], "\r\n"
	case strings.HasSuffix(raw, "\n"):
		return raw[:len(raw)-1], "\n"
	default:
		return raw, ""
	}
}

// splitLinesKeepEnds splits content into lines that each retain their own
// terminator, so rewriting one line cannot normalise the rest of the file's
// line endings or append a terminator the file never had.
func splitLinesKeepEnds(content string) []string {
	if content == "" {
		return nil
	}
	var lines []string
	for len(content) > 0 {
		idx := strings.IndexByte(content, '\n')
		if idx < 0 {
			lines = append(lines, content)
			break
		}
		lines = append(lines, content[:idx+1])
		content = content[idx+1:]
	}
	return lines
}

// renameFileWrite is one file's fully-computed rewrite, held in memory until
// every affected file has passed verification.
type renameFileWrite struct {
	RelPath  string
	AbsPath  string
	Old      []byte
	New      []byte
	Edits    int
	NewSHA   string
	Gate     parseGateResult
	GateInfo map[string]any
}

// planRenameWrites resolves every affected file, re-verifies each target line
// against current disk content, and computes the rewritten bytes — without
// writing anything.
//
// Verification and the parse gate run across the whole edit set before the
// caller commits any file, so a rename either lands completely or not at all.
// A half-applied rename is worse than a refused one: it leaves the tree
// uncompilable with no record of how far it got.
func (s *Server) planRenameWrites(ctx context.Context, edits []renameEdit, allowParseErrors bool) ([]*renameFileWrite, error) {
	byFile := make(map[string][]renameEdit)
	for _, e := range edits {
		byFile[e.File] = append(byFile[e.File], e)
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	writes := make([]*renameFileWrite, 0, len(files))
	for _, relPath := range files {
		absPath, err := s.resolveGraphPath(ctx, relPath)
		if err != nil {
			return nil, fmt.Errorf("could not resolve %s: %w", relPath, err)
		}
		content, err := os.ReadFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("could not read %s: %w", relPath, err)
		}
		lines := splitLinesKeepEnds(string(content))

		fileEdits := byFile[relPath]
		sort.Slice(fileEdits, func(i, j int) bool { return fileEdits[i].Line < fileEdits[j].Line })
		for _, e := range fileEdits {
			if e.Line < 1 || e.Line > len(lines) {
				return nil, fmt.Errorf(
					"%s:%d is outside the file (%d lines) — the index is stale relative to disk; re-run after reindexing",
					relPath, e.Line, len(lines))
			}
			body, term := splitLineTerminator(lines[e.Line-1])
			if body != e.OldText {
				// The graph and disk disagree, so the planned replacement was
				// computed against content that is no longer there. Refuse
				// rather than write a line we never actually inspected.
				return nil, fmt.Errorf(
					"%s:%d changed on disk since it was planned — refusing the rename so no unreviewed line is rewritten. Re-run rename after the file settles",
					relPath, e.Line)
			}
			lines[e.Line-1] = e.NewText + term
		}

		newContent := []byte(strings.Join(lines, ""))
		w := &renameFileWrite{
			RelPath: relPath,
			AbsPath: absPath,
			Old:     content,
			New:     newContent,
			Edits:   len(fileEdits),
			NewSHA:  gitBlobSHA(newContent),
		}
		if parseGateEnabled() {
			w.Gate = checkParseGate(relPath, content, newContent)
			if w.Gate.Blocked && !allowParseErrors {
				return nil, fmt.Errorf("%s", parseGateError(relPath, w.Gate))
			}
		}
		w.GateInfo = parseGateInfo(w.Gate, allowParseErrors)
		writes = append(writes, w)
	}
	return writes, nil
}

// commitRenameWrites writes each verified file atomically and reports the
// per-file outcome. Callers must already hold the mutation locks for every
// path in writes.
func (s *Server) commitRenameWrites(ctx context.Context, writes []*renameFileWrite) []map[string]any {
	sess := s.sessionFor(ctx)
	results := make([]map[string]any, 0, len(writes))
	for _, w := range writes {
		entry := map[string]any{
			"file":          w.RelPath,
			"edits":         w.Edits,
			"bytes_written": len(w.New),
			"new_sha":       w.NewSHA,
		}
		if w.GateInfo != nil {
			entry["parse_gate"] = w.GateInfo
		}
		perm := os.FileMode(0o644)
		if info, err := os.Stat(w.AbsPath); err == nil {
			perm = info.Mode().Perm()
		}
		// A multi-file rename stops at the first cancelled write rather than
		// grinding through the rest: every remaining file would be refused
		// anyway, and the receipts already recorded say exactly how far the
		// rename got.
		commit, err := s.commitFileMutation(ctx, "rename_symbol", "", "", w.RelPath, w.AbsPath, w.New, perm)
		if err != nil {
			entry["status"] = "failed"
			entry["error"] = err.Error()
			attachMutationCommit(entry, commit)
			results = append(results, entry)
			if errors.Is(err, errMutationNotApplied) {
				return results
			}
			continue
		}
		entry["status"] = "applied"
		sess.recordModified(w.RelPath)
		outcome := s.mutationReindexState(ctx, w.AbsPath)
		commit.recordGraph(outcome)
		if outcome.Err != nil {
			entry["reindex_error"] = outcome.Err.Error()
		}
		s.attachMutationFreshness(entry, w.RelPath, w.AbsPath, outcome)
		attachMutationCommit(entry, commit)
		results = append(results, entry)
	}
	return results
}

// previewRenameWrites renders the same per-file shape as a committed rename
// with nothing written, so a caller can diff a dry run against the real thing.
func previewRenameWrites(writes []*renameFileWrite) []map[string]any {
	results := make([]map[string]any, 0, len(writes))
	for _, w := range writes {
		entry := map[string]any{
			"file":          w.RelPath,
			"edits":         w.Edits,
			"bytes_written": len(w.New),
			"new_sha":       w.NewSHA,
			"status":        "would_apply",
			"reindexed":     false,
		}
		if w.GateInfo != nil {
			entry["parse_gate"] = w.GateInfo
		}
		results = append(results, entry)
	}
	return results
}
