package store_sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestContractBoundaryReceiptsForPathsExactPendingMissingAndCancel(t *testing.T) {
	s := openCatalogStore(t)
	row := boundaryStorageReceipt("provider.go", "old")
	if err := s.SetContractBoundaryReceiptsContext(context.Background(), []graph.ContractBoundaryReceipt{row}); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptContractBoundaryReceiptsContext(context.Background(), []graph.ContractBoundaryReceipt{row}); err != nil {
		t.Fatal(err)
	}
	next := row
	next.Fingerprint = "new"
	next.SourceFingerprint = "source-new"
	if err := s.SetContractBoundaryReceiptsContext(context.Background(), []graph.ContractBoundaryReceipt{next}); err != nil {
		t.Fatal(err)
	}
	paths := []string{row.FilePath, "repo/missing.go", row.FilePath}
	got, err := s.ContractBoundaryReceiptsForPathsContext(context.Background(), "repo", "", paths)
	if err != nil || len(got) != 2 || got[row.FilePath] == nil || got[row.FilePath].Accepted || got[row.FilePath].Previous == nil || got[row.FilePath].Previous.Fingerprint != "old" || got["repo/missing.go"] != nil {
		t.Fatalf("pending exactrows=%#v err=%v", got, err)
	}
	got, err = s.AtGeneration(7).ContractBoundaryReceiptsForPathsContext(context.Background(), "repo", "", paths)
	if err != nil || len(got) != 2 || got[row.FilePath] != nil {
		t.Fatalf("generationabsence=%#v err=%v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = s.ContractBoundaryReceiptsForPathsContext(ctx, "repo", "", paths)
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("cancel=%#v err=%v", got, err)
	}
	got, err = s.ContractBoundaryReceiptsForPathsContext(context.Background(), "repo", "", make([]string, 65))
	if !errors.Is(err, ErrContractBoundaryReceiptLimit) || got != nil {
		t.Fatalf("limit=%#v err=%v", got, err)
	}
}
