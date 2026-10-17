package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A checkout routing another checkout's commit layer built from the same
// inputs (sharedCommit's adoption) is admitted like one routing its own; a
// foreign layer built for another tree, another base or of another kind is
// refused as a moved HEAD or base.
func TestAdmissionAcceptsAnAdoptedCommitLayer(t *testing.T) {
	c := &CheckoutCoordinator{checkoutID: "wt", configHash: "cfg", extractors: "x1", resolverVersion: "r1"}
	base := primaryBase{graphID: "g", generationID: 7, treeOID: "basetree"}
	row := func(identity GenerationIdentity) store_sqlite.ViewGeneration {
		return store_sqlite.ViewGeneration{
			OwnerKind: identity.OwnerKind, GraphID: identity.GraphID, LayerID: identity.LayerID,
			CheckoutID: identity.CheckoutID, GenerationKind: identity.GenerationKind,
			BaseGenerationID: identity.BaseGenerationID, LowerViewFingerprint: identity.LowerViewFingerprint,
			TreeOID: identity.TreeOID, ProvenanceCommitOID: identity.ProvenanceCommitOID,
			ConfigHash: identity.ConfigHash, ExtractorVersions: identity.ExtractorVersions,
			ResolverVersion: identity.ResolverVersion, DependencyRevision: identity.DependencyRevision,
		}
	}
	own := row(c.commitIdentity(base, "head"))
	other := &CheckoutCoordinator{checkoutID: "primary", configHash: "cfg", extractors: "x1", resolverVersion: "r1"}
	adopted := row(other.commitIdentity(base, "head"))
	if !c.routedCommitLayerFor(own, base, "head") {
		t.Fatal("the checkout's own commit layer was refused")
	}
	if !c.routedCommitLayerFor(adopted, base, "head") {
		t.Fatal("an adopted commit layer built from the same inputs was refused")
	}
	if c.routedCommitLayerFor(adopted, base, "othertree") {
		t.Fatal("an adopted layer built for another tree was admitted")
	}
	moved := base
	moved.generationID = 8
	if c.routedCommitLayerFor(adopted, moved, "head") {
		t.Fatal("an adopted layer over another base was admitted")
	}
	wrongKind := adopted
	wrongKind.GenerationKind = "working_tree"
	if c.routedCommitLayerFor(wrongKind, base, "head") {
		t.Fatal("a foreign layer of another kind was admitted")
	}
}
