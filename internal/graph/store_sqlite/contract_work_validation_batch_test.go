package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestCapturedWorkBatchPreservesFirstPhysicalOccurrence(t *testing.T) {
	cases := []struct {
		name         string
		upper, lower bool
		mutate       func(*graph.ContractWork)
		stale        bool
	}{
		{name: "missing upper reaches lower", lower: true},
		{name: "valid upper shadows stale lower", upper: true, lower: true},
		{name: "missing everywhere", stale: true},
	}
	for _, field := range []string{"origin", "checkout", "repo", "file", "version", "fingerprint", "state", "scope"} {
		cases = append(cases, struct {
			name         string
			upper, lower bool
			mutate       func(*graph.ContractWork)
			stale        bool
		}{
			name: "invalid upper shadows valid lower " + field, upper: true, lower: true, stale: true,
			mutate: func(w *graph.ContractWork) {
				switch field {
				case "origin":
					w.OriginGeneration++
				case "checkout":
					w.CheckoutID += "-other"
				case "repo":
					w.RepoPrefix += "-other"
				case "file":
					w.FilePath += "-other"
				case "version":
					w.InputVersion += "-other"
				case "fingerprint":
					w.InputFingerprint += "-other"
				case "state":
					w.State = graph.ContractWorkComplete
				case "scope":
					w.Scope.Unknown = true
				}
			},
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openCatalogStore(t)
			ctx := context.Background()
			upper, lower := reservedGeneration(t, store, "upper"), reservedGeneration(t, store, "lower")
			expected := contractWorkFixture("token")
			expected.OriginGeneration = lower
			if tc.lower {
				row := expected
				if tc.upper && tc.mutate == nil {
					row.InputFingerprint = "stale lower"
				}
				if err := store.AtGeneration(lower).SetContractWork(ctx, []graph.ContractWork{row}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.upper {
				row := expected
				if tc.mutate != nil {
					tc.mutate(&row)
				}
				if err := store.AtGeneration(upper).SetContractWork(ctx, []graph.ContractWork{row}); err != nil {
					t.Fatal(err)
				}
			}
			encoded, err := encodeContractWork([]graph.ContractWork{expected})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := store.writerDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			err = validateCapturedContractWorkBatchTx(ctx, tx, []int64{upper, lower}, []graph.ContractWork{expected}, encoded, 20)
			if tc.stale {
				if !errors.Is(err, ErrCatalogStaleGuard) {
					t.Fatalf("got %v, want stale guard", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCapturedWorkBatchKeepsExactTokensAcrossBoundedPages(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	upper, lower := reservedGeneration(t, store, "upper"), reservedGeneration(t, store, "lower")
	var expected, top, bottom []graph.ContractWork
	for i := 0; i < 263; i++ {
		w := contractWorkFixture(fmt.Sprintf("token-%03d", i))
		w.OriginGeneration = lower
		w.InputFingerprint = fmt.Sprintf("fingerprint-%d", i)
		expected = append(expected, w)
		if i%2 == 0 {
			top = append(top, w)
		} else {
			bottom = append(bottom, w)
		}
	}
	// These are distinct SQLite text keys. A JSON-normalized token transport
	// would risk conflating invalid UTF-8 with its replacement character.
	for _, token := range []string{"quote'\"", "nul\x00tail", "raw\xff", "raw\ufffd"} {
		w := contractWorkFixture(token)
		w.OriginGeneration = lower
		expected = append(expected, w)
		bottom = append(bottom, w)
	}
	for _, seed := range []struct {
		id   int64
		work []graph.ContractWork
	}{{upper, top}, {lower, bottom}} {
		if err := store.AtGeneration(seed.id).SetContractWork(ctx, seed.work); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := encodeContractWork(expected)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.writerDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, limit := range []int{20, sqliteFallbackVariableLimit} {
		if err := validateCapturedContractWorkBatchTx(ctx, tx, []int64{upper, lower}, expected, encoded, limit); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	// Existing admission must still reject conflicting duplicate expected tokens.
	duplicate := append([]graph.ContractWork(nil), expected...)
	conflict := expected[0]
	conflict.InputFingerprint = "conflicting"
	duplicate = append(duplicate, conflict)
	if _, err := encodeContractWork(duplicate); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("duplicate admission = %v", err)
	}
}

func TestCapturedWorkBatchPropagatesCancellationAndQueryFailure(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	work := []graph.ContractWork{contractWorkFixture("token")}
	encoded, err := encodeContractWork(work)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.writerDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := validateCapturedContractWorkBatchTx(canceled, tx, []int64{0}, work, encoded, 20); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := validateCapturedContractWorkBatchTx(ctx, tx, []int64{0}, work, encoded, 20); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("closed transaction = %v", err)
	}
}
