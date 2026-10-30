package lsp

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// wholeFileEnd returns the (line, character) position of the end of
// the file at absPath, suitable for a full-file LSP Range. Position
// is 0-based and exclusive — line N where the file has N+1 lines, or
// line N character C when the last line has C characters before EOL.
//
// gopls (and other strict servers) reject `Position{Line: 1<<30}` as
// "line number out of range"; this helper returns a real bound so the
// fix-all loop survives strict validators. On stat / read errors we
// fall back to a bounded-but-still-large sentinel — better to ask for
// "the first ~1M lines" than to fail the whole pass on a transient
// read error.
func wholeFileEnd(absPath string) (line, character int) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return 1_000_000, 0
	}
	if len(data) == 0 {
		return 0, 0
	}
	// Trailing newline → last "line" is empty. LSP positions are
	// 0-based and an "End" pointing at the start of an empty line N
	// is the canonical way to span everything before it.
	lineCount := bytes.Count(data, []byte{'\n'})
	lastNL := bytes.LastIndexByte(data, '\n')
	tail := data[lastNL+1:]
	if len(tail) == 0 {
		return lineCount, 0
	}
	return lineCount, len(tail)
}

// FixAllOptions controls FixAllInFile behaviour.
type FixAllOptions struct {
	// AbsPath is the absolute path to the file under repair.
	AbsPath string
	// Kinds restricts which CodeAction kinds are eligible. Empty
	// defaults to {quickfix, source.organizeImports}.
	Kinds []string
	// MaxIterations bounds how many round-trips the loop performs;
	// each iteration applies at most one batch of actions and
	// re-collects diagnostics. Zero defaults to 5.
	MaxIterations int
	// MaxActionsPerIter caps the number of actions applied per
	// iteration. Zero defaults to 50.
	MaxActionsPerIter int
	// DiagnosticTimeout is how long to wait for the server to publish
	// updated diagnostics after each apply. Zero defaults to 5s.
	DiagnosticTimeout time.Duration
}

// FixAllResult summarises a fix-all run.
type FixAllResult struct {
	// Iterations is the number of apply/re-diagnose cycles run.
	Iterations int
	// AppliedActions is the total number of actions written to disk.
	AppliedActions int
	// FilesTouched lists the absolute paths whose contents changed.
	FilesTouched []string
	// FinalDiagnostics is the diagnostics list after the last apply.
	FinalDiagnostics []Diagnostic
}

// FixAllInFile loops codeAction → apply → re-collect-diagnostics until
// no further changes are produced or the iteration cap is reached.
// Modelled on agent-lsp's `/lsp-fix-all` workflow.
//
// The provider must have been initialised (EnsureClient + the file
// opened — FixAllInFile will open the file itself if necessary).
//
// FixAllInFile is conservative: when a CodeAction has no Edit and
// only a Command (legacy) form, the command is *not* executed by
// default — most servers route those through workspace/applyEdit
// which Gortex doesn't follow up on by default. Enable explicit
// command execution by setting FixAllOptions.Kinds to include
// "source.fixAll" or by calling ExecuteCommand directly.
func (p *Provider) FixAllInFile(opts FixAllOptions) (*FixAllResult, error) {
	if p.Client() == nil {
		return nil, errors.New("LSP client not initialized")
	}
	if opts.AbsPath == "" {
		return nil, errors.New("AbsPath is required")
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 5
	}
	if opts.MaxActionsPerIter <= 0 {
		opts.MaxActionsPerIter = 50
	}
	if opts.DiagnosticTimeout <= 0 {
		opts.DiagnosticTimeout = 5 * time.Second
	}
	kinds := opts.Kinds
	if len(kinds) == 0 {
		kinds = []string{
			CodeActionKindQuickFix,
			CodeActionKindSourceOrganizeImports,
		}
	}

	// Open if needed.
	repoRoot := r0(p.spec, opts.AbsPath)
	if err := p.openDocument(repoRoot, relForOpen(repoRoot, opts.AbsPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Already-open is a no-op; only abort on non-trivial errors.
		return nil, fmt.Errorf("open document: %w", err)
	}

	result := &FixAllResult{}
	touched := make(map[string]bool)

	for iter := 0; iter < opts.MaxIterations; iter++ {
		result.Iterations = iter + 1
		// Snapshot diagnostics for this iteration.
		diags, _ := p.LastDiagnostics(opts.AbsPath)

		// Build the request — full-file range, restricted to the
		// caller's allowed kinds. We can't pass an unbounded sentinel
		// (1 << 30 etc.) because gopls and a few other servers
		// validate End.Line against the file's real line count and
		// reject with `line number N out of range 0-M`. Read the
		// file's actual line count once per iteration so re-edits
		// during fix-all stay in sync.
		endLine, endChar := wholeFileEnd(opts.AbsPath)
		req := CodeActionsRequest{
			AbsPath:     opts.AbsPath,
			Range:       Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: endLine, Character: endChar}},
			Diagnostics: diags,
			Only:        kinds,
		}
		actions, err := p.GetCodeActions(req)
		if err != nil {
			return result, fmt.Errorf("get code actions: %w", err)
		}
		// Sort: preferred → by kind → by title — keeps results
		// deterministic across servers.
		sortActions(actions)

		applied := 0
		for _, a := range actions {
			if applied >= opts.MaxActionsPerIter {
				break
			}
			if a.Disabled != nil {
				continue
			}
			edit := a.Edit
			if edit == nil && a.Data != nil {
				resolved, err := p.ResolveCodeAction(a)
				if err == nil {
					edit = resolved.Edit
				}
			}
			if edit == nil {
				// Legacy Command-form. Skip unless it's a source.* kind
				// where the server commits the changes itself.
				if a.IsCommand() && (a.Kind == CodeActionKindSourceOrganizeImports || a.Kind == CodeActionKindSourceFixAll) {
					_, _ = p.ExecuteCommand(Command{Command: a.Command, Arguments: a.Arguments, Title: a.Title})
					applied++
				}
				continue
			}
			files, err := WriteWorkspaceEdit(*edit)
			if err != nil {
				return result, fmt.Errorf("apply workspace edit: %w", err)
			}
			for _, f := range files {
				touched[f] = true
				_ = p.changeDocument(f, mustReadFile(f))
			}
			applied++
		}
		result.AppliedActions += applied
		if applied == 0 {
			// No work this iteration — converge.
			break
		}
		// Wait for the server to refresh diagnostics so the next
		// iteration sees the new state. Best-effort; if no waiter
		// fires we move on with the cached snapshot.
		_ = p.WaitForDiagnostics(opts.AbsPath, opts.DiagnosticTimeout)
	}
	if d, ok := p.LastDiagnostics(opts.AbsPath); ok {
		result.FinalDiagnostics = d
	}
	for f := range touched {
		result.FilesTouched = append(result.FilesTouched, f)
	}
	sort.Strings(result.FilesTouched)
	return result, nil
}

// ApplyCodeAction is the single-action analogue of FixAllInFile.
// Resolves the action if its Edit is nil, then applies the resulting
// WorkspaceEdit. Returns the list of touched files.
func (p *Provider) ApplyCodeAction(action CodeActionOrCommand) ([]string, error) {
	edit := action.Edit
	if edit == nil && (action.Data != nil || action.Title != "") {
		// Try to resolve.
		resolved, err := p.ResolveCodeAction(action)
		if err == nil && resolved.Edit != nil {
			edit = resolved.Edit
		}
	}
	if edit == nil && action.IsCommand() {
		_, err := p.ExecuteCommand(Command{Command: action.Command, Arguments: action.Arguments, Title: action.Title})
		return nil, err
	}
	if edit == nil {
		return nil, errors.New("code action has no applicable edit or command")
	}
	files, err := WriteWorkspaceEdit(*edit)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		_ = p.changeDocument(f, mustReadFile(f))
	}
	return files, nil
}

// WriteWorkspaceEdit applies a WorkspaceEdit to disk. Both the legacy
// `changes` field and the modern `documentChanges` field are
// supported. Returns the absolute paths of files whose contents
// changed.
//
// Edits within a single file are applied in reverse position order so
// earlier edits don't shift the offsets of later ones.
func WriteWorkspaceEdit(edit WorkspaceEdit) ([]string, error) {
	type fileEdit struct {
		path  string
		edits []TextEdit
	}
	var grouped []fileEdit
	if len(edit.DocumentChanges) > 0 {
		for _, dc := range edit.DocumentChanges {
			path := uriToAbsPath(dc.TextDocument.URI)
			if path == "" {
				continue
			}
			grouped = append(grouped, fileEdit{path: path, edits: dc.Edits})
		}
	} else {
		for uri, edits := range edit.Changes {
			path := uriToAbsPath(uri)
			if path == "" {
				continue
			}
			grouped = append(grouped, fileEdit{path: path, edits: edits})
		}
	}

	touched := make([]string, 0, len(grouped))
	for _, fe := range grouped {
		content, err := os.ReadFile(fe.path)
		if err != nil {
			return touched, fmt.Errorf("read %s: %w", fe.path, err)
		}
		newContent, err := applyEditsToContent(content, fe.edits)
		if err != nil {
			return touched, fmt.Errorf("apply edits to %s: %w", fe.path, err)
		}
		// Atomic write: temp + rename.
		tmp, err := os.CreateTemp(filepath.Dir(fe.path), ".gortex-edit-*")
		if err != nil {
			return touched, err
		}
		if _, err := tmp.Write(newContent); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return touched, err
		}
		if err := tmp.Close(); err != nil {
			return touched, err
		}
		if err := os.Rename(tmp.Name(), fe.path); err != nil {
			_ = os.Remove(tmp.Name())
			return touched, err
		}
		touched = append(touched, fe.path)
	}
	return touched, nil
}

// applyEditsToContent walks the edits in reverse-position order and
// returns the resulting bytes. Position arithmetic is line/char in
// UTF-16 units per LSP — for the ASCII-only common case this matches
// byte offsets; for unicode-heavy code it underestimates UTF-16 units
// (a known LSP/Go interop wart, present across major LSP clients).
func applyEditsToContent(content []byte, edits []TextEdit) ([]byte, error) {
	if len(edits) == 0 {
		return content, nil
	}
	// Sort edits by start position descending so applying one
	// doesn't invalidate the offsets of the next.
	sorted := make([]TextEdit, len(edits))
	copy(sorted, edits)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Range.Start.Line != sorted[j].Range.Start.Line {
			return sorted[i].Range.Start.Line > sorted[j].Range.Start.Line
		}
		return sorted[i].Range.Start.Character > sorted[j].Range.Start.Character
	})

	out := make([]byte, len(content))
	copy(out, content)
	for _, e := range sorted {
		startOff, err := lspPositionToOffset(out, e.Range.Start)
		if err != nil {
			return nil, err
		}
		endOff, err := lspPositionToOffset(out, e.Range.End)
		if err != nil {
			return nil, err
		}
		if startOff > endOff || startOff < 0 || endOff > len(out) {
			return nil, fmt.Errorf("invalid edit range: start=%d end=%d len=%d", startOff, endOff, len(out))
		}
		newBytes := []byte(e.NewText)
		merged := make([]byte, 0, len(out)-(endOff-startOff)+len(newBytes))
		merged = append(merged, out[:startOff]...)
		merged = append(merged, newBytes...)
		merged = append(merged, out[endOff:]...)
		out = merged
	}
	return out, nil
}

// lspPositionToOffset converts an LSP Position (line, char) into a
// byte offset within content. Treats char as UTF-16 code units only
// when the line contains non-ASCII bytes; for ASCII-only lines it's
// equivalent to a byte index — the common case.
func lspPositionToOffset(content []byte, pos Position) (int, error) {
	off := 0
	line := 0
	for off < len(content) && line < pos.Line {
		if content[off] == '\n' {
			line++
		}
		off++
	}
	if line < pos.Line {
		// Position past EOF — clamp to len(content).
		return len(content), nil
	}
	// Walk Character UTF-16 code units within the line.
	col := 0
	for off < len(content) && col < pos.Character {
		b := content[off]
		if b == '\n' {
			break
		}
		// ASCII fast path.
		if b < 0x80 {
			off++
			col++
			continue
		}
		// UTF-8 multi-byte sequence.
		size := utf8RuneSize(b)
		if size <= 0 {
			size = 1
		}
		if off+size > len(content) {
			break
		}
		// One UTF-16 code unit for BMP, two for supplementary.
		r := decodeUTF8(content[off : off+size])
		if r >= 0x10000 {
			col += 2
		} else {
			col++
		}
		off += size
	}
	return off, nil
}

func utf8RuneSize(b byte) int {
	switch {
	case b&0x80 == 0:
		return 1
	case b&0xE0 == 0xC0:
		return 2
	case b&0xF0 == 0xE0:
		return 3
	case b&0xF8 == 0xF0:
		return 4
	}
	return 1
}

// decodeUTF8 returns the rune for the given byte slice (assumed to
// start a valid UTF-8 sequence).
func decodeUTF8(b []byte) int32 {
	switch len(b) {
	case 1:
		return int32(b[0])
	case 2:
		return int32(b[0]&0x1F)<<6 | int32(b[1]&0x3F)
	case 3:
		return int32(b[0]&0x0F)<<12 | int32(b[1]&0x3F)<<6 | int32(b[2]&0x3F)
	case 4:
		return int32(b[0]&0x07)<<18 | int32(b[1]&0x3F)<<12 | int32(b[2]&0x3F)<<6 | int32(b[3]&0x3F)
	}
	return 0
}

// sortActions stabilises code-action ordering across servers: prefer
// IsPreferred, then by kind specificity (quickfix first, then
// organizeImports, then refactor, then source), then by title.
func sortActions(actions []CodeActionOrCommand) {
	rank := func(k string) int {
		switch k {
		case CodeActionKindQuickFix:
			return 0
		case CodeActionKindSourceOrganizeImports:
			return 1
		case CodeActionKindSourceFixAll:
			return 2
		case CodeActionKindRefactor, CodeActionKindRefactorExtract,
			CodeActionKindRefactorInline, CodeActionKindRefactorRewrite:
			return 3
		case CodeActionKindSource:
			return 4
		}
		return 5
	}
	sort.SliceStable(actions, func(i, j int) bool {
		a, b := actions[i], actions[j]
		if a.IsPreferred != b.IsPreferred {
			return a.IsPreferred
		}
		ra, rb := rank(a.Kind), rank(b.Kind)
		if ra != rb {
			return ra < rb
		}
		return a.Title < b.Title
	})
}

// r0 picks a sensible workspace root for an opened document — for
// most LSP servers the workspace root in the initialize call is the
// truth, but FixAllInFile may be called against a path under that
// root. We don't have access to the original workspaceRoot here, so
// fall back to the directory containing the file.
func r0(spec *ServerSpec, abs string) string {
	_ = spec
	return filepath.Dir(abs)
}

// relForOpen returns abs minus the leading repoRoot — so openDocument
// can join them back together. Used when the caller has only the
// absolute path. Returns the original absolute path when it isn't
// under repoRoot (openDocument tolerates absolute paths via
// filepath.Join's semantics: when relPath is absolute it overrides
// the prefix).
func relForOpen(repoRoot, abs string) string {
	if rel, err := filepath.Rel(repoRoot, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return abs
}

// mustReadFile reads the entire file as a string. Returns the empty
// string on error so the caller's didChange notify still has a
// well-formed payload.
func mustReadFile(abs string) string {
	b, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	return string(b)
}
