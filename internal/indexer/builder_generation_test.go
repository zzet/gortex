package indexer

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// What a generation may say about literal and regex search.
//
// The searcher an answer comes from is built over a checkout root, so the bytes
// it reports are the working copy's. Only the layer that IS that working copy
// claims the capability; a ref view, which no checkout holds, withdraws it; and
// every layer in between says nothing at all, because a narrowing there is
// worst-cased over the whole stack and would refuse the live routed search the
// checkout can answer exactly.

// dedicatedBaseGenerationKind is the kind a dedicated graph's own base
// generation carries. It is spelled as a literal by the builders that write it
// (builder_dedicated_claimed.go, builder_dedicated_delta.go), and spelled the
// same way here so a rename that misses one of them shows up as a failure
// rather than as a silently different classification.
const dedicatedBaseGenerationKind = "dedicated"

// TestTextSearchProducerIsDeclaredPerIdentityKind pins what every identity kind
// a build can carry declares, including one outside the vocabulary.
func TestTextSearchProducerIsDeclaredPerIdentityKind(t *testing.T) {
	cases := []struct {
		name        string
		identity    GenerationIdentity
		wantDeclare bool
		wantState   store_sqlite.ProducerState
		wantServes  bool
		wantReason  string
	}{
		{
			name: "working-tree layer is the working copy",
			identity: GenerationIdentity{
				OwnerKind:      checkoutLayerOwnerKind,
				GenerationKind: DirtyLayerGenerationKind,
				CheckoutID:     "checkout-worktree",
			},
			wantDeclare: true,
			wantState:   store_sqlite.ProducerStateComplete,
			wantServes:  true,
		},
		{
			name: "commit layer answers for no working copy of its own",
			identity: GenerationIdentity{
				OwnerKind:      checkoutLayerOwnerKind,
				GenerationKind: CommitLayerGenerationKind,
				CheckoutID:     "checkout-worktree",
				TreeOID:        strings.Repeat("a", 40),
			},
			wantDeclare: false,
		},
		{
			name: "dedicated base answers for no working copy of its own",
			identity: GenerationIdentity{
				OwnerKind:      checkoutLayerOwnerKind,
				GenerationKind: dedicatedBaseGenerationKind,
				CheckoutID:     "checkout-primary",
				TreeOID:        strings.Repeat("b", 40),
			},
			wantDeclare: false,
		},
		{
			name: "ref view has no working copy at all",
			identity: GenerationIdentity{
				OwnerKind:      refViewOwnerKind,
				GenerationKind: CommitLayerGenerationKind,
				TreeOID:        strings.Repeat("c", 40),
			},
			wantDeclare: true,
			wantState:   store_sqlite.ProducerStateUnavailable,
			wantReason:  noWorkingCopyTextSearchReason,
		},
		{
			name: "a ref view is withdrawn whatever kind it carries",
			identity: GenerationIdentity{
				OwnerKind:      refViewOwnerKind,
				GenerationKind: DirtyLayerGenerationKind,
				TreeOID:        strings.Repeat("d", 40),
			},
			wantDeclare: true,
			wantState:   store_sqlite.ProducerStateUnavailable,
			wantReason:  noWorkingCopyTextSearchReason,
		},
		{
			name: "an unrecognised kind is not vouched for",
			identity: GenerationIdentity{
				OwnerKind:      checkoutLayerOwnerKind,
				GenerationKind: "something-new",
			},
			wantDeclare: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row, declared := textSearchProducer(tc.identity)
			if declared != tc.wantDeclare {
				t.Fatalf("declared = %v (row %+v), want %v", declared, row, tc.wantDeclare)
			}
			if !declared {
				if row != (store_sqlite.ProducerCompleteness{}) {
					t.Errorf("an undeclared identity returned a row: %+v", row)
				}
				if servesTextSearch(tc.identity) {
					t.Errorf("servesTextSearch = true for an identity that declares nothing")
				}
				return
			}
			if row.Producer != string(graphview.CapSearchText) {
				t.Fatalf("declared producer %q, want %q", row.Producer, graphview.CapSearchText)
			}
			if row.State != tc.wantState {
				t.Errorf("state = %q, want %q (reason %q)", row.State, tc.wantState, row.Reason)
			}
			if row.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", row.Reason, tc.wantReason)
			}
			if got := servesTextSearch(tc.identity); got != tc.wantServes {
				t.Errorf("servesTextSearch = %v, want %v", got, tc.wantServes)
			}
		})
	}
}

// TestCommittedIdentityNeverClaimsCompleteTextSearch is the invariant the table
// above is one reading of: an identity whose bytes are a committed tree does not
// get to say the capability is whole. A committed tree is not what the checkout
// root holds the moment the root is edited, and the root is what a search reads.
func TestCommittedIdentityNeverClaimsCompleteTextSearch(t *testing.T) {
	committed := []GenerationIdentity{
		{OwnerKind: checkoutLayerOwnerKind, GenerationKind: CommitLayerGenerationKind},
		{OwnerKind: checkoutLayerOwnerKind, GenerationKind: dedicatedBaseGenerationKind},
		{OwnerKind: checkoutLayerOwnerKind, GenerationKind: "something-new"},
		{OwnerKind: refViewOwnerKind, GenerationKind: CommitLayerGenerationKind},
		{OwnerKind: refViewOwnerKind, GenerationKind: DirtyLayerGenerationKind},
	}
	for _, identity := range committed {
		row, declared := textSearchProducer(identity)
		if declared && row.State == store_sqlite.ProducerStateComplete {
			t.Errorf("%s/%s claims complete text search",
				identity.OwnerKind, identity.GenerationKind)
		}
		if declared && row.Reason == "" {
			t.Errorf("%s/%s narrows the capability without saying why",
				identity.OwnerKind, identity.GenerationKind)
		}
	}
}

// TestCommittedCheckoutLayerNarrowsNothing is the other half, and it is the one
// that keeps a live routed search answerable.
//
// A view's completeness is the WORST state any generation in its stack declares
// (graphview.Materializer.completeness), and a checkout view stacks the commit
// layer, the working-tree layer and the whole ancestry beneath them. A layer
// under the working-tree layer that declared anything but complete would narrow
// every routed view built on it — so it declares nothing, and any state at all
// on one of those identities is a regression.
func TestCommittedCheckoutLayerNarrowsNothing(t *testing.T) {
	beneathTheWorkingCopy := []GenerationIdentity{
		{OwnerKind: checkoutLayerOwnerKind, GenerationKind: CommitLayerGenerationKind},
		{OwnerKind: checkoutLayerOwnerKind, GenerationKind: dedicatedBaseGenerationKind},
	}
	for _, identity := range beneathTheWorkingCopy {
		row, declared := textSearchProducer(identity)
		if declared {
			t.Errorf("%s/%s declares %q for %s; a layer beneath the working-tree layer "+
				"narrows the whole stack and must stay silent",
				identity.OwnerKind, identity.GenerationKind, row.State, graphview.CapSearchText)
		}
	}
}
