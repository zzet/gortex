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
// every layer in between says nothing at all, because a narrowing there would
// be worst-cased over the whole stack and would refuse the live routed search
// the checkout can answer exactly.
//
// The reader is the other half, and it is what makes the silence a claim rather
// than an omission: for CapSearchText alone it does NOT worst-case — the top
// layer of a view's stack decides, and silence at the top is read as a denial
// (graphview/materialize.go, Materializer.completeness). So a silent layer is
// harmless under a working-tree layer that claims the capability, and decisive
// when it IS the top.

// The identity a dedicated graph's own base generation is minted under. Both
// halves are spelled as literals by the builders that write it
// (builder_dedicated_claimed.go:90, builder_dedicated_delta.go:98), and spelled
// the same way here so a rename that misses one of them shows up as a failure
// rather than as a silently different classification.
const (
	dedicatedBaseGenerationKind = "dedicated"
	dedicatedBaseOwnerKind      = "dedicated_graph"
)

// TestBuilderIdentityLiteralsMatchTheBuilders keeps the fixtures above honest
// about the identity production actually mints. checkoutLayerOwnerKind is not
// a second owner kind beside the dedicated graph's — it IS "dedicated_graph"
// (checkout_coordinator.go:81) — so a test that spells a dedicated base with it
// is exercising the production identity and not a lookalike. If that ever stops
// being true, every classification test in this file is reading a shape no
// builder writes, and this is where it says so.
func TestBuilderIdentityLiteralsMatchTheBuilders(t *testing.T) {
	if checkoutLayerOwnerKind != dedicatedBaseOwnerKind {
		t.Fatalf("checkoutLayerOwnerKind = %q, want %q: the fixtures below spell a "+
			"dedicated base with checkoutLayerOwnerKind and would stop being the "+
			"identity builder_dedicated_claimed.go mints",
			checkoutLayerOwnerKind, dedicatedBaseOwnerKind)
	}
	if refViewOwnerKind == dedicatedBaseOwnerKind {
		t.Fatalf("refViewOwnerKind = %q collides with the dedicated graph's owner kind; "+
			"textSearchProducer classifies by exactly that difference", refViewOwnerKind)
	}
	if DirtyLayerGenerationKind == dedicatedBaseGenerationKind ||
		CommitLayerGenerationKind == dedicatedBaseGenerationKind {
		t.Fatalf("the generation kinds collide: dirty=%q commit=%q dedicated=%q",
			DirtyLayerGenerationKind, CommitLayerGenerationKind, dedicatedBaseGenerationKind)
	}
}

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
				OwnerKind:      dedicatedBaseOwnerKind,
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
	// Every row carries force, in one of the two directions. A committed layer
	// of a checkout's own stack must stay silent AND must not classify itself
	// as serving — silence is only safe while the predicate behind it agrees,
	// because the reader turns silence at the top of a stack into a denial. A
	// ref view must do the opposite: it names a tree no checkout holds, so it
	// has to WITHDRAW the capability explicitly rather than fall through to the
	// silent default.
	//
	// Guarding every assertion on `declared`, as this test first did, left the
	// three silent rows checking nothing at all and the two ref rows checking
	// nothing if the withdrawal disappeared.
	cases := []struct {
		identity     GenerationIdentity
		wantWithdraw bool
	}{
		{GenerationIdentity{OwnerKind: dedicatedBaseOwnerKind, GenerationKind: CommitLayerGenerationKind}, false},
		{GenerationIdentity{OwnerKind: dedicatedBaseOwnerKind, GenerationKind: dedicatedBaseGenerationKind}, false},
		{GenerationIdentity{OwnerKind: dedicatedBaseOwnerKind, GenerationKind: "something-new"}, false},
		{GenerationIdentity{OwnerKind: refViewOwnerKind, GenerationKind: CommitLayerGenerationKind}, true},
		{GenerationIdentity{OwnerKind: refViewOwnerKind, GenerationKind: DirtyLayerGenerationKind}, true},
	}
	for _, tc := range cases {
		identity := tc.identity
		row, declared := textSearchProducer(identity)
		if !tc.wantWithdraw {
			if declared {
				t.Errorf("%s/%s declared %+v; a layer beneath a working copy stays silent",
					identity.OwnerKind, identity.GenerationKind, row)
			}
			if servesTextSearch(identity) {
				t.Errorf("%s/%s declares nothing yet classifies itself as serving text "+
					"search; the reader reads silence at the top of a stack as a denial, "+
					"so a layer that serves the capability has to say so",
					identity.OwnerKind, identity.GenerationKind)
			}
			continue
		}
		if !declared {
			t.Errorf("%s/%s declared nothing; a tree no checkout holds has to withdraw "+
				"%s rather than fall through to the silent default",
				identity.OwnerKind, identity.GenerationKind, graphview.CapSearchText)
			continue
		}
		if row.Producer != string(graphview.CapSearchText) {
			t.Errorf("%s/%s declared producer %q, want %q",
				identity.OwnerKind, identity.GenerationKind, row.Producer, graphview.CapSearchText)
		}
		if row.State == store_sqlite.ProducerStateComplete {
			t.Errorf("%s/%s claims complete text search",
				identity.OwnerKind, identity.GenerationKind)
		}
		if row.Reason == "" {
			t.Errorf("%s/%s narrows the capability without saying why",
				identity.OwnerKind, identity.GenerationKind)
		}
		if servesTextSearch(identity) {
			t.Errorf("%s/%s withdraws the capability and still classifies itself as serving it",
				identity.OwnerKind, identity.GenerationKind)
		}
	}
}

// TestCommittedCheckoutLayerNarrowsNothing is the other half, and it is the one
// that keeps a live routed search answerable.
//
// A checkout view stacks the commit layer, the working-tree layer and the whole
// ancestry beneath them. For CapSearchText the reader reads the top layer alone
// — so a commit layer under a working-tree layer is not consulted, and it must
// not become consulted by acquiring a declaration: the moment it declares
// anything, a stack whose commit layer is the top (a withdrawn working-tree
// slot, a dedicated base with nothing over it) inherits that declaration
// instead of the denial its silence earns. Any state at all on one of these
// identities is a regression.
func TestCommittedCheckoutLayerNarrowsNothing(t *testing.T) {
	beneathTheWorkingCopy := []GenerationIdentity{
		{OwnerKind: dedicatedBaseOwnerKind, GenerationKind: CommitLayerGenerationKind},
		{OwnerKind: dedicatedBaseOwnerKind, GenerationKind: dedicatedBaseGenerationKind},
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
