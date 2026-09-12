package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/semantic"
)

// W3.2 — the LSP half of the enrichment-output item.
//
// enrichNodeOnDemand and confirmSymbolRefsOnDemand are WRITES raised from READ
// paths: get_symbol faults in an LSP-grade semantic_type, and
// find_usages / get_callers fault in compiler-confirmed incoming references.
// Both used to stamp `s.graph` — generation zero, the shared corpus — from a
// language server rooted in whatever tracked repository contained the file,
// no matter which view the request read. They now name their output through
// the authority like every other enrichment, and a request that reads a
// checkout of its own is refused before a server is spawned or a byte hovered.
//
// These tests do not need a live language server, and deliberately run without
// one: the thing under test is the ADMISSION — whether a receipt naming the
// corpus output was taken, and whether the workspace was touched at all —
// which is resolved before lspProviderForPath is ever called. A standing
// receipt for the same owner is the observable: the helper's own admission
// supersedes it, and only its admission can.

// lspEnrichmentStack is a viewStack with a semantic manager attached (no
// providers, so nothing spawns) and a real graph node to enrich.
type lspEnrichmentStack struct {
	*viewStack
	node      *graph.Node
	root      string
	authority *indexer.OutputGenerationAuthority
}

func newLSPEnrichmentStack(t *testing.T) *lspEnrichmentStack {
	t.Helper()
	stack := newViewStack(t)
	// A zero Manager registers no providers and no router, so
	// lspProviderForPath refuses after the output has been resolved. That is
	// the point: it isolates the admission from the subprocess.
	stack.srv.semanticMgr = &semantic.Manager{}

	var node *graph.Node
	for _, n := range stack.store.AllNodes() {
		if n.Kind == graph.KindFunction && n.RepoPrefix == "repo" && n.Name == "Keeper" {
			node = n
			break
		}
	}
	if node == nil {
		t.Fatal("the fixture indexed no repo.Keeper function to enrich")
	}
	abs, err := stack.srv.absolutePath(node.FilePath)
	if err != nil {
		t.Fatalf("absolutePath(%q): %v", node.FilePath, err)
	}
	root, err := stack.srv.workspaceRootFor(abs)
	if err != nil {
		t.Fatalf("workspaceRootFor(%q): %v", abs, err)
	}
	if root != stack.repoRoot {
		t.Fatalf("the fixture node resolves to workspace %q, want %q", root, stack.repoRoot)
	}
	return &lspEnrichmentStack{
		viewStack: stack, node: node, root: root,
		authority: stack.srv.outputGenerationAuthority(),
	}
}

// standingReceipt admits an enrichment for the owner the on-demand helper will
// name, so a later admission for the same owner supersedes it and no admission
// leaves it fulfilable. It is the only way to observe an admission that is
// abandoned immediately afterwards.
func (s *lspEnrichmentStack) standingReceipt(t *testing.T, producer string) *EnrichmentOutput {
	t.Helper()
	out, err := BeginBaseEnrichment(context.Background(), s.authority, s.srv.graph,
		producer, s.node.RepoPrefix, s.root)
	if err != nil {
		t.Fatalf("standing %s receipt: %v", producer, err)
	}
	return out
}

// TestOnDemandSemanticTypeNamesItsOutputGeneration is the production-entrypoint
// trace for the get_symbol fault-in: enrichNodeOnDemand is the function
// tools_core.go calls, and it must name the corpus output through the
// authority before it touches a language server.
//
// Revert-red: with the pre-item body — provider.EnrichNode(s.graph, root, node)
// with no ctx and no admission — nothing supersedes the standing receipt and
// Complete returns nil.
func TestOnDemandSemanticTypeNamesItsOutputGeneration(t *testing.T) {
	s := newLSPEnrichmentStack(t)
	standing := s.standingReceipt(t, EnrichProducerSemanticType)

	s.srv.enrichNodeOnDemand(context.Background(), s.node)

	err := standing.Complete()
	if !errors.Is(err, indexer.ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("the on-demand semantic_type write named no output generation (standing receipt settled with %v)", err)
	}
}

// TestOnDemandSymbolRefsNameTheirOutputGeneration is the same trace for the
// find_usages / get_callers fault-in.
func TestOnDemandSymbolRefsNameTheirOutputGeneration(t *testing.T) {
	s := newLSPEnrichmentStack(t)
	standing := s.standingReceipt(t, EnrichProducerSymbolRefs)

	s.srv.confirmSymbolRefsOnDemand(context.Background(), s.node)

	err := standing.Complete()
	if !errors.Is(err, indexer.ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("the on-demand symbol-refs write named no output generation (standing receipt settled with %v)", err)
	}
}

// TestOnDemandLSPEnrichmentUnderARoutedViewTouchesNothing is the refusal.
//
// A request that reads a checkout of its own has no writable output — its
// generations are published and sealed — so the fault-in must not admit an
// output, must not spawn a language server rooted in the tracked primary's
// tree, and must not stamp the shared corpus behind the request's back.
//
// Revert-red: with beginEnrichmentOutput handing every request the base output
// (the pre-item behaviour of every producer), the standing receipt is
// superseded and this fails.
func TestOnDemandLSPEnrichmentUnderARoutedViewTouchesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		producer string
		call     func(*Server, context.Context, *graph.Node)
	}{
		{"semantic_type", EnrichProducerSemanticType,
			func(srv *Server, ctx context.Context, n *graph.Node) { srv.enrichNodeOnDemand(ctx, n) }},
		{"symbol_refs", EnrichProducerSymbolRefs,
			func(srv *Server, ctx context.Context, n *graph.Node) { srv.confirmSymbolRefsOnDemand(ctx, n) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLSPEnrichmentStack(t)
			ctx := routedEnrichmentCtx(t, s.viewStack)
			standing := s.standingReceipt(t, tc.producer)

			tc.call(s.srv, ctx, s.node)

			if err := standing.Complete(); err != nil {
				t.Fatalf("a routed request admitted an on-demand LSP enrichment: %v", err)
			}
		})
	}
}

// TestRoutedSymbolRefsRefusalDoesNotBurnTheConfirmationLedger guards the
// follow-on cost of the refusal: refsConfirmed is a per-process "already done"
// ledger, so a refusal that still marked the symbol would make every LATER
// unrouted request skip the fault-in for it too — the refusal would silently
// degrade the corpus answers as well as the routed one.
//
// Revert-red: move s.refsConfirmed.Store above the `if out == nil { return }`
// guard in confirmSymbolRefsOnDemand and this fails.
//
// Scope note: the fixture registers no language server, so the corpus arm
// cannot be used as the positive control here — a run with no provider has
// never marked the ledger, before this item or after (the pre-item body
// returned on the same lspProviderForPath error). What this pins is that the
// OUTPUT refusal returns on the same terms, ahead of the ledger write.
func TestRoutedSymbolRefsRefusalDoesNotBurnTheConfirmationLedger(t *testing.T) {
	s := newLSPEnrichmentStack(t)
	ctx := routedEnrichmentCtx(t, s.viewStack)

	s.srv.confirmSymbolRefsOnDemand(ctx, s.node)

	if _, marked := s.srv.refsConfirmed.Load(s.node.ID); marked {
		t.Fatal("a refused routed confirmation marked the symbol confirmed for every later request")
	}
	marked := 0
	s.srv.refsConfirmed.Range(func(any, any) bool { marked++; return true })
	if marked != 0 {
		t.Fatalf("a refused routed confirmation wrote %d ledger entries", marked)
	}
}

// TestRoutedLSPEnrichmentRefusalIsAnnotated closes the "silently thinner
// answer" half. The refusal is right, but a get_symbol / find_usages answer
// served without the fault-in genuinely carries less than the same call
// against the corpus does, and an answer that is thinner without saying so is
// the kind of quiet degradation the view contract exists to prevent.
//
// Revert-red: delete the noteDegraded call in lspEnrichmentTarget and the
// request records nothing.
func TestRoutedLSPEnrichmentRefusalIsAnnotated(t *testing.T) {
	for _, tc := range []struct {
		name string
		want graphview.CapabilityID
		call func(*Server, context.Context, *graph.Node)
	}{
		{"semantic_type", graphview.CapLSPHover,
			func(srv *Server, ctx context.Context, n *graph.Node) { srv.enrichNodeOnDemand(ctx, n) }},
		{"symbol_refs", graphview.CapLSPReferences,
			func(srv *Server, ctx context.Context, n *graph.Node) { srv.confirmSymbolRefsOnDemand(ctx, n) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLSPEnrichmentStack(t)
			ctx, view := routedEnrichmentView(t, s.viewStack)

			tc.call(s.srv, ctx, s.node)

			degraded, _ := view.annotations()
			found := false
			for _, status := range degraded {
				if status.Capability == tc.want {
					found = true
					if status.State != graphview.StateUnavailable {
						t.Fatalf("%s annotated as %q, want %q", tc.want, status.State, graphview.StateUnavailable)
					}
				}
			}
			if !found {
				t.Fatalf("a refused on-demand enrichment left the answer unannotated: %v", degraded)
			}
		})
	}
}

// TestLSPEnrichmentCapabilityNamesWhatWasNotServed pins the producer →
// capability mapping the annotation above reports, so the two on-demand
// enrichments are never collapsed onto one capability.
func TestLSPEnrichmentCapabilityNamesWhatWasNotServed(t *testing.T) {
	if got := lspEnrichmentCapability(EnrichProducerSymbolRefs); got != graphview.CapLSPReferences {
		t.Fatalf("symbol refs annotate %q, want %q", got, graphview.CapLSPReferences)
	}
	if got := lspEnrichmentCapability(EnrichProducerSemanticType); got != graphview.CapLSPHover {
		t.Fatalf("semantic type annotates %q, want %q", got, graphview.CapLSPHover)
	}
}
