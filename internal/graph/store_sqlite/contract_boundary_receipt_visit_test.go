package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

func TestContractBoundaryReceiptVisitDenseRangeAndSparseCorruption(t *testing.T) {
	s := openCatalogStore(t)
	ctx := t.Context()
	var paths []string
	for i := 0; i < 2*contractBoundaryReceiptVisitChunkSize+4; i++ {
		row := boundaryStorageReceipt(fmt.Sprintf("%04d.go", i), fmt.Sprint(i))
		if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row}); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, row.FilePath)
	}
	// An unrequested corrupt row inside a wide interval must never be decoded.
	bad := paths[contractBoundaryReceiptVisitChunkSize+1]
	if _, err := s.writerDB.ExecContext(ctx, `UPDATE generation_contract_boundary_receipt SET receipt=? WHERE view_gen=0 AND repo_prefix='repo' AND file_path=?`, []byte(`{"malformed":true}`), bad); err != nil {
		t.Fatal(err)
	}
	for name, requested := range map[string][]string{
		"full_dense_chunk":        paths[:contractBoundaryReceiptVisitChunkSize],
		"sparse_over_chunk_limit": {paths[0], paths[len(paths)-1]},
		"one_plus_missing":        {paths[0], "repo/missing.go"},
		"all_missing":             {"repo/missing-a.go", "repo/missing-b.go"},
		"repeated":                {paths[0], paths[0], paths[1]},
	} {
		t.Run(name, func(t *testing.T) {
			want := map[string]graph.ContractBoundaryReceipt{}
			for _, path := range requested {
				row, _, err := s.ContractBoundaryReceiptContext(ctx, "repo", "", path)
				if err != nil {
					t.Fatal(err)
				}
				if row != nil {
					want[path] = *row
				}
			}
			got := map[string]graph.ContractBoundaryReceipt{}
			if err := s.VisitContractBoundaryReceiptsContext(ctx, "repo", "", requested, func(row graph.ContractBoundaryReceipt) error {
				if _, duplicate := got[row.FilePath]; duplicate {
					t.Fatal("duplicate")
				}
				got[row.FilePath] = row
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("complete physical receipt parity lost")
			}
		})
	}
	// The dense branch still uses the full decoder and preserves corruption errors.
	err := s.VisitContractBoundaryReceiptsContext(ctx, "repo", "", paths[contractBoundaryReceiptVisitChunkSize:contractBoundaryReceiptVisitChunkSize+3], func(row graph.ContractBoundaryReceipt) error {
		if row.FilePath == bad {
			t.Fatal("corrupt row reached callback")
		}
		return nil
	})
	if err == nil {
		t.Fatal("requested corrupt receipt was silently accepted")
	}
}

func TestContractBoundaryReceiptVisitPlansRangeAndPointFallback(t *testing.T) {
	s := openCatalogStore(t)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+contractBoundaryReceiptVisitQuery(2), s.viewGen, "repo", "", "repo/a.go", "repo/b.go")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	for _, needed := range []string{
		"SEARCH r USING PRIMARY KEY (view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path>? AND file_path<?)",
		"SEARCH r USING PRIMARY KEY (view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path=?)",
		"SEARCH s USING COVERING INDEX contract_boundary_receipt_stamps",
	} {
		if !strings.Contains(joined, needed) {
			t.Fatalf("missing bounded access %q: %s", needed, joined)
		}
	}
	for _, detail := range plan {
		if detail == "SCAN r" || detail == "SCAN s" {
			t.Fatalf("unbounded receipt scan: %s", joined)
		}
	}
}
