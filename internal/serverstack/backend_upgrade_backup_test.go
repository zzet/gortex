package serverstack

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestOpenBackendBacksUpAnUpgradeOnlyUnderTheStoreLock(t *testing.T) {
	for name, lockHeld := range map[string]bool{"locked": true, "unlocked": false} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.sqlite")
			s, err := store_sqlite.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			// An index-only upgrade from v40 is pending on the next open.
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("PRAGMA user_version=40"); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			g, cleanup, err := OpenBackend("sqlite", path, zap.NewNop(), lockHeld)
			if err != nil {
				t.Fatalf("open older store: %v", err)
			}
			if g == nil {
				t.Fatal("no store")
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}

			_, statErr := os.Stat(store_sqlite.UpgradeBackupPath(path, 40))
			if lockHeld && statErr != nil {
				t.Fatalf("the lock holder's upgrade must leave a backup: %v", statErr)
			}
			if !lockHeld && !os.IsNotExist(statErr) {
				t.Fatalf("an open without the store lock must not copy the store (stat err %v)", statErr)
			}
		})
	}
}
