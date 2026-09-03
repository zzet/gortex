package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/tools/go/ast/astutil"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/graph"
)

// registerMoveInlineTools registers the move_symbol and inline_symbol
// MCP tools. They build on the same resolution and atomic-write
// primitives as edit_file / edit_symbol / rename_symbol; the Go-AST
// work lives in this file so the rename/edit pipeline stays focused on
// language-agnostic edits.
func (s *Server) registerMoveInlineTools() {
	s.addTool(
		mcp.NewTool("move_symbol",
			mcp.WithDescription("Relocate a Go function, type, method, variable, or const to another file. Same-package moves leave callers untouched; cross-package moves rewrite every qualified reference (`pkga.Foo` becomes `pkgb.Foo`), strip the import in the new home, and add the new import in the old home. Returns the touched files plus a per-file before/after summary. Non-Go symbols are refused with an explicit unsupported-language error."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol ID to move (e.g. pkga/a.go::Foo)")),
			mcp.WithString("target_file", mcp.Required(), mcp.Description("Destination file path (relative repo-prefixed or absolute). The tool detects same-package vs cross-package by comparing the parent directories of the source and target files.")),
			mcp.WithString("target_package", mcp.Description("Optional package-name override. When the target file does not yet exist and the target directory has no other Go files, this names the new package. Defaults to the existing target-dir package, then to the directory leaf name.")),
			mcp.WithBoolean("dry_run", mcp.Description("Preview the change set without writing anything (default: false)")),
		),
		s.handleMoveSymbol,
	)

	s.addTool(
		mcp.NewTool("inline_symbol",
			mcp.WithDescription("Replace every callsite of a trivial Go function with the callee's body. Supports single-statement / single-expression functions with at most one return value. Refuses (without partial inlines) when the callee contains a defer / go / closure, has multiple return values, or when any callsite passes a side-effecting argument (a call expression). With delete_after=true the callee definition is removed once every callsite has been rewritten. Non-Go callees are refused with an explicit unsupported-language error."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Function symbol ID to inline (e.g. pkg/foo.go::Get)")),
			mcp.WithBoolean("delete_after", mcp.Description("Remove the callee from disk after every callsite has been rewritten (default: true). Set false to keep the original function.")),
			mcp.WithBoolean("dry_run", mcp.Description("Preview the change set without writing anything (default: false)")),
		),
		s.handleInlineSymbol,
	)
}

// ---------------------------------------------------------------------------
// move_symbol
// ---------------------------------------------------------------------------

// moveTouchedFile records what changed at a single file during a move.
type moveTouchedFile struct {
	Path         string `json:"path"`
	Role         string `json:"role"` // source, target, caller, target_caller
	BytesBefore  int    `json:"bytes_before"`
	BytesAfter   int    `json:"bytes_after"`
	LinesBefore  int    `json:"lines_before"`
	LinesAfter   int    `json:"lines_after"`
	References   int    `json:"references_rewritten,omitempty"`
	ImportAdded  bool   `json:"import_added,omitempty"`
	ImportRemove bool   `json:"import_removed,omitempty"`
	Created      bool   `json:"created,omitempty"`
}

func (s *Server) handleMoveSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	targetFileArg, err := req.RequireString("target_file")
	if err != nil {
		return mcp.NewToolResultError("target_file is required"), nil
	}
	targetPackageOverride := strings.TrimSpace(req.GetString("target_package", ""))
	dryRun := req.GetBool("dry_run", false)

	node := s.engineFor(ctx).GetSymbol(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}
	if !isGoSourcePath(node.FilePath) {
		return mcp.NewToolResultError(fmt.Sprintf("unsupported language: move_symbol only supports Go files (got %q)", node.FilePath)), nil
	}
	if node.Name == "" {
		return mcp.NewToolResultError("symbol has no name: " + id), nil
	}
	if node.Kind != graph.KindFunction && node.Kind != graph.KindMethod &&
		node.Kind != graph.KindType && node.Kind != graph.KindInterface &&
		node.Kind != graph.KindVariable && node.Kind != graph.KindConstant {
		return mcp.NewToolResultError(fmt.Sprintf("unsupported symbol kind for move: %s", node.Kind)), nil
	}

	srcAbs, err := s.resolveNodePath(ctx, node)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	tgtAbs, tgtRel, err := s.resolveFilePath(ctx, targetFileArg)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	srcAbs = filepath.Clean(srcAbs)
	tgtAbs = filepath.Clean(tgtAbs)
	if srcAbs == tgtAbs {
		return mcp.NewToolResultError("target_file is the same as the source file"), nil
	}
	if !isGoSourcePath(tgtAbs) {
		return mcp.NewToolResultError(fmt.Sprintf("unsupported language: target_file must be a .go file (got %q)", targetFileArg)), nil
	}

	srcDir := filepath.Dir(srcAbs)
	tgtDir := filepath.Dir(tgtAbs)
	samePackage := srcDir == tgtDir

	srcContent, err := os.ReadFile(srcAbs)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not read source file: %v", err)), nil
	}

	// Parse source file with comments so we keep doc-comments intact.
	fset := token.NewFileSet()
	srcFile, err := parser.ParseFile(fset, srcAbs, srcContent, parser.ParseComments)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not parse source file: %v", err)), nil
	}

	srcPackage := srcFile.Name.Name

	// Locate the declaration we're moving inside the source AST.
	moveDecl, _, moveSnippet, err := extractGoTopLevelDecl(fset, srcFile, srcContent, node.Name)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Determine target package name.
	tgtContent, tgtExists, err := readGoFileIfExists(tgtAbs)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	targetPackage := ""
	if tgtExists {
		tgtFileTmp, perr := parser.ParseFile(fset, tgtAbs, tgtContent, parser.ParseComments)
		if perr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not parse target file: %v", perr)), nil
		}
		targetPackage = tgtFileTmp.Name.Name
	} else {
		targetPackage = inferGoPackageForDir(tgtDir, targetPackageOverride)
		if targetPackage == "" {
			return mcp.NewToolResultError("could not determine target package; pass target_package"), nil
		}
	}

	// Resolve module-level import paths for both packages so we can rewrite
	// qualified references on cross-package moves.
	srcImportPath, err := resolveGoImportPathForFile(srcAbs)
	if err != nil && !samePackage {
		return mcp.NewToolResultError(fmt.Sprintf("could not resolve import path of source package: %v", err)), nil
	}
	tgtImportPath, err := resolveGoImportPathForFile(tgtAbs)
	if err != nil && !samePackage {
		return mcp.NewToolResultError(fmt.Sprintf("could not resolve import path of target package: %v", err)), nil
	}

	if samePackage {
		srcImportPath = ""
		tgtImportPath = ""
	}

	// Remove the declaration from the source file content (byte splice).
	newSrcContent, err := spliceOutGoDecl(srcContent, fset, moveDecl)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// On cross-package moves the source file may now need the target import
	// (if it still calls the moved symbol's siblings — handled separately
	// for callers below).
	if !samePackage {
		// Determine if the source package still references the moved symbol
		// elsewhere; if so, the source file needs to import the target
		// package and rewrite bare references.
		newSrcContent, _, err = rewriteCallerFileContent(newSrcContent, srcAbs, callerRewriteOpts{
			Name:              node.Name,
			Mode:              callerSourcePkgRewrite,
			TargetImportPath:  tgtImportPath,
			TargetPackageName: targetPackage,
			SourcePackageName: srcPackage,
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	// Build new target content: append the moved snippet (with leading blank
	// line) to the existing target file, or synthesise a new file with a
	// package declaration.
	newTgtContent, targetCreated, err := buildTargetContent(tgtContent, tgtExists, targetPackage, moveSnippet)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Cross-package moves: strip the source import from the target file if
	// the moved symbol's body referenced it bare (the move turns the
	// reference into a same-package call after relocation).
	if !samePackage {
		newTgtContent, _, err = rewriteCallerFileContent(newTgtContent, tgtAbs, callerRewriteOpts{
			Name:              node.Name,
			Mode:              callerTargetPkgRewrite,
			SourceImportPath:  srcImportPath,
			SourcePackageName: srcPackage,
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	// Locate caller files. Prefer graph edges (FindUsages) for prune
	// candidates; fall back to walking the module tree so we catch
	// callers in test fixtures where the graph hasn't yet resolved
	// cross-package edges.
	var (
		callerEdits    []moveCallerEdit
		totalRewritten int
	)
	callerFiles := map[string]struct{}{}
	usages := s.engineFor(ctx).FindUsages(id)
	for _, edge := range usages.Edges {
		fromNode := nodeFromUsages(usages.Nodes, edge.From)
		if fromNode == nil {
			continue
		}
		if !isGoSourcePath(fromNode.FilePath) {
			continue
		}
		abs, rerr := s.resolveNodePath(ctx, fromNode)
		if rerr != nil {
			continue
		}
		if abs == srcAbs || abs == tgtAbs {
			continue
		}
		callerFiles[abs] = struct{}{}
	}
	// Fallback / completion: scan the module tree so callers without a
	// resolved graph edge are still rewritten.
	modRoot := findGoModRoot(srcAbs)
	if modRoot != "" {
		_ = filepath.Walk(modRoot, func(p string, info os.FileInfo, werr error) error {
			if werr != nil {
				return nil
			}
			if info.IsDir() {
				base := info.Name()
				if base == ".git" || base == "vendor" || base == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !isGoSourcePath(p) {
				return nil
			}
			abs := filepath.Clean(p)
			if abs == srcAbs || abs == tgtAbs {
				return nil
			}
			callerFiles[abs] = struct{}{}
			return nil
		})
	}

	for absPath := range callerFiles {
		content, rerr := os.ReadFile(absPath)
		if rerr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not read caller file %s: %v", absPath, rerr)), nil
		}
		// Decide rewrite mode based on whether this caller lives in the
		// source package, the target package, or a third package.
		callerDir := filepath.Dir(absPath)
		mode := callerThirdPartyRewrite
		switch callerDir {
		case srcDir:
			mode = callerSourcePkgRewrite
		case tgtDir:
			mode = callerTargetPkgRewrite
		}
		newContent, info, rerr := rewriteCallerFileContent(content, absPath, callerRewriteOpts{
			Name:              node.Name,
			Mode:              mode,
			SourceImportPath:  srcImportPath,
			TargetImportPath:  tgtImportPath,
			SourcePackageName: srcPackage,
			TargetPackageName: targetPackage,
		})
		if rerr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not rewrite caller %s: %v", absPath, rerr)), nil
		}
		if info.references == 0 && !info.importChanged() {
			continue
		}
		callerEdits = append(callerEdits, moveCallerEdit{
			absPath:       absPath,
			origContent:   content,
			newContent:    newContent,
			rewriteResult: info,
			role:          roleForMode(mode),
		})
		totalRewritten += info.references
	}

	touched := []moveTouchedFile{}
	srcRel := s.repoRelative(srcAbs)
	tgtRelOut := tgtRel
	if filepath.IsAbs(tgtRelOut) {
		tgtRelOut = s.repoRelative(tgtAbs)
	}

	touched = append(touched, moveTouchedFile{
		Path:        srcRel,
		Role:        "source",
		BytesBefore: len(srcContent),
		BytesAfter:  len(newSrcContent),
		LinesBefore: countLines(srcContent),
		LinesAfter:  countLines(newSrcContent),
	})
	touched = append(touched, moveTouchedFile{
		Path:        tgtRelOut,
		Role:        "target",
		BytesBefore: len(tgtContent),
		BytesAfter:  len(newTgtContent),
		LinesBefore: countLines(tgtContent),
		LinesAfter:  countLines(newTgtContent),
		Created:     targetCreated,
	})
	for _, ce := range callerEdits {
		rel := s.repoRelative(ce.absPath)
		touched = append(touched, moveTouchedFile{
			Path:         rel,
			Role:         ce.role,
			BytesBefore:  len(ce.origContent),
			BytesAfter:   len(ce.newContent),
			LinesBefore:  countLines(ce.origContent),
			LinesAfter:   countLines(ce.newContent),
			References:   ce.rewriteResult.references,
			ImportAdded:  ce.rewriteResult.importAdded,
			ImportRemove: ce.rewriteResult.importRemoved,
		})
	}

	sort.SliceStable(touched, func(i, j int) bool {
		if touched[i].Role != touched[j].Role {
			return moveRoleRank(touched[i].Role) < moveRoleRank(touched[j].Role)
		}
		return touched[i].Path < touched[j].Path
	})

	resp := map[string]any{
		"source_id":            id,
		"target_id":            buildMovedSymbolID(s, tgtAbs, node.Name),
		"target_package":       targetPackage,
		"source_package":       srcPackage,
		"same_package":         samePackage,
		"references_rewritten": totalRewritten,
		"touched":              touched,
		"files_touched":        len(touched),
		"dry_run":              dryRun,
	}
	if dryRun {
		return s.respondJSONOrTOON(ctx, req, resp)
	}

	// Apply writes atomically. Order: target first (so file exists for any
	// caller that triggers a follow-up open), then source, then callers.
	if err := writeMoveFile(tgtAbs, newTgtContent); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := writeMoveFile(srcAbs, newSrcContent); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	for _, ce := range callerEdits {
		if err := writeMoveFile(ce.absPath, ce.newContent); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	// Re-index every touched file so the graph picks up the new location.
	sess := s.sessionFor(ctx)
	for _, t := range touched {
		sess.recordModified(t.Path)
	}
	_ = s.reindexFile(srcAbs)
	_ = s.reindexFile(tgtAbs)
	for _, ce := range callerEdits {
		_ = s.reindexFile(ce.absPath)
	}

	return s.respondJSONOrTOON(ctx, req, resp)
}

type moveCallerEdit struct {
	absPath       string
	origContent   []byte
	newContent    []byte
	rewriteResult callerRewriteResult
	role          string
}

func roleForMode(m callerRewriteMode) string {
	switch m {
	case callerSourcePkgRewrite:
		return "caller_source_pkg"
	case callerTargetPkgRewrite:
		return "caller_target_pkg"
	default:
		return "caller"
	}
}

func moveRoleRank(role string) int {
	switch role {
	case "source":
		return 0
	case "target":
		return 1
	case "caller_source_pkg":
		return 2
	case "caller_target_pkg":
		return 3
	default:
		return 4
	}
}

func buildMovedSymbolID(s *Server, tgtAbs, name string) string {
	rel := s.repoRelative(tgtAbs)
	if rel == "" {
		rel = tgtAbs
	}
	return rel + "::" + name
}

func writeMoveFile(absPath string, content []byte) error {
	perm := os.FileMode(0o644)
	if info, err := os.Stat(absPath); err == nil {
		perm = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("could not create parent dir for %s: %w", absPath, err)
	}
	if err := agents.AtomicWriteFile(absPath, content, perm); err != nil {
		return fmt.Errorf("could not write %s: %w", absPath, err)
	}
	return nil
}

// extractGoTopLevelDecl finds the top-level declaration named `name` inside
// the parsed file and returns the decl plus its rendered byte snippet
// (including any leading doc-comment lines and the trailing newline).
// The second result is the index of the decl in file.Decls — retained
// for callers that want to relate the decl to its slice position.
func extractGoTopLevelDecl(fset *token.FileSet, file *ast.File, src []byte, name string) (ast.Decl, int, string, error) {
	for i, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			if d.Name.Name != name {
				continue
			}
			snippet, err := renderDeclSnippet(fset, src, d, d.Doc)
			if err != nil {
				return nil, -1, "", err
			}
			return d, i, snippet, nil
		case *ast.GenDecl:
			// Type / Var / Const blocks. We only support the case where the
			// matched spec is the sole spec in the block — multi-spec
			// declarations (var ( a int; b int )) get treated as a whole
			// block and we refuse the move (callers should split first).
			matchedSpecIdx := -1
			matchedName := ""
			for si, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if sp.Name != nil && sp.Name.Name == name {
						matchedSpecIdx = si
						matchedName = name
					}
				case *ast.ValueSpec:
					for _, ident := range sp.Names {
						if ident.Name == name {
							matchedSpecIdx = si
							matchedName = name
							break
						}
					}
				}
				if matchedSpecIdx >= 0 {
					break
				}
			}
			if matchedSpecIdx < 0 {
				continue
			}
			if len(d.Specs) > 1 {
				return nil, -1, "", fmt.Errorf("symbol %q is declared inside a multi-spec %s block; split it into its own declaration before moving", matchedName, d.Tok.String())
			}
			snippet, err := renderDeclSnippet(fset, src, d, d.Doc)
			if err != nil {
				return nil, -1, "", err
			}
			return d, i, snippet, nil
		}
	}
	return nil, -1, "", fmt.Errorf("top-level declaration %q not found in source file", name)
}

// renderDeclSnippet emits the byte range covering an optional doc-comment
// block and the declaration itself, normalised so it can be appended to
// another file as a self-contained block.
func renderDeclSnippet(fset *token.FileSet, src []byte, decl ast.Decl, doc *ast.CommentGroup) (string, error) {
	startPos := decl.Pos()
	if doc != nil {
		startPos = doc.Pos()
	}
	endPos := decl.End()
	start := fset.Position(startPos).Offset
	end := fset.Position(endPos).Offset
	if start < 0 || end > len(src) || start > end {
		return "", fmt.Errorf("invalid declaration byte range")
	}
	// Capture trailing newline if present so appended decls are visually
	// separated.
	if end < len(src) && src[end] == '\n' {
		end++
	}
	snippet := string(src[start:end])
	// Trim trailing whitespace, then add a single trailing newline.
	snippet = strings.TrimRight(snippet, " \t\r\n") + "\n"
	return snippet, nil
}

// spliceOutGoDecl removes the byte range corresponding to a top-level
// declaration (including its leading doc-comment block) from the source.
// Adjacent blank lines around the splice are collapsed so the file stays
// gofmt-friendly.
func spliceOutGoDecl(src []byte, fset *token.FileSet, decl ast.Decl) ([]byte, error) {
	var doc *ast.CommentGroup
	switch d := decl.(type) {
	case *ast.FuncDecl:
		doc = d.Doc
	case *ast.GenDecl:
		doc = d.Doc
	}
	startPos := decl.Pos()
	if doc != nil {
		startPos = doc.Pos()
	}
	endPos := decl.End()
	start := fset.Position(startPos).Offset
	end := fset.Position(endPos).Offset
	if start < 0 || end > len(src) || start > end {
		return nil, fmt.Errorf("invalid declaration byte range during splice")
	}
	// Expand start backwards over the blank line right before the decl
	// (newline + optional whitespace). At most one extra newline so we
	// don't eat unrelated content.
	for start > 0 && (src[start-1] == ' ' || src[start-1] == '\t') {
		start--
	}
	// Expand end forwards over the trailing newline + one optional blank
	// line, so the splice doesn't leave a double blank.
	if end < len(src) && src[end] == '\n' {
		end++
	}
	if end < len(src) && src[end] == '\n' {
		end++
	}
	// Pull start back one more newline if we now sit on a blank line so
	// the file doesn't grow blank lines on every move.
	if start > 0 && src[start-1] == '\n' && end < len(src) {
		start--
	}
	out := make([]byte, 0, len(src)-(end-start))
	out = append(out, src[:start]...)
	out = append(out, src[end:]...)
	// Run gofmt to tidy up any whitespace artefacts.
	formatted, err := format.Source(out)
	if err != nil {
		// Tolerate format errors — the splice may have left content that
		// the parser still accepts but format chokes on (rare). Return
		// the unformatted bytes so the caller can still write them.
		return out, nil //nolint:nilerr // explicit fallback
	}
	return formatted, nil
}

// readGoFileIfExists returns the file's content if it exists and an empty
// slice with exists=false otherwise. Errors other than os.IsNotExist are
// surfaced.
func readGoFileIfExists(absPath string) ([]byte, bool, error) {
	content, err := os.ReadFile(absPath)
	if err == nil {
		return content, true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("could not read target file: %w", err)
}

// inferGoPackageForDir picks a package name for a new Go file in `dir`.
// Priority: explicit override, package of an existing .go sibling, then
// the directory's leaf name.
func inferGoPackageForDir(dir, override string) string {
	if override != "" {
		return override
	}
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			content, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				continue
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, "", content, parser.PackageClauseOnly)
			if perr == nil && f.Name != nil {
				return f.Name.Name
			}
		}
	}
	leaf := filepath.Base(dir)
	leaf = strings.ReplaceAll(leaf, "-", "_")
	if leaf == "." || leaf == "/" || leaf == "" {
		return ""
	}
	return leaf
}

// resolveGoImportPathForFile walks up from the file's directory looking for a
// go.mod file. Returns "<module>/<rel-dir-to-mod>" — the standard Go import
// path for the file's package.
func resolveGoImportPathForFile(absPath string) (string, error) {
	dir := filepath.Dir(absPath)
	cur := dir
	for {
		modPath := filepath.Join(cur, "go.mod")
		if data, err := os.ReadFile(modPath); err == nil {
			mod := parseGoModModule(data)
			if mod == "" {
				return "", fmt.Errorf("could not parse module path from %s", modPath)
			}
			rel, err := filepath.Rel(cur, dir)
			if err != nil {
				return "", err
			}
			if rel == "." {
				return mod, nil
			}
			return mod + "/" + filepath.ToSlash(rel), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no go.mod found above %s", absPath)
		}
		cur = parent
	}
}

// findGoModRoot walks upward from a file path looking for a go.mod and
// returns the directory containing it, or "" when none is found.
func findGoModRoot(start string) string {
	cur := filepath.Dir(start)
	for {
		if _, err := os.Stat(filepath.Join(cur, "go.mod")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// parseGoModModule extracts the module path from go.mod content. The
// parser is intentionally minimal — modfile would pull in another
// dependency edge into the mcp package and we only need the first
// `module <path>` line.
func parseGoModModule(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			rest := strings.TrimSpace(strings.TrimPrefix(line, "module"))
			rest = strings.TrimSuffix(rest, "//")
			rest = strings.TrimSpace(rest)
			rest = strings.Trim(rest, "\"")
			return rest
		}
	}
	return ""
}

// buildTargetContent appends the moved snippet to the target file's content,
// or synthesises a new file (with `package <pkg>`) when the target doesn't
// yet exist.
func buildTargetContent(existing []byte, exists bool, packageName, snippet string) ([]byte, bool, error) {
	if !exists {
		var b strings.Builder
		b.WriteString("package ")
		b.WriteString(packageName)
		b.WriteString("\n\n")
		b.WriteString(snippet)
		out, err := format.Source([]byte(b.String()))
		if err != nil {
			return []byte(b.String()), true, nil //nolint:nilerr
		}
		return out, true, nil
	}
	// Append with a separating blank line.
	out := bytes.TrimRight(existing, "\n")
	var b bytes.Buffer
	b.Write(out)
	b.WriteString("\n\n")
	b.WriteString(snippet)
	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return b.Bytes(), false, nil //nolint:nilerr
	}
	return formatted, false, nil
}

// callerRewriteMode tells rewriteCallerFileContent which sense to apply.
type callerRewriteMode int

const (
	// callerThirdPartyRewrite: caller lives outside both packages. It
	// imported the source package and called <srcpkg>.<Name>; after the
	// move it should import the target package and call <tgtpkg>.<Name>.
	callerThirdPartyRewrite callerRewriteMode = iota
	// callerSourcePkgRewrite: caller lives in the source package (a
	// sibling of the moved decl). After the move the call must qualify
	// with the target package, and the target import must be present.
	callerSourcePkgRewrite
	// callerTargetPkgRewrite: caller lives in the target package. After
	// the move the call is bare and the source import is dropped if it
	// has no other uses.
	callerTargetPkgRewrite
)

type callerRewriteOpts struct {
	Name              string
	Mode              callerRewriteMode
	SourceImportPath  string
	TargetImportPath  string
	SourcePackageName string
	TargetPackageName string
}

type callerRewriteResult struct {
	references    int
	importAdded   bool
	importRemoved bool
}

func (r callerRewriteResult) importChanged() bool { return r.importAdded || r.importRemoved }

// rewriteCallerFileContent rewrites a Go file in-place (content -> content)
// so qualified references to a symbol that just moved are updated. It also
// manages the file's import block.
func rewriteCallerFileContent(content []byte, absPath string, opts callerRewriteOpts) ([]byte, callerRewriteResult, error) {
	if len(content) == 0 {
		return content, callerRewriteResult{}, nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, absPath, content, parser.ParseComments)
	if err != nil {
		return nil, callerRewriteResult{}, fmt.Errorf("could not parse %s: %w", absPath, err)
	}

	result := callerRewriteResult{}
	switch opts.Mode {
	case callerThirdPartyRewrite:
		// Rewrite `<srcAlias>.<Name>` -> `<tgtAlias>.<Name>` where srcAlias /
		// tgtAlias are the local names of those imports (PkgName.Name from
		// the resolved ast.Ident.Obj.Decl — we look it up via the file's
		// imports). Then drop the src import if it's now unreferenced, and
		// ensure the tgt import exists.
		srcAlias := findImportAlias(file, opts.SourceImportPath)
		if srcAlias == "" {
			return content, result, nil
		}
		tgtAlias := findImportAlias(file, opts.TargetImportPath)
		if tgtAlias == "" {
			tgtAlias = opts.TargetPackageName
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if x.Name != srcAlias || sel.Sel == nil || sel.Sel.Name != opts.Name {
				return true
			}
			x.Name = tgtAlias
			result.references++
			return true
		})
		if result.references == 0 {
			return content, result, nil
		}
		if !goFileStillUsesIdent(file, srcAlias) {
			if astutil.DeleteImport(fset, file, opts.SourceImportPath) {
				result.importRemoved = true
			}
		}
		if !goFileImportsPath(file, opts.TargetImportPath) {
			if astutil.AddImport(fset, file, opts.TargetImportPath) {
				result.importAdded = true
			}
		}
	case callerSourcePkgRewrite:
		// Caller is in the source package. Bare references to `<Name>`
		// must become `<tgtAlias>.<Name>`. We walk with astutil so we
		// can replace the ast.Ident node with a SelectorExpr in its
		// proper slot — mutating ident.Name to a dotted string would
		// produce a malformed printer output.
		tgtAlias := opts.TargetPackageName
		// Skip the rewrite at decl-name positions: those name the
		// declaration itself, not a usage.
		declPositions := collectDeclNamePositions(file)
		// Also skip when the ident is the right-hand side of a
		// SelectorExpr (struct field access / method call).
		count := 0
		rewrittenRoot := astutil.Apply(file, func(c *astutil.Cursor) bool {
			ident, ok := c.Node().(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name != opts.Name {
				return true
			}
			if _, isDecl := declPositions[ident.Pos()]; isDecl {
				return true
			}
			// Detect "x.Foo" — the parent is a SelectorExpr and we're
			// in the Sel slot. Cursor.Parent() returns the enclosing
			// node, Cursor.Name() tells us which field we are.
			if sel, ok := c.Parent().(*ast.SelectorExpr); ok && sel.Sel == ident {
				return true
			}
			// Skip when we're the Name field of an explicit declaration
			// (defensive — declPositions should already cover this).
			c.Replace(&ast.SelectorExpr{
				X:   &ast.Ident{Name: tgtAlias, NamePos: ident.Pos()},
				Sel: &ast.Ident{Name: opts.Name, NamePos: ident.Pos()},
			})
			count++
			return true
		}, nil)
		if newFile, ok := rewrittenRoot.(*ast.File); ok {
			file = newFile
		}
		result.references = count
		if count > 0 {
			if !goFileImportsPath(file, opts.TargetImportPath) {
				if astutil.AddImport(fset, file, opts.TargetImportPath) {
					result.importAdded = true
				}
			}
		}
	case callerTargetPkgRewrite:
		// Caller is in the target package. References look like
		// `<srcAlias>.<Name>` and should become bare `<Name>`. Then
		// drop the source import if unused.
		srcAlias := findImportAlias(file, opts.SourceImportPath)
		if srcAlias == "" {
			return content, result, nil
		}
		count := 0
		rewrittenRoot := astutil.Apply(file, func(c *astutil.Cursor) bool {
			sel, ok := c.Node().(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if x.Name != srcAlias || sel.Sel == nil || sel.Sel.Name != opts.Name {
				return true
			}
			c.Replace(&ast.Ident{Name: opts.Name, NamePos: sel.Pos()})
			count++
			return true
		}, nil)
		if newFile, ok := rewrittenRoot.(*ast.File); ok {
			file = newFile
		}
		result.references = count
		if count > 0 && !goFileStillUsesIdent(file, srcAlias) {
			if astutil.DeleteImport(fset, file, opts.SourceImportPath) {
				result.importRemoved = true
			}
		}
	}

	if result.references == 0 && !result.importChanged() {
		return content, result, nil
	}

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return nil, callerRewriteResult{}, fmt.Errorf("could not format rewritten %s: %w", absPath, err)
	}
	out, err := format.Source(buf.Bytes())
	if err != nil {
		out = buf.Bytes()
	}
	return out, result, nil
}

// findImportAlias returns the local name an import is referenced under in
// the file (the explicit alias if set, otherwise the package's declared
// name — which we can't easily resolve from path; default to the path's
// last segment).
func findImportAlias(file *ast.File, importPath string) string {
	for _, imp := range file.Imports {
		if unquoteImport(imp.Path.Value) != importPath {
			continue
		}
		if imp.Name != nil && imp.Name.Name != "" {
			return imp.Name.Name
		}
		// Default: the last path segment.
		parts := strings.Split(importPath, "/")
		return parts[len(parts)-1]
	}
	return ""
}

func goFileImportsPath(file *ast.File, importPath string) bool {
	for _, imp := range file.Imports {
		if unquoteImport(imp.Path.Value) == importPath {
			return true
		}
	}
	return false
}

func unquoteImport(raw string) string {
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

// goFileStillUsesIdent returns true if any ast.Ident in the file (outside of
// import specs) bears the supplied name. Used to decide whether an import is
// safe to drop after a rename.
func goFileStillUsesIdent(file *ast.File, name string) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		if _, ok := n.(*ast.ImportSpec); ok {
			return false // don't recurse into import specs
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// collectDeclNamePositions records the token positions of every
// top-level declaration name in the file. Used by the caller-rewrite
// to skip declarations themselves so `func Foo() {}` doesn't get its
// own name rewritten.
func collectDeclNamePositions(file *ast.File) map[token.Pos]struct{} {
	out := map[token.Pos]struct{}{}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name != nil {
				out[d.Name.Pos()] = struct{}{}
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if sp.Name != nil {
						out[sp.Name.Pos()] = struct{}{}
					}
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						out[n.Pos()] = struct{}{}
					}
				}
			}
		}
	}
	return out
}

func nodeFromUsages(nodes []*graph.Node, id string) *graph.Node {
	for _, n := range nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func isGoSourcePath(p string) bool {
	return strings.HasSuffix(p, ".go")
}

// ---------------------------------------------------------------------------
// inline_symbol
// ---------------------------------------------------------------------------

type inlineRefusal struct {
	Site   string `json:"site,omitempty"`
	Reason string `json:"reason"`
}

func (s *Server) handleInlineSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	deleteAfter := req.GetBool("delete_after", true)
	dryRun := req.GetBool("dry_run", false)

	node := s.engineFor(ctx).GetSymbol(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}
	if !isGoSourcePath(node.FilePath) {
		return mcp.NewToolResultError(fmt.Sprintf("unsupported language: inline_symbol only supports Go files (got %q)", node.FilePath)), nil
	}
	if node.Kind != graph.KindFunction && node.Kind != graph.KindMethod {
		return mcp.NewToolResultError(fmt.Sprintf("inline_symbol only supports functions and methods (got %s)", node.Kind)), nil
	}

	calleeAbs, err := s.resolveNodePath(ctx, node)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	calleeContent, err := os.ReadFile(calleeAbs)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not read callee file: %v", err)), nil
	}

	calleeFset := token.NewFileSet()
	calleeFile, err := parser.ParseFile(calleeFset, calleeAbs, calleeContent, parser.ParseComments)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not parse callee file: %v", err)), nil
	}

	calleeDecl, calleeDocless, err := findGoFunc(calleeFile, node.Name)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := checkGoInlinable(calleeDecl); err != nil {
		return mcp.NewToolResultError("cannot inline: " + err.Error()), nil
	}

	// Compute the substitution recipe: the body expression (return X)
	// or the single statement.
	recipe, err := buildInlineRecipe(calleeDecl)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Collect callsites via FindUsages, filter to EdgeCalls, group by file.
	usages := s.engineFor(ctx).FindUsages(id)
	calleePkg := calleeFile.Name.Name
	calleeImportPath, _ := resolveGoImportPathForFile(calleeAbs)

	callerFiles := map[string]struct{}{}
	for _, edge := range usages.Edges {
		if edge.Kind != graph.EdgeCalls {
			continue
		}
		fromNode := nodeFromUsages(usages.Nodes, edge.From)
		if fromNode == nil {
			continue
		}
		if !isGoSourcePath(fromNode.FilePath) {
			continue
		}
		abs, rerr := s.resolveNodePath(ctx, fromNode)
		if rerr != nil {
			continue
		}
		callerFiles[abs] = struct{}{}
	}
	// Fallback: walk the module tree so callsites without resolved
	// graph edges are still considered.
	if modRoot := findGoModRoot(calleeAbs); modRoot != "" {
		_ = filepath.Walk(modRoot, func(p string, info os.FileInfo, werr error) error {
			if werr != nil {
				return nil
			}
			if info.IsDir() {
				base := info.Name()
				if base == ".git" || base == "vendor" || base == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !isGoSourcePath(p) {
				return nil
			}
			callerFiles[filepath.Clean(p)] = struct{}{}
			return nil
		})
	}

	// Iterate callers, rewrite each, collect refusals.
	type callerPatch struct {
		absPath    string
		orig       []byte
		patched    []byte
		count      int
		removedImp bool
	}

	var (
		patches  []callerPatch
		refusals []inlineRefusal
		total    int
	)
	for absPath := range callerFiles {
		raw, rerr := os.ReadFile(absPath)
		if rerr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not read caller %s: %v", absPath, rerr)), nil
		}
		patched, count, removedImp, callerRefusals, rerr := inlineCallsInFile(raw, absPath, inlineApplyOpts{
			calleeName:       node.Name,
			calleePackage:    calleePkg,
			calleeImportPath: calleeImportPath,
			calleeDir:        filepath.Dir(calleeAbs),
			callerIsCallee:   absPath == calleeAbs,
			recipe:           recipe,
		})
		if rerr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not rewrite %s: %v", absPath, rerr)), nil
		}
		refusals = append(refusals, callerRefusals...)
		if count == 0 {
			continue
		}
		patches = append(patches, callerPatch{
			absPath:    absPath,
			orig:       raw,
			patched:    patched,
			count:      count,
			removedImp: removedImp,
		})
		total += count
	}

	if len(refusals) > 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status":   "refused",
			"refusals": refusals,
			"note":     "inline_symbol refuses partial inlines; resolve every refusal and retry",
		})
	}

	if total == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status":  "no_callsites",
			"callee":  id,
			"note":    "no inlineable callsites were found",
			"dry_run": dryRun,
		})
	}

	// Optionally delete the callee.
	var calleeNewContent []byte
	calleeDeleted := false
	if deleteAfter {
		spliced, derr := spliceOutGoDecl(calleeContent, calleeFset, calleeDocless)
		if derr != nil {
			return mcp.NewToolResultError(derr.Error()), nil
		}
		calleeNewContent = spliced
		calleeDeleted = true
	}

	type touched struct {
		Path        string `json:"path"`
		Role        string `json:"role"`
		BytesBefore int    `json:"bytes_before"`
		BytesAfter  int    `json:"bytes_after"`
		Inlined     int    `json:"inlined,omitempty"`
		ImportRem   bool   `json:"import_removed,omitempty"`
	}
	var touchedList []touched
	for _, p := range patches {
		touchedList = append(touchedList, touched{
			Path:        s.repoRelative(p.absPath),
			Role:        "caller",
			BytesBefore: len(p.orig),
			BytesAfter:  len(p.patched),
			Inlined:     p.count,
			ImportRem:   p.removedImp,
		})
	}
	if calleeDeleted {
		touchedList = append(touchedList, touched{
			Path:        s.repoRelative(calleeAbs),
			Role:        "callee_deleted",
			BytesBefore: len(calleeContent),
			BytesAfter:  len(calleeNewContent),
		})
	}

	sort.SliceStable(touchedList, func(i, j int) bool {
		return touchedList[i].Path < touchedList[j].Path
	})

	resp := map[string]any{
		"callee":            id,
		"callsites_inlined": total,
		"callee_deleted":    calleeDeleted,
		"delete_after":      deleteAfter,
		"touched":           touchedList,
		"files_touched":     len(touchedList),
		"dry_run":           dryRun,
		"status":            "applied",
	}
	if dryRun {
		resp["status"] = "previewed"
		return s.respondJSONOrTOON(ctx, req, resp)
	}

	for _, p := range patches {
		if err := writeMoveFile(p.absPath, p.patched); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	if calleeDeleted {
		if err := writeMoveFile(calleeAbs, calleeNewContent); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	sess := s.sessionFor(ctx)
	for _, p := range patches {
		sess.recordModified(s.repoRelative(p.absPath))
		_ = s.reindexFile(p.absPath)
	}
	if calleeDeleted {
		sess.recordModified(s.repoRelative(calleeAbs))
		_ = s.reindexFile(calleeAbs)
	}

	return s.respondJSONOrTOON(ctx, req, resp)
}

// findGoFunc locates a top-level FuncDecl by name (or T.Method form).
func findGoFunc(file *ast.File, name string) (*ast.FuncDecl, *ast.FuncDecl, error) {
	want := name
	if dot := strings.LastIndex(name, "."); dot > 0 {
		want = name[dot+1:]
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name == nil {
			continue
		}
		if fd.Name.Name != want {
			continue
		}
		return fd, fd, nil
	}
	return nil, nil, fmt.Errorf("function %q not found in %s", name, file.Name.Name)
}

// checkGoInlinable enforces the conservative inlinability rules: single
// return or none, no closures / goroutines / defers, single body stmt.
func checkGoInlinable(fd *ast.FuncDecl) error {
	if fd.Body == nil {
		return errors.New("function has no body")
	}
	if fd.Recv != nil {
		return errors.New("methods are not yet supported")
	}
	if fd.Type != nil && fd.Type.Results != nil && len(fd.Type.Results.List) > 0 {
		// Count return values across all fields.
		count := 0
		for _, f := range fd.Type.Results.List {
			if len(f.Names) == 0 {
				count++
			} else {
				count += len(f.Names)
			}
		}
		if count > 1 {
			return errors.New("function has multiple return values")
		}
	}
	if len(fd.Body.List) != 1 {
		return errors.New("function body must be a single statement or expression-return")
	}
	// Forbid closures / defers / goroutines anywhere in the body.
	var refusal error
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if refusal != nil {
			return false
		}
		switch n.(type) {
		case *ast.DeferStmt:
			refusal = errors.New("function contains a defer statement")
			return false
		case *ast.GoStmt:
			refusal = errors.New("function contains a goroutine spawn")
			return false
		case *ast.FuncLit:
			refusal = errors.New("function contains a closure")
			return false
		}
		return true
	})
	return refusal
}

// inlineRecipe captures everything we need to splice the callee body into a
// callsite: the formal parameter names and the body expression / statement
// to substitute.
type inlineRecipe struct {
	paramNames []string
	// bodyExpr is the single expression returned by a `return <expr>`
	// body. Nil when the body is a non-return statement (in which case
	// bodyStmtSrc is set instead).
	bodyExpr ast.Expr
	// bodyStmtSrc is the literal Go source of the body's only statement
	// when bodyExpr is nil (e.g. `x.foo = 1`).
	bodyStmtSrc string
	// resultType is true when the function returns a value (we substitute
	// an expression). False for `func F(...)` with no results (we replace
	// the call expression statement with the substituted statement).
	hasResult bool
}

func buildInlineRecipe(fd *ast.FuncDecl) (inlineRecipe, error) {
	r := inlineRecipe{}
	if fd.Type != nil && fd.Type.Params != nil {
		for _, f := range fd.Type.Params.List {
			for _, n := range f.Names {
				r.paramNames = append(r.paramNames, n.Name)
			}
		}
	}
	r.hasResult = fd.Type != nil && fd.Type.Results != nil && len(fd.Type.Results.List) > 0
	stmt := fd.Body.List[0]
	switch s := stmt.(type) {
	case *ast.ReturnStmt:
		if len(s.Results) != 1 {
			return r, errors.New("function must return exactly one value")
		}
		r.bodyExpr = s.Results[0]
	default:
		var buf bytes.Buffer
		fset := token.NewFileSet()
		if err := format.Node(&buf, fset, stmt); err != nil {
			return r, fmt.Errorf("could not render body: %w", err)
		}
		r.bodyStmtSrc = buf.String()
	}
	return r, nil
}

type inlineApplyOpts struct {
	calleeName       string
	calleePackage    string
	calleeImportPath string
	calleeDir        string
	callerIsCallee   bool
	recipe           inlineRecipe
}

// inlineCallsInFile applies the inline recipe to every callsite in a single
// caller file. Returns the patched bytes, the rewritten call count, whether
// the source-package import was removed, and any refusals (per-site).
func inlineCallsInFile(content []byte, absPath string, opts inlineApplyOpts) ([]byte, int, bool, []inlineRefusal, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, absPath, content, parser.ParseComments)
	if err != nil {
		return nil, 0, false, nil, err
	}
	// We need callsites of the callee. Two forms:
	// - bare:        `Name(args...)` inside the callee's own package
	// - qualified:   `pkgAlias.Name(args...)` outside the callee package
	pkgAlias := ""
	for _, imp := range file.Imports {
		if unquoteImport(imp.Path.Value) == opts.calleeImportPath {
			if imp.Name != nil {
				pkgAlias = imp.Name.Name
			} else {
				pkgAlias = opts.calleePackage
			}
		}
	}
	// Same-package detection: caller's directory must match the
	// callee's directory. Two packages with the same name in different
	// dirs do not count as the same package.
	callerDir := filepath.Dir(absPath)
	sameFilePkg := callerDir == opts.calleeDir

	var refusals []inlineRefusal
	rewritten := 0

	apply := func(c *astutil.Cursor) bool {
		call, ok := c.Node().(*ast.CallExpr)
		if !ok {
			return true
		}
		match := false
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if sameFilePkg && fun.Name == opts.calleeName {
				match = true
			}
		case *ast.SelectorExpr:
			if pkgAlias == "" {
				return true
			}
			x, ok := fun.X.(*ast.Ident)
			if !ok {
				return true
			}
			if x.Name == pkgAlias && fun.Sel != nil && fun.Sel.Name == opts.calleeName {
				match = true
			}
		}
		if !match {
			return true
		}

		// Refusal: any argument that's itself a call expression — its
		// side effects would be reordered by naive substitution.
		for _, arg := range call.Args {
			if hasSideEffectArg(arg) {
				pos := fset.Position(call.Pos())
				refusals = append(refusals, inlineRefusal{
					Site:   fmt.Sprintf("%s:%d", absPath, pos.Line),
					Reason: "argument has side effects (call expression); inlining would change evaluation order",
				})
				return true
			}
		}

		// Arity must match.
		if len(call.Args) != len(opts.recipe.paramNames) {
			pos := fset.Position(call.Pos())
			refusals = append(refusals, inlineRefusal{
				Site:   fmt.Sprintf("%s:%d", absPath, pos.Line),
				Reason: fmt.Sprintf("call has %d args, callee declares %d params (variadic / spread not yet supported)", len(call.Args), len(opts.recipe.paramNames)),
			})
			return true
		}

		// Substitute param -> arg in a clone of the body expression.
		if opts.recipe.hasResult && opts.recipe.bodyExpr != nil {
			substituted, serr := substituteExprForParams(opts.recipe.bodyExpr, opts.recipe.paramNames, call.Args)
			if serr != nil {
				pos := fset.Position(call.Pos())
				refusals = append(refusals, inlineRefusal{
					Site:   fmt.Sprintf("%s:%d", absPath, pos.Line),
					Reason: serr.Error(),
				})
				return true
			}
			c.Replace(substituted)
			rewritten++
			return true
		}
		// No-result body: only inlineable when the call appears as an
		// ExprStmt by itself; otherwise refuse.
		// We can't easily check the parent here without a parent map, so
		// we render the body-statement source with param substitution and
		// rely on the printer to merge it cleanly.
		stmtSrc, serr := substituteStmtForParams(opts.recipe.bodyStmtSrc, opts.recipe.paramNames, call.Args, fset)
		if serr != nil {
			pos := fset.Position(call.Pos())
			refusals = append(refusals, inlineRefusal{
				Site:   fmt.Sprintf("%s:%d", absPath, pos.Line),
				Reason: serr.Error(),
			})
			return true
		}
		// Synthesize an Ident with the resolved source so the printer
		// emits it verbatim. Wrap it as an *ast.BadExpr would, but a
		// simpler trick: emit the stmt-source as a comment-anchored
		// literal via ast.Ident with a special marker, then run a
		// regex/string fix-up pass on the formatted output.
		marker := fmt.Sprintf("__INLINE_MARK_%d__", rewritten)
		c.Replace(&ast.Ident{Name: marker})
		rewritten++
		// Record the substitution in a side-channel — the apply func
		// doesn't carry state easily, so attach to the file's comments.
		file.Comments = append(file.Comments, &ast.CommentGroup{List: []*ast.Comment{{Text: "//gortex-inline-marker:" + marker + ":" + stmtSrc}}})
		return true
	}

	rewrittenRoot := astutil.Apply(file, apply, nil)
	if newFile, ok := rewrittenRoot.(*ast.File); ok {
		file = newFile
	}

	if len(refusals) > 0 {
		return nil, 0, false, refusals, nil
	}
	if rewritten == 0 {
		return content, 0, false, nil, nil
	}

	// Render and apply marker substitutions.
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return nil, 0, false, nil, err
	}
	out := buf.Bytes()
	// Extract markers from comments + their substitutions.
	type sub struct{ marker, src string }
	subs := []sub{}
	keepComments := file.Comments[:0]
	for _, cg := range file.Comments {
		newList := cg.List[:0]
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//gortex-inline-marker:") {
				rest := strings.TrimPrefix(c.Text, "//gortex-inline-marker:")
				parts := strings.SplitN(rest, ":", 2)
				if len(parts) == 2 {
					subs = append(subs, sub{marker: parts[0], src: parts[1]})
				}
				continue
			}
			newList = append(newList, c)
		}
		if len(newList) > 0 {
			cg.List = newList
			keepComments = append(keepComments, cg)
		}
	}
	for _, sub := range subs {
		out = bytes.ReplaceAll(out, []byte(sub.marker), []byte(sub.src))
	}
	// Strip residual marker comments that survived the format pass.
	cleaned := stripInlineMarkerComments(out)

	// Drop unused imports for the callee package if we converted a
	// qualified call into a bare expression that no longer references
	// the alias.
	formatted, err := format.Source(cleaned)
	if err != nil {
		// Fall back to unformatted bytes.
		formatted = cleaned
	}

	// After inlining, the import may be orphaned.
	removedImp := false
	if pkgAlias != "" && !bytes.Contains(formatted, []byte(pkgAlias+".")) {
		// Reparse and drop the import.
		fset2 := token.NewFileSet()
		file2, perr := parser.ParseFile(fset2, absPath, formatted, parser.ParseComments)
		if perr == nil {
			if astutil.DeleteImport(fset2, file2, opts.calleeImportPath) {
				removedImp = true
				var b2 bytes.Buffer
				if err := format.Node(&b2, fset2, file2); err == nil {
					if f3, err := format.Source(b2.Bytes()); err == nil {
						formatted = f3
					} else {
						formatted = b2.Bytes()
					}
				}
			}
		}
	}

	return formatted, rewritten, removedImp, nil, nil
}

func stripInlineMarkerComments(src []byte) []byte {
	lines := bytes.Split(src, []byte{'\n'})
	out := lines[:0]
	for _, ln := range lines {
		t := bytes.TrimSpace(ln)
		if bytes.HasPrefix(t, []byte("//gortex-inline-marker:")) {
			continue
		}
		out = append(out, ln)
	}
	return bytes.Join(out, []byte{'\n'})
}

func hasSideEffectArg(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		switch n.(type) {
		case *ast.CallExpr:
			found = true
			return false
		}
		return true
	})
	return found
}

func substituteExprForParams(body ast.Expr, params []string, args []ast.Expr) (ast.Expr, error) {
	if len(params) != len(args) {
		return nil, fmt.Errorf("arity mismatch: %d params, %d args", len(params), len(args))
	}
	mapping := map[string]ast.Expr{}
	for i, p := range params {
		mapping[p] = args[i]
	}
	cloned := cloneExpr(body)
	cloned = substituteIdentsInExpr(cloned, mapping)
	// Wrap in parens to preserve precedence when spliced into the caller
	// expression.
	if needsParens(body) {
		cloned = &ast.ParenExpr{X: cloned}
	}
	return cloned, nil
}

func substituteStmtForParams(stmtSrc string, params []string, args []ast.Expr, fset *token.FileSet) (string, error) {
	if len(params) != len(args) {
		return "", fmt.Errorf("arity mismatch: %d params, %d args", len(params), len(args))
	}
	// Render each arg to source.
	rendered := make([]string, len(args))
	for i, a := range args {
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, a); err != nil {
			return "", err
		}
		rendered[i] = buf.String()
	}
	// Do a token-aware identifier replacement by re-parsing the stmt with
	// a placeholder package, walking idents, and rewriting.
	wrapped := "package _p\nfunc _f() {\n" + stmtSrc + "\n}\n"
	fset2 := token.NewFileSet()
	f, err := parser.ParseFile(fset2, "", wrapped, 0)
	if err != nil {
		// Fall back to naive replacement.
		out := stmtSrc
		for i, p := range params {
			out = strings.ReplaceAll(out, p, rendered[i])
		}
		return out, nil
	}
	mapping := map[string]string{}
	for i, p := range params {
		mapping[p] = rendered[i]
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if r, has := mapping[ident.Name]; has {
				ident.Name = r
			}
			return true
		})
	}
	// Pretty-print the body back out.
	if len(f.Decls) > 0 {
		fd, ok := f.Decls[0].(*ast.FuncDecl)
		if ok && fd.Body != nil && len(fd.Body.List) > 0 {
			var buf bytes.Buffer
			if err := format.Node(&buf, fset2, fd.Body.List[0]); err == nil {
				return buf.String(), nil
			}
		}
	}
	return stmtSrc, nil
}

func substituteIdentsInExpr(e ast.Expr, mapping map[string]ast.Expr) ast.Expr {
	if e == nil {
		return nil
	}
	rewritten := astutil.Apply(e, func(c *astutil.Cursor) bool {
		ident, ok := c.Node().(*ast.Ident)
		if !ok {
			return true
		}
		if rep, has := mapping[ident.Name]; has {
			c.Replace(cloneExpr(rep))
			return false
		}
		return true
	}, nil)
	if expr, ok := rewritten.(ast.Expr); ok {
		return expr
	}
	return e
}

func cloneExpr(e ast.Expr) ast.Expr {
	if e == nil {
		return nil
	}
	// Re-render and re-parse for a structural clone. ast doesn't ship
	// a deep clone, so a print+parse round-trip is the simplest robust
	// option for the small expressions we handle here.
	fset := token.NewFileSet()
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, e); err != nil {
		return e
	}
	parsed, err := parser.ParseExpr(buf.String())
	if err != nil {
		return e
	}
	return parsed
}

func needsParens(e ast.Expr) bool {
	switch e.(type) {
	case *ast.BinaryExpr, *ast.UnaryExpr:
		return true
	}
	return false
}
