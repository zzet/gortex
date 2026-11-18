package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// This file covers the Rust item kinds the original query set never
// captured. Measured against 4,000 files of real crate source, they are
// not corner cases: 10,094 type aliases, 4,206 modules, 1,148
// `macro_rules!` definitions, 416 `extern crate` declarations and 38,973
// `extern` blocks produced exactly zero graph nodes between them.

// rustContainerIDs maps a container declaration's 0-based start row to
// the node id actually minted for it. Two same-named types in one file
// — the norm once inline modules exist — get distinct ids from
// disambiguateID, so a member can no longer reconstruct its owner's id
// by name alone. Without this the second `mod`'s fields silently
// attached to the first `mod`'s type.
type rustContainerIDs map[int]string

// register records the id minted for a container node.
func (c rustContainerIDs) register(def *sitter.Node, id string) {
	if def != nil && id != "" {
		c[int(def.StartPoint().Row)] = id
	}
}

// lookup returns the minted id for a container node, falling back to the
// by-name form for containers emitted before this map existed.
func (c rustContainerIDs) lookup(def *sitter.Node, fallback string) string {
	if def == nil {
		return fallback
	}
	if id, ok := c[int(def.StartPoint().Row)]; ok {
		return id
	}
	return fallback
}

// emitRustModule mints a node for a `mod` item. Rust modules are a
// namespace, not a dependency, so they follow the C++/C# namespace
// precedent (KindPackage) rather than KindModule — which Gortex reserves
// for package-manager dependencies (see dotnet_project.go, mcp_config.go).
//
// Items inside the module keep their flat `<file>::<Name>` ids and gain a
// `scope_mod` meta instead, mirroring C#'s `scope_ns`. Qualifying the ids
// themselves would rewrite every Rust node id in the graph and every
// resolver path that reconstructs one; the scope meta buys attribution
// without that churn.
func emitRustModule(def *sitter.Node, name, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) string {
	id, ok := disambiguateID(seen, filePath+"::"+name, int(def.StartPoint().Row)+1)
	if !ok {
		return ""
	}
	meta := map[string]any{
		"visibility":  rustVisibility(def, src),
		"type_flavor": "module",
	}
	// `mod foo;` (no body) points at another file — worth distinguishing
	// from an inline module because the resolver has to go find it.
	if def.ChildByFieldName("body") == nil {
		meta["declaration_only"] = true
	}
	if path := rustEnclosingModulePath(def, src); path != "" {
		meta["scope_mod"] = path
	}
	if doc := ExtractDocAbove(src, int(def.StartPoint().Row), DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: name,
		FilePath:  filePath,
		StartLine: int(def.StartPoint().Row) + 1,
		EndLine:   int(def.EndPoint().Row) + 1,
		Language:  "rust",
		Meta:      meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(def.StartPoint().Row) + 1,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def), id, filePath, src, result, annotationSeen)
	return id
}

// emitRustTypeAlias mints a node for `type Alias<T> = Target;`. The alias
// target is recorded both as meta and as a type-usage edge so
// find_usages on the aliased type reaches the alias.
func emitRustTypeAlias(def *sitter.Node, name, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	line := int(def.StartPoint().Row) + 1
	id, ok := disambiguateID(seen, filePath+"::"+name, line)
	if !ok {
		return
	}
	meta := map[string]any{
		"visibility":  rustVisibility(def, src),
		"type_flavor": "alias",
	}
	if t := def.ChildByFieldName("type"); t != nil {
		target := strings.TrimSpace(t.Content(src))
		meta["aliases"] = target
		emitRustTypeUseEdges(id, target, filePath, line, result)
	}
	if doc := ExtractDocAbove(src, int(def.StartPoint().Row), DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	if scope := rustEnclosingModulePath(def, src); scope != "" {
		meta["scope_mod"] = scope
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath:  filePath,
		StartLine: line,
		EndLine:   int(def.EndPoint().Row) + 1,
		Language:  "rust",
		Meta:      meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def), id, filePath, src, result, annotationSeen)
	emitRustGenericParamNodes(id, def, src, filePath, line, result)
}

// emitRustUnion mints a node for a `union`. Its fields flow through the
// ordinary field pass once that stops skipping non-struct containers.
func emitRustUnion(def *sitter.Node, name, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, containers rustContainerIDs) {
	line := int(def.StartPoint().Row) + 1
	id, ok := disambiguateID(seen, filePath+"::"+name, line)
	if !ok {
		return
	}
	containers.register(def, id)
	meta := map[string]any{
		"visibility":  rustVisibility(def, src),
		"type_flavor": "union",
		// A union is inherently unsafe to read; surfacing that lets an
		// audit query find every one without re-parsing.
		"unsafe_access": true,
	}
	if doc := ExtractDocAbove(src, int(def.StartPoint().Row), DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	if scope := rustEnclosingModulePath(def, src); scope != "" {
		meta["scope_mod"] = scope
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath:  filePath,
		StartLine: line,
		EndLine:   int(def.EndPoint().Row) + 1,
		Language:  "rust",
		Meta:      meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def), id, filePath, src, result, annotationSeen)
	emitRustGenericParamNodes(id, def, src, filePath, line, result)
}

// emitRustMacroDef mints a node for a `macro_rules!` definition so
// invocation sites have something to bind to.
func emitRustMacroDef(def *sitter.Node, name, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	line := int(def.StartPoint().Row) + 1
	id, ok := disambiguateID(seen, filePath+"::"+name+"!", line)
	if !ok {
		return
	}
	meta := map[string]any{"type_flavor": "macro_rules"}
	// #[macro_export] is what makes a macro reachable from other crates —
	// the macro equivalent of `pub`.
	for _, attr := range rustCollectAttributes(def) {
		if strings.Contains(attr.Content(src), "macro_export") {
			meta["visibility"] = VisibilityPublic
			meta["exported"] = true
			break
		}
	}
	if _, ok := meta["visibility"]; !ok {
		meta["visibility"] = VisibilityPrivate
	}
	if doc := ExtractDocAbove(src, int(def.StartPoint().Row), DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	if scope := rustEnclosingModulePath(def, src); scope != "" {
		meta["scope_mod"] = scope
	}
	// Arity is the number of `macro_rule` arms — a cheap proxy for how
	// much surface the macro has.
	arms := 0
	for child := range def.NamedChildren() {
		if child.Type() == "macro_rule" {
			arms++
		}
	}
	if arms > 0 {
		meta["rules"] = arms
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMacro, Name: name,
		FilePath:  filePath,
		StartLine: line,
		EndLine:   int(def.EndPoint().Row) + 1,
		Language:  "rust",
		Meta:      meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def), id, filePath, src, result, annotationSeen)
}

// emitRustExternCrate records a 2015-edition `extern crate serde;` as an
// import, so a crate dependency declared that way is as visible as one
// declared with `use`.
func emitRustExternCrate(def *sitter.Node, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	nameNode := def.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	crate := nameNode.Content(src)
	line := int(def.StartPoint().Row) + 1
	edge := &graph.Edge{
		From: fileID, To: "unresolved::import::" + crate,
		Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		Meta: map[string]any{"extern_crate": true},
	}
	if alias := def.ChildByFieldName("alias"); alias != nil {
		edge.Meta["alias"] = alias.Content(src)
	}
	if v := rustVisibility(def, src); v == VisibilityPublic || v == VisibilityInternal {
		edge.Meta["reexport"] = true
	}
	result.Edges = append(result.Edges, edge)
}

// emitRustAssociatedType mints a node for a trait's `type Item;`. The
// associated type is part of the trait's contract, so it hangs off the
// trait the same way a trait method does.
func emitRustAssociatedType(def *sitter.Node, filePath string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	nameNode := def.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	owner := findEnclosingRustContainer(def, "trait_item")
	if owner == nil {
		owner = findEnclosingRustContainer(def, "impl_item")
	}
	if owner == nil {
		return
	}
	ownerName := rustDeclName(owner, src)
	if ownerName == "" {
		if t := owner.ChildByFieldName("type"); t != nil {
			ownerName = rustTraitPathBaseName(strings.TrimSpace(t.Content(src)))
		}
	}
	if ownerName == "" {
		return
	}
	name := nameNode.Content(src)
	line := int(def.StartPoint().Row) + 1
	ownerID := filePath + "::" + ownerName
	id, ok := disambiguateID(seen, ownerID+"."+name, line)
	if !ok {
		return
	}
	meta := map[string]any{
		"receiver":    ownerName,
		"type_flavor": "associated_type",
	}
	// A bound on an associated type (`type Item: Display;`) is part of
	// the contract callers can rely on.
	for child := range def.NamedChildren() {
		if child.Type() == "trait_bounds" {
			meta["bound"] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(child.Content(src)), ":"))
			break
		}
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath:  filePath,
		StartLine: line,
		EndLine:   int(def.EndPoint().Row) + 1,
		Language:  "rust",
		Meta:      meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
	})
}

// emitRustImplBlock handles what an `impl` block carries beyond its
// methods: the trait it implements, and the attributes written on the
// block itself.
//
// `impl Display for Buf` left no relationship between Buf and Display at
// all — only a method-level EdgeOverrides — so find_implementations and
// get_class_hierarchy could not see the trait hierarchy, and a marker
// impl with no methods (`impl Send for Handle {}`) left no trace
// whatsoever.
//
// Attributes on the block were dropped outright, which is how
// #[wasm_bindgen] impl, #[async_trait] impl and #[pymethods] all went
// unrecorded — including the file-level wasm_bindgen sentinel, which
// only ever saw function-level attributes.
func emitRustImplBlock(def *sitter.Node, filePath string, src []byte, result *parser.ExtractionResult, annotationSeen map[string]bool) {
	if def == nil {
		return
	}
	typeNode := def.ChildByFieldName("type")
	if typeNode == nil {
		return
	}
	implType := rustImplReceiverName(typeNode.Content(src))
	if implType == "" {
		return
	}
	line := int(def.StartPoint().Row) + 1
	// The impl target may be a generic instantiation (`Replacer<M>`);
	// the node it belongs to is declared under the bare name.
	typeID := filePath + "::" + rustTraitPathBaseName(implType)

	if tr := def.ChildByFieldName("trait"); tr != nil {
		if base := rustTraitPathBaseName(strings.TrimSpace(tr.Content(src))); base != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From: typeID, To: "unresolved::" + base,
				Kind: graph.EdgeImplements, FilePath: filePath, Line: line,
				Origin: graph.OriginASTInferred,
				Meta:   map[string]any{"via": "impl"},
			})
		}
	}
	emitRustAnnotationEdges(rustCollectAttributes(def), typeID, filePath, src, result, annotationSeen)
}

// dropRustImplEdgesWithoutSource removes the impl-block edges whose
// source type has no node in this file. emitRustImplBlock sources them at
// <file>::<Type> whatever the target is, so `impl From<u32> for Foreign`
// (Foreign declared in another file or crate) and `impl Ext for u8`
// produced implements / annotated edges from a node the extraction never
// emits. Nothing owns such an edge: repository eviction deletes edges by
// joining on existing nodes, so they outlived their repository.
func dropRustImplEdgesWithoutSource(result *parser.ExtractionResult, implEdges []*graph.Edge) {
	if len(implEdges) == 0 {
		return
	}
	nodeIDs := make(map[string]struct{}, len(result.Nodes))
	for _, n := range result.Nodes {
		nodeIDs[n.ID] = struct{}{}
	}
	dangling := make(map[*graph.Edge]struct{})
	for _, e := range implEdges {
		if _, ok := nodeIDs[e.From]; !ok {
			dangling[e] = struct{}{}
		}
	}
	if len(dangling) == 0 {
		return
	}
	kept := result.Edges[:0]
	for _, e := range result.Edges {
		if _, drop := dangling[e]; !drop {
			kept = append(kept, e)
		}
	}
	for i := len(kept); i < len(result.Edges); i++ {
		result.Edges[i] = nil
	}
	result.Edges = kept
}

// emitRustTupleFields emits positional fields for a tuple struct or a
// tuple enum variant. `struct Meters(f64)` and `Msg::Failed(Error)` used
// to emit no member at all, which also meant the wrapped type was
// invisible: find_usages on Error could not see that Msg::Failed carries
// one. Fields are named by position, matching how Rust addresses them
// (`self.0`).
func emitRustTupleFields(list *sitter.Node, filePath string, src []byte, result *parser.ExtractionResult, seen map[string]bool, containers rustContainerIDs) {
	if list == nil {
		return
	}
	ownerNode, ownerName, variant := rustTupleFieldOwner(list, src)
	if ownerNode == nil || ownerName == "" {
		return
	}
	ownerID := containers.lookup(ownerNode, filePath+"::"+ownerName)
	if variant != "" {
		// A variant payload nests under the variant, not the enum, so
		// two payload-carrying variants cannot collide on ".0".
		ownerID += "." + variant
		ownerName += "." + variant
	}
	pos := 0
	for i := 0; i < int(list.ChildCount()); i++ {
		c := list.Child(i)
		if c == nil || list.FieldNameForChild(i) != "type" {
			continue
		}
		typeText := strings.TrimSpace(c.Content(src))
		line := int(c.StartPoint().Row) + 1
		name := strconv.Itoa(pos)
		id, ok := disambiguateID(seen, ownerID+"."+name, line)
		pos++
		if !ok {
			continue
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindField, Name: name,
			FilePath: filePath, StartLine: line, EndLine: int(c.EndPoint().Row) + 1,
			Language: "rust",
			Meta: map[string]any{
				"receiver":   ownerName,
				"field_type": typeText,
				"positional": true,
				"visibility": rustVisibility(list.Parent(), src),
			},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
		})
		emitRustTypeUseEdges(id, typeText, filePath, line, result)
	}
}

// rustTupleFieldOwner resolves the struct or enum variant that owns a
// positional field list. The third result is the variant name when the
// owner is an enum variant, so the caller can nest the field under it.
func rustTupleFieldOwner(list *sitter.Node, src []byte) (*sitter.Node, string, string) {
	parent := list.Parent()
	if parent == nil {
		return nil, "", ""
	}
	switch parent.Type() {
	case "struct_item":
		return parent, rustDeclName(parent, src), ""
	case "enum_variant":
		variant := ""
		if n := parent.ChildByFieldName("name"); n != nil {
			variant = n.Content(src)
		}
		enum := findEnclosingRustContainer(parent, "enum_item")
		if enum == nil || variant == "" {
			return nil, "", ""
		}
		enumName := rustDeclName(enum, src)
		if enumName == "" {
			return nil, "", ""
		}
		return enum, enumName, variant
	}
	return nil, "", ""
}

// rustEnclosingTraitVisibility returns the visibility of the trait
// declaring n. A trait member has no visibility modifier of its own, so
// its reachability is the trait's.
func rustEnclosingTraitVisibility(n *sitter.Node, src []byte) string {
	if tr := findEnclosingRustContainer(n, "trait_item"); tr != nil {
		return rustVisibility(tr, src)
	}
	return rustVisibility(n, src)
}

// rustTraitImplVisibility returns the visibility to record for a method
// in `impl Trait for Type`. Such a method is callable by anyone who can
// name the trait, whatever the impl block looks like, so it is public
// rather than the "private" an absent modifier would otherwise imply.
// An inherent impl keeps ordinary per-method visibility.
func rustTraitImplVisibility(fn *sitter.Node, src []byte) string {
	explicit := rustVisibility(fn, src)
	if explicit != VisibilityPrivate {
		return explicit
	}
	if rustImplTraitName(fn, src) != "" {
		return VisibilityPublic
	}
	return explicit
}

// rustAssociatedItemOwner returns the impl or trait an associated item
// (a const, static or type declared inside a `declaration_list`) belongs
// to, together with the owner's type name. Returns a nil node and an
// empty name for a file-scope item.
func rustAssociatedItemOwner(item *sitter.Node, src []byte) (*sitter.Node, string) {
	if item == nil {
		return nil, ""
	}
	parent := item.Parent()
	if parent == nil || parent.Type() != "declaration_list" {
		return nil, ""
	}
	owner := parent.Parent()
	if owner == nil {
		return nil, ""
	}
	switch owner.Type() {
	case "trait_item":
		return owner, rustDeclName(owner, src)
	case "impl_item":
		t := owner.ChildByFieldName("type")
		if t == nil {
			return nil, ""
		}
		return owner, rustTraitPathBaseName(strings.TrimSpace(t.Content(src)))
	}
	return nil, ""
}

// rustFieldOwner resolves the declaration a named field belongs to. A
// `field_declaration` can appear under a struct, a union, or a
// struct-shaped enum variant; the owner name for a variant is
// `Enum.Variant` so it nests under the enum rather than floating free.
func rustFieldOwner(field *sitter.Node, src []byte) (*sitter.Node, string, string) {
	for p := field.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "struct_item", "union_item":
			return p, rustDeclName(p, src), ""
		case "enum_variant":
			variant := ""
			if n := p.ChildByFieldName("name"); n != nil {
				variant = n.Content(src)
			}
			enum := findEnclosingRustContainer(p, "enum_item")
			if enum == nil || variant == "" {
				return nil, "", ""
			}
			enumName := rustDeclName(enum, src)
			if enumName == "" {
				return nil, "", ""
			}
			// Anchor on the enum so the field id reads
			// <file>::<Enum>.<Variant>.<field>.
			return enum, enumName, variant
		}
	}
	return nil, "", ""
}

// rustEnclosingModulePath walks the parent chain and joins every
// enclosing `mod` name with "::", so an item in `mod a { mod b { .. } }`
// reports "a::b". Empty for a top-level item.
func rustEnclosingModulePath(n *sitter.Node, src []byte) string {
	var parts []string
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() != "mod_item" {
			continue
		}
		if name := p.ChildByFieldName("name"); name != nil {
			parts = append(parts, name.Content(src))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	// The walk collects innermost-first; the path reads outermost-first.
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "::")
}

// rustForeignABI returns the ABI string of the `extern "C" { .. }` block
// enclosing n (defaulting to "C", which is what a bare `extern` means),
// plus whether n is inside a foreign block at all.
func rustForeignABI(n *sitter.Node, src []byte) (string, bool) {
	block := findEnclosingRustContainer(n, "foreign_mod_item")
	if block == nil {
		return "", false
	}
	for child := range block.NamedChildren() {
		if child.Type() != "extern_modifier" {
			continue
		}
		for lit := range child.NamedChildren() {
			if lit.Type() == "string_literal" {
				return strings.Trim(lit.Content(src), `"`), true
			}
		}
	}
	return "C", true
}

// rustFnQualifiers reads the keyword prefix of a function item —
// `pub async unsafe extern "C" fn` — which the extractor previously
// discarded wholesale. Without it the graph cannot answer "which
// functions are async" or "which cross the FFI boundary".
func rustFnQualifiers(fn *sitter.Node, src []byte) map[string]any {
	out := map[string]any{}
	if fn == nil {
		return out
	}
	for i := 0; i < int(fn.ChildCount()); i++ {
		c := fn.Child(i)
		if c == nil {
			continue
		}
		// Stop at the name: everything after it is signature, not
		// qualifier.
		if c.Type() == "identifier" {
			break
		}
		switch c.Type() {
		case "function_modifiers":
			text := c.Content(src)
			if strings.Contains(text, "async") {
				out["is_async"] = true
			}
			if strings.Contains(text, "unsafe") {
				out["is_unsafe"] = true
			}
			if strings.Contains(text, "const") {
				out["is_const"] = true
			}
			if strings.Contains(text, "extern") {
				out["is_extern"] = true
				if q := strings.Index(text, `"`); q >= 0 {
					if end := strings.Index(text[q+1:], `"`); end >= 0 {
						out["abi"] = text[q+1 : q+1+end]
					}
				} else {
					out["abi"] = "C"
				}
			}
		}
	}
	return out
}

// buildRustSignature reconstructs a readable signature instead of the
// placeholder `fn name(...)` the extractor stamped on every function.
// Parameters, return type, generics and the where-clause all survive.
func buildRustSignature(fn *sitter.Node, name string, src []byte) string {
	if fn == nil {
		return "fn " + name + "(...)"
	}
	var b strings.Builder
	if mods := childOfTypeRust(fn, "function_modifiers"); mods != nil {
		b.WriteString(strings.Join(strings.Fields(mods.Content(src)), " "))
		b.WriteByte(' ')
	}
	b.WriteString("fn ")
	b.WriteString(name)
	if tp := fn.ChildByFieldName("type_parameters"); tp != nil {
		b.WriteString(collapseRustWhitespace(tp.Content(src)))
	}
	if params := fn.ChildByFieldName("parameters"); params != nil {
		b.WriteString(collapseRustWhitespace(params.Content(src)))
	} else {
		b.WriteString("()")
	}
	if rt := fn.ChildByFieldName("return_type"); rt != nil {
		b.WriteString(" -> ")
		b.WriteString(collapseRustWhitespace(rt.Content(src)))
	}
	if wc := childOfTypeRust(fn, "where_clause"); wc != nil {
		b.WriteByte(' ')
		b.WriteString(collapseRustWhitespace(wc.Content(src)))
	}
	return b.String()
}

// childOfTypeRust returns the first direct child of n with the given
// node type, or nil.
func childOfTypeRust(n *sitter.Node, t string) *sitter.Node {
	if n == nil {
		return nil
	}
	for i := 0; i < int(n.ChildCount()); i++ {
		c := n.Child(i)
		if c != nil && c.Type() == t {
			return c
		}
	}
	return nil
}

// collapseRustWhitespace flattens a multi-line signature fragment onto
// one line so the stamped signature stays a single readable string.
func collapseRustWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// applyRustFnQualifiers merges the async/unsafe/const/extern markers
// into a node's meta.
func applyRustFnQualifiers(meta map[string]any, fn *sitter.Node, src []byte) {
	for k, v := range rustFnQualifiers(fn, src) {
		meta[k] = v
	}
}

// applyRustScopeMod stamps the enclosing module path on an item. Ids stay
// flat, so this is what tells two same-named items in different modules
// apart, and what lets a consumer reconstruct `crate::a::b::Item`.
func applyRustScopeMod(meta map[string]any, n *sitter.Node, src []byte) {
	if path := rustEnclosingModulePath(n, src); path != "" {
		meta["scope_mod"] = path
	}
}
