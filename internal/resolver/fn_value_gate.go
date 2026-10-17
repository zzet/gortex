package resolver

import "github.com/zzet/gortex/internal/graph"

// Function-as-value callback gate.
//
// A large class of real call relationships is wired by passing a function as a
// *value* — registering a handler (`router.Get("/x", handler)`), a callback
// (`list.forEach(process)`), an observer (`signal.connect(onChange)`) — rather
// than calling it directly. The per-language extractors capture each such
// value-position identifier as a placeholder reference edge
// (To = "unresolved::fnvalue::<name>", Meta via="callback_candidate",
// fn_value_name=<name>); see EmitFnValueCandidates in the languages package.
//
// Capture alone floods: every bare identifier in a value position is a
// candidate, and most are locals, parameters, or builtins, not functions. This
// gate is the other half of the pair — it binds each candidate to a real
// function/method in the SAME FILE and drops the rest, so an unbound identifier
// never becomes an edge.
//
// Beat: the landed edge rides a provenance TIER (OriginASTInferred — a
// scope-bound name resolution, strictly above text_matched) so callback edges
// are min_tier-filterable like every other Gortex edge, instead of carrying a
// single flat heuristic flag. The per-language value-position capture lands on
// top of this skeleton.
const (
	// SynthFnValueCallback is the provenance tag for a bound callback edge.
	SynthFnValueCallback = "fn-value-callback"

	// fnValueCandidateVia marks an extractor-emitted placeholder awaiting the
	// gate; fnValueRegistrationVia marks the bound edge the gate lands.
	fnValueCandidateVia    = "callback_candidate"
	fnValueRegistrationVia = "callback_registration"

	// metaFnValueName carries the captured bare identifier on both the
	// placeholder and the bound edge.
	metaFnValueName = "fn_value_name"
)

// ResolveFnValueCallbacks binds each captured function-as-value placeholder to a
// same-file function/method and lands a tiered callback-registration reference
// edge, dropping any candidate that does not resolve to a real function. It is a
// full-recompute, idempotent synthesizer: graph.AddEdge dedupes and
// graph.EvictFile drops the edges on reindex. Returns the number of edges
// landed.
func ResolveFnValueCallbacks(g graph.Store) int { return resolveFnValueCallbacks(g, nil) }

// ResolveFnValueCallbacksScoped is the incremental counterpart of
// ResolveFnValueCallbacks: it gates only the callback candidates that originate
// in the given changed repos, leaving an unchanged repo's already-bound
// registrations on disk (they were never dropped). A nil scope gates the whole
// graph, so ResolveFnValueCallbacks and the whole-index path stay identical.
//
// Only the CANDIDATE scan is scoped. A candidate placeholder lives in (is
// emitted from) the repo that declared the registration, so a changed repo owns
// exactly the candidates whose binding its reindex dropped. RESOLUTION stays
// whole-graph — the resolve helpers below scan the entire graph by name — so a
// changed-repo callback still binds to a handler that lives in an unchanged repo.
func ResolveFnValueCallbacksScoped(g graph.Store, scope map[string]bool) int {
	return resolveFnValueCallbacks(g, scope)
}

func resolveFnValueCallbacks(g graph.Store, scope map[string]bool) int {
	return resolveFnValueCallbacksWithPrior(g, scope, nil)
}

// resolveFnValueCallbacksWithPrior is resolveFnValueCallbacks that republishes
// the prior registrations of every candidate a save cannot have moved
// (FnValuePrior) instead of resolving it again.
func resolveFnValueCallbacksWithPrior(g graph.Store, scope map[string]bool, prior *FnValuePrior) int {
	if g == nil {
		return 0
	}

	// Finish the candidate cursor before opening the callable projection or
	// writing synthesized edges. Besides avoiding store re-entry, this lets the
	// same candidate order feed both the compact and compatibility paths.
	var candidates []*graph.Edge
	collect := func(edge *graph.Edge) {
		if edge == nil || edge.Meta == nil {
			return
		}
		if via, _ := edge.Meta["via"].(string); via != fnValueCandidateVia {
			return
		}
		name, _ := edge.Meta[metaFnValueName].(string)
		if name == "" || isFnValueNonTarget(name) {
			return
		}
		candidates = append(candidates, edge)
	}
	if scope == nil {
		// The gate needs only the placeholders parked in the fn-value namespace,
		// not every reference edge. The Meta predicate remains in collect because
		// a non-candidate edge can share that namespace.
		edges := g.EdgesByKind(graph.EdgeReferences)
		if fp, ok := g.(graph.FnValuePlaceholderScanner); ok {
			edges = fp.FnValuePlaceholderEdges()
		}
		for edge := range edges {
			collect(edge)
		}
	} else {
		// Scoped: walk only changed repositories' source-owned edges.
		for prefix := range scope {
			if prefix == "" {
				continue
			}
			for _, edge := range g.GetRepoEdges(prefix) {
				if edge != nil && edge.Kind == graph.EdgeReferences {
					collect(edge)
				}
			}
		}
	}
	return resolveFnValueCandidates(g, candidates, prior)
}

// resolveFnValueCandidates gates the collected candidates and lands their
// registrations, republishing from prior what the save cannot have moved.
func resolveFnValueCandidates(g graph.Store, candidates []*graph.Edge, prior *FnValuePrior) int {
	if len(candidates) == 0 {
		return 0
	}

	var landed []*graph.Edge
	if prior != nil {
		changed := prior.changedNames(g)
		resolve := candidates[:0:0]
		for _, edge := range candidates {
			name, _ := edge.Meta[metaFnValueName].(string)
			regs, ok := prior.reusable(edge, name, changed)
			if !ok {
				resolve = append(resolve, edge)
				continue
			}
			seenTarget := make(map[string]struct{}, len(regs))
			for _, reg := range regs {
				if _, dup := seenTarget[reg.To]; dup || reg.To == edge.From {
					continue
				}
				seenTarget[reg.To] = struct{}{}
				landed = append(landed, republishFnValueRegistration(reg, edge))
			}
		}
		fnValueReused.Add(int64(len(candidates) - len(resolve)))
		candidates = resolve
	}
	fnValueResolved.Add(int64(len(candidates)))

	sameFileTarget := newFnValueSameFileTargetLookup(g, candidates)
	// Resolve global names lazily. Most candidates bind within their file, so a
	// pass-wide FindNodesByNames prefetch decodes and retains a much larger node
	// set than the gate uses. The memo still collapses repeated positive and
	// negative lookups to one bounded store query per distinct global name.
	nameMemo := make(map[string][]*graph.Node)
	prefetchCertainFnValueNames(g, candidates, nameMemo)
	for _, edge := range candidates {
		name, _ := edge.Meta[metaFnValueName].(string)
		// Resolution scope depends on the captured form. A special form's
		// receiver hint (`<self>` / a concrete type) binds the member against
		// that type's methods; a plain candidate binds same-file.
		recvHint, _ := edge.Meta["fn_ref_recv_hint"].(string)
		ungated, _ := edge.Meta["fn_value_ungated"].(bool)
		skipGate, _ := edge.Meta["skip_gate"].(bool)
		var targets []string
		conf := 0.6
		origin := graph.OriginASTInferred
		switch {
		case skipGate:
			target := ""
			if recvHint != "" {
				target = resolveMemberByTypeMemo(g, recvHint, name, nameMemo)
			}
			if target == "" {
				target = resolveUniqueFnValueMemo(g, name, nameMemo)
			}
			targets, conf = appendTarget(nil, target), 0.5
		case recvHint == "<self>":
			if targets = resolveFnValueSelfMembersMemo(g, edge.From, name, nameMemo); len(targets) > 0 {
				conf, origin = 0.85, graph.OriginASTResolved
			} else {
				targets = appendTarget(nil, sameFileTarget(edge.FilePath, name))
			}
		case recvHint != "":
			if targets = resolveMembersByTypeMemo(g, recvHint, name, nameMemo); len(targets) > 0 {
				conf, origin = 0.85, graph.OriginASTResolved
			} else if ungated {
				targets = appendTarget(nil, resolveFnValueCrossModuleMemo(g, name, nameMemo))
				conf = 0.45
			}
		default:
			targets = appendTarget(nil, sameFileTarget(edge.FilePath, name))
			if len(targets) == 0 && ungated {
				targets = appendTarget(nil, resolveFnValueCrossModuleMemo(g, name, nameMemo))
				conf = 0.45
			}
		}
		// An overload set is a genuine ambiguity — exactly one member is the
		// real target and the graph cannot say which — so each edge in the set
		// rides the inferred tier at reduced confidence rather than claiming
		// the resolved-match certainty a lone hit earns.
		if len(targets) > 1 {
			conf, origin = 0.6, graph.OriginASTInferred
		}
		for _, target := range targets {
			if target == "" || target == edge.From {
				continue
			}
			meta := map[string]any{
				"via":             fnValueRegistrationVia,
				metaFnValueName:   name,
				MetaSynthesizedBy: SynthFnValueCallback,
				MetaProvenance:    ProvenanceHeuristic,
			}
			if form, _ := edge.Meta["fn_ref_form"].(string); form != "" {
				meta["fn_ref_form"] = form
			}
			if len(targets) > 1 {
				meta["overload_set"] = len(targets)
			}
			landed = append(landed, &graph.Edge{
				From:            edge.From,
				To:              target,
				Kind:            graph.EdgeReferences,
				FilePath:        edge.FilePath,
				Line:            edge.Line,
				Confidence:      conf,
				ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, conf),
				Origin:          origin,
				Meta:            meta,
			})
		}
	}

	// One bounded AddBatch per chunk instead of an AddEdge per edge: on a
	// disk-backed store the per-edge form is one ACID round-trip each.
	const fnValueAddBatchChunk = 4096
	for start := 0; start < len(landed); start += fnValueAddBatchChunk {
		end := start + fnValueAddBatchChunk
		if end > len(landed) {
			end = len(landed)
		}
		g.AddBatch(nil, landed[start:end])
	}
	return len(landed)
}

type fnValueFileName struct {
	file string
	name string
}

// newFnValueSameFileTargetLookup indexes only the (file,name) pairs requested
// by this pass through the existing metadata-free callable projection. A pair
// with more than one callable falls back to the legacy GetFileNodes order, so
// overloads and malformed duplicate declarations remain byte-identical.
func newFnValueSameFileTargetLookup(g graph.Store, candidates []*graph.Edge) func(string, string) string {
	fileNodes := map[string][]*graph.Node{}
	legacy := func(filePath, name string) string {
		nodes, ok := fileNodes[filePath]
		if !ok {
			nodes = g.GetFileNodes(filePath)
			fileNodes[filePath] = nodes
		}
		return resolveFnValueName(nodes, name)
	}

	sequencer, ok := g.(graph.CallableBindingNodeSequencer)
	if !ok {
		return legacy
	}
	wanted := make(map[fnValueFileName]struct{}, len(candidates))
	for _, edge := range candidates {
		if edge == nil || edge.Meta == nil {
			continue
		}
		recvHint, _ := edge.Meta["fn_ref_recv_hint"].(string)
		skipGate, _ := edge.Meta["skip_gate"].(bool)
		if skipGate || (recvHint != "" && recvHint != "<self>") {
			continue
		}
		name, _ := edge.Meta[metaFnValueName].(string)
		wanted[fnValueFileName{file: edge.FilePath, name: name}] = struct{}{}
	}

	targets := make(map[fnValueFileName]string, len(wanted))
	ambiguous := make(map[fnValueFileName]struct{})
	for node := range sequencer.CallableBindingNodesSeq(graph.KindFunction, graph.KindMethod) {
		key := fnValueFileName{file: node.FilePath, name: node.Name}
		if _, requested := wanted[key]; !requested {
			continue
		}
		if prior, found := targets[key]; found && prior != node.ID {
			ambiguous[key] = struct{}{}
		} else if !found {
			targets[key] = node.ID
		}
	}
	return func(filePath, name string) string {
		key := fnValueFileName{file: filePath, name: name}
		if _, needsLegacyOrder := ambiguous[key]; needsLegacyOrder {
			return legacy(filePath, name)
		}
		return targets[key]
	}
}

// resolveFnValueName returns the ID of a function or method named name among
// fileNodes (the caller's already-fetched same-file node list), or "" when
// none exists. Same-file scope is the conservative default; per-language
// capture extends the gate with imported-symbol and C-family file-scope
// rules on top of this skeleton.
func resolveFnValueName(fileNodes []*graph.Node, name string) string {
	if name == "" {
		return ""
	}
	for _, n := range fileNodes {
		if n == nil {
			continue
		}
		if n.Name != name {
			continue
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			return n.ID
		}
	}
	return ""
}

// resolveUniqueFnValue returns the ID of the sole function/method named name in
// the repo, or "" when none or more than one exists (unique-or-drop). The
// shared repo-wide resolution rule for qualified-path and gate-skipping
// (curated-HOF string) function values. Prototype declarations of the name
// never make it ambiguous — see uniqueFnValueMatchMemo.
func resolveUniqueFnValue(g graph.Store, name string) string {
	return resolveUniqueFnValueMemo(g, name, nil)
}

// resolveUniqueFnValueMemo is resolveUniqueFnValue with a shared per-pass
// FindNodesByName memo (nil disables memoization).
func resolveUniqueFnValueMemo(g graph.Store, name string, memo map[string][]*graph.Node) string {
	return uniqueFnValueMatchMemo(g, name, nil, memo)
}

// resolveFnValueCrossModuleMemo binds a function value to a uniquely-named
// function/method anywhere in the repo, skipping any candidate with file-local
// linkage (a C/C++ `static` function, stamped scope_static): such a definition
// is invisible outside its translation unit, so a cross-module reference can
// never target it, and a same-named static in an unrelated file must not make
// the name look ambiguous. The same-file path is preferred by the caller; this
// is the cross-module fallback. A shared per-pass FindNodesByName memo collapses
// repeated lookups of the same name (nil disables memoization).
func resolveFnValueCrossModuleMemo(g graph.Store, name string, memo map[string][]*graph.Node) string {
	return uniqueFnValueMatchMemo(g, name, isFileLocalLinkage, memo)
}

// findNodesByNameMemo wraps g.FindNodesByName with an optional per-pass cache.
// The gate calls it for the same registration names many times; caching the
// result collapses those to one backend lookup per distinct name. Safe only
// within a pass that does not add or remove nodes between lookups. A nil memo
// forwards straight through, so non-pass callers see identical behaviour.
func findNodesByNameMemo(g graph.Store, name string, memo map[string][]*graph.Node) []*graph.Node {
	if memo == nil {
		return g.FindNodesByName(name)
	}
	if ns, ok := memo[name]; ok {
		return ns
	}
	ns := g.FindNodesByName(name)
	memo[name] = ns
	return ns
}

// uniqueFnValueMatchMemo is the shared unique-or-drop scan over every
// function/method named name, with an optional per-node exclusion and a shared
// per-pass FindNodesByName memo (nil disables memoization).
//
// A C-family forward declaration (`void strlenCommand(client *c);` in a
// header, stamped Meta["prototype"]) names the SAME extern symbol as its
// definition, not a competitor — C has one flat namespace per linked program.
// Counting it as a distinct candidate made every prototyped function
// permanently ambiguous (definition + header declaration = two nodes), which
// silently dropped the entire generated-command-table reference surface: a
// codebase that declares its handlers in a shared header is exactly the
// codebase that wires them through a table. Definitions therefore win:
// prototypes are consulted only when no definition matches at all (the
// definition's translation unit isn't indexed), and then under the same
// unique-or-drop rule.
func uniqueFnValueMatchMemo(g graph.Store, name string, exclude func(*graph.Node) bool, memo map[string][]*graph.Node) string {
	def, proto := "", ""
	for _, n := range findNodesByNameMemo(g, name, memo) {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if exclude != nil && exclude(n) {
			continue
		}
		if isPrototypeDecl(n) {
			if proto != "" && proto != n.ID {
				proto = ambiguousFnValue
			} else {
				proto = n.ID
			}
			continue
		}
		if def != "" && def != n.ID {
			return "" // two real definitions — genuinely ambiguous
		}
		def = n.ID
	}
	if def != "" {
		return def
	}
	if proto == ambiguousFnValue {
		return ""
	}
	return proto
}

// ambiguousFnValue is a sentinel marking a name matched by more than one
// prototype declaration; it can never collide with a real node ID because the
// ID convention is "<file>::<name>".
const ambiguousFnValue = "\x00ambiguous"

// isPrototypeDecl reports whether a node is a C-family forward declaration
// (stamped Meta["prototype"] by the extractor) rather than a definition.
func isPrototypeDecl(n *graph.Node) bool {
	if n.Meta == nil {
		return false
	}
	v, _ := n.Meta["prototype"].(bool)
	return v
}

// isFileLocalLinkage reports whether a node was stamped with translation-unit
// (C/C++ static) linkage, so it cannot be the target of a cross-module value
// reference.
func isFileLocalLinkage(n *graph.Node) bool {
	if n.Meta == nil {
		return false
	}
	v, _ := n.Meta["scope_static"].(bool)
	return v
}

// resolveMemberByType binds member to a uniquely-named method of typeName
// (matched via Meta["receiver"]), or "" when none or more than one matches.
// Shared scope rule for `Foo::bar`-style references and self-member resolution.
func resolveMemberByType(g graph.Store, typeName, member string) string {
	return resolveMemberByTypeMemo(g, typeName, member, nil)
}

// resolveMemberByTypeMemo is resolveMemberByType with a shared per-pass
// FindNodesByName memo (nil disables memoization).
func resolveMemberByTypeMemo(g graph.Store, typeName, member string, memo map[string][]*graph.Node) string {
	if typeName == "" || member == "" {
		return ""
	}
	match := ""
	for _, n := range findNodesByNameMemo(g, member, memo) {
		if n == nil || n.Kind != graph.KindMethod {
			continue
		}
		if recv, _ := n.Meta["receiver"].(string); recv != typeName {
			continue
		}
		if match != "" && match != n.ID {
			return "" // ambiguous within the type — drop
		}
		match = n.ID
	}
	return match
}

// resolveMembersByTypeMemo returns every method of typeName named member —
// the complete overload set, not the unique-or-drop single match
// resolveMemberByTypeMemo yields.
//
// In Java (and C#, Kotlin, C++) a type may declare one name several times, and
// a method reference such as `Type::handle` names the whole set: which overload
// applies is decided by the target functional interface's shape, which the
// graph does not model. Unique-or-drop therefore discarded the reference
// outright the moment a target was overloaded, leaving every overload with no
// incoming reference at all — so an overloaded method reached only through a
// method reference was reported dead. Binding the set keeps each overload
// reachable; the caller reduces confidence to mark the ambiguity.
func resolveMembersByTypeMemo(g graph.Store, typeName, member string, memo map[string][]*graph.Node) []string {
	if typeName == "" || member == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range findNodesByNameMemo(g, member, memo) {
		if n == nil || n.Kind != graph.KindMethod {
			continue
		}
		if recv, _ := n.Meta["receiver"].(string); recv != typeName {
			continue
		}
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		out = append(out, n.ID)
	}
	return out
}

// resolveFnValueSelfMembersMemo binds a `this.m` / `self.m` member reference
// against the methods of the registration site's enclosing type, so it can
// never bind a coincidentally-named top-level function. Returns the full
// overload set. A shared per-pass FindNodesByName memo collapses repeated
// lookups (nil disables memoization).
func resolveFnValueSelfMembersMemo(g graph.Store, fromID, member string, memo map[string][]*graph.Node) []string {
	from := g.GetNode(fromID)
	if from == nil || from.Meta == nil {
		return nil
	}
	recv, _ := from.Meta["receiver"].(string)
	if recv == "" {
		return nil
	}
	return resolveMembersByTypeMemo(g, recv, member, memo)
}

// appendTarget appends a resolved target ID to a list, skipping the empty
// "unresolved" sentinel so single-match paths compose with the set-valued ones.
func appendTarget(dst []string, target string) []string {
	if target == "" {
		return dst
	}
	return append(dst, target)
}

// isFnValueNonTarget reports whether name is a literal/keyword/builtin that
// can never be a captured function value, so the gate skips it before the
// same-file lookup. The set is deliberately small and language-agnostic; the
// per-language capture passes refine it with isGoBuiltinOrKeyword-style checks.
func isFnValueNonTarget(name string) bool {
	switch name {
	case "true", "false", "nil", "null", "none", "None", "undefined",
		"this", "self", "super", "new", "delete", "typeof", "void":
		return true
	}
	return false
}

// prefetchCertainFnValueNames reads, in one batch, the names the gate is
// certain to look up across the repository: a gate-skipping value and a
// member of a named receiver type. Every other candidate binds in its own
// file first and reads its name lazily only if that fails (an ungated
// fallback, a self member), so the batch holds exactly the rows the per-name
// reads would have decoded, in one round trip instead of one per name.
func prefetchCertainFnValueNames(g graph.Store, candidates []*graph.Edge, memo map[string][]*graph.Node) {
	var names []string
	seen := make(map[string]struct{})
	for _, edge := range candidates {
		name, _ := edge.Meta[metaFnValueName].(string)
		if name == "" {
			continue
		}
		recvHint, _ := edge.Meta["fn_ref_recv_hint"].(string)
		skipGate, _ := edge.Meta["skip_gate"].(bool)
		if !skipGate && (recvHint == "" || recvHint == "<self>") {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) == 0 {
		return
	}
	found := g.FindNodesByNames(names)
	for _, name := range names {
		memo[name] = found[name]
	}
}
