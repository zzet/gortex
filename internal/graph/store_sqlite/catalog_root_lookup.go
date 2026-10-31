package store_sqlite

import (
	"context"
	"database/sql"
	"strings"

	"modernc.org/sqlite"

	"github.com/zzet/gortex/internal/pathkey"
)

const checkoutRootIndexesSQL = `
CREATE INDEX IF NOT EXISTS checkouts_by_root_exact
    ON checkouts(root_path COLLATE gortex_path_exact);
CREATE INDEX IF NOT EXISTS checkouts_by_root_folded
    ON checkouts(root_path COLLATE gortex_path_folded);
`

func init() {
	// Both orders are immutable and filesystem-free. SQLite's NOCASE only
	// folds ASCII; request binding also preserves NFC and volume identity.
	sqlite.MustRegisterCollationUtf8("gortex_path_exact", func(a, b string) int {
		return strings.Compare(pathkey.IdentityKeyForCase(a, false), pathkey.IdentityKeyForCase(b, false))
	})
	sqlite.MustRegisterCollationUtf8("gortex_path_folded", func(a, b string) int {
		return strings.Compare(pathkey.IdentityKeyForCase(a, true), pathkey.IdentityKeyForCase(b, true))
	})
}

func createCheckoutRootIndexes(tx *sql.Tx) error {
	_, err := tx.Exec(checkoutRootIndexesSQL)
	return err
}

func checkoutRootsQuery(count int, folded bool) string {
	index, collation := "checkouts_by_root_exact", "gortex_path_exact"
	if folded {
		index, collation = "checkouts_by_root_folded", "gortex_path_folded"
	}
	return `SELECT checkout_id, ` + checkoutColumns + `
FROM checkouts INDEXED BY ` + index + `
WHERE root_path COLLATE ` + collation + ` IN (` + inPlaceholders(count) + `)
ORDER BY family_id, admin_name, incarnation`
}

// CheckoutsByRootPaths returns only checkouts whose lexical root matches one
// of roots under the host's path identity rules. Each ancestor is an indexed
// seek; unrelated families and checkouts are never fetched. The order matches
// ListRepositoryFamilies followed by ListCheckouts, including reincarnations.
func (c *Catalog) CheckoutsByRootPaths(ctx context.Context, roots []string) ([]Checkout, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	args := make([]any, len(roots))
	for i, root := range roots {
		args[i] = root
	}
	rows, err := c.store.db.QueryContext(ctx, checkoutRootsQuery(len(roots), pathkey.CaseInsensitivePaths), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Checkout
	for rows.Next() {
		var checkout Checkout
		if err := scanCheckout(func(dest ...any) error {
			return rows.Scan(append([]any{&checkout.CheckoutID}, dest...)...)
		}, &checkout); err != nil {
			return nil, err
		}
		out = append(out, checkout)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
