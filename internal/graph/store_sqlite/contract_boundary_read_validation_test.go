package store_sqlite

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestContractBoundaryReadValidationRetainsDecodedRows(t *testing.T) {
	s := openCatalogStore(t)
	row := boundaryStorageReceipt("read.go", "pending")
	if err := s.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}); err != nil {
		t.Fatal(err)
	}
	// The read validates rather than canonicalizes an already decoded row.
	// Preserve duplicate/order-sensitive physical data and the full predecessor.
	row.LookupKeys = []string{"z", "a", "z"}
	row.ProducedKeys = []string{"y", "b", "y"}
	prior := row
	prior.Accepted, prior.Fingerprint = true, "accepted"
	row.Previous = &prior
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.writerDB.ExecContext(t.Context(), `UPDATE generation_contract_boundary_receipt SET receipt=? WHERE view_gen=0 AND repo_prefix='repo' AND file_path=?`, encoded, row.FilePath); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ContractBoundaryReceiptContext(t.Context(), "repo", "", row.FilePath)
	if err != nil || !reflect.DeepEqual(got, &row) {
		t.Fatalf("physical receipt changed: got=%#v err=%v", got, err)
	}
	if err := s.VisitContractBoundaryReceiptsContext(t.Context(), "repo", "", []string{row.FilePath}, func(got graph.ContractBoundaryReceipt) error {
		if !reflect.DeepEqual(got, row) {
			t.Fatalf("streamed physical receipt changed: %#v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestContractBoundaryReadValidationRejectsInvalidCurrentAndPrevious(t *testing.T) {
	for _, previous := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			edit func(*graph.ContractBoundaryReceipt)
			want error
		}{
			{"missing_identity", func(r *graph.ContractBoundaryReceipt) { r.SourceFingerprint = "" }, ErrCatalogInvalidValue},
			{"payload_limit", func(r *graph.ContractBoundaryReceipt) { r.Payload = make([]byte, contractBoundaryPayloadLimit+1) }, ErrContractBoundaryReceiptLimit},
			{"key_limit", func(r *graph.ContractBoundaryReceipt) { r.LookupKeys = make([]string, contractBoundaryKeyLimit+1) }, ErrContractBoundaryReceiptLimit},
			{"empty_key", func(r *graph.ContractBoundaryReceipt) { r.ProducedKeys = []string{"valid", ""} }, ErrCatalogInvalidValue},
			{"long_key", func(r *graph.ContractBoundaryReceipt) { r.LookupKeys = []string{strings.Repeat("k", 8193)} }, ErrCatalogInvalidValue},
		} {
			name := "current/"
			if previous {
				name = "previous/"
			}
			t.Run(name+tc.name, func(t *testing.T) {
				s := openCatalogStore(t)
				row := boundaryStorageReceipt("invalid.go", "pending")
				if err := s.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}); err != nil {
					t.Fatal(err)
				}
				if previous {
					prior := row
					prior.Accepted = true
					tc.edit(&prior)
					row.Previous = &prior
				} else {
					tc.edit(&row)
				}
				encoded, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.writerDB.ExecContext(t.Context(), `UPDATE generation_contract_boundary_receipt SET receipt=? WHERE view_gen=0 AND repo_prefix='repo' AND file_path='repo/invalid.go'`, encoded); err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.ContractBoundaryReceiptContext(t.Context(), "repo", "", "repo/invalid.go"); !errors.Is(err, tc.want) {
					t.Fatalf("point read accepted invalid receipt: %v", err)
				}
				if err := s.VisitContractBoundaryReceiptsContext(t.Context(), "repo", "", []string{"repo/invalid.go"}, func(graph.ContractBoundaryReceipt) error {
					t.Fatal("invalid receipt reached callback")
					return nil
				}); !errors.Is(err, tc.want) {
					t.Fatalf("streamed read accepted invalid receipt: %v", err)
				}
			})
		}
	}
}
