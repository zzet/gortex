package analyzer

// PURPOSE — pure computation core for the resolution-outcomes analyzer:
// classifies every unresolved call/reference edge by the structured reason
// the resolver gave up and returns a per-reason rollup plus example rows.
// RATIONALE — extracted from the MCP handler so the taxonomy logic is
// independently testable and reusable across surfaces (MCP, CLI, etc.).
// KEYWORDS — resolution_outcomes, unresolved, taxonomy, pure, calculation

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// Resolution-outcome taxonomy constants. These are the canonical source of
// the taxonomy; the MCP layer aliases them so both surfaces agree.
const (
	// OutcomeAmbiguousMultiMatch: two or more same-name, same-language
	// definitions exist — the resolver punted.
	OutcomeAmbiguousMultiMatch = "ambiguous_multi_match"
	// OutcomeCandidateOutOfScope: exactly one same-language definition
	// exists but the edge stayed unresolved.
	OutcomeCandidateOutOfScope = "candidate_out_of_scope"
	// OutcomeCrossLanguageOnly: the only definitions are in a different
	// language family.
	OutcomeCrossLanguageOnly = "cross_language_only"
	// OutcomeStubOnly: the name matches only stub/external-placeholder nodes.
	OutcomeStubOnly = "stub_only"
	// OutcomeNoDefinition: no definition of this name exists in the graph.
	OutcomeNoDefinition = "no_definition"
	// OutcomeStdlibHeader: a C/C++/Objective-C angle-include of a standard-
	// library header (<stdio.h>, <vector>, …) — external by construction,
	// left unresolved deliberately so it never binds to an in-tree file
	// sharing its basename.
	OutcomeStdlibHeader = "stdlib_header"
)

// ResolutionRow is one unresolved edge in the result.
// JSON field names mirror the MCP output shape exactly.
type ResolutionRow struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Kind       string `json:"edge_kind"`
	Name       string `json:"name"`
	Reason     string `json:"reason"`
	Candidates int    `json:"candidates"`
}

// ResolutionOutcomesResult is the return type of AnalyzeResolutionOutcomes.
// JSON field names mirror the MCP output shape exactly.
type ResolutionOutcomesResult struct {
	ByReason map[string]int  `json:"by_reason"`
	Total    int             `json:"total"`
	Rows     []ResolutionRow `json:"rows"`
}

// AnalyzeResolutionOutcomes classifies every unresolved call/reference edge
// in the graph by the structured reason the resolver gave up. reasonFilter
// restricts the returned rows to a single outcome; limit caps the row count.
// It is a pure Calculation: no side effects, no I/O.
func AnalyzeResolutionOutcomes(g graph.Reader, reasonFilter string, limit int) ResolutionOutcomesResult {
	type pending struct {
		edge *graph.Edge
		name string
	}
	var todo []pending
	fromIDs := map[string]struct{}{}
	for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
		for e := range g.EdgesByKind(kind) {
			if e == nil || !graph.IsUnresolvedTarget(e.To) {
				continue
			}
			name := graph.UnresolvedName(e.To)
			if name == "" {
				continue
			}
			// A receiver-qualified placeholder (`unresolved::*.foo`) keeps
			// its method name after the dot; normalise to the bare name.
			if i := strings.LastIndexByte(name, '.'); i >= 0 && i+1 < len(name) {
				name = name[i+1:]
			}
			todo = append(todo, pending{edge: e, name: name})
			if e.From != "" {
				fromIDs[e.From] = struct{}{}
			}
		}
	}
	fromList := make([]string, 0, len(fromIDs))
	for id := range fromIDs {
		fromList = append(fromList, id)
	}
	fromNodes := g.GetNodesByIDs(fromList)

	byReason := map[string]int{}
	var rows []ResolutionRow

	// Memoise classification by (name, caller-language).
	type classKey struct{ name, lang string }
	type classVal struct {
		reason string
		ncand  int
	}
	classCache := map[classKey]classVal{}

	for _, p := range todo {
		fromLang := ""
		if n := fromNodes[p.edge.From]; n != nil {
			fromLang = n.Language
		}
		key := classKey{name: p.name, lang: fromLang}
		cv, ok := classCache[key]
		if !ok {
			cv.reason, cv.ncand = ClassifyUnresolved(g, p.name, fromLang)
			classCache[key] = cv
		}
		reason, ncand := cv.reason, cv.ncand
		byReason[reason]++
		if reasonFilter != "" && reason != reasonFilter {
			continue
		}
		if len(rows) < limit {
			rows = append(rows, ResolutionRow{
				From: p.edge.From, To: p.edge.To, Kind: string(p.edge.Kind),
				Name: p.name, Reason: reason, Candidates: ncand,
			})
		}
	}

	// C/C++/ObjC standard-library angle-includes (<stdio.h>, <vector>, …) are
	// external by construction: the resolver leaves them on an unresolved
	// import placeholder rather than binding to an in-tree file that happens
	// to share the basename. Surface them under their own reason so the
	// outcome reads as "stdlib" instead of an opaque unresolved import.
	for e := range g.EdgesByKind(graph.EdgeImports) {
		if e == nil || !graph.IsUnresolvedTarget(e.To) {
			continue
		}
		if k, _ := e.Meta["include_kind"].(string); k != "system" {
			continue
		}
		hdr := strings.TrimPrefix(e.To, "unresolved::import::")
		if !resolver.IsCppStdlibHeader(hdr) {
			continue
		}
		byReason[OutcomeStdlibHeader]++
		if reasonFilter != "" && reasonFilter != OutcomeStdlibHeader {
			continue
		}
		if len(rows) < limit {
			rows = append(rows, ResolutionRow{
				From: e.From, To: e.To, Kind: string(e.Kind),
				Name: hdr, Reason: OutcomeStdlibHeader, Candidates: 0,
			})
		}
	}

	total := 0
	for _, n := range byReason {
		total += n
	}
	return ResolutionOutcomesResult{ByReason: byReason, Total: total, Rows: rows}
}

// ClassifyUnresolved returns the structured suppression reason for an
// unresolved name relative to the caller's language, plus the number of
// real (non-stub) definition candidates considered. It is a pure Calculation.
func ClassifyUnresolved(g graph.Reader, name, fromLang string) (reason string, candidates int) {
	var realSameLang, realOtherLang, stubs int
	for _, n := range g.FindNodesByName(name) {
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
	case realSameLang >= 2:
		return OutcomeAmbiguousMultiMatch, realSameLang
	case realSameLang == 1:
		return OutcomeCandidateOutOfScope, 1
	case realOtherLang >= 1:
		return OutcomeCrossLanguageOnly, realOtherLang
	case stubs >= 1:
		return OutcomeStubOnly, 0
	default:
		return OutcomeNoDefinition, 0
	}
}

// nodeIsDefinitionKind reports whether a node kind is a callable/type
// definition an unresolved call or reference could legitimately bind to.
func nodeIsDefinitionKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindVariable, graph.KindConstant, graph.KindField:
		return true
	}
	return false
}

// sameLanguageFamily folds the TS/JS pair so a cross-file TS→JS reference
// is not mis-reported as a cross-language suppression.
func sameLanguageFamily(a, b string) bool {
	if a == b {
		return true
	}
	norm := func(l string) string {
		switch l {
		case "javascript", "typescript", "tsx", "jsx":
			return "jsts"
		}
		return l
	}
	return norm(a) == norm(b)
}
