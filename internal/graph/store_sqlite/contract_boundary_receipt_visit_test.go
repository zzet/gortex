package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestContractBoundaryReceiptVisitKeepsFullPhysicalRows(t *testing.T) {
	s := openCatalogStore(t)
	ctx := t.Context()
	var paths []string
	for i := 0; i < contractBoundaryReceiptVisitChunkSize+3; i++ {
		row := boundaryStorageReceipt(fmt.Sprintf("%04d.go", i), fmt.Sprint(i))
		if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row}); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row}); err != nil {
				t.Fatal(err)
			}
		}
		paths = append(paths, row.FilePath)
	}
	// Duplicate requests and sparse misses do not duplicate or invent records.
	paths = append(paths, paths[0], "repo/missing.go")
	want := map[string]graph.ContractBoundaryReceipt{}
	for _, path := range paths {
		row, _, err := s.ContractBoundaryReceiptContext(ctx, "repo", "", path)
		if err != nil {
			t.Fatal(err)
		}
		if row != nil {
			want[path] = *row
		}
	}
	got := map[string]graph.ContractBoundaryReceipt{}
	if err := s.VisitContractBoundaryReceiptsContext(ctx, "repo", "", paths, func(row graph.ContractBoundaryReceipt) error {
		if _, duplicate := got[row.FilePath]; duplicate {
			t.Fatal("duplicate physical row")
		}
		got[row.FilePath] = row
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("complete receipt metadata, pending state or membership differs")
	}
	for _, scope := range [][2]string{{"other", ""}, {"repo", "other"}} {
		if err := s.VisitContractBoundaryReceiptsContext(ctx, scope[0], scope[1], paths, func(graph.ContractBoundaryReceipt) error {
			t.Fatal("different namespace leaked")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContractBoundaryReceiptVisitClosesCursorAndRejectsCorruption(t *testing.T) {
	s := openCatalogStore(t)
	s.db.SetMaxOpenConns(1)
	row := boundaryStorageReceipt("owner.go", "accepted")
	if err := s.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("visitor stopped")
	if err := s.VisitContractBoundaryReceiptsContext(t.Context(), "repo", "", []string{row.FilePath}, func(graph.ContractBoundaryReceipt) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error lost: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := s.ContractBoundaryReceiptContext(ctx, "repo", "", row.FilePath); err != nil {
		t.Fatalf("visitor retained single connection: %v", err)
	}
	if _, err := s.writerDB.ExecContext(ctx, `UPDATE generation_contract_boundary_receipt SET receipt=? WHERE view_gen=0 AND repo_prefix='repo' AND file_path=?`, []byte(`{"malformed":true}`), row.FilePath); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := s.VisitContractBoundaryReceiptsContext(ctx, "repo", "", []string{row.FilePath}, func(graph.ContractBoundaryReceipt) error { called = true; return nil }); err == nil || called {
		t.Fatalf("corrupt row reached callback: %v %v", called, err)
	}
	canceled, stopContext := context.WithCancel(ctx)
	stopContext()
	if err := s.VisitContractBoundaryReceiptsContext(canceled, "repo", "", nil, func(graph.ContractBoundaryReceipt) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
