package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/c"
)

// qCAll is a single tree-sitter query alternating over every pattern
// the C extractor needs. One tree walk per file replaces the 8
// `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch in
// Extract can branch on which name is set.
const qCAll = `
[
  (function_definition) @func.def

  (declaration
    declarator: (function_declarator
      declarator: (identifier) @proto.name)) @proto.def

  (declaration
    declarator: (pointer_declarator
      declarator: (function_declarator
        declarator: (identifier) @proto.name))) @proto.def

  (declaration
    declarator: (pointer_declarator
      declarator: (pointer_declarator
        declarator: (function_declarator
          declarator: (identifier) @proto.name)))) @proto.def

  (struct_specifier
    name: (type_identifier) @struct.name) @struct.def

  (enum_specifier
    name: (type_identifier) @enum.name) @enum.def

  (type_definition
    declarator: (type_identifier) @typedef.name) @typedef.def

  (preproc_include
    path: (_) @include.path) @include.def

  (preproc_def
    name: (identifier) @macro.name) @macro.def

  (preproc_function_def
    name: (identifier) @macrofn.name) @macrofn.def

  (call_expression
    function: (identifier) @call.name) @call.expr

  (declaration
    declarator: (init_declarator
      declarator: (identifier) @var.name)) @var.def
]
`

// CExtractor extracts C source files into graph nodes and edges.
type CExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewCExtractor() *CExtractor {
	lang := c.GetLanguage()
	return &CExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qCAll, lang),
	}
}

func (e *CExtractor) Language() string { return "c" }

// Extensions includes ".def" — the conventional extension for a generated C
// fragment #include'd into a translation unit (an X-macro / command table like
// redis's src/commands.def). No other extractor claims ".def"; a non-C ".def"
// (a Windows linker module-definition file) degrades to tree-sitter ERROR nodes
// and emits nothing meaningful. ".inc" is contested (assembly / PHP / Pascal),
// so a clearly-C ".inc" fragment is routed to this extractor by content sniffing
// (sniffAmbiguous) instead of an extension claim.
func (e *CExtractor) Extensions() []string { return []string{".c", ".h", ".def"} }

// --- Deferred match buffers ----------------------------------------

type cDeferredCall struct {
	name string
	line int
}

type cDeferredVar struct {
	name    string
	line    int
	endLine int
	isConst bool
}

// cDeclIsConst reports whether a C declaration carries a leading `const` type
// qualifier — a file-scope `const int MAX = 10;` is a genuine named constant,
// not a mutable global. The capturing query only matches plain-identifier
// declarators, so pointer-to-const (`const char *p`, where the pointer itself
// stays mutable) never reaches here; a direct-child qualifier scan is exact.
func cDeclIsConst(decl *sitter.Node, src []byte) bool {
	if decl == nil {
		return false
	}
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		ch := decl.Child(i)
		if ch != nil && ch.Type() == "type_qualifier" && ch.Content(src) == "const" {
			return true
		}
	}
	return false
}

func (e *CExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	checked, parseErrors := parser.ParseErrorVerdict(root)
	result := &parser.ExtractionResult{ParseChecked: checked, ParseErrors: parseErrors}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "c",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	var calls []cDeferredCall
	var vars []cDeferredVar

	// Query captures and every wrapper reached from them are consumed
	// synchronously below; only graph values and scalar deferred records
	// escape. Rewind the traversal wrappers before the later whole-tree passes.
	root.WithScratch(func() {
		parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
			switch {

			case m.Captures["func.def"] != nil:
				e.emitFunction(m, filePath, fileID, src, result, seen)

			case m.Captures["proto.def"] != nil:
				e.emitPrototype(m, filePath, fileID, result, seen)

			case m.Captures["struct.def"] != nil:
				e.emitKindType(m, "struct", filePath, fileID, result, seen)

			case m.Captures["enum.def"] != nil:
				e.emitKindType(m, "enum", filePath, fileID, result, seen)

			case m.Captures["typedef.def"] != nil:
				e.emitKindType(m, "typedef", filePath, fileID, result, seen)

			case m.Captures["include.def"] != nil:
				e.emitInclude(m, filePath, fileID, result)

			case m.Captures["macro.def"] != nil:
				emitCMacro(m.Captures["macro.def"].Node, false, filePath, fileID, "c", src, result, seen)

			case m.Captures["macrofn.def"] != nil:
				emitCMacro(m.Captures["macrofn.def"].Node, true, filePath, fileID, "c", src, result, seen)

			case m.Captures["call.expr"] != nil:
				expr := m.Captures["call.expr"]
				calls = append(calls, cDeferredCall{
					name: m.Captures["call.name"].Text,
					line: expr.StartLine + 1,
				})

			case m.Captures["var.def"] != nil:
				def := m.Captures["var.def"]
				vars = append(vars, cDeferredVar{
					name:    m.Captures["var.name"].Text,
					line:    def.StartLine + 1,
					endLine: def.EndLine + 1,
					isConst: cDeclIsConst(def.Node, src),
				})
			}
		})
	})

	// Globals and call edges both need funcRanges; build once.
	funcRanges := buildFuncRanges(result)

	for _, v := range vars {
		// Skip locals inside function bodies.
		if findEnclosingFunc(funcRanges, v.line) != "" {
			continue
		}
		id := filePath + "::" + v.name
		if seen[id] {
			continue
		}
		seen[id] = true
		kind := graph.KindVariable
		if v.isConst {
			// A file-scope const is a named constant — joins the value-ref
			// impact surface alongside #define macros, not the mutable-global set.
			kind = graph.KindConstant
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: v.name,
			FilePath: filePath, StartLine: v.line, EndLine: v.endLine,
			Language: "c",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: v.line,
		})
	}

	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		})
	}

	root.WithScratch(func() { captureValueRefCandidates(result, root, filePath, src) })
	root.WithScratch(func() { captureFnValueCandidates(result, root, filePath, src) })
	captureCFnPointerDispatch(result, root, filePath, src)
	captureCFnAddressRefs(result, root, filePath, fileID, src)
	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *CExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["func.def"]
	// The name lives at the bottom of the declarator chain. A plain
	// `int f()` nests function_declarator directly, but a pointer-return
	// `robj *f()` / `robj **f()` wraps it in one or two pointer_declarators
	// (and a fn-pointer-returning signature adds a parenthesized_declarator).
	// cDeclName peels every wrapper down to the identifier, so the extractor
	// no longer drops pointer-return definitions on the floor.
	name := cDeclName(def.Node.ChildByFieldName("declarator"), src)
	if name == "" {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"signature": strings.TrimSpace(extractCSignature(def.Node, src)),
	}
	// File-local-static linkage detection. A C function declared
	// `static` has translation-unit scope; the scope-aware resolver
	// uses this stamp to prefer a same-file static candidate over a
	// global candidate of the same name. Mirrors `resolver.MetaScopeStatic`.
	if isCStaticFunction(def.Node, src) {
		meta["scope_static"] = true
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "c", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *CExtractor) emitPrototype(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["proto.name"].Text
	def := m.Captures["proto.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "c", Meta: map[string]any{
			"signature": strings.TrimSuffix(strings.TrimSpace(def.Text), ";"),
			"prototype": true,
		},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitKindType collapses struct/enum/typedef emission — all produce a
// KindType node with the same shape. The prefix selects the
// capture-name pair.
func (e *CExtractor) emitKindType(m parser.QueryResult, prefix, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures[prefix+".name"].Text
	def := m.Captures[prefix+".def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "c", Meta: map[string]any{"type_flavor": prefix},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *CExtractor) emitInclude(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	pathCap := m.Captures["include.path"]
	// `"foo.h"` is a local (quoted) include — resolvable relative to the
	// including file; `<stdio.h>` is a system include. Recording which kind it
	// is lets the resolver bind local includes to real file nodes and leave
	// system headers external (codegraph treats both identically).
	raw := strings.TrimSpace(pathCap.Text)
	kind := "system"
	if strings.HasPrefix(raw, `"`) {
		kind = "quoted"
	}
	includePath := strings.Trim(strings.Trim(raw, `"`), "<>")
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       "unresolved::import::" + includePath,
		Kind:     graph.EdgeImports,
		FilePath: filePath,
		Line:     pathCap.StartLine + 1,
		Meta:     map[string]any{"include_kind": kind},
	})
}

// --- Helpers --------------------------------------------------------

// extractCSignature extracts a function signature from its definition node.
// It takes the text up to (but not including) the compound_statement body.
func extractCSignature(node *sitter.Node, src []byte) string {
	fullText := node.Content(src)
	// Find the opening brace of the function body and trim there.
	idx := strings.Index(fullText, "{")
	if idx > 0 {
		return strings.TrimSpace(fullText[:idx])
	}
	return fullText
}

// isCStaticFunction reports whether the function_definition node was
// declared with C's `static` storage-class specifier (file-local
// linkage). Scans the declaration prefix before the parameter list —
// that's where `static` legally appears. Tolerates leading attributes
// and inline / extern keywords that may bracket `static`.
func isCStaticFunction(node *sitter.Node, src []byte) bool {
	if node == nil {
		return false
	}
	full := node.Content(src)
	paren := strings.IndexByte(full, '(')
	if paren < 0 {
		return false
	}
	prefix := full[:paren]
	for _, word := range strings.Fields(prefix) {
		if word == "static" {
			return true
		}
	}
	return false
}
