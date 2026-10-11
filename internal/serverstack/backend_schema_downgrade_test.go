package serverstack

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestOpenBackendRetainsNewerSchemaWithoutRestartAdvice(t *testing.T) {
	for _, allowRebuild := range []bool{false, true} {
		name := "refuse"
		if allowRebuild {
			name = "rebuild_requested"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "future.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("CREATE TABLE future_catalog (value TEXT); INSERT INTO future_catalog VALUES ('retain'); PRAGMA user_version=2147483647"); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			g, cleanup, err := OpenBackend("sqlite", path, zap.NewNop(), allowRebuild)
			if cleanup != nil {
				cleanup()
			}
			if g != nil || !errors.Is(err, store_sqlite.ErrSchemaTooNew) {
				t.Fatalf("newer schema: store=%v, error=%v", g, err)
			}
			if strings.Contains(err.Error(), "daemon stop") || strings.Contains(err.Error(), "daemon restart") {
				t.Fatalf("version incompatibility must not recommend disrupting another daemon: %v", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("backend refusal changed the newer database")
			}
		})
	}
}

func TestSharedServerReleasesLockWhenBackendOpenFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invalid.sqlite")
	original := []byte("not a SQLite database")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	// Use explicit private paths and no user configuration for this failed open.
	cfg := SharedServerConfig{
		Lifecycle:         LifecycleDaemon,
		Backend:           "sqlite",
		BackendPath:       path,
		Index:             dir,
		Config:            &config.Config{},
		Logger:            zap.NewNop(),
		SavingsPath:       filepath.Join(dir, "savings.sqlite"),
		SavingsLegacyJSON: filepath.Join(dir, "savings.json"),
	}
	server, err := NewSharedServer(cfg)
	if server != nil {
		_ = server.Close()
	}
	if err == nil {
		t.Fatal("invalid database unexpectedly opened")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, original) {
		t.Fatalf("failed open changed database: read error=%v", readErr)
	}
	lock := flock.New(path + ".lock")
	locked, lockErr := lock.TryLock()
	if lockErr != nil {
		t.Fatal(lockErr)
	}
	if !locked {
		t.Fatal("failed constructor retained the store lock")
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkOpenBackendFutureSchemaRefusal(b *testing.B) {
	path := filepath.Join(b.TempDir(), "future.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE future_catalog (value TEXT); INSERT INTO future_catalog VALUES ('retain'); PRAGMA user_version=2147483647"); err != nil {
		_ = db.Close()
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		g, cleanup, err := OpenBackend("sqlite", path, nil, true)
		if cleanup != nil {
			cleanup()
		}
		if g != nil || !errors.Is(err, store_sqlite.ErrSchemaTooNew) {
			b.Fatalf("newer schema: store=%v, error=%v", g, err)
		}
	}
	b.StopTimer()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		b.Fatalf("future database changed: %v", err)
	}
}
