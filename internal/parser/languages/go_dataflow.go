package languages

import (
	"strconv"
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// emitGoDataflow walks a Go function/method body and emits the
// CPG-lite dataflow primitives — EdgeValueFlow, EdgeArgOf, and
// EdgeReturnsTo — alongside the existing call/channel/closure
// machinery. The output captures intra-procedural data dependence
// (assignment LHS↔RHS, range source↔induction var, return
// expressions↔owner) and the inter-procedural binding at every
// call site (caller-side argument source↔callee parameter; callee
// return↔assignment LHS).
//
// Local-binding identity convention. Each Go local introduced by
// `x := …` / `var x = …` / a range clause / a type-switch / a for-
// statement init clause maps to a synthetic ID:
//
//	<ownerID>#local:<name>@+<offsetFromOwnerStartLine>
//
// where ownerID is the enclosing function/method node and the
// offset is the local's 1-based line minus the function-decl's
// 1-based line. The leading `+` flags the value as a relative
// offset rather than an absolute line — important for the
// incremental indexer: adding a line *above* the enclosing
// function leaves every local-binding ID inside it stable, so the
// per-save edge churn collapses from O(locals-in-file) to
// O(locals-below-the-edit).
//
// Each binding is materialised as a KindLocal graph node anchored
// to the enclosing function via EdgeMemberOf, so dataflow edges
// targeting locals are not orphan endpoints — they navigate to a
// first-class node like every other edge. KindLocal nodes are
// excluded from the BM25 search index (see
// internal/indexer.shouldIndexForSearch) so identifiers like
// `err` / `data` / `n` / `i` don't flood search results.
//
// v1 limitations:
//
//   - Closures (`func_literal`) are not recursed into. Their inner
//     dataflow attributes to the closure node when the closure
//     pass walks them separately. Captures are already covered by
//     `emitGoClosureCaptures`.
//   - Field writes to selector LHS produce a value_flow into the
//     existing field node when the receiver type is known via
//     paramsByName; otherwise the write target falls back to
//     `unresolved::*.<field>` (same convention the writes pass
//     uses for downstream resolution).
//   - The arg_of / returns_to targets carry placeholder text that
//     mirrors the call edge for the same call site. Indexer post-
//     resolution rewrites them once the callee is known — see
//     `materializeDataflowParams` in internal/indexer.
func emitGoDataflow(ownerID string, ownerStartLine int, body *sitter.Node, paramsByName map[string]string, imports map[string]string, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	scope := newGoFlowScope()
	for name := range paramsByName {
		if name == "" || name == "_" {
			continue
		}
		paramID := goParamNodeID(ownerID, name, 0)
		scope.bindings[name] = []string{paramID}
	}
	walker := &goFlowWalker{
		ownerID:        ownerID,
		ownerStartLine: ownerStartLine,
		filePath:       filePath,
		src:            src,
		scope:          scope,
		result:         result,
		emittedLocals:  map[string]struct{}{},
		imports:        imports,
	}
	walker.walk(body)
}

// bindLocal computes the canonical local-binding ID, registers it in
// scope, and on first sight emits the corresponding KindLocal node +
// EdgeMemberOf edge so the binding is a first-class graph element
// rather than a phantom edge endpoint. Returns the ID. Dedupe key is
// the ID itself: a binding visited through multiple walk paths still
// produces one node row.
func (w *goFlowWalker) bindLocal(name string, line int) string {
	id := w.localID(name, line)
	w.scope.bindings[name] = []string{id}
	if _, ok := w.emittedLocals[id]; ok {
		return id
	}
	w.emittedLocals[id] = struct{}{}
	w.result.Nodes = append(w.result.Nodes, &graph.Node{
		ID:        id,
		Kind:      graph.KindLocal,
		Name:      name,
		FilePath:  w.filePath,
		StartLine: line,
		EndLine:   line,
		Language:  "go",
	})
	w.result.Edges = append(w.result.Edges, &graph.Edge{
		From:     id,
		To:       w.ownerID,
		Kind:     graph.EdgeMemberOf,
		FilePath: w.filePath,
		Line:     line,
		Origin:   graph.OriginASTResolved,
	})
	return id
}

// goFlowScope tracks the most recent source IDs for each named
// binding inside a function body. Reassignment replaces the slice
// (the new value supersedes the old one); short_var_decl creates a
// fresh local-binding ID anchored at the decl line.
type goFlowScope struct {
	bindings map[string][]string
}

func newGoFlowScope() *goFlowScope {
	return &goFlowScope{bindings: map[string][]string{}}
}

// goFlowWalker carries the per-function state needed to emit
// dataflow edges. ownerID is the enclosing function node ID;
// ownerStartLine is the 1-based source line of the function's
// declaration — local-binding IDs are anchored to it so edits
// above the function don't churn every binding inside;
// scope tracks live bindings; result accumulates emitted edges;
// emittedLocals dedupes KindLocal node emissions so a binding
// visited through more than one walk path doesn't produce
// duplicate node rows.
type goFlowWalker struct {
	ownerID        string
	ownerStartLine int
	filePath       string
	src            []byte
	scope          *goFlowScope
	result         *parser.ExtractionResult
	emittedLocals  map[string]struct{}
	// imports maps the file's package aliases to their import paths
	// (`fmt → "fmt"`, `assert → "github.com/stretchr/testify/assert"`).
	// Threaded through so the selector-expression cases in calleeRef /
	// exprSources can emit `unresolved::extern::<importPath>::<method>`
	// when the LHS identifier is an imported package — matching the
	// shape the call extractor uses — instead of collapsing the
	// qualifier to `*.` and losing the resolution evidence.
	imports map[string]string
}

func (w *goFlowWalker) walk(n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Type() {
	case "func_literal":
		// v1 limitation: don't recurse into closures. Their
		// dataflow is owned by the closure node, walked separately.
		return
	case "short_var_declaration":
		w.handleShortVarDecl(n)
		return
	case "var_spec":
		w.handleVarSpec(n)
		return
	case "assignment_statement":
		w.handleAssignment(n)
		return
	case "return_statement":
		w.handleReturn(n)
		return
	case "range_clause":
		w.handleRangeClause(n)
		return
	case "call_expression":
		w.handleCall(n, nil)
		return
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		w.walk(n.NamedChild(i))
	}
}

// localID returns the synthetic local-binding ID for `name` at the
// given absolute line. Always anchored to ownerID so two functions
// can have identically-named locals without colliding. The line is
// encoded as an offset from the owner's declaration line (prefixed
// `+` so it's unambiguous): a same-function shift caused by an edit
// above the function leaves the ID stable. A defensive zero-anchor
// fallback handles cases where the caller didn't supply an owner
// start line (the walker is constructed with one in production; the
// fallback keeps misuse from producing IDs missing the @ separator).
func (w *goFlowWalker) localID(name string, line int) string {
	offset := line
	if w.ownerStartLine > 0 {
		offset = line - w.ownerStartLine + 1
	}
	return w.ownerID + "#local:" + name + "@+" + strconv.Itoa(offset)
}

func (w *goFlowWalker) handleShortVarDecl(n *sitter.Node) {
	left := n.ChildByFieldName("left")
	right := n.ChildByFieldName("right")
	if left == nil || right == nil {
		return
	}
	w.bindAssignment(left, right, true, int(n.StartPoint().Row)+1)
}

func (w *goFlowWalker) handleAssignment(n *sitter.Node) {
	left := n.ChildByFieldName("left")
	right := n.ChildByFieldName("right")
	if left == nil || right == nil {
		return
	}
	w.bindAssignment(left, right, false, int(n.StartPoint().Row)+1)
}

func (w *goFlowWalker) handleVarSpec(n *sitter.Node) {
	nameNode := n.ChildByFieldName("name")
	value := n.ChildByFieldName("value")
	if nameNode == nil {
		return
	}
	if value == nil {
		// A zero-initialized declaration still introduces bindings. Without
		// these, reads before the first assignment point at phantom sources
		// and a later assignment can appear to be the declaration instead.
		for i := 0; i < int(n.NamedChildCount()); i++ {
			name := n.NamedChild(i)
			if name != nil && name.Type() == "identifier" {
				w.declareTarget(name, true, int(n.StartPoint().Row)+1)
			}
		}
		return
	}
	w.bindAssignment(nameNode, value, true, int(n.StartPoint().Row)+1)
}

// bindAssignment is the shared LHS↔RHS handler for short_var,
// var_spec, and ordinary assignment. `decl` distinguishes
// short_var/var_spec (which introduce fresh locals) from `=`
// (which targets an existing binding or a field).
func (w *goFlowWalker) bindAssignment(left, right *sitter.Node, decl bool, line int) {
	leftItems := w.expressionList(left)
	rightItems := w.expressionList(right)

	// Single call on RHS — emit one returns_to per LHS via
	// handleCall, which also emits the arg_of edges for each
	// argument source. Covers both `v := f(x)` (single LHS) and
	// `a, b := f(x)` (multi-return Go signature).
	if len(rightItems) == 1 && rightItems[0].Type() == "call_expression" {
		callExpr := rightItems[0]
		lhsLocals := make([]string, 0, len(leftItems))
		for _, lhs := range leftItems {
			id, ok := w.declareTarget(lhs, decl, line)
			if !ok {
				lhsLocals = append(lhsLocals, "")
				continue
			}
			lhsLocals = append(lhsLocals, id)
		}
		w.handleCall(callExpr, lhsLocals)
		return
	}

	// Pair-wise binding. Surplus LHS get empty source sets; surplus
	// RHS expressions still emit arg_of inside their own walks via
	// handleCall when we descend through w.exprSources.
	for i, lhs := range leftItems {
		var rhsSources []string
		if i < len(rightItems) {
			rhsSources = w.exprSources(rightItems[i])
		}
		lhsID, ok := w.declareTarget(lhs, decl, line)
		if !ok {
			continue
		}
		for _, src := range rhsSources {
			if src == "" || src == lhsID {
				continue
			}
			w.emitValueFlow(src, lhsID, line)
		}
	}
}

// declareTarget interprets a single LHS item (identifier or
// selector). Returns the resulting binding ID and true on success.
// For declarations (short_var / var_spec) it registers the new
// local in scope; for plain assignments to identifiers it overwrites
// the binding. Selector LHSes resolve to the field node when the
// receiver type is known and otherwise return the unresolved
// `*.<field>` form so the resolver / writes pass can lift them.
func (w *goFlowWalker) declareTarget(lhs *sitter.Node, decl bool, line int) (string, bool) {
	if lhs == nil {
		return "", false
	}
	switch lhs.Type() {
	case "identifier":
		name := lhs.Content(w.src)
		if name == "" || name == "_" {
			return "", false
		}
		return w.bindLocal(name, line), true
	case "selector_expression":
		// `x.field = …` — write goes to the field node when known.
		field := lhs.ChildByFieldName("field")
		if field == nil {
			return "", false
		}
		fieldName := field.Content(w.src)
		if fieldName == "" {
			return "", false
		}
		// Best-effort field resolution: leave the resolver / writes
		// pass to do the heavy lifting; we use the same unresolved
		// form so the post-pass can join.
		return "unresolved::*." + fieldName, true
	case "index_expression":
		// `m[k] = …` — flow target is the indexed expression's source.
		operand := lhs.ChildByFieldName("operand")
		if operand == nil {
			return "", false
		}
		srcs := w.exprSources(operand)
		if len(srcs) == 0 {
			return "", false
		}
		return srcs[0], true
	}
	if !decl {
		return "", false
	}
	return "", false
}

// expressionList unwraps an expression_list node into its named
// children. Single expressions are returned wrapped as a one-element
// slice so callers can iterate uniformly.
func (w *goFlowWalker) expressionList(n *sitter.Node) []*sitter.Node {
	if n == nil {
		return nil
	}
	if n.Type() != "expression_list" {
		return []*sitter.Node{n}
	}
	out := make([]*sitter.Node, 0, n.NamedChildCount())
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (w *goFlowWalker) handleReturn(n *sitter.Node) {
	if n == nil {
		return
	}
	// Collect direct return expressions. Bare `return` has no
	// expression list — owner gets no extra edges (the function
	// signature carries the structural contract via EdgeReturns).
	var exprs []*sitter.Node
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "expression_list" {
			for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
				e := c.NamedChild(j)
				if e != nil {
					exprs = append(exprs, e)
				}
			}
		} else {
			exprs = append(exprs, c)
		}
	}
	for i, e := range exprs {
		sources := w.exprSources(e)
		for _, src := range sources {
			if src == "" {
				continue
			}
			edge := &graph.Edge{
				From:     src,
				To:       w.ownerID,
				Kind:     graph.EdgeValueFlow,
				FilePath: w.filePath,
				Line:     int(e.StartPoint().Row) + 1,
				Origin:   graph.OriginASTResolved,
				Meta: map[string]any{
					"return_position": i,
				},
			}
			w.result.Edges = append(w.result.Edges, edge)
		}
	}
}

func (w *goFlowWalker) handleRangeClause(n *sitter.Node) {
	left := n.ChildByFieldName("left")
	right := n.ChildByFieldName("right")
	if left == nil || right == nil {
		return
	}
	rhsSources := w.exprSources(right)
	line := int(n.StartPoint().Row) + 1
	leftItems := w.expressionList(left)
	for _, lhs := range leftItems {
		if lhs == nil || lhs.Type() != "identifier" {
			continue
		}
		name := lhs.Content(w.src)
		if name == "" || name == "_" {
			continue
		}
		id := w.bindLocal(name, line)
		for _, src := range rhsSources {
			if src == "" || src == id {
				continue
			}
			w.emitValueFlow(src, id, line)
		}
	}
}

// handleCall emits arg_of for every argument and (when lhsLocals is
// non-nil) one returns_to placeholder per LHS. Recurses through
// argument expressions so nested calls also emit their own bindings.
func (w *goFlowWalker) handleCall(n *sitter.Node, lhsLocals []string) {
	if n == nil {
		return
	}
	calleeText := w.calleeRef(n)
	args := w.callArguments(n)
	line := int(n.StartPoint().Row) + 1

	if calleeText != "" {
		for i, arg := range args {
			if arg == nil {
				continue
			}
			// Recurse so nested call expressions get their own
			// arg_of edges before we use their callee as a source.
			w.walk(arg)
			sources := w.exprSources(arg)
			for _, src := range sources {
				if src == "" {
					continue
				}
				edge := &graph.Edge{
					From:     src,
					To:       calleeText,
					Kind:     graph.EdgeArgOf,
					FilePath: w.filePath,
					Line:     line,
					Origin:   graph.OriginASTInferred,
					Meta: map[string]any{
						"arg_position": i,
					},
				}
				w.result.Edges = append(w.result.Edges, edge)
			}
		}
		// returns_to: emitted with placeholder From=ownerID. The
		// post-resolution pass joins on (caller, line) against the
		// EdgeCalls edge for this call site to lift From to the
		// resolved callee.
		for i, lhs := range lhsLocals {
			if lhs == "" {
				continue
			}
			edge := &graph.Edge{
				From:     w.ownerID,
				To:       lhs,
				Kind:     graph.EdgeReturnsTo,
				FilePath: w.filePath,
				Line:     line,
				Origin:   graph.OriginASTInferred,
				Meta: map[string]any{
					"returns_to_call": true,
					"call_line":       line,
					"return_position": i,
					"callee_target":   calleeText,
				},
			}
			w.result.Edges = append(w.result.Edges, edge)
		}
	} else {
		// Even if we couldn't compute a callee, still recurse so
		// nested calls inside arguments emit their own bindings.
		for _, arg := range args {
			w.walk(arg)
		}
	}
}

// callArguments returns the arguments node's children as a flat
// slice. Returns nil when the call has no argument list.
func (w *goFlowWalker) callArguments(call *sitter.Node) []*sitter.Node {
	if call == nil {
		return nil
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	out := make([]*sitter.Node, 0, args.NamedChildCount())
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

// calleeRef returns the unresolved-target string used as the
// outgoing/incoming callee position on dataflow edges. Mirrors the
// existing EdgeCalls extraction so the resolver lifts both kinds
// consistently. Empty string when the call form is unsupported
// (e.g. dynamic call via interface() value the extractor can't
// resolve).
func (w *goFlowWalker) calleeRef(call *sitter.Node) string {
	if call == nil {
		return ""
	}
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		name := fn.Content(w.src)
		if name == "" {
			return ""
		}
		return "unresolved::" + name
	case "selector_expression":
		recv := fn.ChildByFieldName("operand")
		field := fn.ChildByFieldName("field")
		if field == nil {
			return ""
		}
		method := field.Content(w.src)
		if method == "" {
			return ""
		}
		// Package-qualified call: when the receiver is a bare
		// identifier matching one of the file's import aliases,
		// emit the same `unresolved::extern::<importPath>::<method>`
		// shape the call extractor uses for explicit calls (see
		// golang.go::Extract `imports[c.receiver]` branch). The
		// resolver's resolveExtern pass then lands these on
		// stdlib::/dep::/external:: targets or the real cross-repo
		// symbol when the import path resolves to an indexed file.
		// Without this branch the qualifier is dropped and we leak
		// `unresolved::*.<method>` for every package call inside a
		// dataflow context.
		if recv != nil && recv.Type() == "identifier" {
			if importPath := w.importPathFor(recv.Content(w.src)); importPath != "" {
				return "unresolved::extern::" + importPath + "::" + method
			}
		}
		return "unresolved::*." + method
	case "generic_function":
		// `f[T](args)` — strip the type instantiation wrapper.
		inner := fn.ChildByFieldName("function")
		if inner != nil {
			synthetic := *fn // shallow copy; reuse logic on inner.
			_ = synthetic
		}
		// Inline: try the inner identifier.
		for i, _nc := 0, int(fn.NamedChildCount()); i < _nc; i++ {
			c := fn.NamedChild(i)
			if c == nil {
				continue
			}
			if c.Type() == "identifier" {
				name := c.Content(w.src)
				if name != "" {
					return "unresolved::" + name
				}
			}
			if c.Type() == "selector_expression" {
				field := c.ChildByFieldName("field")
				if field != nil {
					if name := field.Content(w.src); name != "" {
						return "unresolved::*." + name
					}
				}
			}
		}
	}
	return ""
}

// exprSources computes the dataflow source IDs for an expression.
// Constants and unsupported shapes return an empty slice. Flow is
// over-approximated: a binary expression contributes both operand
// sources, a selector contributes the resolved field (or unresolved
// `*.<name>` form) — same convention the existing reads/writes
// pipeline uses.
func (w *goFlowWalker) exprSources(n *sitter.Node) []string {
	if n == nil {
		return nil
	}
	switch n.Type() {
	case "identifier":
		name := n.Content(w.src)
		if name == "" || name == "_" {
			return nil
		}
		if isGoDataflowSkip(name) {
			return nil
		}
		if bindings, ok := w.scope.bindings[name]; ok {
			out := make([]string, len(bindings))
			copy(out, bindings)
			return out
		}
		// Package-level / import / sentinel — let the post-pass try
		// to resolve via the resolver pipeline later.
		return []string{"unresolved::" + name}
	case "selector_expression":
		field := n.ChildByFieldName("field")
		if field == nil {
			return nil
		}
		fieldName := field.Content(w.src)
		if fieldName == "" {
			return nil
		}
		// Package-qualified value: when the receiver is a bare
		// identifier matching one of the file's import aliases,
		// emit `unresolved::extern::<importPath>::<field>` so the
		// resolver can land it on stdlib::/dep::/external::. See
		// the matching comment in calleeRef.
		operand := n.ChildByFieldName("operand")
		if operand != nil && operand.Type() == "identifier" {
			if importPath := w.importPathFor(operand.Content(w.src)); importPath != "" {
				return []string{"unresolved::extern::" + importPath + "::" + fieldName}
			}
		}
		return []string{"unresolved::*." + fieldName}
	case "call_expression":
		ref := w.calleeRef(n)
		if ref == "" {
			return nil
		}
		// Recurse into arguments so nested calls emit their own
		// arg_of edges; the call's value source is the callee.
		w.handleCall(n, nil)
		return []string{ref}
	case "parenthesized_expression":
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			if r := w.exprSources(n.NamedChild(i)); len(r) > 0 {
				return r
			}
		}
		return nil
	case "unary_expression":
		operand := n.ChildByFieldName("operand")
		if operand == nil {
			return nil
		}
		return w.exprSources(operand)
	case "binary_expression":
		left := n.ChildByFieldName("left")
		right := n.ChildByFieldName("right")
		var out []string
		out = append(out, w.exprSources(left)...)
		out = append(out, w.exprSources(right)...)
		return out
	case "type_assertion_expression":
		operand := n.ChildByFieldName("operand")
		if operand != nil {
			return w.exprSources(operand)
		}
	case "type_conversion_expression":
		operand := n.ChildByFieldName("operand")
		if operand != nil {
			return w.exprSources(operand)
		}
	case "index_expression", "slice_expression":
		operand := n.ChildByFieldName("operand")
		if operand != nil {
			return w.exprSources(operand)
		}
	case "composite_literal":
		// composite_literal's element values may carry flow; collect
		// from each named element to surface field-init dataflow.
		var out []string
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c == nil {
				continue
			}
			out = append(out, w.exprSources(c)...)
		}
		return out
	case "keyed_element", "literal_element":
		// Collect from whichever child carries a value subtree.
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c == nil {
				continue
			}
			if r := w.exprSources(c); len(r) > 0 {
				return r
			}
		}
	case "func_literal":
		// v1: closures aren't traced as sources; the closure node
		// is the value the outer expression produces.
		return nil
	}
	return nil
}

// isGoDataflowSkip filters identifiers that aren't useful as
// dataflow sources — Go keywords, predeclared constants, and the
// builtin functions whose name doesn't reference a value the agent
// would chase through the graph. Distinct from
// isGoBuiltinOrKeyword (which is purposefully aggressive for the
// closure-captures pass — e.g. it skips single-letter loop vars).
func isGoDataflowSkip(name string) bool {
	switch name {
	case "nil", "true", "false", "iota",
		"string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"float32", "float64", "complex64", "complex128",
		"bool", "byte", "rune", "error", "any", "comparable",
		"make", "new", "len", "cap", "append", "copy", "delete",
		"panic", "recover", "print", "println", "close",
		"min", "max", "clear":
		return true
	}
	return false
}

// emitValueFlow appends an EdgeValueFlow edge with the standard
// Origin / Confidence stamps. Skips no-op self-loops.
func (w *goFlowWalker) emitValueFlow(src, dst string, line int) {
	if src == "" || dst == "" || src == dst {
		return
	}
	if !strings.Contains(src, "::") && !strings.Contains(src, "#") {
		return
	}
	w.result.Edges = append(w.result.Edges, &graph.Edge{
		From:     src,
		To:       dst,
		Kind:     graph.EdgeValueFlow,
		FilePath: w.filePath,
		Line:     line,
		Origin:   graph.OriginASTResolved,
	})
}

// importPathFor returns the import path the given identifier names
// as a package alias in the current file, or "" when the identifier
// doesn't match any import. The walker's imports map is the same
// map populated by the Go extractor's emitImport handler, so an
// `assert` alias for `github.com/stretchr/testify/assert` resolves
// here exactly as it does in the call extractor's
// `imports[c.receiver]` branch.
func (w *goFlowWalker) importPathFor(name string) string {
	if name == "" || w.imports == nil {
		return ""
	}
	return w.imports[name]
}
