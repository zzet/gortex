package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Qik Basic is a QBasic/QuickBASIC-shaped dialect stored under the
// `.qik` extension. Keyword-delimited, case-insensitive. Procedures
// are `SUB name ... END SUB`, value-returning procedures are
// `FUNCTION name ... END FUNCTION`, legacy one-liners are `DEF FN`,
// user types are `TYPE name ... END TYPE`, forward decls are
// `DECLARE {SUB|FUNCTION}`, and modules are pulled in via
// `$INCLUDE 'path'` / `INCLUDE "path"`. Calls are `CALL name`, bare
// `name args` after DECLARE, and `GOSUB label`.
//
// No upstream tree-sitter grammar exists for this dialect, so
// extraction is regex-only (signature + import + call edges).
var (
	qikSubRe = regexp.MustCompile(
		`(?im)^\s*SUB\s+([A-Za-z][\w.]*)\s*(?:\(|STATIC\b|$)`)
	qikFunctionRe = regexp.MustCompile(
		`(?im)^\s*FUNCTION\s+([A-Za-z][\w.%&!#$]*)\s*(?:\(|STATIC\b|$)`)
	qikDefFnRe = regexp.MustCompile(
		`(?im)^\s*DEF\s+FN([A-Za-z][\w.%&!#$]*)`)
	qikTypeRe = regexp.MustCompile(
		`(?im)^\s*TYPE\s+([A-Za-z][\w.]*)`)
	qikDeclareRe = regexp.MustCompile(
		`(?im)^\s*DECLARE\s+(?:SUB|FUNCTION)\s+([A-Za-z][\w.%&!#$]*)`)
	qikIncludeRe = regexp.MustCompile(
		`(?im)^\s*(?:\$?INCLUDE)\s+['"]([^'"]+)['"]`)
	// CALL name[(...)] — classic explicit call form.
	qikCallRe = regexp.MustCompile(
		`(?im)^\s*CALL\s+([A-Za-z][\w.]*)`)
	// GOSUB label — legacy subroutine jump; treated as a call edge.
	qikGosubRe = regexp.MustCompile(
		`(?im)^\s*GOSUB\s+([A-Za-z][\w.]*)`)
	// Line label definitions: Label: at column start (not SUB/FUNCTION).
	qikLabelRe = regexp.MustCompile(
		`(?im)^\s*([A-Za-z][\w.]*)\s*:`)
)

// reservedQikLabels are statement keywords that can look like labels
// when a trailing colon is used as a statement separator (e.g. IF x THEN:).
var reservedQikLabels = map[string]bool{
	"if": true, "for": true, "do": true, "while": true, "select": true,
	"case": true, "else": true, "elseif": true, "end": true, "next": true,
	"loop": true, "wend": true, "sub": true, "function": true, "type": true,
	"declare": true, "def": true, "dim": true, "const": true, "static": true,
	"shared": true, "common": true, "call": true, "gosub": true, "goto": true,
	"return": true, "exit": true, "print": true, "input": true, "let": true,
	"rem": true, "data": true, "read": true, "restore": true, "on": true,
	"error": true, "resume": true, "open": true, "close": true, "with": true,
}

// QikBasicExtractor extracts Qik Basic (.qik) source using regex.
type QikBasicExtractor struct{}

func NewQikBasicExtractor() *QikBasicExtractor { return &QikBasicExtractor{} }

func (e *QikBasicExtractor) Language() string     { return "qikbasic" }
func (e *QikBasicExtractor) Extensions() []string { return []string{".qik"} }

func (e *QikBasicExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	lines := strings.Split(string(src), "\n")
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: len(lines),
		Language: "qikbasic",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	add := func(name string, kind graph.NodeKind, start, end int) {
		name = stripQikTypeSuffix(name)
		if name == "" {
			return
		}
		id := filePath + "::" + name
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: name,
			FilePath: filePath, StartLine: start, EndLine: end,
			Language: "qikbasic",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: start,
		})
	}

	for _, m := range qikSubRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		end := findKeywordBlockEnd(lines, line, "end sub")
		add(name, graph.KindFunction, line, end)
	}
	for _, m := range qikFunctionRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		end := findKeywordBlockEnd(lines, line, "end function")
		add(name, graph.KindFunction, line, end)
	}
	for _, m := range qikDefFnRe.FindAllSubmatchIndex(src, -1) {
		name := "FN" + string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		// DEF FN is often one line; END DEF closes multi-line forms.
		end := findKeywordBlockEnd(lines, line, "end def")
		add(name, graph.KindFunction, line, end)
	}
	for _, m := range qikTypeRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		end := findKeywordBlockEnd(lines, line, "end type")
		add(name, graph.KindType, line, end)
	}
	// DECLARE is a forward signature only — record as a variable so the
	// name is searchable without inventing a second function body node
	// when the real SUB/FUNCTION also lives in this file.
	for _, m := range qikDeclareRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		idName := stripQikTypeSuffix(name)
		if idName == "" {
			continue
		}
		// Skip when the body already defines the same name.
		if seen[filePath+"::"+idName] {
			continue
		}
		add(name, graph.KindVariable, line, line)
	}
	for _, m := range qikLabelRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		if reservedQikLabels[strings.ToLower(name)] {
			continue
		}
		line := lineAt(src, m[0])
		// Labels are jump targets, not full procedures — variable kind.
		add(name, graph.KindVariable, line, line)
	}

	for _, m := range qikIncludeRe.FindAllSubmatchIndex(src, -1) {
		mod := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + mod,
			Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		})
	}

	funcRanges := buildFuncRanges(result)
	emitCall := func(name string, pos int) {
		name = stripQikTypeSuffix(name)
		if name == "" {
			return
		}
		line := lineAt(src, pos)
		callerID := findEnclosingFunc(funcRanges, line)
		if callerID == "" || strings.HasSuffix(callerID, "::"+name) {
			// Attribute module-level calls to the file node.
			if callerID == "" {
				callerID = fileNode.ID
			} else {
				return
			}
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		})
	}
	for _, m := range qikCallRe.FindAllSubmatchIndex(src, -1) {
		emitCall(string(src[m[2]:m[3]]), m[0])
	}
	for _, m := range qikGosubRe.FindAllSubmatchIndex(src, -1) {
		emitCall(string(src[m[2]:m[3]]), m[0])
	}

	return result, nil
}

// stripQikTypeSuffix drops BASIC type-declaration characters
// (%, &, !, #, $) so FUNCTION Foo$ and CALL Foo share one symbol id.
func stripQikTypeSuffix(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	switch name[len(name)-1] {
	case '%', '&', '!', '#', '$':
		return name[:len(name)-1]
	}
	return name
}

var _ parser.Extractor = (*QikBasicExtractor)(nil)
