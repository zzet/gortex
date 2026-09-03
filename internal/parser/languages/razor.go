package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

var (
	// razorBlockRe matches a C# block opener: @code{ / @functions{ (group 1
	// names the keyword) or a bare @{ statement block (group 1 empty).
	razorBlockRe  = regexp.MustCompile(`@(code|functions)\s*\{|@\{`)
	razorModelRe  = regexp.MustCompile(`(?m)^\s*@(?:model|inherits)\s+([A-Za-z_][\w.]*(?:<[^>]*>)?)`)
	razorInjectRe = regexp.MustCompile(`(?m)^\s*@inject\s+([A-Za-z_][\w.]*(?:<[^>]*>)?)\s+\w+`)
	razorTypeofRe = regexp.MustCompile(`@typeof\(\s*([A-Za-z_][\w.]*)`)
	// razorUsingRe matches an `@using Some.Namespace` directive (optionally
	// `@using static`, whose member-import we skip). The captured namespace
	// feeds the resolver's import cascade.
	razorUsingRe = regexp.MustCompile(`(?m)^\s*@using\s+(?:static\s+)?([A-Za-z_][\w.]*)`)
)

// RazorExtractor extracts Razor / Blazor files (.razor, .cshtml). It carves
// every @code{...} / @functions{...} block and delegates it to the C# extractor
// (rebased into host-file coordinates), and emits type references for the
// @model / @inherits / @inject directives.
type RazorExtractor struct {
	cs *CSharpExtractor
}

// NewRazorExtractor constructs a Razor extractor.
func NewRazorExtractor() *RazorExtractor {
	return &RazorExtractor{cs: NewCSharpExtractor()}
}

func (e *RazorExtractor) Language() string     { return "razor" }
func (e *RazorExtractor) Extensions() []string { return []string{".razor", ".cshtml"} }

func (e *RazorExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	lineCount := 1 + strings.Count(string(src), "\n")
	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: lineCount, Language: "razor",
	}
	result.Nodes = append(result.Nodes, fileNode)

	// One navigable component node per .razor file: a Blazor component IS the
	// file, so emitting it as a real KindType node (not just a reference) lets
	// it participate in find_usages / renders_child as a first-class symbol —
	// where codegraph only ever emits a reference.
	componentID := ""
	if name := razorComponentName(filePath); name != "" {
		componentID = filePath + "::" + name
		compMeta := map[string]any{"component": true, "ui_component": "razor", "type_flavor": "component"}
		// The component's Blazor namespace is its directory path dotted
		// (`App/Widgets/Counter.razor` → `App.Widgets`), so an `@using`
		// import of that namespace can bind a `<Counter/>` reference.
		if ns := razorComponentNamespace(filePath); ns != "" {
			compMeta["scope_ns"] = ns
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: componentID, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: 1, EndLine: lineCount, Language: "razor",
			Meta: compMeta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: componentID, Kind: graph.EdgeDefines, FilePath: filePath, Line: 1,
		})
	}

	// @code{...} / @functions{...} blocks hold C# class members; a bare @{...}
	// holds statements. Each is wrapped in a synthetic class (or class+method
	// for bare blocks) so tree-sitter parses it, then delegated;
	// delegateRazorCode strips the wrapper and rebases into host coordinates.
	for _, span := range razorCodeSpans(src) {
		lineOffset := strings.Count(string(src[:span.start]), "\n")
		wrapPrefix, wrapSuffix := razorCodeWrapPrefix+"\n", "\n}"
		if span.bare {
			wrapPrefix, wrapSuffix = razorCodeWrapPrefix+"\nvoid __Body() {\n", "\n}\n}"
		}
		e.delegateRazorCode(src[span.start:span.end], lineOffset, wrapPrefix, wrapSuffix, filePath, fileNode.ID, result)
	}

	// Directive type references: @model / @inherits name the view-model or base
	// type; @inject names the injected service type; @typeof(X) references X.
	for _, m := range razorModelRe.FindAllSubmatch(src, -1) {
		emitRazorTypeRef(result, fileNode.ID, filePath, string(m[1]))
	}
	for _, m := range razorInjectRe.FindAllSubmatch(src, -1) {
		emitRazorTypeRef(result, fileNode.ID, filePath, string(m[1]))
	}
	for _, m := range razorTypeofRe.FindAllSubmatch(src, -1) {
		emitRazorTypeRef(result, fileNode.ID, filePath, string(m[1]))
	}

	// `@using Some.Namespace` directives (and the cascading _Imports.razor)
	// feed the resolver's namespace-scoped simple-type binding. Emitted as a
	// per-file marker the resolver consumes and removes; no new node kind.
	for _, m := range razorUsingRe.FindAllSubmatch(src, -1) {
		ns := strings.TrimSpace(string(m[1]))
		if ns == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::razor_using::" + ns,
			Kind: graph.EdgeImports, FilePath: filePath, Line: 1,
		})
	}

	// Markup component tags (`<Child />`) and Blazor generic type-arg
	// references (`<Grid TItem="CatalogItem" />`), scanned with @code /
	// @functions / @{} bodies blanked so the C# inside them is never parsed
	// for tags.
	if componentID != "" {
		blanked := razorBlankCode(src)
		mineTemplateComponentUsages(blanked, filePath, componentID, "razor", result)
		for _, m := range razorGenericArgRE.FindAllSubmatch(blanked, -1) {
			emitRazorTypeRef(result, fileNode.ID, filePath, string(m[1]))
		}
	}
	return result, nil
}

// razorComponentName derives the Blazor component name from a .razor file path
// (the PascalCase base name). Returns "" for .cshtml views, which are not
// components.
func razorComponentName(filePath string) string {
	if !strings.HasSuffix(filePath, ".razor") {
		return ""
	}
	base := filePath
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".razor")
	if base == "" || base[0] < 'A' || base[0] > 'Z' {
		return ""
	}
	return base
}

// razorComponentNamespace derives a Blazor component's namespace from its
// repo-relative directory path, dotted (`App/Widgets/Counter.razor` →
// `App.Widgets`). Returns "" for a root-level component. Path-derived (not
// RootNamespace-prefixed) so it matches the `@using` namespaces the resolver
// compares against, which are likewise path-relative in practice.
func razorComponentNamespace(filePath string) string {
	filePath = strings.ReplaceAll(filePath, "\\", "/")
	dir := ""
	if i := strings.LastIndex(filePath, "/"); i >= 0 {
		dir = filePath[:i]
	}
	if dir == "" {
		return ""
	}
	return strings.ReplaceAll(dir, "/", ".")
}

// razorCodeWrapPrefix is a single line so the wrap shifts content by exactly one
// line (compensated when rebasing).
const razorCodeWrapPrefix = "class __RazorCode {"

// delegateRazorCode wraps a @code block body in a synthetic class, runs the C#
// extractor over it, and merges the result rebased into host coordinates,
// dropping the synthetic file and wrapper-class nodes.
func (e *RazorExtractor) delegateRazorCode(content []byte, lineOffset int, wrapPrefix, wrapSuffix, filePath, fileID string, result *parser.ExtractionResult) {
	if strings.TrimSpace(string(content)) == "" {
		return
	}
	virtual := filePath + "#code"
	wrapped := []byte(wrapPrefix + string(content) + wrapSuffix)
	sub, err := e.cs.Extract(virtual, wrapped)
	if err != nil || sub == nil {
		return
	}
	// The wrapper prefix occupies one line per newline it carries, so a wrapped
	// line W is content line W-(prefix lines); combined with the block's host
	// offset that is lineOffset-(prefix lines).
	shift := lineOffset - strings.Count(wrapPrefix, "\n")
	wrapperID := ""
	for _, n := range sub.Nodes {
		if n == nil || n.ID == virtual {
			continue
		}
		if n.Kind == graph.KindType && n.Name == "__RazorCode" {
			wrapperID = n.ID
			continue
		}
		// Drop the synthetic method that wraps a bare @{ } statement block.
		if n.Kind == graph.KindMethod && n.Name == "__Body" {
			continue
		}
		n.FilePath = filePath
		n.Language = "razor"
		if n.StartLine > 0 {
			n.StartLine += shift
		}
		if n.EndLine > 0 {
			n.EndLine += shift
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["inline_script"] = true
		// The recorded ownership span is spelled in the same coordinates
		// as the node's lines, so it moves with them; left in wrapper
		// coordinates it would name unrelated host lines.
		for _, k := range []string{graph.MetaOwnershipStartLine, graph.MetaOwnershipEndLine} {
			if v, ok := n.Meta[k].(int); ok && v > 0 {
				n.Meta[k] = v + shift
			}
		}
		result.Nodes = append(result.Nodes, n)
	}
	for _, ed := range sub.Edges {
		if ed == nil || ed.From == wrapperID || ed.To == wrapperID {
			continue // drop edges to/from the synthetic wrapper class
		}
		if ed.From == virtual {
			ed.From = fileID
		}
		ed.FilePath = filePath
		if ed.Line > 0 {
			ed.Line += shift
		}
		result.Edges = append(result.Edges, ed)
	}
}

type razorSpan struct {
	start, end int
	// bare is true for a @{ } statement block (delegated wrapped in a method
	// body), false for a @code / @functions member block (wrapped in a class).
	bare bool
}

// razorCodeSpans returns the inner content span of every @code{...} /
// @functions{...} member block and bare @{...} statement block. Brace matching
// is string-, char-, and comment-aware (matchRazorBrace), so a `}` inside a C#
// string literal or comment cannot end the block early — which would otherwise
// truncate the delegated C# and silently drop every member after it.
// razorGenericArgRE matches a Blazor generic type-parameter attribute on a
// component tag — `TItem="CatalogItem"`, `TValue="int"` — whose value is a type
// reference. The `T[A-Z]` shape is the Blazor type-param convention, so it does
// not match ordinary attributes like `Title=` / `Text=`.
var razorGenericArgRE = regexp.MustCompile(`\bT[A-Z]\w*\s*=\s*"([A-Za-z_][\w.]*)"`)

// razorBlankCode returns a copy of src with every @code / @functions / @{}
// body blanked (newlines preserved), so the markup tag/type scan never reads
// the C# inside those blocks.
func razorBlankCode(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	for _, span := range razorCodeSpans(src) {
		if span.start <= span.end && span.end <= len(out) {
			copy(out[span.start:span.end], blankPreservingNewlines(out[span.start:span.end]))
		}
	}
	return out
}

func razorCodeSpans(src []byte) []razorSpan {
	var spans []razorSpan
	for _, loc := range razorBlockRe.FindAllSubmatchIndex(src, -1) {
		open := loc[1] - 1 // position of the opening '{'
		bare := loc[2] < 0 // group 1 absent → bare @{ block
		end := matchRazorBrace(src, open)
		if end < 0 {
			continue
		}
		spans = append(spans, razorSpan{start: open + 1, end: end, bare: bare})
	}
	return spans
}

// matchRazorBrace returns the index of the '}' closing the '{' at open, scanning
// the C# body with awareness of string ("..."), verbatim (@"..."), and char
// ('...') literals and // and /* */ comments, so a brace inside any of them
// never shifts the depth. Returns -1 when the brace is unbalanced.
func matchRazorBrace(src []byte, open int) int {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		case '"':
			i = skipCSharpQuoted(src, i, '"', false)
		case '\'':
			i = skipCSharpQuoted(src, i, '\'', false)
		case '@':
			if i+1 < len(src) && src[i+1] == '"' {
				i = skipCSharpQuoted(src, i+1, '"', true)
			}
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				i += 2
				for i < len(src) && src[i] != '\n' {
					i++
				}
			} else if i+1 < len(src) && src[i+1] == '*' {
				i += 2
				for i < len(src) {
					if src[i] == '*' && i+1 < len(src) && src[i+1] == '/' {
						i++
						break
					}
					i++
				}
			}
		}
	}
	return -1
}

// skipCSharpQuoted returns the index of the closing quote of a literal that
// opens at i. For a regular literal a backslash escapes the next byte; for a
// verbatim (@"...") literal backslashes are literal and the quote is escaped by
// doubling ("") instead. On an unterminated literal it returns len-1 so the
// caller advances to EOF.
func skipCSharpQuoted(src []byte, i int, quote byte, verbatim bool) int {
	for j := i + 1; j < len(src); j++ {
		switch {
		case !verbatim && src[j] == '\\':
			j++ // skip the escaped byte
		case src[j] == quote:
			if verbatim && j+1 < len(src) && src[j+1] == quote {
				j++ // doubled quote inside a verbatim string
				continue
			}
			return j
		}
	}
	return len(src) - 1
}

func emitRazorTypeRef(result *parser.ExtractionResult, fromID, filePath, typeName string) {
	typeName = strings.TrimSpace(typeName)
	// Split generic type arguments into their own references: a `List<Foo>`
	// directive references both List and Foo, so the type graph reaches every
	// type the generic names rather than only the outer container.
	if i := strings.IndexByte(typeName, '<'); i >= 0 {
		if j := strings.LastIndexByte(typeName, '>'); j > i {
			for _, arg := range splitRazorTypeArgs(typeName[i+1 : j]) {
				emitRazorTypeRef(result, fromID, filePath, arg)
			}
		}
		typeName = typeName[:i]
	}
	if i := strings.LastIndexByte(typeName, '.'); i >= 0 {
		typeName = typeName[i+1:]
	}
	if typeName = strings.TrimSpace(typeName); typeName == "" {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: fromID, To: "unresolved::" + typeName, Kind: graph.EdgeReferences, FilePath: filePath,
	})
}

// splitRazorTypeArgs splits a comma-separated generic argument list at the top
// nesting level, so `string, List<int>` yields "string" and "List<int>".
func splitRazorTypeArgs(s string) []string {
	var args []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			depth--
		case ',':
			if depth == 0 {
				if a := strings.TrimSpace(s[start:i]); a != "" {
					args = append(args, a)
				}
				start = i + 1
			}
		}
	}
	if a := strings.TrimSpace(s[start:]); a != "" {
		args = append(args, a)
	}
	return args
}
