package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Function-as-value capture.
//
// When a function is passed as a *value* rather than called — registering a
// handler (`router.Get("/x", handler)`), a callback (`list.forEach(process)`),
// an observer (`signal.connect(onChange)`), `&fn` / `Class::method` /
// `method(:sym)` special forms — no direct call edge exists, yet the function
// is genuinely reachable through the registration. A per-language AST walk
// collects each such value-position identifier as a FnValueCandidate; this file
// is the shared capture table and placeholder emitter. The resolver's
// ResolveFnValueCallbacks gate then binds each candidate to a real same-file
// function and drops the unbound ones.
//
// Splitting capture (here) from the gate (resolver) keeps the two halves
// independently testable and lets every language reuse one emitter instead of
// hand-rolling placeholder edges. The per-language value-position walks land on
// top of this skeleton.

// fnValueUnresolvedPrefix is the synthetic-target namespace a captured
// function value occupies until the gate binds it — the graph-owned
// graph.FnValuePlaceholderMarker, aliased here so the emit sites read locally
// and the resolver's pending-scan exclusion keys off the same constant. Kept
// human-readable (the bare name is appended) to match Gortex's navigable-ID
// convention.
const fnValueUnresolvedPrefix = graph.FnValuePlaceholderMarker

// fnValueCandidateVia marks a placeholder edge as awaiting the callback gate.
// It mirrors the resolver-side constant of the same value; the two packages
// share the string convention, not a symbol, to avoid an import cycle.
const fnValueCandidateVia = "callback_candidate"

// FnValueCandidate is one captured function-as-value reference: the identifier
// used in a value position (Name), the enclosing symbol or registration site it
// was found in (FromID), and its source location. A per-language walk
// accumulates these during extraction and flushes them with
// EmitFnValueCandidates.
type FnValueCandidate struct {
	FromID   string
	Name     string
	FilePath string
	Line     int
	// Form is the wrapper form the value sat in: "" (plain), "address_of"
	// (`&fn` / `@fn`), or "eta" (Scala `f _`).
	Form string
	// Lang is the capturing grammar; Ungated marks a qualified-path candidate
	// the gate may resolve cross-module.
	Lang    string
	Ungated bool
	// RecvHint scopes the gate's resolution for a special form: "<self>" (the
	// enclosing type), a concrete type name, or "" (repo-wide).
	RecvHint string
	// SkipGate marks a curated-HOF string callable that bypasses the same-file
	// gate and resolves by a repo-wide unique-or-drop rule.
	SkipGate bool
}

// captureFnValueCandidates records a function-as-value candidate for every
// identifier in the parse tree that names a same-file function/method yet is
// NOT in callee position — i.e. a function passed by bare name as a call
// argument, assigned to a field/variable, or placed in an initializer, rather
// than invoked. The resolver's ResolveFnValueCallbacks then binds each to its
// definition as a tiered callback-registration edge, recovering a real
// dependency that static call extraction misses (callers(handler) was empty).
//
// It is grammar-agnostic in two ways: it keys on `identifier` leaf nodes, and
// it distinguishes a call from a value by a source-level check — an identifier
// immediately followed (past whitespace) by '(' is the callee of a call (an
// existing call edge), so only the non-called uses become candidates. That also
// excludes a function's own `name(params)` declaration. Pre-filtering to names
// the file actually declares means it never emits a candidate that cannot bind.
// Call it once at the end of an extractor's Extract with the parse-tree root.
func captureFnValueCandidates(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	captureFnValueCandidatesWithGoWork(result, root, filePath, src, nil)
}

// captureFnValueCandidatesWithGoWork retains the common semantic callback.
// Only Go supplies the two qualifying grammar kinds from its existing tail
// preorder; other extractors keep the original all-node walk through the wrapper.
func captureFnValueCandidatesWithGoWork(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte, goWork *goTailWork) {
	if root == nil || result == nil {
		return
	}
	lang, fileID := "", ""
	funcs := map[string]bool{}
	for _, n := range result.Nodes {
		if n == nil || n.FilePath != filePath {
			continue
		}
		if n.Kind == graph.KindFile {
			if lang == "" {
				lang = n.Language
			}
			if fileID == "" {
				fileID = n.ID
			}
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			funcs[n.Name] = true
		}
	}
	funcRanges := buildFuncRanges(result)
	if len(funcRanges) == 0 && fileID == "" {
		return
	}
	spec := fnRefSpecFor(lang)
	var cands []FnValueCandidate
	seen := map[string]bool{}
	visit := func(n *sitter.Node) {
		// Special whole-node forms (method references, this.m / self.m,
		// Ruby symbol callables, selectors) are normalised to a (member,
		// receiver-hint) pair before the plain-identifier path.
		if refName, recvHint, ok := normalizeFnRefSpecial(n, src); ok {
			typeQualified := recvHint != "" && recvHint != "<self>"
			// Flood guard: a `<self>` / selector member must name a same-file
			// function/method; a type-qualified ref (`Foo::bar`) may bind
			// cross-file via the gate.
			if !typeQualified && !funcs[refName] {
				return
			}
			if fnRefStartsCall(spec, n, src) {
				return
			}
			line := int(n.StartPoint().Row) + 1
			fromID := findEnclosingFunc(funcRanges, line)
			if fromID == "" {
				// A `Foo::bar` in a field or static initializer sits outside
				// every method body, yet is one of the commonest places a
				// method reference appears (constant handler tables,
				// `Comparator` fields). Anchor it to the file node, as the
				// Java type-use pass already does for class-level type refs,
				// instead of dropping the reference entirely. Only the
				// unambiguous `::`-style / selector forms take this fallback —
				// a bare identifier outside any function is far more likely a
				// local or a builtin than a callable reference.
				fromID = fileID
			}
			if fromID == "" {
				return
			}
			key := fromID + "\x00special\x00" + refName + "\x00" + recvHint
			if seen[key] {
				return
			}
			seen[key] = true
			cands = append(cands, FnValueCandidate{
				FromID: fromID, Name: refName, FilePath: filePath, Line: line,
				Form: "special", RecvHint: recvHint, Lang: lang, Ungated: typeQualified,
			})
			return
		}
		if !spec.matchesIDNode(n.Type()) {
			return
		}
		// Skip an identifier that is a component of a special form already
		// captured as a whole (the `valueOf` in `Demo::valueOf`, the `handle`
		// in `self.handle`), so it is not double-counted as a bare name.
		if p := n.Parent(); p != nil {
			if _, _, ok := normalizeFnRefSpecial(p, src); ok {
				return
			}
		}
		name := fnRefNodeName(n, src)
		if name == "" {
			return
		}
		ungated := false
		if !funcs[name] {
			// A name the file does not declare can only bind cross-module, and
			// only when it is an explicit qualified path (e.g. Rust `m::f`) —
			// a bare identifier that is not a same-file function is a local /
			// param / builtin and is dropped to avoid flooding the gate.
			if !spec.ungated || !isQualifiedFnRefNode(n.Type()) {
				return
			}
			ungated = true
		}
		if fnRefStartsCall(spec, n, src) {
			return // callee of a call (incl. tagged template), not a value use
		}
		if fnRefIsCallableDeclName(n) {
			return // the declaration of the function, not a use of it
		}
		line := int(n.StartPoint().Row) + 1
		fromID := findEnclosingFunc(funcRanges, line)
		if fromID == "" {
			return
		}
		key := fromID + "\x00" + name
		if seen[key] {
			return
		}
		seen[key] = true
		cands = append(cands, FnValueCandidate{
			FromID: fromID, Name: name, FilePath: filePath, Line: line,
			Form: spec.fnRefForm(n), Lang: lang, Ungated: ungated,
		})
	}
	if goWork == nil {
		walkNodes(root, visit)
	} else {
		for _, n := range goWork.fnValueNodes {
			visit(n)
		}
	}
	EmitFnValueCandidates(result, cands)
}

// byteAfterIdentStartsCall reports whether the first non-whitespace byte at or
// after i begins a call of the preceding identifier — '(' for an ordinary call
// or '`' for a tagged-template call (`tag`...“). Either means the identifier is
// a callee, not a function-as-value reference.
func byteAfterIdentStartsCall(src []byte, i int) bool {
	for i < len(src) {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '(', '`':
			return true
		default:
			return false
		}
	}
	return false
}

// EmitFnValueCandidates appends one placeholder reference edge per candidate to
// result. Each edge targets the fn-value namespace and carries the captured
// name in Meta so the resolver gate can bind it; the edge rides
// OriginSpeculative until then. Candidates missing a source site or name are
// skipped.
func EmitFnValueCandidates(result *parser.ExtractionResult, cands []FnValueCandidate) {
	for _, c := range cands {
		if c.FromID == "" || c.Name == "" {
			continue
		}
		meta := map[string]any{
			"via":           fnValueCandidateVia,
			"fn_value_name": c.Name,
		}
		if c.Form != "" {
			meta["fn_ref_form"] = c.Form
		}
		if c.Lang != "" {
			meta["fn_ref_lang"] = c.Lang
		}
		if c.Ungated {
			meta["fn_value_ungated"] = true
		}
		if c.RecvHint != "" {
			meta["fn_ref_recv_hint"] = c.RecvHint
		}
		if c.SkipGate {
			meta["skip_gate"] = true
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     c.FromID,
			To:       fnValueUnresolvedPrefix + c.Name,
			Kind:     graph.EdgeReferences,
			FilePath: c.FilePath,
			Line:     c.Line,
			Origin:   graph.OriginSpeculative,
			Meta:     meta,
		})
	}
}
