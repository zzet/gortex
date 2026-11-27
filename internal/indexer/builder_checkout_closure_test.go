package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// A linked checkout can have a small committed delta with more than 200
// unchanged dependencies. It needs a complete resolution corpus before a
// subsequent checkout can safely use that generation as its parent.
func TestCheckoutCommitClosureUsesChangeSizedDefault(t *testing.T) {
	f := newDeltaWriteSetFixture(t, deltaWriteSetTree(220, false))
	f.builder.Config.AffectedByReresolveMax = 0
	deltaWriteSetWriteTree(t, f.request.RootPath, deltaWriteSetTree(220, true))
	f.git("add", "-A")
	f.git("-c", "user.name=Delta Test", "-c", "user.email=delta@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "-qm", "checkout ten-file edit")
	identity := f.request.Identity
	identity.OwnerKind = checkoutLayerOwnerKind
	identity.LayerID = "checkout-commit-closure"
	identity.CheckoutID = "checkout-closure"
	identity.BaseGenerationID = f.baseID
	identity.LowerViewFingerprint = identity.TreeOID
	identity.ProvenanceCommitOID = ""
	id, report, err := f.builder.BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: identity, Base: f.base, RepoDir: f.request.RootPath,
		RootPath: f.request.RootPath, BaseTreeOID: identity.TreeOID,
		TargetTreeOID: f.git("rev-parse", "HEAD^{tree}"),
		RepoPrefix:    f.request.RepoPrefix, WorkspaceID: f.request.WorkspaceID, ProjectID: f.request.ProjectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.ClosureCap != 320 || report.ClosureCapSource != ClosureCapFromChangeSized {
		t.Fatalf("ten-file checkout used closure cap (%d, %q), want (320, change_sized)", report.ClosureCap, report.ClosureCapSource)
	}
	if report.ClosureTruncated || report.ClosureFiles <= 200 {
		t.Fatalf("checkout did not resolve its complete dependency corpus: %+v", report)
	}
	states, err := f.builder.Store.AtGeneration(id).ProducerStates()
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range []graphview.CapabilityID{graphview.CapResolutionLocal, graphview.CapIncomingEdges} {
		found := false
		for _, state := range states {
			if state.Producer == string(capability) {
				found = true
				if state.State != store_sqlite.ProducerStateComplete {
					t.Fatalf("%s = %s, want complete", capability, state.State)
				}
			}
		}
		if !found {
			t.Fatalf("checkout omitted completeness for %s", capability)
		}
	}
}

func TestCheckoutCommitClosurePreservesOperatorBound(t *testing.T) {
	f := newDeltaWriteSetFixture(t, deltaWriteSetTree(220, false))
	f.builder.Config.AffectedByReresolveMax = 200
	deltaWriteSetWriteTree(t, f.request.RootPath, deltaWriteSetTree(220, true))
	f.git("add", "-A")
	f.git("-c", "user.name=Delta Test", "-c", "user.email=delta@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "-qm", "checkout ten-file edit")
	identity := f.request.Identity
	identity.OwnerKind = checkoutLayerOwnerKind
	identity.LayerID = "checkout-commit-operator"
	identity.CheckoutID = "checkout-operator"
	identity.BaseGenerationID = f.baseID
	identity.LowerViewFingerprint = identity.TreeOID
	identity.ProvenanceCommitOID = ""
	id, report, err := f.builder.BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: identity, Base: f.base, RepoDir: f.request.RootPath,
		RootPath: f.request.RootPath, BaseTreeOID: identity.TreeOID,
		TargetTreeOID: f.git("rev-parse", "HEAD^{tree}"),
		RepoPrefix:    f.request.RepoPrefix, WorkspaceID: f.request.WorkspaceID, ProjectID: f.request.ProjectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.ClosureCap != 200 || report.ClosureCapSource != ClosureCapFromOperator || !report.ClosureTruncated {
		t.Fatalf("checkout exceeded or misreported the explicit operator bound: %+v", report)
	}
	states, err := f.builder.Store.AtGeneration(id).ProducerStates()
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range []graphview.CapabilityID{graphview.CapResolutionLocal, graphview.CapIncomingEdges} {
		found := false
		for _, state := range states {
			if state.Producer == string(capability) {
				found = true
				if state.State != store_sqlite.ProducerStateIncomplete {
					t.Fatalf("%s = %s, want incomplete", capability, state.State)
				}
			}
		}
		if !found {
			t.Fatalf("checkout omitted incompleteness for %s", capability)
		}
	}
}
