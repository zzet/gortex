package graphview

import (
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// MaxRepoViewLayers is how deep one repository's view may stack.
//
// Three is the whole vocabulary, not a tuning knob: a commit layer over
// the indexed corpus, the checkout's uncommitted working tree over that,
// and the editor buffers on top. There is nothing a fourth would say —
// those are the four kinds of content a request can read — and each
// nesting level costs every read another indirection, so a stack that
// grew past this is a bug in whoever assembled it rather than a view
// anyone asked for.
//
// The cap counts the layers handed to one ComposeRepoView call, and the
// three levels are not always spent in one call. MaterializeCheckout
// spends the bottom one itself: a checkout's view is identified by its
// commit generation, so that generation IS the base reader and only the
// working tree is passed as a layer, leaving the third level for the
// session buffer layer the MCP overlay composes at request time.
const MaxRepoViewLayers = 3

// Compile-time assertion that a pinned store handle reads as a base
// graph. ComposeRepoView takes the wider graph.Reader so a test can
// compose over an in-memory corpus, but the handle is what production
// passes and the stack would be pointless if it did not fit.
var _ graph.Reader = (*store_sqlite.Store)(nil)

// ComposeRepoView stacks layers over a base reader and returns the
// composed reader together with the identity that names it.
//
// base is the reader for id.BaseGeneration — for a checkout that is the
// corpus with its commit generation already composed on, which is what
// MaterializeCheckout hands in. layers are ordered bottom to top: the
// first is applied to base, the second to that result, and so on, so
// the last layer in the slice is the one whose content wins. Each level
// is a graph.OverlaidView, which composes any layer implementation — a
// GenerationLayer over persisted content, or the in-memory buffer
// layer — so a stack can mix them.
//
// The identity is validated, not derived: the caller knows which
// generations and buffer fingerprints its layers came from and this
// function does not, so it checks that what it was handed is
// well-formed and that the layer count matches the stack it is about to
// build. That count is the one cross-check available here, and it
// catches the caller that named one set of layers and composed another.
// Composing zero layers is legal and returns base unchanged, which is
// what a checkout with nothing on top of its commit reads.
func ComposeRepoView(base graph.Reader, layers []graph.OverlayLayerReader, id RepoViewID) (graph.Reader, RepoViewID, error) {
	if base == nil {
		return nil, RepoViewID{}, NewViewError(CodeInvalidViewSelector, "a repo view needs a base reader")
	}
	if len(layers) > MaxRepoViewLayers {
		return nil, RepoViewID{}, NewViewError(CodeInvalidViewSelector,
			fmt.Sprintf("a repo view stacks at most %d layers, got %d", MaxRepoViewLayers, len(layers)))
	}
	if err := id.Validate(); err != nil {
		return nil, RepoViewID{}, err
	}
	if len(id.Layers) != len(layers) {
		return nil, RepoViewID{}, NewViewError(CodeInvalidViewSelector,
			fmt.Sprintf("view identity names %d layers but %d were composed", len(id.Layers), len(layers)))
	}

	reader := base
	for i, layer := range layers {
		if layer == nil {
			return nil, RepoViewID{}, NewViewError(CodeInvalidViewSelector,
				fmt.Sprintf("layer %d is missing", i))
		}
		reader = graph.NewOverlaidViewWithLayer(reader, layer)
	}
	return reader, id, nil
}
