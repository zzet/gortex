package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// VB.NET is keyword-delimited and case-insensitive. Every container and
// callable closes with an explicit `End <keyword>` — `Class`/`End Class`,
// `Sub`/`End Sub`, `Function`/`End Function` — so block extents come from
// findKeywordBlockEnd rather than brace matching or indentation.
//
// Node identity follows csharp.go rather than the flat filePath+"::"+name
// used by the simpler regex extractors (abap.go, apex.go, al.go): VB and C#
// share the .NET member model, and a VB graph is queried alongside C# in the
// same store, so members are owner-qualified (`File::Type.Member`) and
// constructors land on `Type.<init>`. That also keeps overloads distinct —
// with a flat name-keyed ID, `Sub New` overloads and same-named members of
// two classes in one file collapse into a single node.
//
// Deliberately NOT emitted:
//   - Plain fields / `Dim` / `WithEvents` declarations. In real VB estates
//     these are dominated by designer-generated control fields (measured on
//     a 196-file estate: 1319 `WithEvents` lines, against the 2945
//     declaration nodes this extractor emits for those same files), which
//     would swamp the graph with low-value nodes.
//   - Bare parenthesised calls. VB uses `(` for array indexing, default
//     properties and conversions as well as invocation, so `foo(i)` is
//     genuinely ambiguous without types. Only qualified (`x.M(`) and
//     explicit (`Call M(`) invocations are emitted — a false CALLS edge
//     corrupts blast-radius queries far more than a missing one.
//
// Known limitation: a qualified expression is only *mostly* unambiguous.
// `New X.Y(...)` is filtered out and re-emitted as a type reference, but an
// indexed property access — `dt.Rows(0)`, `dr.Fields(i)` — is syntactically
// identical to a call and is still emitted as one. Measured on a 196-file
// estate, accessor-shaped names (Rows/Cells/Fields/Element/SelectedRows)
// account for a visible share of CALLS edges. Resolving that needs type
// information this extractor does not have; a hardcoded accessor blocklist
// was rejected as too framework-specific to be correct upstream.
var (
	vbNamespaceRe = regexp.MustCompile(`(?im)^[ \t]*Namespace[ \t]+([A-Za-z_][\w.]*)`)
	vbClassRe     = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend|Partial|MustInherit|NotInheritable|Shadows|Shared)[ \t]+)*Class[ \t]+([A-Za-z_]\w*)`)
	vbModuleRe    = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend)[ \t]+)*Module[ \t]+([A-Za-z_]\w*)`)
	vbStructRe    = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend|Partial)[ \t]+)*Structure[ \t]+([A-Za-z_]\w*)`)
	vbInterfaceRe = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend|Partial)[ \t]+)*Interface[ \t]+([A-Za-z_]\w*)`)
	vbEnumRe      = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend)[ \t]+)*Enum[ \t]+([A-Za-z_]\w*)`)

	// A Delegate is a type declaration, not a callable, and carries no body.
	vbDelegateRe = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend|Shadows)[ \t]+)*Delegate[ \t]+(?:Sub|Function)[ \t]+([A-Za-z_]\w*)`)
	// A Declare is a P/Invoke entry point — bodyless, external.
	vbDeclareRe = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend|Shared)[ \t]+)*Declare[ \t]+(?:(?:Auto|Ansi|Unicode)[ \t]+)?(?:Sub|Function)[ \t]+([A-Za-z_]\w*)`)
	vbEventRe   = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend|Shadows)[ \t]+)*(?:Custom[ \t]+)?Event[ \t]+([A-Za-z_]\w*)`)

	// Sub / Function / Property. `Declare` and `Delegate` forms are matched by
	// their own patterns above and filtered out here by requiring the keyword
	// to sit directly after the modifier run.
	vbSubRe      = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend|Shared|Overrides|Overridable|MustOverride|NotOverridable|Shadows|Partial|Async|Iterator)[ \t]+)*Sub[ \t]+([A-Za-z_]\w*)`)
	vbFunctionRe = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend|Shared|Overrides|Overridable|MustOverride|NotOverridable|Shadows|Partial|Async|Iterator)[ \t]+)*Function[ \t]+([A-Za-z_]\w*)`)
	vbPropertyRe = regexp.MustCompile(`(?im)^[ \t]*(?:(?:Public|Private|Protected|Friend|Shared|Overrides|Overridable|MustOverride|NotOverridable|Shadows|ReadOnly|WriteOnly|Default|Iterator)[ \t]+)*Property[ \t]+([A-Za-z_]\w*)`)

	vbImportsRe    = regexp.MustCompile(`(?im)^[ \t]*Imports[ \t]+(?:[A-Za-z_]\w*[ \t]*=[ \t]*)?([A-Za-z_][\w.]*)`)
	vbInheritsRe   = regexp.MustCompile(`(?im)^[ \t]*Inherits[ \t]+([A-Za-z_][\w.]*)`)
	vbImplementsRe = regexp.MustCompile(`(?im)^[ \t]*Implements[ \t]+([A-Za-z_][\w.(),\t ]*)`)

	// Qualified invocation: the receiver chain is discarded and the final
	// member name becomes the callee, matching how razor.go reduces a dotted
	// directive type to its last segment for the resolver's bare-name bind.
	vbQualifiedCallRe = regexp.MustCompile(`[A-Za-z_)\]]\s*\.\s*([A-Za-z_]\w*)\s*\(`)
	// Explicit `Call Foo(...)` / `Call obj.Foo(...)` statement.
	vbCallStmtRe = regexp.MustCompile(`(?im)^[ \t]*Call[ \t]+(?:[\w.]*\.)?([A-Za-z_]\w*)[ \t]*\(`)
	// `New System.Drawing.Size(...)` is a type instantiation, not a call to a
	// member named Size. Matched first so its span can be excluded from the
	// call scan and emitted as a type reference instead — otherwise designer
	// code alone contributes thousands of bogus CALLS edges.
	vbNewRe = regexp.MustCompile(`(?i)\bNew[ \t]+([A-Za-z_][\w.]*)`)

	vbVisibilityRe = regexp.MustCompile(`(?i)^[ \t]*(Public|Private|Protected|Friend)\b`)
)

// vbRange is one container (namespace or type) with its line extent, used to
// attribute members to an owner and to stamp the enclosing namespace.
type vbRange struct {
	name  string
	id    string
	kind  graph.NodeKind
	start int
	end   int
}

// VBNetExtractor extracts VB.NET source using regex.
type VBNetExtractor struct{}

func NewVBNetExtractor() *VBNetExtractor { return &VBNetExtractor{} }

func (e *VBNetExtractor) Language() string     { return "vbnet" }
func (e *VBNetExtractor) Extensions() []string { return []string{".vb"} }

func (e *VBNetExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	lines := strings.Split(string(src), "\n")
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: len(lines),
		Language: "vbnet",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := map[string]bool{filePath: true}

	// add emits a node plus its file->node DEFINES edge, disambiguating the ID
	// so overloads and repeated member names stay distinct.
	add := func(baseID, name string, kind graph.NodeKind, start, end int, meta map[string]any) string {
		if name == "" {
			return ""
		}
		id, ok := disambiguateID(seen, baseID, start)
		if !ok {
			return ""
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: name,
			FilePath: filePath, StartLine: start, EndLine: end,
			Language: "vbnet", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: start,
		})
		return id
	}

	// --- Pass 1: containers -------------------------------------------------
	// Namespaces first so a type can be stamped with its scope_ns, then types
	// so members can be attributed to an owner.
	var namespaces []vbRange
	for _, m := range vbNamespaceRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		end := vbContainerEnd(lines, line, vbNamespaceRe, "end namespace")
		id := add(filePath+"::"+name, name, graph.KindPackage, line, end, nil)
		if id != "" {
			namespaces = append(namespaces, vbRange{name: name, id: id, start: line, end: end})
		}
	}

	// enclosingNS returns the innermost namespace covering line, or "".
	enclosingNS := func(line int) string {
		best, bestSpan := "", int(^uint(0)>>1)
		for _, ns := range namespaces {
			if line < ns.start || line > ns.end {
				continue
			}
			if span := ns.end - ns.start; span < bestSpan {
				best, bestSpan = ns.name, span
			}
		}
		return best
	}

	var types []vbRange
	for _, spec := range []struct {
		re     *regexp.Regexp
		endKw  string
		kind   graph.NodeKind
		flavor string
	}{
		{vbClassRe, "end class", graph.KindType, "class"},
		{vbModuleRe, "end module", graph.KindModule, "module"},
		{vbStructRe, "end structure", graph.KindType, "structure"},
		{vbInterfaceRe, "end interface", graph.KindInterface, "interface"},
		{vbEnumRe, "end enum", graph.KindType, "enum"},
	} {
		for _, m := range spec.re.FindAllSubmatchIndex(src, -1) {
			name := string(src[m[2]:m[3]])
			line := lineAt(src, m[0])
			end := vbContainerEnd(lines, line, spec.re, spec.endKw)
			meta := map[string]any{"type_flavor": spec.flavor}
			if ns := enclosingNS(line); ns != "" {
				meta["scope_ns"] = ns
			}
			if vis := vbVisibility(lines, line); vis != "" {
				meta["visibility"] = vis
			}
			id := add(filePath+"::"+name, name, spec.kind, line, end, meta)
			if id != "" {
				types = append(types, vbRange{
					name: name, id: id, kind: spec.kind, start: line, end: end,
				})
			}
		}
	}

	// Delegates and P/Invoke declarations are bodyless: they occupy one logical
	// line, so their extent is the declaration line itself.
	for _, m := range vbDelegateRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		add(filePath+"::"+name, name, graph.KindType, line, line,
			map[string]any{"type_flavor": "delegate"})
	}

	// --- Pass 2: members ----------------------------------------------------
	// Interface method names, keyed by the interface's node ID (not its name --
	// two interfaces in one file can share a name and still be distinct nodes).
	// Stamped onto Meta["methods"] below, which is what the resolver's
	// InferImplements reads to derive an interface's method set.
	ifaceMethods := map[string][]string{}
	// ownerAt returns the innermost type covering line. Members inside a type
	// become methods owned by it; members at file scope stay free functions,
	// which is the normal shape for VB's script-style top-level code.
	ownerAt := func(line int) *vbRange {
		var best *vbRange
		bestSpan := int(^uint(0) >> 1)
		for i := range types {
			t := &types[i]
			if line < t.start || line > t.end {
				continue
			}
			if span := t.end - t.start; span < bestSpan {
				best, bestSpan = t, span
			}
		}
		return best
	}

	// emitCallable places a Sub/Function/Property/Event, owner-qualifying and
	// emitting MEMBER_OF when it sits inside a type.
	emitCallable := func(name string, line, end int, freeKind, memberKind graph.NodeKind,
		extra map[string]any) {
		owner := ownerAt(line)
		meta := map[string]any{}
		for k, v := range extra {
			meta[k] = v
		}
		if ns := enclosingNS(line); ns != "" {
			meta["scope_ns"] = ns
		}
		if vis := vbVisibility(lines, line); vis != "" {
			meta["visibility"] = vis
		}
		baseID, nodeName, kind := filePath+"::"+name, name, freeKind
		if owner != nil {
			kind = memberKind
			meta["receiver"] = owner.name
			// A constructor is keyed on <init> so it is addressable per type
			// rather than as one of many nodes all named "New".
			if strings.EqualFold(name, "New") {
				nodeName = owner.name + ".<init>"
				baseID = filePath + "::" + owner.name + ".<init>"
			} else {
				baseID = filePath + "::" + owner.name + "." + name
			}
		}
		id := add(baseID, nodeName, kind, line, end, meta)
		if id != "" && owner != nil {
			result.Edges = append(result.Edges, &graph.Edge{
				From: id, To: owner.id, Kind: graph.EdgeMemberOf,
				FilePath: filePath, Line: line,
			})
			// Only Sub/Function count as the interface's method set; a
			// Property is emitted as a field and an Event as an event.
			if kind == graph.KindMethod && owner.kind == graph.KindInterface {
				ifaceMethods[owner.id] = append(ifaceMethods[owner.id], name)
			}
		}
	}

	for _, spec := range []struct {
		re         *regexp.Regexp
		endKw      string
		freeKind   graph.NodeKind
		memberKind graph.NodeKind
		skip       *regexp.Regexp
	}{
		{vbSubRe, "end sub", graph.KindFunction, graph.KindMethod, vbDeclareRe},
		{vbFunctionRe, "end function", graph.KindFunction, graph.KindMethod, vbDeclareRe},
		{vbPropertyRe, "end property", graph.KindField, graph.KindField, nil},
		{vbEventRe, "end event", graph.KindEvent, graph.KindEvent, nil},
	} {
		for _, m := range spec.re.FindAllSubmatchIndex(src, -1) {
			line := lineAt(src, m[0])
			// A `Declare Sub` also satisfies vbSubRe's modifier run; skip it
			// here so it is emitted once, by the Declare pass below.
			if spec.skip != nil && spec.skip.MatchString(lines[line-1]) {
				continue
			}
			name := string(src[m[2]:m[3]])
			// Interface members, MustOverride members and auto-properties have
			// no terminator; findKeywordBlockEnd returns the start line, which
			// is the correct single-line extent.
			end := findKeywordBlockEnd(lines, line, spec.endKw)
			emitCallable(name, line, end, spec.freeKind, spec.memberKind, nil)
		}
	}

	// A Declare is bodyless, so its extent is its own line. VB requires it
	// inside a Class, Structure or Module, so it goes through the member
	// path to pick up its owner; the free-function arm only catches the
	// invalid-but-parseable case of one at file scope.
	for _, m := range vbDeclareRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		emitCallable(name, line, line, graph.KindFunction, graph.KindMethod,
			map[string]any{"external": true})
	}

	// Stamp interface method names onto interface nodes' Meta["methods"],
	// matching csharp.go so a VB interface supports the same IMPLEMENTS
	// inference as a C# one.
	for _, n := range result.Nodes {
		if n.Kind != graph.KindInterface {
			continue
		}
		if methods, ok := ifaceMethods[n.ID]; ok {
			if n.Meta == nil {
				n.Meta = make(map[string]any)
			}
			n.Meta["methods"] = methods
		}
	}

	// --- Imports, inheritance, implementations ------------------------------
	for _, m := range vbImportsRe.FindAllSubmatchIndex(src, -1) {
		ns := string(src[m[2]:m[3]])
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + ns,
			Kind: graph.EdgeImports, FilePath: filePath, Line: lineAt(src, m[0]),
		})
	}
	// `Inherits`/`Implements` follow the type they belong to, so the enclosing
	// type is the innermost one covering the clause's line.
	for _, m := range vbInheritsRe.FindAllSubmatchIndex(src, -1) {
		line := lineAt(src, m[0])
		from := fileNode.ID
		if owner := ownerAt(line); owner != nil {
			from = owner.id
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: from, To: "unresolved::" + vbSimpleName(string(src[m[2]:m[3]])),
			Kind: graph.EdgeReferences, FilePath: filePath, Line: line,
		})
	}
	for _, m := range vbImplementsRe.FindAllSubmatchIndex(src, -1) {
		line := lineAt(src, m[0])
		from := fileNode.ID
		if owner := ownerAt(line); owner != nil {
			from = owner.id
		}
		// `Implements IA, IB` names several interfaces on one clause.
		for _, part := range strings.Split(string(src[m[2]:m[3]]), ",") {
			if n := vbSimpleName(strings.TrimSpace(part)); n != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From: from, To: "unresolved::" + n,
					Kind: graph.EdgeImplements, FilePath: filePath, Line: line,
				})
			}
		}
	}

	// --- Instantiations -----------------------------------------------------
	// `New X.Y(...)` names a type; the trailing `(` is a constructor argument
	// list, not an invocation of a member called Y. Each span is recorded so
	// the call scan below can skip it, and emitted as a type reference.
	type vbSpan struct{ start, end int }
	var newSpans []vbSpan
	// Comments and string literals are blanked first so call-shaped text in
	// them (`' obj.Delete()`, `"x.Drop("`) is not scanned. The masked copy
	// keeps every byte offset, so names and lines are still read from src.
	code := vbMaskCommentsAndStrings(src)
	for _, m := range vbNewRe.FindAllSubmatchIndex(code, -1) {
		newSpans = append(newSpans, vbSpan{start: m[2], end: m[3]})
		if n := vbSimpleName(string(src[m[2]:m[3]])); n != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: "unresolved::" + n,
				Kind: graph.EdgeReferences, FilePath: filePath, Line: lineAt(src, m[0]),
			})
		}
	}
	inNewSpan := func(pos int) bool {
		for _, s := range newSpans {
			if pos >= s.start && pos < s.end {
				return true
			}
		}
		return false
	}

	// --- Calls --------------------------------------------------------------
	funcRanges := buildFuncRanges(result)
	emitCall := func(name string, line int) {
		callerID := findEnclosingFunc(funcRanges, line)
		if callerID == "" || strings.HasSuffix(callerID, "."+name) ||
			strings.HasSuffix(callerID, "::"+name) {
			return
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		})
	}
	for _, m := range vbQualifiedCallRe.FindAllSubmatchIndex(code, -1) {
		if inNewSpan(m[2]) {
			continue // `New X.Y(` — already emitted as a type reference
		}
		emitCall(string(src[m[2]:m[3]]), lineAt(src, m[0]))
	}
	for _, m := range vbCallStmtRe.FindAllSubmatchIndex(code, -1) {
		emitCall(string(src[m[2]:m[3]]), lineAt(src, m[0]))
	}

	return result, nil
}

// vbMaskCommentsAndStrings returns a copy of src in which comments and
// string literals are blanked to spaces. Newlines are kept, so the copy has
// the same length and every offset and line number still indexes src.
//
// Comments are `'` to end of line, and `REM` when it opens a statement (at
// line start or after a `:` separator). String literals are `"..."` with
// `""` as the escaped quote, and may span lines as they can since VB 14. In
// an interpolated string `$"..."`, the `{...}` holes are code and stay
// visible, except for a `:format` clause, which is blanked with the text.
func vbMaskCommentsAndStrings(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	const (
		inCode = iota
		inString
		inInterpolated
		inFormat
	)
	mode := inCode
	// holes holds the `{` nesting depth of each open interpolation hole,
	// innermost last. Code inside a hole may use braces of its own.
	var holes []int
	stmtStart := true
	for i := 0; i < len(src); i++ {
		c := src[i]
		next := byte(0)
		if i+1 < len(src) {
			next = src[i+1]
		}
		switch mode {
		case inString, inInterpolated:
			switch {
			case c == '"' && next == '"', mode == inInterpolated && c == '{' && next == '{':
				blank(i)
				blank(i + 1)
				i++
			case c == '"':
				blank(i)
				mode = inCode
			case mode == inInterpolated && c == '{':
				blank(i)
				holes = append(holes, 0)
				mode = inCode
			default:
				blank(i)
			}
		case inFormat:
			blank(i)
			if c == '}' {
				mode = inInterpolated
			}
		default:
			top := len(holes) - 1
			atStart := stmtStart
			stmtStart = c == '\n' || c == ':' ||
				(stmtStart && (c == ' ' || c == '\t' || c == '\r'))
			switch {
			case c == '\'' || (atStart && vbIsRem(src, i)):
				for ; i < len(src) && src[i] != '\n'; i++ {
					blank(i)
				}
				i-- // leave the newline for the next iteration
			case c == '"':
				blank(i)
				mode = inString
			case c == '$' && next == '"':
				blank(i)
				blank(i + 1)
				i++
				mode = inInterpolated
			case top >= 0 && holes[top] == 0 && (c == '}' || (c == ':' && next != '=')):
				blank(i)
				holes = holes[:top]
				stmtStart = false
				mode = inInterpolated
				if c == ':' {
					mode = inFormat
				}
			case top >= 0 && c == '{':
				holes[top]++
			case top >= 0 && c == '}':
				holes[top]--
			}
		}
	}
	return out
}

// vbIsRem reports whether a case-insensitive REM keyword starts at i and is
// followed by whitespace or the end of input.
func vbIsRem(src []byte, i int) bool {
	if i+3 > len(src) || !strings.EqualFold(string(src[i:i+3]), "rem") {
		return false
	}
	return i+3 == len(src) || src[i+3] == ' ' || src[i+3] == '\t' ||
		src[i+3] == '\r' || src[i+3] == '\n'
}

// vbContainerEnd returns the line of the terminator that closes the
// container opened at startLine, counting nesting depth rather than
// stopping at the first match. findKeywordBlockEnd cannot be used here: a
// nested `Class Inner ... End Class` would end the OUTER class early, and
// every member declared after it would lose its owner and its MEMBER_OF
// edge. Namespaces nest the same way, and a truncated one drops scope_ns
// from every type declared after the inner block. Only the same keyword
// pair is counted, so a Structure nested in a Class does not perturb the
// Class depth.
//
// Returns startLine when no terminator is found, matching
// findKeywordBlockEnd's degradation for an unterminated block.
func vbContainerEnd(lines []string, startLine int, openRe *regexp.Regexp, endKw string) int {
	if startLine < 1 || startLine > len(lines) {
		return startLine
	}
	depth := 1
	for i := startLine; i < len(lines); i++ {
		trimmedLine := trimmed(lines[i])
		if trimmedLine == "" {
			continue
		}
		// Terminator first: "End Class" never satisfies the opening pattern
		// (End is not a modifier), but checking it first keeps that
		// independent of the pattern's exact shape.
		if hasPrefixWord(toLower(trimmedLine), endKw) {
			depth--
			if depth == 0 {
				return i + 1
			}
			continue
		}
		if openRe.MatchString(lines[i]) {
			depth++
		}
	}
	return startLine
}

// vbVisibility reads the access modifier off a declaration line. VB defaults
// differ by container, so an absent modifier is reported as "" rather than
// guessed.
func vbVisibility(lines []string, line int) string {
	if line < 1 || line > len(lines) {
		return ""
	}
	m := vbVisibilityRe.FindStringSubmatch(lines[line-1])
	if m == nil {
		return ""
	}
	switch strings.ToLower(m[1]) {
	case "public":
		return VisibilityPublic
	case "private":
		return VisibilityPrivate
	case "protected":
		return VisibilityProtected
	case "friend":
		return VisibilityInternal
	}
	return ""
}

// vbSimpleName reduces a dotted, optionally generic type reference to the bare
// name the resolver binds against: `System.Web.UI.Page` -> `Page`,
// `List(Of String)` -> `List`.
func vbSimpleName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

var _ parser.Extractor = (*VBNetExtractor)(nil)
