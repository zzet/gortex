package graphview

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestRegisteredCheckoutScopedLookupMatchesCatalogOracle(t *testing.T) {
	store := openStackStore(t, "registered-binding")
	seedStackControlPlane(t, store)
	base := t.TempDir()
	outer := filepath.Join(base, "Caf\u00e9")
	inner := filepath.Join(outer, "nested")
	seedBindingCheckout(t, store, "outer", outer, store_sqlite.CheckoutModeDedicated, store_sqlite.CheckoutStateReady)
	seedBindingCheckout(t, store, "inner", inner, store_sqlite.CheckoutModeAutomatic, store_sqlite.CheckoutStateUnavailable)
	for i := range 16 {
		id := fmt.Sprintf("unrelated-%02d", i)
		seedBindingCheckout(t, store, id, filepath.Join(base, id), store_sqlite.CheckoutModeDedicated, store_sqlite.CheckoutStateReady)
	}
	before := pathkey.CaseInsensitivePaths
	t.Cleanup(func() { pathkey.CaseInsensitivePaths = before })
	for _, folded := range []bool{false, true} {
		pathkey.CaseInsensitivePaths = folded
		for _, path := range []string{outer, inner, filepath.Join(inner, "pkg", "missing.go"), outer + "-sibling", filepath.Join(base, "Cafe\u0301", "nested", "file.go")} {
			want, wantFound, wantErr := CheckoutForPath(t.Context(), store.Catalog(), []string{testFamilyID}, path)
			got, found, err := RegisteredCheckoutForPath(t.Context(), store.Catalog(), path)
			if err != nil || wantErr != nil || found != wantFound || got != want {
				t.Fatalf("folded=%v path=%q: got=%+v/%v/%v want=%+v/%v/%v", folded, path, got, found, err, want, wantFound, wantErr)
			}
		}
	}
}

func TestRegisteredCheckoutScopedLookupPreservesCanonicalAliasPrecedence(t *testing.T) {
	store := openStackStore(t, "registered-alias-binding")
	seedStackControlPlane(t, store)
	base := t.TempDir()
	real := filepath.Join(base, "real")
	inner := filepath.Join(real, "outer", "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "stored-alias")
	queryAlias := filepath.Join(base, "query-alias")
	for _, path := range []string{alias, queryAlias} {
		if err := os.Symlink(real, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	seedBindingCheckout(t, store, "canonical-outer", filepath.Join(real, "outer"), store_sqlite.CheckoutModeDedicated, store_sqlite.CheckoutStateReady)
	seedBindingCheckout(t, store, "aliased-inner", filepath.Join(alias, "outer", "inner"), store_sqlite.CheckoutModeAutomatic, store_sqlite.CheckoutStateReady)
	for _, path := range []string{filepath.Join(queryAlias, "outer", "inner", "file.go"), filepath.Join(real, "outer", "inner", "file.go")} {
		want, wantFound, wantErr := CheckoutForPath(t.Context(), store.Catalog(), []string{testFamilyID}, path)
		got, found, err := RegisteredCheckoutForPath(t.Context(), store.Catalog(), path)
		if err != nil || wantErr != nil || found != wantFound || got != want {
			t.Fatalf("path=%q: got=%+v/%v/%v want=%+v/%v/%v", path, got, found, err, want, wantFound, wantErr)
		}
	}
}
