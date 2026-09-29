package resolver

import (
	"os"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Terminal-edge skipping. The warm-restart pending population is dominated by
// permanently external / stdlib / definition-less stubs — `*.Errorf`,
// `*.QueryRow`, `<vector>`, a bare `unresolved::Name` no repo defines — that
// are re-fed to and re-fail every scoped warm resolve, forever (see the
// warmLookupCache 200k-external-method-stub note). A FULL (unscoped) resolve
// has the global evidence to conclude such an edge can never bind; it stamps a
// durable flag so a later SCOPED pass can drop it up front. Full passes ignore
// the flag and re-examine everything, so a stamp self-heals the moment a
// matching definition appears.

const (
	// metaResolveTerminal marks an unresolved edge the resolver has concluded
	// is permanently unbindable. Read by the scoped pending-scan skip.
	metaResolveTerminal = graph.EdgeMetaResolveTerminal
	// metaResolveTerminalReason carries a short human-readable reason for the
	// stamp (aligned by string value with internal/analyzer's outcome vocab).
	metaResolveTerminalReason = graph.EdgeMetaResolveTerminalReason
)

// Terminal reasons. These deliberately match internal/analyzer's resolution-
// outcome constants by STRING VALUE (the analyzer imports resolver, so resolver
// cannot import the analyzer's constants) so both surfaces speak one vocabulary.
const (
	terminalReasonNoDefinition = "no_definition"
	terminalReasonStubOnly     = "stub_only"
	terminalReasonStdlibHeader = "stdlib_header"
)

// warmupFullResolve reports whether the operator forced the warm-restart
// master resolve to re-examine every edge (ignoring the durable terminal
// flag). The daemon already translates this override into a nil resolve scope,
// so the scope-empty path covers it; the resolver honours it directly too so a
// scoped pass under the override still self-heals.
func warmupFullResolve() bool {
	v := os.Getenv("GORTEX_WARMUP_FULL_RESOLVE")
	return v == "1" || strings.EqualFold(v, "true")
}

// edgeTerminalFlag reports whether e carries a live resolve_terminal stamp.
func edgeTerminalFlag(e *graph.Edge) bool {
	if e == nil || e.Meta == nil {
		return false
	}
	v, ok := e.Meta[metaResolveTerminal].(bool)
	return ok && v
}

// setEdgeTerminal stamps e as terminal in place (allocating Meta if needed).
func setEdgeTerminal(e *graph.Edge, reason string) {
	if e == nil {
		return
	}
	if e.Meta == nil {
		e.Meta = make(map[string]any, 2)
	}
	e.Meta[metaResolveTerminal] = true
	if reason != "" {
		e.Meta[metaResolveTerminalReason] = reason
	}
}

// clearEdgeTerminal drops any resolve_terminal stamp from e in place.
func clearEdgeTerminal(e *graph.Edge) {
	if e == nil || e.Meta == nil {
		return
	}
	delete(e.Meta, metaResolveTerminal)
	delete(e.Meta, metaResolveTerminalReason)
}

// classifyTerminal decides whether an unresolved edge is permanently
// unbindable and, if so, the reason. It is the resolver-native twin of
// internal/analyzer.ClassifyUnresolved (which resolver cannot import): an edge
// is terminal only when NO real definition of its name exists anywhere in the
// graph (the name matches nothing, or only stub / external placeholders) or the
// target is a C/C++ standard-library angle-include. An edge with any real
// in-graph candidate — same-language (candidate_out_of_scope /
// ambiguous_multi_match) or a different language family (cross_language_only) —
// is genuinely pending and NEVER stamped, so more evidence can still bind it.
func (r *Resolver) classifyTerminal(e *graph.Edge) (reason string, terminal bool) {
	if e == nil || !graph.IsUnresolvedTarget(e.To) {
		return "", false
	}
	target := graph.UnresolvedName(e.To)

	// C/C++/ObjC standard-library angle-include (<stdio.h>, <vector>, …):
	// external by construction, deliberately never bound to an in-tree file of
	// the same basename. A quoted / non-system include may still resolve to a
	// local header, so it stays pending.
	if e.Kind == graph.EdgeImports && strings.HasPrefix(target, "import::") {
		if k, _ := e.Meta["include_kind"].(string); k == "system" {
			if IsCppStdlibHeader(strings.TrimPrefix(target, "import::")) {
				return terminalReasonStdlibHeader, true
			}
		}
		return "", false
	}

	name := identifierFromTarget(target)
	if name == "" {
		// Module-path shapes (import:: / pyrel:: / grpc::) are owned by
		// dedicated whole-graph passes; never classified terminal here.
		return "", false
	}

	fromLang := ""
	if n := r.cachedGetNode(e.From); n != nil {
		fromLang = n.Language
	}
	var realSameLang, realOtherLang, stubs int
	// The stub-inclusive lookup: this classifies, it never binds, and
	// "matches only a stub" is its own outcome (stub_only).
	for _, n := range r.cachedFindNodesByNameWithStubs(name) {
		if n == nil {
			continue
		}
		if graph.IsStub(n.ID) {
			stubs++
			continue
		}
		if !nodeIsDefinitionKind(n.Kind) {
			continue
		}
		if fromLang != "" && n.Language != "" && !sameLanguageFamily(fromLang, n.Language) {
			realOtherLang++
			continue
		}
		realSameLang++
	}
	switch {
	case realSameLang >= 1 || realOtherLang >= 1:
		// A real definition exists — genuinely pending, not terminal.
		return "", false
	case stubs >= 1:
		return terminalReasonStubOnly, true
	default:
		return terminalReasonNoDefinition, true
	}
}

// nodeIsDefinitionKind reports whether a node kind is a callable / type
// definition an unresolved call or reference could legitimately bind to. Kept
// resolver-local (the analyzer's identical helper is package-private) so this
// package takes no analyzer dependency.
func nodeIsDefinitionKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindVariable, graph.KindConstant, graph.KindField:
		return true
	}
	return false
}

// definitionKinds is nodeIsDefinitionKind's predicate reified as a list, for
// callers (the bulk terminal classifier) that need to hand it to a Store
// capability instead of calling the predicate per node.
var definitionKinds = []graph.NodeKind{
	graph.KindFunction, graph.KindMethod, graph.KindType,
	graph.KindInterface, graph.KindVariable, graph.KindConstant, graph.KindField,
}

// filterTerminalSkip drops the terminally-unresolved edges a scoped pass need
// not reconsider, composing with (running AFTER) filterPendingByScope. A
// terminal edge is kept only when it is specifically anchored to a changed
// repo — its source could re-target, or its target is repo-qualified to a repo
// that just re-indexed. A terminal edge that survived the scope filter only
// because it is a bare, unqualified name is dropped: no definition exists and
// no changed repo owns it, so it would re-fail. (A changed repo that newly ADDS
// a matching name binds the edge in the subsequent whole-graph cross-repo
// resolve, which is unaffected by this flag — so the drop is a pure work
// saving.) Filters in place; the returned slice reuses pending's backing array.
func filterTerminalSkip(pending []*graph.Edge, scope map[string]struct{}) (kept []*graph.Edge, skipped int) {
	out := pending[:0]
	for _, e := range pending {
		if e == nil {
			continue
		}
		if !edgeTerminalFlag(e) || terminalEdgeAnchoredToScope(e, scope) {
			out = append(out, e)
			continue
		}
		skipped++
	}
	return out, skipped
}

// terminalEdgeAnchoredToScope reports whether a terminal edge is tied to a
// changed repo (source repo in scope, or target repo-qualified to a changed
// repo). It mirrors edgeInResolveScope MINUS the bare-unqualified rule — a bare
// terminal edge is exactly the skippable case.
func terminalEdgeAnchoredToScope(e *graph.Edge, scope map[string]struct{}) bool {
	if _, ok := scope[graph.RepoPrefixOfID(e.From)]; ok {
		return true
	}
	targetRepo := graph.UnresolvedRepoPrefix(e.To)
	if targetRepo == "" {
		targetRepo = graph.StubRepoPrefix(e.To)
	}
	if targetRepo == "" {
		return false
	}
	_, ok := scope[targetRepo]
	return ok
}

// reconcileTerminalStamps runs at the END of a FULL (unscoped) ResolveAll: it
// re-classifies every edge still unresolved after all post-passes and brings
// its durable terminal flag into agreement with the current global evidence —
// stamping the newly-terminal, un-stamping any edge that used to be terminal
// but now has a real candidate (self-healing). Edges already in the right state
// are left untouched, so a converged graph performs no writes on later full
// passes. Disk backends may persist only the two terminal columns through
// EdgeTerminalStampPersister; stores without it retain the full-attribute
// EdgeMetaBatchPersister fallback. The in-memory backend's live edge pointers
// need neither capability. Returns the counts stamped / un-stamped.
//
// Classification itself takes the bulk path (reconcileTerminalStampsBulk)
// when the backend implements graph.NodeNameClassCounter, falling back to
// the original per-edge classifyTerminal loop otherwise (e.g. the in-memory
// backend). The two paths make IDENTICAL decisions — see
// reconcileTerminalStampsBulk's doc for why the bulk path can skip fromLang /
// language-family entirely without changing the outcome.
func (r *Resolver) reconcileTerminalStamps() (stamped, unstamped int) {
	return r.reconcileTerminalStampsExcluding(nil)
}

// reconcileTerminalStampsExcluding applies the normal full-pass terminal
// classification except for deferred LSP edges the pass budget (or the
// expensive-path cutoff) did not let us query. Those skips carry no
// terminality evidence and must remain retryable. Exclusions are keyed by
// the spool work key — {from, kind, file, line, target}, deliberately
// To-less — so a record whose edge was heuristically bound and then
// guard-reverted still matches its live, again-unresolved edge here.
func (r *Resolver) reconcileTerminalStampsExcluding(excluded map[deferredLSPWorkKey]struct{}) (stamped, unstamped int) {
	var stillPending []*graph.Edge
	var changed []*graph.Edge
	for e := range r.graph.EdgesWithUnresolvedTarget() {
		if e == nil {
			continue
		}
		if _, skip := excluded[deferredLSPWorkKeyForEdge(e)]; skip {
			// A pass-budget skip is not evidence that the edge is terminal.
			// Clear an older stamp so a later scoped pass may retry LSP.
			if edgeTerminalFlag(e) {
				clearEdgeTerminal(e)
				changed = append(changed, e)
				unstamped++
			}
			continue
		}
		stillPending = append(stillPending, e)
	}

	if counter, ok := r.graph.(graph.NodeNameClassCounter); ok {
		bulkChanged, bulkStamped, bulkUnstamped := r.reconcileTerminalStampsBulk(stillPending, counter)
		changed = append(changed, bulkChanged...)
		stamped += bulkStamped
		unstamped += bulkUnstamped
	} else {
		for _, e := range stillPending {
			reason, terminal := r.classifyTerminal(e)
			marked := edgeTerminalFlag(e)
			switch {
			case terminal && !marked:
				setEdgeTerminal(e, reason)
				changed = append(changed, e)
				stamped++
			case !terminal && marked:
				clearEdgeTerminal(e)
				changed = append(changed, e)
				unstamped++
			}
		}
	}

	if len(changed) > 0 {
		if p, ok := r.graph.(graph.EdgeTerminalStampPersister); ok {
			p.PersistEdgeTerminalStamps(changed)
		} else if p, ok := r.graph.(graph.EdgeMetaBatchPersister); ok {
			p.PersistEdgeAttributesBatch(changed)
		}
	}
	return stamped, unstamped
}

// classifyTerminalBulk replicates classifyTerminal's decision for every edge
// in pending EXCEPT EdgeImports edges, which route through the unchanged
// per-edge classifyTerminal: its C/C++ stdlib-header special case reads
// e.Meta["include_kind"] plus a 200+-entry lookup table (IsCppStdlibHeader),
// and porting that to SQL would risk the two drifting out of sync for a
// small, rarely-hit case. Every other unresolved-target edge is classified
// from one batched name lookup instead of N cachedFindNodesByName + IsStub
// calls. Pure — no mutation, no persistence — so it can be compared
// element-wise against classifyTerminal's own per-edge output.
//
// classifyTerminal computes realSameLang / realOtherLang separately but its
// own terminal/non-terminal decision only ever checks
// "realSameLang >= 1 || realOtherLang >= 1" — both branches mean exactly the
// same thing to the switch below them. So this bulk path never needs
// e.From's language or the target's language: a real (non-stub,
// definition-kind) node matching the target name by ANY language counts
// identically. Only stubs-vs-real-vs-neither matters here.
func (r *Resolver) classifyTerminalBulk(pending []*graph.Edge, counter graph.NodeNameClassCounter) (reasons []string, terminals []bool) {
	names := make(map[string]struct{})
	for _, e := range pending {
		if e.Kind == graph.EdgeImports {
			continue
		}
		if name := identifierFromTarget(graph.UnresolvedName(e.To)); name != "" {
			names[name] = struct{}{}
		}
	}
	nameList := make([]string, 0, len(names))
	for name := range names {
		nameList = append(nameList, name)
	}
	counts := counter.CountNodesByNameClass(nameList, definitionKinds)

	reasons = make([]string, len(pending))
	terminals = make([]bool, len(pending))
	for i, e := range pending {
		switch e.Kind {
		case graph.EdgeImports:
			reasons[i], terminals[i] = r.classifyTerminal(e)
		default:
			name := identifierFromTarget(graph.UnresolvedName(e.To))
			switch c := counts[name]; {
			case name == "" || c.Real >= 1:
				reasons[i], terminals[i] = "", false
			case c.Stub >= 1:
				reasons[i], terminals[i] = terminalReasonStubOnly, true
			default:
				reasons[i], terminals[i] = terminalReasonNoDefinition, true
			}
		}
	}
	return reasons, terminals
}

// reconcileTerminalStampsBulk is reconcileTerminalStamps' SQL-accelerated
// path: classify every pending edge via classifyTerminalBulk, then apply the
// same stamp/unstamp diff logic as the per-edge fallback.
func (r *Resolver) reconcileTerminalStampsBulk(pending []*graph.Edge, counter graph.NodeNameClassCounter) (changed []*graph.Edge, stamped, unstamped int) {
	reasons, terminals := r.classifyTerminalBulk(pending, counter)
	for i, e := range pending {
		reason, terminal := reasons[i], terminals[i]
		marked := edgeTerminalFlag(e)
		switch {
		case terminal && !marked:
			setEdgeTerminal(e, reason)
			changed = append(changed, e)
			stamped++
		case !terminal && marked:
			clearEdgeTerminal(e)
			changed = append(changed, e)
			unstamped++
		}
	}
	return changed, stamped, unstamped
}
