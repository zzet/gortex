package languages

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Qik / ABA QIK is the Sabre mid-office scripting dialect used under
// `.qik` (e.g. ITS.ABA.QIK SCRIPT trees). It is NOT classic QBasic.
//
// Scripts are imperative command lists. Each file is one script.
// Structural surface worth indexing:
//
//	label  'name'                 jump targets
//	goto   'name'                 jumps → call-like edge to a label
//	call   'script'               invoke another .qik script
//	build_local_data_item  name   local data slot
//	; comment / ' comment
//
// Control keywords (if / end_if / while / …) are flow, not symbols.
// No upstream tree-sitter grammar — regex only.
var (
	// label 'foo'  |  label foo
	qikLabelCmdRe = regexp.MustCompile(
		`(?im)^\s*label\s+('([^']+)'|"([^"]+)"|([A-Za-z_][\w.]*))`)
	// goto 'foo'   |  goto foo
	qikGotoRe = regexp.MustCompile(
		`(?im)^\s*goto\s+('([^']+)'|"([^"]+)"|([A-Za-z_][\w.]*))`)
	// call 'script' | call script  (optional trailing args ignored)
	qikCallScriptRe = regexp.MustCompile(
		`(?im)^\s*call\s+('([^']+)'|"([^"]+)"|([A-Za-z_][\w.]*))`)
	// build_local_data_item name …
	qikBuildLocalRe = regexp.MustCompile(
		`(?im)^\s*build_local_data_item\s+('([^']+)'|"([^"]+)"|([A-Za-z_][\w.]*))`)
	// Script header: ;* Script Name  : foo
	qikScriptNameRe = regexp.MustCompile(
		`(?im)^\s*;\*\s*Script\s+Name\s*:\s*(\S+)`)
)

// QikBasicExtractor extracts ABA/Sabre QIK (.qik) scripts using regex.
// Language id stays "qikbasic" for registry stability with the first
// registration; the dialect is ABA QIK, not Microsoft QBasic.
type QikBasicExtractor struct{}

func NewQikBasicExtractor() *QikBasicExtractor { return &QikBasicExtractor{} }

func (e *QikBasicExtractor) Language() string     { return "qikbasic" }
func (e *QikBasicExtractor) Extensions() []string { return []string{".qik"} }

func (e *QikBasicExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	lines := strings.Split(string(src), "\n")
	result := &parser.ExtractionResult{}

	endLine := len(lines)
	if endLine < 1 {
		endLine = 1
	}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: endLine,
		Language: "qikbasic",
	}
	result.Nodes = append(result.Nodes, fileNode)

	// Prefer header Script Name; else basename without extension.
	scriptName := qikHeaderScriptName(src)
	if scriptName == "" {
		base := filepath.Base(filePath)
		scriptName = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if scriptName != "" {
		sid := filePath + "::script:" + scriptName
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: sid, Kind: graph.KindFunction, Name: scriptName,
			FilePath: filePath, StartLine: 1, EndLine: endLine,
			Language: "qikbasic",
			Meta:     map[string]any{"qik_role": "script"},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: sid, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: 1,
		})
	}

	seen := make(map[string]bool)
	add := func(name string, kind graph.NodeKind, start int, role string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		id := filePath + "::" + name
		if seen[id] {
			return
		}
		seen[id] = true
		n := &graph.Node{
			ID: id, Kind: kind, Name: name,
			FilePath: filePath, StartLine: start, EndLine: start,
			Language: "qikbasic",
		}
		if role != "" {
			n.Meta = map[string]any{"qik_role": role}
		}
		result.Nodes = append(result.Nodes, n)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: start,
		})
	}

	for _, m := range qikLabelCmdRe.FindAllSubmatchIndex(src, -1) {
		name := qikQuotedGroup(src, m)
		line := lineAt(src, m[0])
		add(name, graph.KindVariable, line, "label")
	}
	for _, m := range qikBuildLocalRe.FindAllSubmatchIndex(src, -1) {
		name := qikQuotedGroup(src, m)
		line := lineAt(src, m[0])
		add(name, graph.KindVariable, line, "local_data_item")
	}

	// call → EdgeCalls to unresolved script (+ EdgeImports so cross-file
	// script graphs light up the same way includes do elsewhere).
	for _, m := range qikCallScriptRe.FindAllSubmatchIndex(src, -1) {
		target := qikQuotedGroup(src, m)
		if target == "" {
			continue
		}
		line := lineAt(src, m[0])
		caller := filePath + "::script:" + scriptName
		if scriptName == "" {
			caller = fileNode.ID
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: caller, To: "unresolved::" + target,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + target,
			Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		})
	}

	// goto → EdgeCalls toward a label name (same-file jump).
	for _, m := range qikGotoRe.FindAllSubmatchIndex(src, -1) {
		target := qikQuotedGroup(src, m)
		if target == "" {
			continue
		}
		line := lineAt(src, m[0])
		caller := filePath + "::script:" + scriptName
		if scriptName == "" {
			caller = fileNode.ID
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: caller, To: "unresolved::" + target,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		})
	}

	return result, nil
}

// qikQuotedGroup returns the first non-empty capture among the
// quote/bare alternatives used by the QIK command regexes. Match index
// layout: 0-1 full, 2-3 group1 (whole token), 4-5 single-quoted, 6-7
// double-quoted, 8-9 bare.
func qikQuotedGroup(src []byte, m []int) string {
	if len(m) < 10 {
		if len(m) >= 4 && m[2] >= 0 {
			return strings.TrimSpace(string(src[m[2]:m[3]]))
		}
		return ""
	}
	for _, pair := range [][2]int{{4, 5}, {6, 7}, {8, 9}, {2, 3}} {
		a, b := m[pair[0]], m[pair[1]]
		if a >= 0 && b > a {
			return strings.TrimSpace(string(src[a:b]))
		}
	}
	return ""
}

func qikHeaderScriptName(src []byte) string {
	m := qikScriptNameRe.FindSubmatch(src)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

var _ parser.Extractor = (*QikBasicExtractor)(nil)
