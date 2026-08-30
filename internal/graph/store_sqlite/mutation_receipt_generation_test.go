package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const (
	receiptGenerationRepo = "receipt-generation"
	receiptGenerationFile = "receipt-generation/shared.go"
	receiptGenerationID   = "receipt-generation/shared.go::Shared"
)

func addReceiptGenerationNode(s *Store, name string) {
	s.AddNode(&graph.Node{
		ID:         receiptGenerationID,
		Kind:       graph.KindFunction,
		Name:       name,
		QualName:   receiptGenerationRepo + "." + name,
		FilePath:   receiptGenerationFile,
		RepoPrefix: receiptGenerationRepo,
		Language:   "go",
	})
}

func receiptContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestSQLiteMutationReceiptsAreGenerationScoped(t *testing.T) {
	base, err := Open(filepath.Join(t.TempDir(), "receipts.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := base.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	overlay := base.AtGeneration(7)
	if overlay == nil {
		t.Fatal("AtGeneration(7) returned nil")
	}
	addReceiptGenerationNode(base, "BaseShared")
	addReceiptGenerationNode(overlay, "OverlayShared")

	baseToken := base.BeginMutationReceipt()
	overlayToken := overlay.BeginMutationReceipt()
	nodes, _ := overlay.EvictFile(receiptGenerationFile)
	if nodes != 1 {
		t.Fatalf("overlay EvictFile removed %d nodes, want 1", nodes)
	}

	overlayReceipt := overlay.EndMutationReceipt(overlayToken)
	if !overlayReceipt.Complete || !overlayReceipt.ResolutionRelevant {
		t.Fatalf("overlay receipt = %+v, want a complete resolution-relevant delta", overlayReceipt)
	}
	if !receiptContains(overlayReceipt.TargetNames, "OverlayShared") ||
		receiptContains(overlayReceipt.TargetNames, "BaseShared") {
		t.Fatalf("overlay target names = %v, want OverlayShared without BaseShared", overlayReceipt.TargetNames)
	}
	baseReceipt := base.EndMutationReceipt(baseToken)
	if !baseReceipt.Complete || baseReceipt.ResolutionRelevant || len(baseReceipt.ResolutionFiles()) != 0 {
		t.Fatalf("base receipt observed an overlay-only mutation: %+v", baseReceipt)
	}
	if got := base.GetNode(receiptGenerationID); got == nil || got.Name != "BaseShared" {
		t.Fatalf("overlay eviction disturbed the base row: %+v", got)
	}
}

func TestSQLiteMutationReceiptRejectsWrongGenerationEnd(t *testing.T) {
	base, err := Open(filepath.Join(t.TempDir(), "receipt-owner.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = base.Close() }()
	overlay := base.AtGeneration(3)
	token := overlay.BeginMutationReceipt()

	wrong := base.EndMutationReceipt(token)
	if wrong.Complete || wrong.IncompleteReason != "receipt_generation_mismatch" {
		t.Fatalf("wrong-generation EndMutationReceipt = %+v", wrong)
	}
	if right := overlay.EndMutationReceipt(token); !right.Complete {
		t.Fatalf("wrong-generation close consumed the rightful token: %+v", right)
	}
}

func TestSQLiteAllGenerationEvictionInvalidatesEveryReceipt(t *testing.T) {
	base, err := Open(filepath.Join(t.TempDir(), "receipt-admin.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = base.Close() }()
	overlay := base.AtGeneration(9)
	addReceiptGenerationNode(base, "BaseShared")
	addReceiptGenerationNode(overlay, "OverlayShared")

	baseToken := base.BeginMutationReceipt()
	overlayToken := overlay.BeginMutationReceipt()
	nodes, _ := base.EvictRepoAllGenerations(receiptGenerationRepo)
	if nodes != 2 {
		t.Fatalf("all-generation eviction removed %d nodes, want 2", nodes)
	}
	for name, receipt := range map[string]graph.MutationReceipt{
		"base":    base.EndMutationReceipt(baseToken),
		"overlay": overlay.EndMutationReceipt(overlayToken),
	} {
		if receipt.Complete {
			t.Fatalf("%s receipt remained complete across an all-generation mutation: %+v", name, receipt)
		}
	}
}

func BenchmarkSQLiteGenerationScopedExactEvictionReceipt(b *testing.B) {
	base, err := Open(":memory:")
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	b.Cleanup(func() { _ = base.Close() })
	overlay := base.AtGeneration(11)
	addReceiptGenerationNode(base, "BaseShared")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		addReceiptGenerationNode(overlay, "OverlayShared")
		token := overlay.BeginMutationReceipt()
		overlay.EvictFile(receiptGenerationFile)
		if receipt := overlay.EndMutationReceipt(token); !receipt.Complete {
			b.Fatalf("exact overlay eviction returned incomplete receipt: %+v", receipt)
		}
	}
	b.StopTimer()
	if got := base.GetNode(receiptGenerationID); got == nil || got.Name != "BaseShared" {
		b.Fatalf("benchmark disturbed base row: %+v", got)
	}
}
