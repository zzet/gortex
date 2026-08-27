package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// COBOL extraction captures PROGRAM-ID, DIVISION and SECTION headers,
// `CALL 'name'` subprogram calls, and `COPY name` library imports. Inside
// the PROCEDURE DIVISION it also captures paragraph labels (bare names in
// area A) and the PERFORM / GO TO control-flow edges between them, which
// form the intra-program call graph.
var (
	cobolProgIDRe = regexp.MustCompile(`(?im)^\s*PROGRAM-ID\.\s*(\w[\w-]*)`)
	// A division header ends at a word boundary, not at a period: the
	// PROCEDURE DIVISION takes an optional USING / CHAINING / RETURNING
	// phrase, so the period lands after the parameter list --
	// `PROCEDURE DIVISION USING LK-PARM.`. Requiring `DIVISION\.` missed
	// every such header, and since extractProcedure gates on this regex to
	// set inProcedure, the whole procedure division was skipped: no
	// paragraphs, no PERFORM edges, no GO TO edges.
	cobolDivRe     = regexp.MustCompile(`(?im)^\s*([A-Z][\w-]*)\s+DIVISION\b`)
	cobolSectionRe = regexp.MustCompile(`(?im)^\s*([A-Z][\w-]*)\s+SECTION\.`)
	cobolCallRe    = regexp.MustCompile(`(?im)\bCALL\s+["']([^"']+)["']`)
	cobolCopyRe    = regexp.MustCompile(`(?im)\bCOPY\s+(\w[\w-]*)`)

	// A paragraph header is a lone name (optionally a trailing period) on
	// its own line in area A. We match it after stripping leading
	// indentation; section/division headers and reserved verbs are
	// filtered out separately.
	cobolParaRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]*)\.?\s*$`)
	// PERFORM <name> [THRU|THROUGH <name2>] — the inline/out-of-line
	// performed paragraph(s). Plain `PERFORM` (no operand, in-line body
	// terminated by END-PERFORM) yields no submatch and is ignored.
	cobolPerformRe = regexp.MustCompile(`(?i)\bPERFORM\s+([A-Za-z0-9][A-Za-z0-9-]*)(?:\s+(?:THRU|THROUGH)\s+([A-Za-z0-9][A-Za-z0-9-]*))?`)
	// GO TO <name> — an unconditional branch to a paragraph/section.
	cobolGoToRe = regexp.MustCompile(`(?i)\bGO\s+TO\s+([A-Za-z0-9][A-Za-z0-9-]*)`)
)

// cobolReservedHeads are tokens that can appear alone on a line but are
// not paragraph labels — paragraph detection skips them. PERFORM-target
// keywords (TIMES, UNTIL, …) only matter mid-statement and never appear
// as lone area-A labels, so the set stays deliberately small.
var cobolReservedHeads = map[string]bool{
	"STOP": true, "EXIT": true, "CONTINUE": true, "GOBACK": true,
	"END-PERFORM": true, "END-IF": true, "END-EVALUATE": true,
	"END-READ": true, "END-CALL": true, "ELSE": true,
}

// CobolExtractor extracts COBOL source using regex.
type CobolExtractor struct{}

func NewCobolExtractor() *CobolExtractor { return &CobolExtractor{} }

func (e *CobolExtractor) Language() string { return "cobol" }
func (e *CobolExtractor) Extensions() []string {
	return []string{".cob", ".cbl", ".cpy", ".COB", ".CBL", ".CPY"}
}

func (e *CobolExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	lines := strings.Split(string(src), "\n")
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: len(lines),
		Language: "cobol",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	add := func(name string, kind graph.NodeKind, start, end int, meta map[string]any) {
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
			Language: "cobol", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: start,
		})
	}

	// progID is the program node ID once seen; it acts as the default
	// caller for PERFORM/GO TO edges emitted before any paragraph.
	progID := ""
	for _, m := range cobolProgIDRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		add(name, graph.KindFunction, line, len(lines), nil)
		if progID == "" {
			progID = filePath + "::" + name
		}
	}
	for _, m := range cobolDivRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]]) + "-DIVISION"
		line := lineAt(src, m[0])
		add(name, graph.KindType, line, line, nil)
	}
	for _, m := range cobolSectionRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]]) + "-SECTION"
		line := lineAt(src, m[0])
		add(name, graph.KindMethod, line, line, nil)
	}

	for _, m := range cobolCopyRe.FindAllSubmatchIndex(src, -1) {
		mod := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + mod,
			Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		})
	}
	for _, m := range cobolCallRe.FindAllSubmatchIndex(src, -1) {
		target := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::" + target,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		})
	}

	// Second walk: line-based, to find paragraphs and PERFORM/GO TO edges
	// once we are inside the PROCEDURE DIVISION. Paragraph nodes resolve
	// by name-derived ID (filePath+"::"+name), so PERFORM edges to a
	// paragraph defined later in the file are emitted by ID without a
	// second pass.
	e.extractProcedure(filePath, lines, fileNode.ID, progID, add, result)

	return result, nil
}

// extractProcedure scans line-by-line for paragraph labels and the
// PERFORM / GO TO control-flow edges between them. It is conservative:
// a line is only treated as a paragraph header when its sole content is a
// bare name (with optional trailing period), it sits in area A, it is not
// a DIVISION/SECTION header, and it is not a reserved verb.
func (e *CobolExtractor) extractProcedure(
	filePath string,
	lines []string,
	fileID, progID string,
	add func(name string, kind graph.NodeKind, start, end int, meta map[string]any),
	result *parser.ExtractionResult,
) {
	inProcedure := false
	curCaller := progID // most-recent paragraph ID, or the program node
	if curCaller == "" {
		curCaller = fileID
	}

	for i, raw := range lines {
		lineNo := i + 1
		body := cobolStripLine(raw)
		if body == "" {
			continue
		}
		upper := strings.ToUpper(body)

		if !inProcedure {
			if cobolDivRe.MatchString(raw) && strings.HasPrefix(upper, "PROCEDURE ") {
				inProcedure = true
			}
			continue
		}

		// A new DIVISION ends the PROCEDURE DIVISION scope.
		if cobolDivRe.MatchString(raw) {
			inProcedure = false
			continue
		}

		// Paragraph-header detection: a lone name in area A. Section
		// headers are filtered (they carry the SECTION keyword); reserved
		// verbs are filtered via cobolReservedHeads.
		if e.isAreaA(raw) && !cobolSectionRe.MatchString(raw) {
			if pm := cobolParaRe.FindStringSubmatch(body); pm != nil {
				name := pm[1]
				head := strings.ToUpper(name)
				if !cobolReservedHeads[head] {
					add(name, graph.KindFunction, lineNo, lineNo,
						map[string]any{"cobol_kind": "paragraph"})
					curCaller = filePath + "::" + name
					continue
				}
			}
		}

		// PERFORM edges from the enclosing paragraph (or program).
		for _, pm := range cobolPerformRe.FindAllStringSubmatch(body, -1) {
			e.addCall(result, curCaller, filePath+"::"+pm[1], filePath, lineNo)
			if pm[2] != "" { // THRU/THROUGH second operand
				e.addCall(result, curCaller, filePath+"::"+pm[2], filePath, lineNo)
			}
		}
		// GO TO edges.
		for _, gm := range cobolGoToRe.FindAllStringSubmatch(body, -1) {
			e.addCall(result, curCaller, filePath+"::"+gm[1], filePath, lineNo)
		}
	}
}

func (e *CobolExtractor) addCall(result *parser.ExtractionResult, from, to, filePath string, line int) {
	result.Edges = append(result.Edges, &graph.Edge{
		From: from, To: to, Kind: graph.EdgeCalls,
		FilePath: filePath, Line: line,
	})
}

// cobolStripLine returns the meaningful content of a COBOL source line:
// it drops fixed-format sequence numbers (cols 1-6) and the area beyond
// col 72, honours the indicator column (col 7) for comment ('*'/'/') and
// continuation lines, and trims surrounding whitespace. Free-format
// source (no sequence area) is handled by the whitespace trim.
func cobolStripLine(raw string) string {
	line := strings.TrimRight(raw, "\r")
	// Fixed-format: indicator is column 7 (index 6). A '*' or '/' there
	// (or anywhere when the line is short) marks a comment line.
	if len(line) >= 7 {
		ind := line[6]
		if ind == '*' || ind == '/' {
			return ""
		}
		// Drop sequence area (cols 1-6) and clip the identification area
		// (col 73+) when the line is in fixed format.
		area := line[7:]
		if len(area) > 65 {
			area = area[:65]
		}
		return strings.TrimSpace(area)
	}
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/") {
		return ""
	}
	return t
}

// isAreaA reports whether a line's first non-blank content begins in
// area A (cols 8-11, indices 7-10 in fixed format) or — for free-format
// source — at a small indentation. Paragraph labels must start in area
// A; statements live in area B (col 12+). The check tolerates both
// formats by accepting a leading-blank count of at most 11.
func (e *CobolExtractor) isAreaA(raw string) bool {
	line := strings.TrimRight(raw, "\r")
	indent := 0
	for indent < len(line) && (line[indent] == ' ' || line[indent] == '\t') {
		indent++
	}
	if indent >= len(line) {
		return false
	}
	// Fixed format: sequence area (0-5), indicator (6), area A (7-10),
	// area B (11+). Accept labels whose first column is within area A.
	// Free format: accept a shallow indent (<= 11) as area-A-equivalent.
	return indent <= 11
}

var _ parser.Extractor = (*CobolExtractor)(nil)
