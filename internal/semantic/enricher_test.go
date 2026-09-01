package semantic

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// Confirmation upgrades the tier but must not erase where the edge came
// from: the LSP dispatch gate's evidence probe distinguishes extractor /
// resolver hierarchy edges from the sweep's own recoveries by origin, and a
// confirm that overwrote the origin in place would decay that evidence one
// pass at a time (self-reinforcing — the gate admits hierarchy types, the
// sweep confirms their edges, the probe loses them).
func TestConfirmEdgePreservesPriorOrigin(t *testing.T) {
	t.Run("non-LSP origin is preserved in meta", func(t *testing.T) {
		e := &graph.Edge{Kind: graph.EdgeExtends, Origin: graph.OriginASTResolved, Confidence: 0.7}
		ConfirmEdge(e, "lsp-go")
		assert.Equal(t, graph.OriginLSPResolved, e.Origin)
		assert.Equal(t, 1.0, e.Confidence)
		assert.Equal(t, string(graph.OriginASTResolved), e.Meta["confirmed_from_origin"])
	})
	t.Run("empty origin still leaves the marker", func(t *testing.T) {
		// An extractor-minted stub edge carries no origin at all; the marker's
		// PRESENCE is the provenance signal, not its value.
		e := &graph.Edge{Kind: graph.EdgeExtends, Confidence: 0.7}
		ConfirmEdge(e, "lsp-go")
		_, ok := e.Meta["confirmed_from_origin"]
		assert.True(t, ok)
	})
	t.Run("an edge already at LSP grade gains no marker", func(t *testing.T) {
		e := &graph.Edge{Kind: graph.EdgeImplements, Origin: graph.OriginLSPDispatch, Confidence: 0.9}
		ConfirmEdge(e, "lsp-go")
		_, ok := e.Meta["confirmed_from_origin"]
		assert.False(t, ok, "there is no non-LSP provenance to preserve")
	})
	t.Run("re-confirmation does not overwrite the original provenance", func(t *testing.T) {
		e := &graph.Edge{Kind: graph.EdgeExtends, Origin: graph.OriginASTResolved, Confidence: 0.7}
		ConfirmEdge(e, "lsp-go")
		ConfirmEdge(e, "lsp-go") // now lsp_resolved — marker must keep ast_resolved
		assert.Equal(t, string(graph.OriginASTResolved), e.Meta["confirmed_from_origin"])
	})
}
