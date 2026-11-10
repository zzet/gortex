package languages

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/php"
)

// PHPExtractor extracts PHP source files.
type PHPExtractor struct {
	lang *sitter.Language
}

func NewPHPExtractor() *PHPExtractor {
	return &PHPExtractor{lang: php.GetLanguage()}
}

func (e *PHPExtractor) Language() string { return "php" }
func (e *PHPExtractor) Extensions() []string {
	// Drupal module files (.module/.install/.inc/.theme/.profile/.engine) are
	// PHP source whose function names follow the hook convention. `.phtml` is
	// the PHP-template extension Zend / Laminas / Magento use, and the grammar
	// is the HTML-framing one, so a template parses as PHP already.
	return []string{
		".php", ".phtml",
		".module", ".install", ".inc", ".theme", ".profile", ".engine",
	}
}

func (e *PHPExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "php",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Walk the AST manually since PHP tree-sitter queries can be tricky.
	e.walkNode(root, src, filePath, fileNode, result, seen, "")

	// Statements outside every declaration — a script body, a bootstrap file,
	// a routes file, a fixture returning a closure — belong to the file node.
	e.extractPHPFileScope(root, src, filePath, fileNode, result)

	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)
	e.capturePHPStringCallables(result, root, filePath, src)

	// Expression-site and inheritance-clause reference forms #143's
	// type-use pass (typed property / param / return EdgeTypedAs) misses:
	// instantiation, extends/implements, instanceof, static / const access,
	// and PHP 8 attributes. Runs after the symbol walk so buildFuncRanges /
	// buildPHPTypeRanges see every function / method / class node.
	emitPHPReferenceForms(root, src, filePath, fileNode.ID, result)

	captureLaravelEvents(result, root, filePath, src)
	captureDrupalHooks(result, filePath)

	// Namespace identity runs last so it sees every node and edge the passes
	// above produced.
	applyPHPNamespaceIdentity(root, src, result)

	return result, nil
}

func (e *PHPExtractor) walkNode(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	currentClass string,
) {
	nodeType := node.Type()

	switch nodeType {
	case "namespace_definition":
		e.extractNamespace(node, src, filePath, fileNode, result, seen)

	case "class_declaration":
		e.extractClass(node, src, filePath, fileNode, result, seen)

	case "interface_declaration":
		e.extractInterface(node, src, filePath, fileNode, result, seen)

	case "trait_declaration":
		e.extractTrait(node, src, filePath, fileNode, result, seen)

	case "enum_declaration":
		e.extractEnum(node, src, filePath, fileNode, result, seen)

	case "function_definition":
		e.extractFunction(node, src, filePath, fileNode, result, seen)

	case "anonymous_class":
		e.extractAnonymousClass(node, src, filePath, fileNode, result, seen)

	case "namespace_use_declaration":
		e.extractUseImport(node, src, filePath, fileNode, result)

	case "const_declaration":
		// Reached only at file scope: a class body's const_declaration is
		// consumed by extractPhpMembers, which never recurses back here.
		e.extractPHPFileConstant(node, src, filePath, fileNode, result, seen)
		return

	case "function_call_expression":
		// `define('NAME', …)` is PHP's other constant declaration, and it is
		// an ordinary call with no declaration node of its own.
		e.extractPHPDefineConstant(node, src, filePath, fileNode, result, seen)
		e.walkChildren(node, src, filePath, fileNode, result, seen, currentClass)
		return

	case "expression_statement":
		// Check for require/include calls.
		e.extractRequireInclude(node, src, filePath, fileNode, result)
		// Also walk children for call expressions.
		e.walkChildren(node, src, filePath, fileNode, result, seen, currentClass)
		return

	default:
		// For class/interface bodies, walk into children with class context.
		e.walkChildren(node, src, filePath, fileNode, result, seen, currentClass)
		return
	}
}

func (e *PHPExtractor) walkChildren(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	currentClass string,
) {
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		e.walkNode(child, src, filePath, fileNode, result, seen, currentClass)
	}
}

func (e *PHPExtractor) extractNamespace(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	name := e.findChildByType(node, "namespace_name")
	if name == nil {
		return
	}
	nsName := name.Content(src)
	id := filePath + "::" + nsName
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: nsName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	// Walk children of namespace body.
	body := e.findChildByType(node, "compound_statement")
	if body != nil {
		for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
			child := body.NamedChild(i)
			e.walkNode(child, src, filePath, fileNode, result, seen, "")
		}
	}
	// Some namespaces don't use braces; walk remaining children.
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		if child.Type() != "namespace_name" && child.Type() != "compound_statement" {
			e.walkNode(child, src, filePath, fileNode, result, seen, "")
		}
	}
}

func (e *PHPExtractor) extractClass(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	className := nameNode.Content(src)
	id := filePath + "::" + className
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	meta := map[string]any{"visibility": VisibilityPublic}
	if doc := ExtractDocAbove(src, int(node.StartPoint().Row), DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	if parent := extractPhpParentClass(node, src); parent != "" {
		meta["scope_parent"] = parent
	}
	if ifaces := phpTypeClauseNames(node, src, "class_interface_clause"); len(ifaces) > 0 {
		meta["scope_interfaces"] = strings.Join(ifaces, ",")
	}
	meta["type_flavor"] = "class"
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: className,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	annotationSeen := map[string]bool{}
	emitPHPAnnotationEdgesFromAttrs(collectPhpAttributes(node, src), id, filePath, result, annotationSeen)
	e.emitPHPDocTestAnnotation(node, src, filePath, id, result, annotationSeen)

	// Extract methods, constants, properties, and trait uses inside the body.
	body := e.findChildByType(node, "declaration_list")
	if body == nil {
		return
	}
	methodNodes := e.extractPhpMembers(body, src, filePath, fileNode, result, seen, className, id)
	// Magic-method surface: a class declaring __call / __callStatic can receive
	// arbitrary member calls the extractor cannot mint nodes for; mark it so the
	// resolver/read path knows unresolved members on it are not necessarily a gap.
	if phpDeclaresMagicCall(methodNodes) {
		meta["has_magic_call"] = true
	}
	// @method phpdoc virtuals: docblock-declared methods (magic accessors,
	// mixins) that have no method_declaration node. Mint them so their ids
	// resolve and member calls to them bind.
	e.emitPHPDocMethods(node, src, filePath, fileNode, className, id, methodNodes, result, seen)
	// Laravel-specific dispatch passes — run after methods are in the
	// graph so action-method IDs are resolvable by name:
	//   1. Controller middleware: `$this->middleware(X)->only([...])`
	//      in the constructor → edges from each action to X.handle.
	//   2. Service provider bindings: `$this->app->bind/singleton(A,B)`
	//      in register() → EdgeProvides from the provider class.
	e.emitLaravelMiddleware(methodNodes, src, filePath, className, result)
	e.emitLaravelBindings(methodNodes, src, filePath, className, result)
	// Symfony attribute-based dispatch: #[AsEventListener] binds a
	// method (or whole class) to an event class as a listener. The
	// framework calls the listener at event-dispatch time with no
	// explicit call site; the attribute is the only static signal.
	e.emitSymfonyAttributeDispatch(node, methodNodes, src, filePath, className, result)
}

func (e *PHPExtractor) extractInterface(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	ifaceName := nameNode.Content(src)
	id := filePath + "::" + ifaceName
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	meta := map[string]any{"visibility": VisibilityPublic}
	if doc := ExtractDocAbove(src, int(node.StartPoint().Row), DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	meta["type_flavor"] = "interface"
	// An interface's `extends A, B` is modelled as a base_clause in
	// tree-sitter-php, so its parent interfaces land under scope_interfaces —
	// feeding the dispatch resolver's ancestor closure.
	if parents := phpTypeClauseNames(node, src, "base_clause"); len(parents) > 0 {
		meta["scope_interfaces"] = strings.Join(parents, ",")
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: ifaceName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	// Extract method signatures and constants inside the interface body.
	body := e.findChildByType(node, "declaration_list")
	if body == nil {
		return
	}
	e.extractPhpMembers(body, src, filePath, fileNode, result, seen, ifaceName, id)
}

// extractPhpMembers walks a class/trait/interface/enum body, emitting methods,
// class constants, properties, trait-use edges, and enum cases. Returns the
// method_declaration nodes by name for the framework-dispatch post-passes.
func (e *PHPExtractor) extractPhpMembers(
	body *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	ownerName, ownerID string,
) map[string]*sitter.Node {
	methodNodes := make(map[string]*sitter.Node)
	// Property types are read once for the whole body so every method's call
	// sites can type a `$this->prop->m()` receiver, including properties
	// declared after the method that uses them.
	props := phpClassPropertyTypes(body, src)
	for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
		child := body.NamedChild(i)
		switch child.Type() {
		case "method_declaration":
			name := ""
			if n := e.findChildByFieldName(child, "name"); n != nil {
				name = n.Content(src)
				methodNodes[name] = child
			}
			e.extractMethod(child, src, filePath, fileNode, result, seen, ownerName, props)
			// PHP 8 promotion declares properties in the constructor's
			// parameter list, so there is no property_declaration to find.
			if strings.EqualFold(name, "__construct") {
				e.extractPHPPromotedProperties(child, src, filePath, fileNode, result, seen, ownerName, ownerID)
			}
		case "const_declaration":
			e.extractPhpClassConst(child, src, filePath, fileNode, result, seen, ownerName, ownerID)
		case "property_declaration":
			e.extractPhpProperty(child, src, filePath, fileNode, result, seen, ownerName, ownerID)
		case "use_declaration":
			e.extractPhpTraitUse(child, src, filePath, ownerID, result)
		case "enum_case":
			e.extractPhpEnumCase(child, src, filePath, fileNode, result, seen, ownerName, ownerID)
		}
	}
	return methodNodes
}

// extractTrait emits a trait as a navigable type plus all its members; a class
// that `use`s the trait gains a trait-composition edge (see extractPhpTraitUse).
func (e *PHPExtractor) extractTrait(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(src)
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	meta := map[string]any{"visibility": VisibilityPublic, "kind": "trait", "type_flavor": "trait"}
	if doc := ExtractDocAbove(src, int(node.StartPoint().Row), DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
		Language: "php", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	if body := e.findChildByType(node, "declaration_list"); body != nil {
		methodNodes := e.extractPhpMembers(body, src, filePath, fileNode, result, seen, name, id)
		if phpDeclaresMagicCall(methodNodes) {
			meta["has_magic_call"] = true
		}
		e.emitPHPDocMethods(node, src, filePath, fileNode, name, id, methodNodes, result, seen)
	}
}

// extractEnum emits a PHP enum (cases, backing type, and methods).
func (e *PHPExtractor) extractEnum(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(src)
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	meta := map[string]any{"visibility": VisibilityPublic, "kind": "enum", "type_flavor": "enum"}
	if bt := e.findChildByType(node, "primitive_type"); bt != nil {
		meta["backing_type"] = strings.TrimSpace(bt.Content(src))
	}
	// A PHP enum may implement interfaces. Without scope_interfaces the
	// dispatch hierarchy cannot see the enum as an implementor, so a call
	// through the interface never reaches the enum's methods.
	if ifaces := phpTypeClauseNames(node, src, "class_interface_clause"); len(ifaces) > 0 {
		meta["scope_interfaces"] = strings.Join(ifaces, ",")
	}
	if doc := ExtractDocAbove(src, int(node.StartPoint().Row), DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
		Language: "php", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	if body := e.findChildByType(node, "enum_declaration_list"); body != nil {
		e.extractPhpMembers(body, src, filePath, fileNode, result, seen, name, id)
	}
}

// extractPhpClassConst emits each `const NAME = ...` in a class/interface/enum
// body as a class-scoped constant member.
func (e *PHPExtractor) extractPhpClassConst(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	ownerName, ownerID string,
) {
	vis := phpMemberVisibility(node, src)
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		ce := node.NamedChild(i)
		if ce.Type() != "const_element" {
			continue
		}
		nameNode := e.findChildByType(ce, "name")
		if nameNode == nil {
			continue
		}
		name := nameNode.Content(src)
		line := int(ce.StartPoint().Row) + 1
		id, ok := disambiguateID(seen, filePath+"::"+ownerName+"."+name, line)
		if !ok {
			continue
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindConstant, Name: name,
			FilePath: filePath, StartLine: line, EndLine: line, Language: "php",
			Meta: map[string]any{"receiver": ownerName, "visibility": vis},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
		})
	}
}

// extractPhpProperty emits each typed property in a class/trait body as a field.
func (e *PHPExtractor) extractPhpProperty(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	ownerName, ownerID string,
) {
	vis := phpMemberVisibility(node, src)
	propType := phpPropertyType(node, src)
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		pe := node.NamedChild(i)
		if pe.Type() != "property_element" {
			continue
		}
		vn := e.findChildByType(pe, "variable_name")
		if vn == nil {
			continue
		}
		nameNode := e.findChildByType(vn, "name")
		if nameNode == nil {
			continue
		}
		name := nameNode.Content(src)
		line := int(pe.StartPoint().Row) + 1
		id, ok := disambiguateID(seen, filePath+"::"+ownerName+"."+name, line)
		if !ok {
			continue
		}
		meta := map[string]any{"receiver": ownerName, "visibility": vis}
		phpMemberModifiers(node, meta)
		if propType != "" {
			meta["field_type"] = propType
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindField, Name: name,
			FilePath: filePath, StartLine: line, EndLine: line, Language: "php", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
		})
		// Typed property (`private Session $s;`): emit a usage edge from
		// the field to the declared class so find_usages lands it without
		// an LSP. Builtins / nullable / union / namespace are handled by
		// emitPHPTypeUseEdges.
		emitPHPTypeUseEdges(id, propType, filePath, line, result)
	}
}

// extractPhpTraitUse emits a composition edge from a class to each trait it
// `use`s — modeling trait composition the way a mixin is modeled, so the
// composed members are reachable through the class.
func (e *PHPExtractor) extractPhpTraitUse(node *sitter.Node, src []byte, filePath, ownerID string, result *parser.ExtractionResult) {
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		if c.Type() != "name" && c.Type() != "qualified_name" {
			continue
		}
		trait := strings.TrimSpace(c.Content(src))
		if trait == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: ownerID, To: "unresolved::" + trait, Kind: graph.EdgeExtends,
			FilePath: filePath, Line: int(c.StartPoint().Row) + 1,
			Meta: map[string]any{"via": "trait"},
		})
	}
}

// extractPhpEnumCase emits one enum case as an enum member of its enum.
func (e *PHPExtractor) extractPhpEnumCase(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	ownerName, ownerID string,
) {
	nameNode := e.findChildByType(node, "name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(src)
	line := int(node.StartPoint().Row) + 1
	id, ok := disambiguateID(seen, filePath+"::"+ownerName+"."+name, line)
	if !ok {
		return
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindEnumMember, Name: name,
		FilePath: filePath, StartLine: line, EndLine: line, Language: "php",
		Meta: map[string]any{"enum": ownerID},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
	})
}

// phpPropertyType returns the declared type of a property_declaration (the type
// node before the first property_element), or "".
func phpPropertyType(node *sitter.Node, src []byte) string {
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		switch c.Type() {
		case "primitive_type", "named_type", "union_type", "disjunctive_normal_form_type", "intersection_type", "optional_type", "qualified_name":
			return strings.TrimSpace(c.Content(src))
		case "property_element":
			return ""
		}
	}
	return ""
}

// phpBuiltinType reports whether a type name is a PHP builtin (no node exists
// for it, so it should not produce an unresolved type-reference edge).
func phpBuiltinType(t string) bool {
	switch strings.ToLower(strings.TrimPrefix(t, "?")) {
	case "int", "integer", "string", "bool", "boolean", "float", "double",
		"void", "array", "object", "mixed", "callable", "iterable",
		"self", "static", "parent", "never", "null", "false", "true":
		return true
	}
	return false
}

// canonicalizePHPTypeRef reduces one PHP type atom (no `|`/`&` left) to its
// bare class name: it strips the nullable `?` prefix and reduces a namespaced
// reference (`\App\Http\HttpResponse`, `App\HttpResponse`) to its last segment,
// so the name-based resolver can land it against the `Type` node defined under
// its short name. Returns "" for an empty atom.
func canonicalizePHPTypeRef(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "?")
	t = strings.TrimSpace(t)
	if i := strings.LastIndex(t, `\`); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimSpace(t)
}

// emitPHPTypeUseEdges parses a PHP type declaration used in property /
// parameter position and emits one EdgeTypedAs from ownerID to the bare named
// type for every non-builtin branch. PHP 8 union (`Foo|Bar`) and intersection
// (`Foo&Bar`) declarations each contribute one branch; the nullable `?Foo`
// shorthand and namespace prefixes are stripped by canonicalizePHPTypeRef.
// Edges ride at OriginASTInferred — the binding is a tree-sitter inference, not
// an LSP-checked fact — so a class used only in a type declaration becomes a
// traversable cross-file reference find_usages can land without a language
// server. Builtin scalar/pseudo types (int, string, void, self, …) are skipped.
func emitPHPTypeUseEdges(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult) {
	if ownerID == "" || typeText == "" {
		return
	}
	seen := map[string]bool{}
	// PHP 8.2 disjunctive normal form — `(Countable&Traversable)|null` —
	// parenthesises its intersection groups, so the parens are separators too;
	// without them an atom reads as `(Countable` and mints a bogus target.
	isSep := func(r rune) bool { return r == '|' || r == '&' || r == '(' || r == ')' }
	for _, atom := range strings.FieldsFunc(typeText, isSep) {
		t := canonicalizePHPTypeRef(atom)
		if t == "" || phpBuiltinType(t) || seen[t] {
			continue
		}
		seen[t] = true
		edge := &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + t,
			Kind:     graph.EdgeTypedAs,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
		}
		phpStampRawTypeRef(edge, atom, t)
		result.Edges = append(result.Edges, edge)
	}
}

// phpReturnType returns the declared return type of a method/function (the type
// node after formal_parameters, before the body), or "".
func phpReturnType(node *sitter.Node, src []byte) string {
	seenParams := false
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		t := c.Type()
		if t == "formal_parameters" {
			seenParams = true
			continue
		}
		if !seenParams {
			continue
		}
		switch t {
		case "primitive_type", "named_type", "union_type", "disjunctive_normal_form_type", "intersection_type", "optional_type", "qualified_name", "bottom_type":
			return strings.TrimSpace(c.Content(src))
		case "compound_statement":
			return ""
		}
	}
	return ""
}

// emitPHPParamTypeUseEdges walks a function/method's formal_parameters and
// emits an EdgeTypedAs from ownerID to each non-builtin parameter type. PHP
// has no separate param node in this extractor, so the usage is attributed to
// the enclosing function/method — enough for find_usages to land a class used
// only in a parameter type declaration (`function f(HttpResponse $r)`).
func (e *PHPExtractor) emitPHPParamTypeUseEdges(node *sitter.Node, src []byte, ownerID, filePath string, result *parser.ExtractionResult) {
	params := e.findChildByType(node, "formal_parameters")
	if params == nil {
		return
	}
	for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
		p := params.NamedChild(i)
		switch p.Type() {
		case "simple_parameter", "variadic_parameter", "property_promotion_parameter":
		default:
			continue
		}
		pt := phpParameterType(p, src)
		if pt == "" {
			continue
		}
		emitPHPTypeUseEdges(ownerID, pt, filePath, int(p.StartPoint().Row)+1, result)
	}
}

// phpParameterType returns the declared type of a parameter node (the type
// node before its variable_name), or "".
func phpParameterType(node *sitter.Node, src []byte) string {
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		switch c.Type() {
		case "primitive_type", "named_type", "union_type", "disjunctive_normal_form_type",
			"intersection_type", "optional_type", "qualified_name":
			return strings.TrimSpace(c.Content(src))
		case "variable_name":
			return ""
		}
	}
	return ""
}

func (e *PHPExtractor) extractFunction(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	funcName := nameNode.Content(src)
	id := filePath + "::" + funcName
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	meta := map[string]any{"visibility": VisibilityPublic}
	if doc := ExtractDocAbove(src, int(node.StartPoint().Row), DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	rt := phpReturnType(node, src)
	if rt != "" {
		meta["return_type"] = rt
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: funcName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	if rt != "" && !phpBuiltinType(rt) {
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: "unresolved::" + rt, Kind: graph.EdgeReturns, FilePath: filePath, Line: startLine,
		})
	}
	// Typed parameters → EdgeTypedAs usage edges on the function.
	e.emitPHPParamTypeUseEdges(node, src, id, filePath, result)

	// Extract call sites within the function body. A free function has no
	// `$this`, so only its typed parameters and local `new` bindings type a
	// receiver.
	body := e.findChildByType(node, "compound_statement")
	if body != nil {
		cyc, cog, loop := BodyComplexityMetrics(body, "php")
		ApplyComplexityMeta(meta, cyc, cog, loop)
		e.extractCallSitesInScope(body, src, filePath, id, result,
			newPHPReceiverEnv(node, src, "", nil))
	}
}

func (e *PHPExtractor) extractMethod(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	className string, props map[string]string,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	methodName := nameNode.Content(src)
	id := filePath + "::" + className + "." + methodName
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	if seen[id] {
		id = filePath + "::" + className + "." + methodName + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"receiver":    className,
		"scope_class": className,
		"visibility":  phpMemberVisibility(node, src),
	}
	phpMemberModifiers(node, meta)
	if doc := ExtractDocAbove(src, int(node.StartPoint().Row), DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	// Return type: stamped so the chained-factory resolver (helpers_chaintype)
	// can infer `Foo::make()->...` receivers, and emitted as a return edge.
	rt := phpReturnType(node, src)
	if rt != "" {
		meta["return_type"] = rt
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: methodName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
		Meta:     meta,
	})
	if rt != "" && !phpBuiltinType(rt) {
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: "unresolved::" + rt, Kind: graph.EdgeReturns, FilePath: filePath, Line: startLine,
		})
	}
	// Typed parameters → EdgeTypedAs usage edges on the method.
	e.emitPHPParamTypeUseEdges(node, src, id, filePath, result)
	annotationSeen := map[string]bool{}
	emitPHPAnnotationEdgesFromAttrs(collectPhpAttributes(node, src), id, filePath, result, annotationSeen)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	// MemberOf edge to containing class/interface.
	classID := filePath + "::" + className
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine,
	})

	// Extract call sites within the method body. `$this` is the declaring
	// class, so every unqualified member call in the body carries a receiver
	// type the resolver can bind exactly.
	body := e.findChildByType(node, "compound_statement")
	if body != nil {
		cyc, cog, loop := BodyComplexityMetrics(body, "php")
		ApplyComplexityMeta(meta, cyc, cog, loop)
		e.extractCallSitesInScope(body, src, filePath, id, result,
			newPHPReceiverEnv(node, src, className, props))
	}
}

func (e *PHPExtractor) extractUseImport(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	// A `use` statement carries an optional `function` / `const` qualifier and
	// is either a flat list of clauses or a braced group sharing one prefix.
	declKind := phpUseQualifier(node, src)

	// Grouped form: `use App\Log\{Handler, Formatter as F, function make};`.
	// The group's prefix is the declaration's own namespace_name; without
	// joining the two, every imported member was dropped and only the shared
	// prefix survived as an import.
	if group := node.ChildByFieldName("body"); group != nil {
		prefix := ""
		if nn := e.findChildByType(node, "namespace_name"); nn != nil {
			prefix = nn.Content(src)
		}
		for i, n := 0, int(group.NamedChildCount()); i < n; i++ {
			clause := group.NamedChild(i)
			if clause.Type() != "namespace_use_clause" {
				continue
			}
			e.emitPHPUseClause(clause, src, filePath, fileNode, result, prefix, declKind)
		}
		return
	}

	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "namespace_use_clause":
			e.emitPHPUseClause(child, src, filePath, fileNode, result, "", declKind)
		case "qualified_name", "namespace_name":
			e.emitPHPImportEdge(child.Content(src), "", declKind, filePath, fileNode,
				int(child.StartPoint().Row)+1, result)
		}
	}
}

// phpUseQualifier returns "function" / "const" for `use function ...` and
// `use const ...`, or "" for an ordinary class import.
func phpUseQualifier(node *sitter.Node, src []byte) string {
	t := node.ChildByFieldName("type")
	if t == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(t.Content(src)))
}

// emitPHPUseClause records one imported name, joining the group prefix when the
// clause came from a braced group and preferring the clause's own
// function/const qualifier over the declaration's.
func (e *PHPExtractor) emitPHPUseClause(
	clause *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
	prefix, declKind string,
) {
	nameNode := e.findChildByType(clause, "qualified_name")
	if nameNode == nil {
		nameNode = e.findChildByType(clause, "namespace_name")
	}
	if nameNode == nil {
		nameNode = e.findChildByType(clause, "name")
	}
	name := ""
	if nameNode != nil {
		name = nameNode.Content(src)
	}
	if name == "" {
		return
	}
	if prefix != "" {
		name = strings.TrimRight(prefix, `\`) + `\` + strings.TrimLeft(name, `\`)
	}
	kind := declKind
	if k := phpUseQualifier(clause, src); k != "" {
		kind = k
	}
	alias := ""
	if a := clause.ChildByFieldName("alias"); a != nil {
		alias = strings.TrimSpace(a.Content(src))
	}
	e.emitPHPImportEdge(name, alias, kind, filePath, fileNode,
		int(clause.StartPoint().Row)+1, result)
}

// emitPHPImportEdge records one import. The target keeps the historical
// slash-separated path shape; the imported symbol, its alias and whether the
// import names a class, function or constant ride on the edge meta so a
// consumer can recover the fully-qualified name the source wrote.
func (e *PHPExtractor) emitPHPImportEdge(
	name, alias, kind string,
	filePath string, fileNode *graph.Node,
	line int, result *parser.ExtractionResult,
) {
	name = strings.TrimLeft(strings.TrimSpace(name), `\`)
	if name == "" {
		return
	}
	edge := &graph.Edge{
		From: fileNode.ID, To: "unresolved::import::" + strings.ReplaceAll(name, `\`, "/"),
		Kind: graph.EdgeImports, FilePath: filePath, Line: line,
	}
	meta := map[string]any{"fqn": name, "symbol": canonicalizePHPTypeRef(name)}
	if alias != "" {
		meta["alias"] = alias
	}
	switch kind {
	case "function", "const":
		meta["import_kind"] = kind
	}
	edge.Meta = meta
	result.Edges = append(result.Edges, edge)
}

func (e *PHPExtractor) extractRequireInclude(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	// Look for require, require_once, include, include_once expressions.
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		ct := child.Type()
		if ct == "require_expression" || ct == "require_once_expression" ||
			ct == "include_expression" || ct == "include_once_expression" {
			// The path is a string/encapsed_string containing a string_content child.
			path := e.extractStringContent(child, src)
			if path != "" {
				line := int(child.StartPoint().Row) + 1
				result.Edges = append(result.Edges, &graph.Edge{
					From: fileNode.ID, To: "unresolved::import::" + path,
					Kind: graph.EdgeImports, FilePath: filePath, Line: line,
				})
			}
		}
	}
}

// extractCallSitesInScope walks a function body emitting its call and access
// edges, carrying the receiver-typing environment of that body. Descending into
// a closure derives a child scope rather than reusing the parent's, so a
// closure parameter shadows an outer variable of the same name instead of
// inheriting its type.
func (e *PHPExtractor) extractCallSitesInScope(
	node *sitter.Node, src []byte,
	filePath string, callerID string,
	result *parser.ExtractionResult,
	env phpReceiverEnv,
) {
	if node.Type() == "anonymous_function" || node.Type() == "anonymous_function_creation_expression" ||
		node.Type() == "arrow_function" {
		env = env.childScope(node, src)
	}
	e.emitPHPCallSiteEdges(node, src, filePath, callerID, result, env)

	// Recurse into children.
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		e.extractCallSitesInScope(child, src, filePath, callerID, result, env)
	}
}

// emitPHPCallSiteEdges emits the call / access edges for one node, without
// recursing. Both the in-function walker and the file-scope walker share it;
// they differ only in where they stop descending.
func (e *PHPExtractor) emitPHPCallSiteEdges(
	node *sitter.Node, src []byte,
	filePath string, callerID string,
	result *parser.ExtractionResult,
	env phpReceiverEnv,
) {
	switch node.Type() {
	case "member_access_expression", "nullsafe_member_access_expression",
		"scoped_property_access_expression", "class_constant_access_expression":
		e.emitPHPMemberAccess(node, src, filePath, callerID, result, env)
	case "function_call_expression":
		funcNode := node.ChildByFieldName("function")
		// A computed callee — `$fn()`, `($this->resolver)()`, `$handlers[0]()`
		// — names no symbol. Emitting its source text produced targets like
		// `unresolved::*.$fn` and `unresolved::*.($this->resolver)` that can
		// never bind (371 of them in guzzle alone); the receiver-typed member
		// call inside such an expression is captured by the walk regardless.
		if funcNode != nil && phpNamesACallee(funcNode) {
			name := funcNode.Content(src)
			if idx := strings.LastIndex(name, "\\"); idx >= 0 {
				name = name[idx+1:]
			}
			line := int(node.StartPoint().Row) + 1
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::*." + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		}
	case "member_call_expression", "nullsafe_member_call_expression", "scoped_call_expression":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil && phpNamesACallee(nameNode) {
			name := nameNode.Content(src)
			line := int(node.StartPoint().Row) + 1
			edge := &graph.Edge{
				From: callerID, To: "unresolved::*." + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			}
			// PHP scope tagging for the static resolver: a
			// `parent::foo()` / `self::foo()` / `static::foo()`
			// scoped_call_expression names its scope under the
			// `scope` field. Stamping it on the edge lets the
			// resolver walk the inheritance chain (`parent`) or
			// pin to the enclosing class (`self` / `static`).
			if node.Type() == "scoped_call_expression" {
				if scope := node.ChildByFieldName("scope"); scope != nil {
					switch strings.ToLower(scope.Content(src)) {
					case "parent":
						edge.Meta = map[string]any{"scope_kind": "parent"}
					case "self", "static":
						edge.Meta = map[string]any{"scope_kind": "self"}
					default:
						scopeName := scope.Content(src)
						// Laravel facade (`Cache::get()`): the static call
						// proxies to the backing service the facade returns.
						// Stamp that class as the receiver type so the resolver
						// binds the call to its method rather than a name-only
						// match against an unrelated `get`.
						if backing, ok := phpFacadeBackingClass(scopeName); ok {
							if edge.Meta == nil {
								edge.Meta = map[string]any{}
							}
							edge.Meta["receiver_type"] = backing
							edge.Meta["facade"] = strings.TrimPrefix(scopeName, "\\")
						} else if model, ok := phpEloquentModelCall(scopeName, name); ok {
							// Eloquent finder (`User::find(1)`): the method lives on
							// the inherited query builder, not the model, so bind
							// the call to the model class node instead of an
							// unresolvable `find`.
							edge.To = "unresolved::" + model
							if edge.Meta == nil {
								edge.Meta = map[string]any{}
							}
							edge.Meta["eloquent_model"] = model
							edge.Meta["eloquent_method"] = name
						} else if rt := env.phpScopeReceiverType(scope, src); rt != "" {
							// `Utils::pick()` / `$cls::make()` — the scope names
							// the class the static method belongs to, which is
							// the strongest receiver evidence PHP offers.
							edge.Meta = map[string]any{"receiver_type": rt}
						}
					}
				}
			}
			if node.Type() == "member_call_expression" || node.Type() == "nullsafe_member_call_expression" {
				obj := node.ChildByFieldName("object")
				if obj == nil && node.NamedChildCount() > 0 {
					obj = node.NamedChild(0)
				}
				if obj != nil {
					if rt := env.phpReceiverTypeFor(obj, src); rt != "" {
						edge.Meta = map[string]any{"receiver_type": rt}
					} else if property := env.phpPropertyReceiver(obj, src); property != nil {
						edge.Meta = property
					} else {
						// Chained-factory receiver: `make()->with()->build()` --
						// carry the receiver object (`->` normalised to `.`) so
						// the shared walker can type the chain or preserve it
						// for the graph-aware resolver.
						recv := strings.ReplaceAll(strings.TrimSpace(obj.Content(src)), "->", ".")
						if strings.Contains(recv, ".") || strings.Contains(recv, "(") {
							stampFactoryChainReceiver(edge, recv, resolveChainType(recv, env.vars, result))
						}
					}
				}
			}
			result.Edges = append(result.Edges, edge)
		}
	}
}

// findChildByType finds the first named child with the given type.
func (e *PHPExtractor) findChildByType(node *sitter.Node, typeName string) *sitter.Node {
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		if child.Type() == typeName {
			return child
		}
	}
	return nil
}

// findChildByFieldName finds a child by its field name.
func (e *PHPExtractor) findChildByFieldName(node *sitter.Node, fieldName string) *sitter.Node {
	return node.ChildByFieldName(fieldName)
}

// extractStringContent recursively finds the first string_content node and returns its text.
func (e *PHPExtractor) extractStringContent(node *sitter.Node, src []byte) string {
	if node.Type() == "string_content" {
		return node.Content(src)
	}
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		if result := e.extractStringContent(node.NamedChild(i), src); result != "" {
			return result
		}
	}
	return ""
}

// emitLaravelMiddleware scans the constructor body of a class for
// `$this->middleware(X)` calls, optionally followed by ->only([...])
// or ->except([...]) chains. For each discovered middleware class
// it emits one EdgeCalls per (action_method, middleware.handle)
// pair, honouring the only:/except: filters. Laravel invokes the
// middleware's handle() before each action at request time with no
// explicit call site.
func (e *PHPExtractor) emitLaravelMiddleware(methodNodes map[string]*sitter.Node, src []byte, filePath, className string, result *parser.ExtractionResult) {
	ctor, ok := methodNodes["__construct"]
	if !ok {
		return
	}
	body := e.findChildByType(ctor, "compound_statement")
	if body == nil {
		return
	}
	// Collect a set of action methods — every public-looking method
	// on the class except __construct, magic methods, and methods
	// that look like middleware-helpers themselves. In a real
	// Laravel controller, most non-ctor methods are actions; we
	// filter by leading underscore (magic methods start with __)
	// and the constructor.
	var actions []string
	for name := range methodNodes {
		if name == "__construct" || len(name) == 0 {
			continue
		}
		if len(name) >= 2 && name[0] == '_' && name[1] == '_' {
			continue
		}
		actions = append(actions, name)
	}
	if len(actions) == 0 {
		return
	}

	// Walk the ctor body for member_call_expression chains whose
	// outermost call participates in the middleware()[->only/except]
	// sequence. We process only the outermost call in each chain —
	// otherwise a chain like `middleware(X)->only([...])` would be
	// visited twice: once as the outer only() call (which descends
	// inward to find the middleware class) and once more as the inner
	// middleware() call on its own (producing a duplicate, UNfiltered
	// edge set).
	processed := make(map[uintptr]struct{})
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "member_call_expression" {
			if _, dup := processed[n.Id()]; !dup {
				// Only process if the parent isn't another
				// member_call_expression whose object is us —
				// that would mean we're the inner call of a filter
				// chain, already handled above.
				parent := n.Parent()
				if parent == nil || parent.Type() != "member_call_expression" || !parent.ChildByFieldName("object").Equal(n) {
					mw, chainCalls := parseLaravelMiddlewareCall(n, src)
					for _, c := range chainCalls {
						processed[c.Id()] = struct{}{}
					}
					if mw.class != "" {
						for _, action := range actions {
							if len(mw.only) > 0 {
								if _, ok := mw.only[action]; !ok {
									continue
								}
							}
							if len(mw.except) > 0 {
								if _, ok := mw.except[action]; ok {
									continue
								}
							}
							actionID := filePath + "::" + className + "." + action
							result.Edges = append(result.Edges, &graph.Edge{
								From:     actionID,
								To:       "unresolved::*.handle",
								Kind:     graph.EdgeCalls,
								FilePath: filePath,
								Line:     int(n.StartPoint().Row) + 1,
								Meta: map[string]any{
									"receiver_type":      mw.class,
									"dispatch_macro":     "middleware",
									"laravel_middleware": mw.class,
								},
							})
						}
					}
				}
			}
		}
		for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
			walk(n.Child(i))
		}
	}
	walk(body)
}

type laravelMiddleware struct {
	class  string
	only   map[string]struct{}
	except map[string]struct{}
}

// parseLaravelMiddlewareCall walks a member_call_expression. If the
// expression is of the form `$this->middleware(X)` (possibly chained
// with ->only([...]) / ->except([...])), returns the middleware
// class name and any action filters. The outermost call is passed in;
// we unwrap nested member_call_expressions to find both the filter
// chain and the original middleware() call.
func parseLaravelMiddlewareCall(outer *sitter.Node, src []byte) (laravelMiddleware, []*sitter.Node) {
	var out laravelMiddleware
	var chain []*sitter.Node
	cur := outer
	for cur != nil && cur.Type() == "member_call_expression" {
		chain = append(chain, cur)
		name := phpMethodName(cur, src)
		if name == "middleware" {
			// Found the base call. Extract the first argument.
			args := phpCallArgs(cur)
			if args != nil && args.NamedChildCount() > 0 {
				out.class = phpExtractClassRef(args.NamedChild(0), src)
			}
			return out, chain
		}
		if name == "only" || name == "except" {
			args := phpCallArgs(cur)
			if args != nil && args.NamedChildCount() > 0 {
				set := phpExtractStringArray(args.NamedChild(0), src)
				if name == "only" {
					out.only = set
				} else {
					out.except = set
				}
			}
		}
		// Follow the chain — the inner object of this call is the
		// next member_call_expression to inspect.
		inner := cur.ChildByFieldName("object")
		if inner == nil {
			return laravelMiddleware{}, chain
		}
		cur = inner
	}
	return laravelMiddleware{}, chain
}

// emitLaravelBindings scans a service provider's register() method for
// $this->app->bind / ->singleton / ->instance calls and emits an
// EdgeProvides from the provider class to the second argument's class
// (the implementation or binding target). Tagged binding="useClass"
// so the contracts tool treats these alongside NestJS useClass edges.
func (e *PHPExtractor) emitLaravelBindings(methodNodes map[string]*sitter.Node, src []byte, filePath, className string, result *parser.ExtractionResult) {
	register, ok := methodNodes["register"]
	if !ok {
		return
	}
	body := e.findChildByType(register, "compound_statement")
	if body == nil {
		return
	}
	classID := filePath + "::" + className
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "member_call_expression" {
			method := phpMethodName(n, src)
			switch method {
			case "bind", "singleton", "instance":
				args := phpCallArgs(n)
				if args == nil || args.NamedChildCount() < 1 {
					break
				}
				first := phpExtractClassRef(args.NamedChild(0), src)
				if first == "" {
					break
				}
				// Second arg may be an identifier (class ref) or a
				// closure — we only emit the useClass-style edge
				// when it's a class reference. Factory closures have
				// their own call edges from the closure body.
				var impl string
				if args.NamedChildCount() >= 2 {
					impl = phpExtractClassRef(args.NamedChild(1), src)
				}
				line := int(n.StartPoint().Row) + 1
				if impl != "" && impl != first {
					// useClass-style: AppServiceProvider → Concrete,
					// tagged with provides_for=Abstract. The resolver
					// uses this to rewrite abstract-typed call sites
					// to the concrete implementation.
					result.Edges = append(result.Edges, &graph.Edge{
						From:     classID,
						To:       "unresolved::" + impl,
						Kind:     graph.EdgeProvides,
						FilePath: filePath,
						Line:     line,
						Meta: map[string]any{
							graph.MetaDIProvidesFor: first,
							graph.MetaDIBinding:     graph.DIBindingUseClass,
							graph.MetaDIOrigin:      method,
						},
					})
				}
				// find_usages visibility: also emit a provider edge
				// pointing at the binding target itself (the abstract
				// / token). Lets callers ask "who provides this
				// interface?" and get the service provider class
				// directly, regardless of the concrete impl.
				result.Edges = append(result.Edges, &graph.Edge{
					From:     classID,
					To:       "unresolved::" + first,
					Kind:     graph.EdgeProvides,
					FilePath: filePath,
					Line:     line,
					Meta: map[string]any{
						graph.MetaDIToken: first,
						// Laravel's binding method name (bind / singleton /
						// instance), not one of the graph's DIBinding* forms —
						// these token providers are deliberately outside the
						// contract switch in di_contracts.go.
						graph.MetaDIBinding: method,
						graph.MetaDIOrigin:  method,
					},
				})
			}
		}
		for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
			walk(n.Child(i))
		}
	}
	walk(body)
}

// phpMethodName returns the method identifier in a member_call_expression.
func phpMethodName(call *sitter.Node, src []byte) string {
	nameNode := call.ChildByFieldName("name")
	if nameNode != nil {
		return nameNode.Content(src)
	}
	return ""
}

// phpCallArgs returns the arguments node of a member_call_expression.
func phpCallArgs(call *sitter.Node) *sitter.Node {
	return call.ChildByFieldName("arguments")
}

// phpExtractClassRef pulls a class name out of an argument node. Handles
// the `X::class` shape (scoped_call_expression / class_constant_access),
// bare identifiers, and simple string literals (middleware aliases).
// Returns "" when the argument isn't a static class reference.
func phpExtractClassRef(arg *sitter.Node, src []byte) string {
	if arg == nil {
		return ""
	}
	switch arg.Type() {
	case "argument":
		// Argument wrapper — unwrap to the contained expression.
		if arg.NamedChildCount() > 0 {
			return phpExtractClassRef(arg.NamedChild(0), src)
		}
		return ""
	case "class_constant_access_expression", "scoped_call_expression":
		// X::class — the class name is the first named child.
		if arg.NamedChildCount() > 0 {
			return arg.NamedChild(0).Content(src)
		}
	case "name", "qualified_name":
		return arg.Content(src)
	case "string":
		// middleware('auth') alias form — return the alias verbatim.
		for i, _nc := 0, int(arg.NamedChildCount()); i < _nc; i++ {
			c := arg.NamedChild(i)
			if c != nil && c.Type() == "string_content" {
				return c.Content(src)
			}
		}
	}
	return ""
}

// phpExtractStringArray collects string values or X::class names out
// of an `array(...)` / `[...]` expression for `->only([...])` /
// `->except([...])` filter chains. Returns a set keyed by method name.
func phpExtractStringArray(arg *sitter.Node, src []byte) map[string]struct{} {
	out := make(map[string]struct{})
	if arg == nil {
		return out
	}
	// Unwrap argument wrapper.
	if arg.Type() == "argument" {
		if arg.NamedChildCount() > 0 {
			return phpExtractStringArray(arg.NamedChild(0), src)
		}
		return out
	}
	if arg.Type() != "array_creation_expression" {
		return out
	}
	for i, _nc := 0, int(arg.NamedChildCount()); i < _nc; i++ {
		el := arg.NamedChild(i)
		if el == nil {
			continue
		}
		if el.Type() == "array_element_initializer" {
			// The actual value is the first named child.
			if el.NamedChildCount() > 0 {
				el = el.NamedChild(0)
			}
		}
		if el == nil {
			continue
		}
		switch el.Type() {
		case "string":
			for j, _nc := 0, int(el.NamedChildCount()); j < _nc; j++ {
				c := el.NamedChild(j)
				if c != nil && c.Type() == "string_content" {
					out[c.Content(src)] = struct{}{}
				}
			}
		case "class_constant_access_expression":
			if el.NamedChildCount() > 0 {
				out[el.NamedChild(0).Content(src)] = struct{}{}
			}
		}
	}
	return out
}

// emitSymfonyAttributeDispatch walks a class's attribute_list and the
// attribute_list of each method_declaration, emitting EdgeConsumes
// edges for every #[AsEventListener(event: X)] binding. The edge
// points from the listener method (or the class itself when the
// attribute is class-level) to the event class, so find_usages on
// the event surfaces the listener as a consumer. Meta identifies the
// attribute name so callers can distinguish these from plain
// references.
func (e *PHPExtractor) emitSymfonyAttributeDispatch(classNode *sitter.Node, methodNodes map[string]*sitter.Node, src []byte, filePath, className string, result *parser.ExtractionResult) {
	classID := filePath + "::" + className

	// Class-level attributes apply to the class as a whole. Symfony
	// treats a class-level #[AsEventListener] as shorthand for "this
	// class has one or more listener methods" — the dispatch
	// signature is discovered via the method attributes. We still
	// emit a class-level consumer edge so find_usages(Event) returns
	// the class even when every listener lives on a method.
	if clsAttrs := collectPhpAttributes(classNode, src); len(clsAttrs) > 0 {
		emitAttributeEdges(clsAttrs, classID, filePath, int(classNode.StartPoint().Row)+1, result)
	}
	// Method-level attributes. Each method_declaration has its own
	// attribute_list immediately before its modifiers.
	for name, methodNode := range methodNodes {
		attrs := collectPhpAttributes(methodNode, src)
		if len(attrs) == 0 {
			continue
		}
		methodID := filePath + "::" + className + "." + name
		emitAttributeEdges(attrs, methodID, filePath, int(methodNode.StartPoint().Row)+1, result)
	}
}

// phpAttribute captures the parsed form of one #[Attr(...)] instance —
// just what we need for dispatch extraction.
type phpAttribute struct {
	name string
	line int
	args map[string]string // key → value (class ref / string literal)
}

// emitPHPAnnotationEdgesFromAttrs turns parsed phpAttribute records
// into EdgeAnnotated edges. Args are flattened to a "k=v, k=v" string
// for storage on the edge — preserves intent without duplicating the
// keyed-args structure the dispatch detectors already consume.
func emitPHPAnnotationEdgesFromAttrs(attrs []phpAttribute, fromID, filePath string, result *parser.ExtractionResult, seen map[string]bool) {
	for _, a := range attrs {
		if a.name == "" {
			continue
		}
		var args string
		if len(a.args) > 0 {
			parts := make([]string, 0, len(a.args))
			for k, v := range a.args {
				if k == "" {
					parts = append(parts, v)
				} else {
					parts = append(parts, k+"="+v)
				}
			}
			args = strings.Join(parts, ", ")
		}
		EmitAnnotationEdge(fromID, "php", a.name, args, filePath, a.line, result, seen)
	}
}

// collectPhpAttributes scans a class_declaration or method_declaration
// for its attribute_list children and parses every attribute inside
// each group. Returns nil when the node carries no attributes.
func collectPhpAttributes(node *sitter.Node, src []byte) []phpAttribute {
	var out []phpAttribute
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		if c == nil || c.Type() != "attribute_list" {
			continue
		}
		for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
			group := c.NamedChild(j)
			if group == nil || group.Type() != "attribute_group" {
				continue
			}
			for k, _nc := 0, int(group.NamedChildCount()); k < _nc; k++ {
				attr := group.NamedChild(k)
				if attr == nil || attr.Type() != "attribute" {
					continue
				}
				parsed := parsePhpAttribute(attr, src)
				if parsed.name != "" {
					out = append(out, parsed)
				}
			}
		}
	}
	return out
}

// parsePhpAttribute turns an `attribute` node (the part inside the
// `#[ ]` wrapper) into a phpAttribute. Pulls named arguments the
// attribute's constructor expects (event:, method:, priority:) when
// they're simple class refs or string literals.
func parsePhpAttribute(attr *sitter.Node, src []byte) phpAttribute {
	var out phpAttribute
	nameNode := attr.ChildByFieldName("name")
	if nameNode == nil {
		// Grammar sometimes lacks the field name; fall back to the
		// first child of type "name".
		for i, _nc := 0, int(attr.NamedChildCount()); i < _nc; i++ {
			c := attr.NamedChild(i)
			if c != nil && (c.Type() == "name" || c.Type() == "qualified_name") {
				nameNode = c
				break
			}
		}
	}
	if nameNode == nil {
		return out
	}
	out.name = nameNode.Content(src)
	out.line = int(attr.StartPoint().Row) + 1
	args := attr.ChildByFieldName("arguments")
	if args == nil {
		// Some grammars expose arguments as a positional NamedChild.
		for i, _nc := 0, int(attr.NamedChildCount()); i < _nc; i++ {
			c := attr.NamedChild(i)
			if c != nil && c.Type() == "arguments" {
				args = c
				break
			}
		}
	}
	if args == nil {
		return out
	}
	out.args = make(map[string]string)
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		arg := args.NamedChild(i)
		if arg == nil || arg.Type() != "argument" {
			continue
		}
		// Named arguments have a `name` child first, then the value.
		var key string
		var valNode *sitter.Node
		for j, _nc := 0, int(arg.NamedChildCount()); j < _nc; j++ {
			c := arg.NamedChild(j)
			if c == nil {
				continue
			}
			if c.Type() == "name" && key == "" {
				key = c.Content(src)
				continue
			}
			valNode = c
			break
		}
		if valNode == nil {
			continue
		}
		if v := phpExtractClassRef(valNode, src); v != "" {
			out.args[key] = v
		}
	}
	return out
}

// symfonyDispatchAttributes names the attributes we extract dispatch
// edges for. Currently only AsEventListener — the most common Symfony
// DI attribute and the one where static dispatch makes a real
// difference. AsController / AsCommand are recognisable but don't
// produce dispatch edges (AsController is a marker; AsCommand binds
// a command name that isn't a node in the graph).
var symfonyDispatchAttributes = map[string]string{
	"AsEventListener": "event",
}

func emitAttributeEdges(attrs []phpAttribute, fromID, filePath string, fallbackLine int, result *parser.ExtractionResult) {
	for _, a := range attrs {
		targetKey, ok := symfonyDispatchAttributes[a.name]
		if !ok {
			continue
		}
		target := a.args[targetKey]
		if target == "" {
			continue
		}
		line := a.line
		if line == 0 {
			line = fallbackLine
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     fromID,
			To:       "unresolved::" + target,
			Kind:     graph.EdgeConsumes,
			FilePath: filePath,
			Line:     line,
			Meta: map[string]any{
				"dispatch_attribute": a.name,
				"symfony_event":      target,
			},
		})
	}
}

// extractPhpParentClass returns the immediate base class for a PHP
// class_declaration, or "" when the class has no `extends` clause.
// PHP supports single inheritance, so the `base_clause` child holds
// exactly one name. Used by the scope-based static resolver to
// answer `parent::method()` calls.
func extractPhpParentClass(classNode *sitter.Node, src []byte) string {
	if classNode == nil {
		return ""
	}
	for i, _nc := 0, int(classNode.NamedChildCount()); i < _nc; i++ {
		child := classNode.NamedChild(i)
		if child.Type() != "base_clause" {
			continue
		}
		for j, _nc := 0, int(child.NamedChildCount()); j < _nc; j++ {
			sub := child.NamedChild(j)
			switch sub.Type() {
			case "name", "qualified_name":
				text := strings.TrimSpace(sub.Content(src))
				if i := strings.LastIndex(text, `\`); i >= 0 {
					text = text[i+1:]
				}
				return text
			}
		}
	}
	return ""
}

// phpTypeClauseNames returns the type names listed in the first direct child
// of clauseType — "class_interface_clause" for a class's `implements I, J`,
// "base_clause" for an interface's `extends A, B`. Names keep their namespace
// qualification; the dispatch resolver reduces them to simple names. Nil when
// there is no such clause.
func phpTypeClauseNames(node *sitter.Node, src []byte, clauseType string) []string {
	if node == nil {
		return nil
	}
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		if child != nil && child.Type() == clauseType {
			return phpClauseTypeNames(child, src)
		}
	}
	return nil
}

// phpDocMethodRe matches a phpdoc `@method` tag: an optional `static`, an
// optional return type token, then the method name immediately before `(`.
// Return types with `|` / `?` / `\` / `[]` are captured as one token; the
// name is always the identifier right before the parameter list.
var phpDocMethodRe = regexp.MustCompile(`@method\s+(?:(static)\s+)?(?:([^\s()]+)\s+)?(\w+)\s*\(`)

// phpDeclaresMagicCall reports whether a class/trait body declared __call or
// __callStatic — the PHP magic-method dispatch hooks.
func phpDeclaresMagicCall(methodNodes map[string]*sitter.Node) bool {
	if methodNodes == nil {
		return false
	}
	_, call := methodNodes["__call"]
	_, callStatic := methodNodes["__callStatic"]
	return call || callStatic
}

// phpDocBlockAbove returns the raw `/** ... */` docblock immediately preceding
// a declaration, or "" when the preceding sibling is not a doc comment. Unlike
// ExtractDocAbove (which keeps only the first paragraph) this returns the full
// block so @method tags below the summary survive.
func phpDocBlockAbove(node *sitter.Node, src []byte) string {
	sib := node.PrevNamedSibling()
	if sib == nil || sib.Type() != "comment" {
		return ""
	}
	txt := sib.Content(src)
	if !strings.HasPrefix(strings.TrimSpace(txt), "/**") {
		return ""
	}
	return txt
}

// emitPHPDocMethods mints KindMethod nodes for `@method` tags in a class/trait
// docblock — magic accessors and mixin methods that have no method_declaration
// and would otherwise resolve not_found. A real declaration of the same name
// always wins (the tag is skipped). Nodes are positioned on the class
// declaration line (a positionless decl breaks the LSP hover path) and carry
// virtual=phpdoc_method so consumers can tell them from concrete methods.
func (e *PHPExtractor) emitPHPDocMethods(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	className, classID string, methodNodes map[string]*sitter.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	doc := phpDocBlockAbove(node, src)
	if doc == "" || !strings.Contains(doc, "@method") {
		return
	}
	classLine := int(node.StartPoint().Row) + 1
	minted := map[string]bool{}
	for _, m := range phpDocMethodRe.FindAllStringSubmatch(doc, -1) {
		isStatic, retType, name := m[1] != "", m[2], m[3]
		if name == "" || minted[name] {
			continue
		}
		minted[name] = true
		if _, real := methodNodes[name]; real {
			continue // a concrete declaration wins over the docblock virtual
		}
		id := filePath + "::" + className + "." + name
		if seen[id] {
			continue
		}
		seen[id] = true
		meta := map[string]any{
			"receiver":    className,
			"scope_class": className,
			"visibility":  VisibilityPublic,
			"virtual":     "phpdoc_method",
		}
		if isStatic {
			meta["static"] = true
		}
		if retType != "" {
			meta["return_type"] = retType
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: classLine, EndLine: classLine,
			Language: "php", Meta: meta,
		})
		result.Edges = append(result.Edges,
			&graph.Edge{From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: classLine},
			&graph.Edge{From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: classLine},
		)
	}
}

// phpMemberVisibility returns the access modifier for a PHP class
// member (method or property). PHP defaults to "public" when no
// visibility modifier is present.
func phpMemberVisibility(member *sitter.Node, src []byte) string {
	if member == nil {
		return VisibilityPublic
	}
	for i, _nc := 0, int(member.ChildCount()); i < _nc; i++ {
		c := member.Child(i)
		if c == nil {
			continue
		}
		if c.Type() != "visibility_modifier" {
			continue
		}
		switch strings.TrimSpace(c.Content(src)) {
		case "public":
			return VisibilityPublic
		case "private":
			return VisibilityPrivate
		case "protected":
			return VisibilityProtected
		}
	}
	return VisibilityPublic
}
