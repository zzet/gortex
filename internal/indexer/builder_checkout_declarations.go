package indexer

import "github.com/zzet/gortex/internal/semantic"

// withCheckoutDeclarations hands a working-tree build's enrichment pass the
// layer the generation sits on, so a use in a re-derived file that binds to a
// declaration in a file the generation does not carry (declared closure
// context, withheld from the payload) keeps its type-resolved provenance: the
// provider maps the use onto the node the layer below serves at the
// declaration's position, exactly as a whole-module load maps it onto its own
// copy. The reader is only read.
func withCheckoutDeclarations(scope semantic.CheckoutCompilerScope, base LayerBase) semantic.CheckoutCompilerScope {
	if base != nil {
		scope.Declarations = base
	}
	return scope
}
