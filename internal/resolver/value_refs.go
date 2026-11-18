package resolver

import (
	"iter"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// SynthValueRef tags a resolved value-reference read edge.
const SynthValueRef = "value-ref"

const (
	// valueRefCandidateVia marks an extractor-emitted placeholder read of a
	// distinctive identifier; valueRefVia marks the bound read this pass lands.
	valueRefCandidateVia = "value_ref_candidate"
	valueRefVia          = "value_ref"
)

// ResolveValueRefs binds each captured distinctive-name value reference to the
// file-scope constant / variable it reads and re-targets the placeholder into a
// tiered EdgeReads from the reader to that constant.
//
// This closes a change-impact gap: a config constant's readers were invisible
// to blast-radius analysis — fillImpactLive follows every incoming edge except
// Defines/MemberOf, so without a read edge "change this constant → who breaks"
// missed every reader that referenced it outside a captured call/arg position.
// Beat: the read rides a provenance tier (min_tier-filterable), where a flat
// reference is not.
//
// Precision gates: only distinctive names bind (>=3 chars with an uppercase
// letter or underscore — the config-constant shape); a candidate whose name is
// shadowed by a same-file parameter, field, or inner-scope local declarator is
// dropped; a reader in a generated file is skipped; self-reads are ignored.
// Unresolved candidates are
// left as inert placeholders. Idempotent: re-targeting to the same constant is
// a no-op and graph.EvictFile drops the edges on reindex.
func ResolveValueRefs(g graph.Store) int { return resolveValueRefs(g, nil) }

// ResolveValueRefsScoped is the incremental counterpart of ResolveValueRefs: it
// resolves only the value-ref candidates that originate in the given changed
// repos, leaving an unchanged repo's already-bound reads on disk (they were
// never dropped). A nil scope resolves the whole graph, so ResolveValueRefs and
// the whole-index path stay byte-identical. Restricting the candidate set is
// binding-preserving: every candidate binds to a constant declared in its OWN
// file, so which other repos are in scope can never change a resolution.
func ResolveValueRefsScoped(g graph.Store, scope map[string]bool) int {
	return resolveValueRefs(g, scope)
}

// ResolveValueRefsForFilesWithSelection restores reads after a structural
// reparse whose unchanged framework inputs do not schedule the registry. The
// declared seed bounds candidates to these files; same-file binding retains
// the normal shadowing, generated-source and conditional-definition rules.
func ResolveValueRefsForFilesWithSelection(
	g graph.Store,
	scope map[string]bool,
	files []string,
	selection FrameworkSynthesizerSelection,
) int {
	if g == nil || len(files) == 0 || !selection.allows(SynthValueRefName) {
		return 0
	}
	seed := newFrameworkDeclaredSeed(g, frameworkScopeForFiles(g, scope, files), files)
	return runLegacyFrameworkSynth(seed.passStore(SynthValueRefName), ResolveValueRefs)
}

func resolveValueRefs(g graph.Store, scope map[string]bool) int {
	if g == nil {
		return 0
	}
	// Two-pass narrowing. A value-ref binds a captured read to a file-scope
	// constant declared in the SAME file, so only the files that actually carry
	// a candidate read need their declarators indexed. Gather the candidate
	// edges first (Pass A), then build the constant/local maps for their files
	// alone (Pass B) — instead of a whole-graph scan over every
	// constant/variable/param/field/local node, which is the largest node
	// population in the graph and was materialised twice (a flat slice plus the
	// nested maps) on every whole-graph run.
	candidates := valueRefCandidateEdges(g, scope)
	if len(candidates) == 0 {
		return 0
	}
	candidateFiles := make(map[string]struct{}, len(candidates))
	for _, e := range candidates {
		if e.FilePath != "" {
			candidateFiles[e.FilePath] = struct{}{}
		}
	}
	// constsByFile records every file-scope constant/variable declarator of a
	// distinctive name (a name may have several — a try/except import, a
	// `#[cfg]` const, an `#ifdef #define`); localsByFile records the
	// param/field/local declarators that may shadow a read in their own scope.
	constsByFile := map[string]map[string][]*graph.Node{}
	localsByFile := map[string]map[string][]*graph.Node{}
	for f := range candidateFiles {
		for _, n := range g.GetFileNodes(f) {
			if n == nil || n.FilePath == "" {
				continue
			}
			switch n.Kind {
			case graph.KindConstant, graph.KindVariable:
				if !isDistinctiveValueName(n.Name) {
					continue
				}
				m := constsByFile[n.FilePath]
				if m == nil {
					m = map[string][]*graph.Node{}
					constsByFile[n.FilePath] = m
				}
				m[n.Name] = append(m[n.Name], n)
			case graph.KindParam, graph.KindField, graph.KindLocal:
				m := localsByFile[n.FilePath]
				if m == nil {
					m = map[string][]*graph.Node{}
					localsByFile[n.FilePath] = m
				}
				m[n.Name] = append(m[n.Name], n)
			}
		}
	}
	if len(constsByFile) == 0 {
		return 0
	}
	for _, m := range constsByFile {
		for _, ns := range m {
			sort.SliceStable(ns, func(i, j int) bool { return ns[i].StartLine < ns[j].StartLine })
		}
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for _, e := range candidates {
		name, _ := e.Meta["name"].(string)
		consts := constsByFile[e.FilePath][name]
		if name == "" || len(consts) == 0 {
			continue
		}
		// Reader-scope shadow: a same-named param/field/local declared *inside
		// the reading function* (its node ID nests under the reader's via a
		// `.`/`#`/`:` scope separator) means the bare read more likely binds
		// that local, not the constant — drop. A same-named local in an
		// unrelated function does NOT shadow this read (the recall the old
		// file-wide boolean census over-dropped).
		if valueRefReaderShadowed(e.From, localsByFile[e.FilePath][name]) {
			continue
		}
		if reader := g.GetNode(e.From); reader != nil && isGeneratedReader(reader) {
			continue
		}
		// Conditional def: more than one file-scope declarator of the name
		// (try/except / #[cfg] / #ifdef) is legitimate — bind the read to the
		// nearest preceding declarator rather than dropping.
		conditional := len(consts) > 1
		target := consts[0]
		if conditional {
			target = nearestPrecedingDecl(consts, e.Line)
		}
		if target == nil || target.ID == e.From {
			continue
		}
		if e.To == target.ID {
			// A file-scoped replay may see an already-bound candidate. Its
			// certified same-file target also invalidates any old failure stamp;
			// persist the metadata even though the edge identity stays put.
			_, hadTerminal := e.Meta[metaResolveTerminal]
			_, hadReason := e.Meta[metaResolveTerminalReason]
			if hadTerminal || hadReason {
				clearEdgeTerminal(e)
				reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: e.To})
			}
			resolved++
			continue
		}
		oldTo := e.To
		clearEdgeTerminal(e)
		e.To = target.ID
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.7
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeReads, 0.7)
		e.Meta["via"] = valueRefVia
		if conditional {
			e.Meta["conditional_def"] = true
		}
		StampSynthesized(e, SynthValueRef)
		reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		resolved++
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

// valueRefCandidateEdges returns the extractor-emitted placeholder read edges
// (Meta via == value_ref_candidate) the pass resolves. With a nil scope it uses
// the backend's target-namespace projection when available, falling back to the
// complete EdgeReads kind for other stores. With a scope it walks only the
// out-edges of the changed repos' nodes (GetRepoEdges is one backend query per
// repo), since a candidate read always originates in the repo that declared it.
func valueRefCandidateEdges(g graph.Store, scope map[string]bool) []*graph.Edge {
	var out []*graph.Edge
	keep := func(e *graph.Edge) {
		if e == nil || e.Kind != graph.EdgeReads || e.Meta == nil {
			return
		}
		if via, _ := e.Meta["via"].(string); via != valueRefCandidateVia {
			return
		}
		out = append(out, e)
	}
	if scope == nil {
		var edges iter.Seq[*graph.Edge]
		if scanner, ok := g.(graph.ValueRefPlaceholderScanner); ok {
			edges = scanner.ValueRefPlaceholderEdges()
		} else {
			edges = g.EdgesByKind(graph.EdgeReads)
		}
		for e := range edges {
			keep(e)
		}
		return out
	}
	for prefix := range scope {
		if prefix == "" {
			continue
		}
		for _, e := range g.GetRepoEdges(prefix) {
			if e == nil || e.Kind != graph.EdgeReads {
				continue
			}
			keep(e)
		}
	}
	return out
}

// valueRefReaderShadowed reports whether any same-named declarator is scoped
// inside the reading function — its node ID nests under the reader's ID via a
// `.` / `#` / `:` scope separator (`f.go::Run.x`, `f.go::Run#x`). Such a
// declarator shadows a bare read in the reader's own scope; a same-named
// declarator in an unrelated function does not.
func valueRefReaderShadowed(readerID string, locals []*graph.Node) bool {
	if readerID == "" {
		return false
	}
	for _, l := range locals {
		if l == nil || !strings.HasPrefix(l.ID, readerID) || len(l.ID) <= len(readerID) {
			continue
		}
		switch l.ID[len(readerID)] {
		case '.', '#', ':':
			return true
		}
	}
	return false
}

// nearestPrecedingDecl returns the conditional-def declarator with the greatest
// line at or before readLine, falling back to the first when the read precedes
// them all. decls is sorted ascending by line.
func nearestPrecedingDecl(decls []*graph.Node, readLine int) *graph.Node {
	best := decls[0]
	for _, d := range decls {
		if d.StartLine <= readLine {
			best = d
		} else {
			break
		}
	}
	return best
}

// isDistinctiveValueName reports whether name has the config-constant shape:
// at least 3 characters and at least one uppercase letter or underscore. This
// keeps the value-ref binding to names unlikely to collide with an ordinary
// local (which is conventionally lowerCamelCase).
func isDistinctiveValueName(name string) bool {
	if len(name) < 3 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}

// isGeneratedReader reports whether a node lives in a generated file, which is
// excluded from value-ref binding (its reads are machine-emitted noise).
func isGeneratedReader(n *graph.Node) bool {
	if n.Meta != nil {
		if gen, _ := n.Meta["generated"].(bool); gen {
			return true
		}
	}
	p := n.FilePath
	return strings.Contains(p, ".pb.go") ||
		strings.Contains(p, ".g.dart") ||
		strings.Contains(p, "_generated.") ||
		strings.Contains(p, ".generated.") ||
		strings.HasSuffix(p, ".gen.go")
}
