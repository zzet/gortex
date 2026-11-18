package store_sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestContractBaselineAcceptanceRequiresExactPendingState(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	state := attachmentState("baseline-input")
	state.CheckoutID = ""
	state.Accepted = false
	if err := s.BeginContractInputMutationContext(ctx, nil, state, nil); err != nil {
		t.Fatal(err)
	}
	baseline := graph.ContractBoundaryReceiptBaseline{RepoPrefix: "repo", Version: "contract-boundary-v1", Fingerprint: "complete-census"}
	stale := state
	stale.InputFingerprint = "superseded"
	if err := s.AcceptContractInputMutationWithBaselineContext(ctx, stale, baseline); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale acceptance=%v", err)
	}
	if got, err := s.ContractBoundaryReceiptBaselineContext(ctx, "repo", ""); err != nil || got != nil {
		t.Fatalf("failed CAS installed baseline=%#v err=%v", got, err)
	}
	if err := s.AcceptContractInputMutationWithBaselineContext(ctx, state, baseline); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.ContractInputStateContext(ctx, "repo", "")
	if err != nil || !found || !got.Accepted || got.InputFingerprint != state.InputFingerprint {
		t.Fatalf("accepted state=%#v found=%v err=%v", got, found, err)
	}
	stored, err := s.ContractBoundaryReceiptBaselineContext(ctx, "repo", "")
	if err != nil || stored == nil || *stored != baseline {
		t.Fatalf("baseline=%#v err=%v", stored, err)
	}
	if err := s.AcceptContractInputMutationWithBaselineContext(ctx, state, baseline); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("already accepted CAS=%v", err)
	}
}
