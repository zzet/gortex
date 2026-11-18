package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

func seedRootLookupCheckout(t *testing.T, store *Store, family, id, root string) {
	t.Helper()
	seedFamilyAndCheckout(t, store.Catalog(), family, id, "inc-"+id)
	row, found, err := store.Catalog().GetCheckout(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("get seeded checkout: found=%v error=%v", found, err)
	}
	row.RootPath, row.GitDir = root, filepath.Join(root, ".git")
	if err := store.Catalog().UpsertCheckout(t.Context(), row); err != nil {
		t.Fatal(err)
	}
}

func TestCheckoutRootLookupNeverFetchesUnrelatedFamilies(t *testing.T) {
	s := openCatalogStore(t)
	root := filepath.Join(t.TempDir(), "repo")
	seedRootLookupCheckout(t, s, "focus-family", "focus", root)
	for pass := range 2 {
		got, err := s.Catalog().CheckoutsByRootPaths(t.Context(), []string{filepath.Join(root, "pkg"), root})
		if err != nil || len(got) != 1 || got[0].CheckoutID != "focus" {
			t.Fatalf("pass %d: rows=%v error=%v", pass, got, err)
		}
		if pass == 0 {
			for i := range 32 {
				id := fmt.Sprintf("unrelated-%02d", i)
				seedRootLookupCheckout(t, s, "family-"+id, id, filepath.Join(t.TempDir(), id))
			}
		}
	}
}

func TestCheckoutRootLookupFixedCollationsAndConnections(t *testing.T) {
	s := openCatalogStore(t)
	root := filepath.Join(t.TempDir(), "Caf\u00e9")
	seedRootLookupCheckout(t, s, "unicode-family", "unicode", root)
	before := pathkey.CaseInsensitivePaths
	t.Cleanup(func() { pathkey.CaseInsensitivePaths = before })
	for _, folded := range []bool{false, true, false} {
		pathkey.CaseInsensitivePaths = folded
		// NFC equivalence applies in both modes; case equivalence only in
		// the fixed folded index, even after changing the host policy.
		for _, spelling := range []string{strings.ReplaceAll(root, "\u00e9", "e\u0301"), strings.ToLower(root)} {
			got, err := s.Catalog().CheckoutsByRootPaths(t.Context(), []string{spelling})
			want := 1
			if spelling == strings.ToLower(root) && !folded {
				want = 0
			}
			if err != nil || len(got) != want {
				t.Fatalf("folded=%v spelling=%q: rows=%d want=%d error=%v", folded, spelling, len(got), want, err)
			}
		}
	}
	// Hold one physical connection so the second assertion opens another.
	s.db.SetMaxOpenConns(2)
	first, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, conn := range []*sql.Conn{first, second} {
		var equal bool
		if err := conn.QueryRowContext(t.Context(), "SELECT ? COLLATE gortex_path_folded = ?", root, strings.ToLower(root)).Scan(&equal); err != nil || !equal {
			t.Fatalf("physical connection collation: equal=%v error=%v", equal, err)
		}
	}
}

func TestCheckoutRootLookupIndexPlanAndCancellation(t *testing.T) {
	s := openCatalogStore(t)
	for _, folded := range []bool{false, true} {
		rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+checkoutRootsQuery(2, folded), "/a", "/a/b")
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
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "SEARCH checkouts USING INDEX checkouts_by_root_") || strings.Contains(joined, "SCAN checkouts") {
			t.Fatalf("root lookup is not an indexed seek: %s", joined)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Catalog().CheckoutsByRootPaths(ctx, []string{"/a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup error=%v", err)
	}
}

func TestCheckoutRootLookupMigrationPreservesAnalysisAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root-lookup.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddBatchChecked([]*graph.Node{{ID: "keep-node", Kind: graph.KindFunction}}, nil); err != nil {
		t.Fatal(err)
	}
	active := buildMinimalAnalysisGeneration(t, s, "keep", 0, true)
	seedRootLookupCheckout(t, s, "kept-family", "kept", "/kept")
	if _, err := s.writerDB.Exec("DROP INDEX checkouts_by_root_exact; DROP INDEX checkouts_by_root_folded; PRAGMA user_version=43"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var got int64
		if err := s.db.QueryRow("SELECT generation_id FROM analysis_active_generation WHERE view_gen=0 AND slot=1").Scan(&got); err != nil || got != active {
			t.Fatalf("active analysis changed: got=%d want=%d error=%v", got, active, err)
		}
		rows, err := s.Catalog().CheckoutsByRootPaths(t.Context(), []string{"/kept"})
		if err != nil || len(rows) != 1 || rows[0].CheckoutID != "kept" {
			t.Fatalf("reopened lookup=%v error=%v", rows, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
