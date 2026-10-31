package lsp

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// referencesAddPass adds call edges for a references-capable server whose
// call hierarchy is absent (e.g. intelephense). For each function / method
// declaration it asks textDocument/references and mints an lsp_resolved
// EdgeCalls from each reference site's enclosing callable to the target —
// the add-phase analogue of a call-hierarchy hop for servers that implement
// only references. Without it the per-file sweep's hierarchy hops never run,
// so the enrich pass confirms existing edges but never ADDS the dispatch
// call sites the server can enumerate (the "confirm-only, edges_added 0"
// outcome). Provider-generic: any references-capable, call-hierarchy-absent
// server benefits.
//
// Targets are ordered dispatch-anchors-first (interface / trait members,
// then abstract-marked methods, then by descending fan-in) so a per-repo
// deadline is spent where recall is scarcest. Each declaration commits under
// rmu as soon as its references land, so a deadline cut loses only the
// unvisited remainder. Runs under the caller's targeted context (the same
// budget the confirm pass uses), before the per-file hover sweep.
func (p *Provider) referencesAddPass(ctx context.Context, g graph.Store, view *lspGraphView, repoPrefix, absRoot string, langNodes []*graph.Node, rmu sync.Locker, session *docSession, result *semantic.EnrichResult) {
	targets := selectReferencesAddTargets(view, langNodes)
	if len(targets) == 0 {
		return
	}
	result.ReferencesAddPass = true

	// Site-file contents cached for the cheap name-token guard, keyed by
	// repo-relative path. Site files are read from disk, never opened on the
	// server — attribution uses the bounded repo graph projection.
	siteSrc := map[string][]byte{}
	mintedEdge := map[string]*graph.Edge{}
	promoted := map[string]bool{}
	mutations := newLSPMutationBatch()

	for _, n := range targets {
		if ctx.Err() != nil {
			break
		}
		line, ok := lspLine(n)
		if !ok {
			continue
		}
		rel := nodeRelPath(n)
		if !p.servesFile(rel) {
			continue
		}
		content, release, err := session.acquire(p.Client(), filepath.Join(absRoot, rel))
		if err != nil {
			continue
		}
		col := identifierColumn(content, n.StartLine, n.Name)
		refs, err := p.findReferences(absRoot, rel, line, col)
		release()
		if err != nil || len(refs) == 0 {
			continue
		}

		for _, loc := range refs {
			sitePath := uriToPath(loc.URI, absRoot)
			if sitePath == "" {
				continue
			}
			siteLine := loc.Range.Start.Line + 1
			// The reference's column picks the owner when several members
			// share the site line; an owner it cannot pin mints nothing.
			enclosing := view.matchCallableAt(scopedPath(repoPrefix, sitePath), lspSite{
				line: siteLine,
				col:  loc.Range.Start.Character,
				src:  func() []byte { return siteFileContent(siteSrc, absRoot, sitePath) },
			})
			if enclosing == nil || enclosing.ID == n.ID {
				continue
			}
			key := enclosing.ID + "\x00" + n.ID
			if content := siteFileContent(siteSrc, absRoot, nodeRelPath(enclosing)); content != nil {
				if _, found := identifierColumnStrict(content, siteLine, n.Name); !found {
					continue
				}
			}
			if e0, ok := mintedEdge[key]; ok {
				// The edge is still staged in mutations.adds; mutating that
				// pointer carries every accumulated call site into the one insert.
				graph.AppendCallSite(e0, enclosing.FilePath, siteLine)
				continue
			}
			if promoted[key] {
				continue
			}
			if existing := view.findMatchingEdge(enclosing.ID, n.ID, graph.EdgeCalls); existing != nil {
				if graph.OriginRank(existing.Origin) < graph.OriginRank(graph.OriginLSPResolved) {
					rmu.Lock()
					semantic.ConfirmEdge(existing, p.Name())
					existing.Origin = graph.OriginLSPResolved
					mutations.stagePersist(existing)
					rmu.Unlock()
					result.EdgesConfirmed++
				}
				promoted[key] = true
				continue
			}
			edge := newLSPResolvedEdge(enclosing.ID, n.ID, graph.EdgeCalls,
				enclosing.FilePath, siteLine, p.Name(), graph.OriginLSPResolved)
			if mutations.stageAdd(view, edge) {
				mintedEdge[key] = edge
				result.EdgesAdded++
			}
		}
	}
	if len(mutations.adds) > 0 || len(mutations.persists) > 0 {
		rmu.Lock()
		mutations.apply(g, nil)
		rmu.Unlock()
	}
}

// selectReferencesAddTargets returns the function / method nodes to query,
// ordered dispatch-anchors-first: interface / trait members (tier 2), then
// abstract-marked methods (tier 1), then plain methods (tier 0); within a
// tier by descending fan-in so a deadline is spent on the most-referenced
// declarations first.
func selectReferencesAddTargets(view *lspGraphView, langNodes []*graph.Node) []*graph.Node {
	// Interface / trait type names in the repo — a method whose receiver is
	// one of these is a dispatch anchor the reference set fans out across.
	dispatchOwners := map[string]bool{}
	for _, n := range langNodes {
		switch {
		case n.Kind == graph.KindInterface:
			dispatchOwners[n.Name] = true
		case n.Kind == graph.KindType && isTraitNode(n):
			dispatchOwners[n.Name] = true
		}
	}

	type scored struct {
		n     *graph.Node
		tier  int
		fanIn int
	}
	var cand []scored
	for _, n := range langNodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		tier := 0
		if recv, _ := n.Meta["receiver"].(string); recv != "" && dispatchOwners[recv] {
			tier = 2
		} else if isAbstractMarked(n) {
			tier = 1
		}
		cand = append(cand, scored{n: n, tier: tier, fanIn: view.fanIn(n.ID)})
	}
	if len(cand) == 0 {
		return nil
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if cand[i].tier != cand[j].tier {
			return cand[i].tier > cand[j].tier
		}
		return cand[i].fanIn > cand[j].fanIn
	})
	out := make([]*graph.Node, len(cand))
	for i := range cand {
		out[i] = cand[i].n
	}
	return out
}

// isTraitNode reports whether a KindType node models a trait (PHP traits are
// KindType with a trait flavor, unlike C#/Rust which use KindInterface).
func isTraitNode(n *graph.Node) bool {
	if n.Meta == nil {
		return false
	}
	if k, _ := n.Meta["kind"].(string); k == "trait" {
		return true
	}
	if tf, _ := n.Meta["type_flavor"].(string); tf == "trait" {
		return true
	}
	return false
}

// isAbstractMarked reports whether a method node is an abstract / interface /
// trait / virtual declaration, across the per-language marker keys the
// extractors stamp (php abstract, csharp iface_member, rust trait_decl,
// phpdoc virtual).
func isAbstractMarked(n *graph.Node) bool {
	if n.Meta == nil {
		return false
	}
	if b, ok := n.Meta["abstract"].(bool); ok && b {
		return true
	}
	if b, ok := n.Meta["iface_member"].(bool); ok && b {
		return true
	}
	if s, ok := n.Meta["trait_decl"].(string); ok && s == "true" {
		return true
	}
	if _, ok := n.Meta["virtual"]; ok {
		return true
	}
	return false
}

// siteFileContent reads (and caches) a repo-relative site file for the
// name-token guard. A read error caches nil so a missing file is not retried.
func siteFileContent(cache map[string][]byte, absRoot, rel string) []byte {
	if rel == "" {
		return nil
	}
	if c, ok := cache[rel]; ok {
		return c
	}
	c, err := os.ReadFile(filepath.Join(absRoot, rel))
	if err != nil {
		cache[rel] = nil
		return nil
	}
	cache[rel] = c
	return c
}
