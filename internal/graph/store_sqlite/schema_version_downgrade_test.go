package store_sqlite

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A newer schema may have a catalog shape this binary has never seen. Open
// must refuse it before any writer pragma, migration, stamp or rebuild.
func TestSchemaNewerStorePreservesDataAndFiles(t *testing.T) {
	for _, journal := range []string{"delete", "persist", "wal"} {
		for _, rebuild := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rebuild=%t", journal, rebuild), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "future.sqlite")
				db := futureSchemaFixture(t, path, journal)
				before := schemaStoreFiles(t, path)
				if journal == "wal" && (len(before["-wal"]) == 0 || binary.BigEndian.Uint32(before[""][60:64]) == 999) {
					t.Fatal("fixture must retain the newer version in WAL, not the main-file header")
				}
				migrationEvents := 0
				options := []Option{WithMigrationObserver(func(MigrationProgress) { migrationEvents++ })}
				if rebuild {
					options = append(options, WithRebuild())
				}
				opened, err := Open(path, options...)
				if opened != nil {
					_ = opened.Close()
					t.Fatal("Open returned a store for a newer schema")
				}
				requireSchemaTooNew(t, err, currentSchemaVersion)
				if migrationEvents != 0 {
					t.Fatalf("refusal ran %d migration callbacks", migrationEvents)
				}
				after := schemaStoreFiles(t, path)
				for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
					if (before[suffix] == nil) != (after[suffix] == nil) || len(before[suffix]) != len(after[suffix]) {
						t.Errorf("refusal replaced companion %q: before=%d bytes after=%d bytes", suffix, len(before[suffix]), len(after[suffix]))
					}
					// SQLite's WAL reader synchronization updates SHM read marks.
					// Retain that file, but require only durable payloads byte-exact.
					if suffix != "-shm" && !bytes.Equal(before[suffix], after[suffix]) {
						t.Errorf("refusal changed durable companion %q", suffix)
					}
				}
				var version int
				if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 999 {
					t.Fatalf("version after refusal = %d, %v; want 999", version, err)
				}
				var root string
				if err := db.QueryRow("SELECT root_path FROM checkouts WHERE checkout_id = 'retained'").Scan(&root); err != nil || root != "future/root" {
					t.Fatalf("catalog after refusal = %q, %v", root, err)
				}
			})
		}
	}
}

func requireSchemaTooNew(t testing.TB, err error, supported int) {
	t.Helper()
	var versionError *SchemaTooNewError
	if !errors.Is(err, ErrSchemaTooNew) || !errors.As(err, &versionError) {
		t.Fatalf("error = %v, want typed ErrSchemaTooNew", err)
	}
	if versionError.Stored != 999 || versionError.Supported != supported {
		t.Fatalf("incompatible versions = %+v, want stored=999 supported=%d", versionError, supported)
	}
	if errors.Is(err, ErrSchemaRebuildRequired) {
		t.Fatal("newer schema must not advertise destructive rebuild as recovery")
	}
}

func schemaFixtureURI(t testing.TB, path, query string) string {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed, RawQuery: query}).String()
}

func futureSchemaFixture(t testing.TB, path, journal string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", schemaFixtureURI(t, path, ""))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("PRAGMA journal_mode = " + journal); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE checkouts (checkout_id TEXT PRIMARY KEY, root_path TEXT, future_payload BLOB);
INSERT INTO checkouts VALUES ('retained', 'future/root', zeroblob(32768));`); err != nil {
		t.Fatal(err)
	}
	// Keep WAL open and checkpoint the old header before placing the newer
	// version in WAL alone. Reading only the main-file header is not sufficient.
	if journal == "wal" {
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	return db
}

func schemaStoreFiles(t testing.TB, path string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		files[suffix] = data
	}
	return files
}

func schemaFixtureFilenames() []string {
	names := []string{"plain.sqlite", "space # percent%.sqlite"}
	if runtime.GOOS != "windows" {
		// These characters are valid in Unix filenames, but not Windows ones.
		names = append(names, "question?.sqlite", "literal:memory:.sqlite", "literal:memory:?query#.sqlite")
	}
	return names
}

func TestSchemaDowngradeProbeInputs(t *testing.T) {
	for _, name := range schemaFixtureFilenames() {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			db := futureSchemaFixture(t, path, "wal")
			inputs := []string{path, schemaFixtureURI(t, path, "mode=rw&immutable=1&_pragma=user_version(1)")}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if relative, err := filepath.Rel(cwd, path); err == nil {
				escaped := (&url.URL{Path: filepath.ToSlash(relative)}).EscapedPath()
				inputs = append(inputs, "file:"+escaped+"?mode=rw")
			} else if runtime.GOOS != "windows" {
				t.Fatalf("relative fixture URI: %v", err)
			}
			for _, input := range inputs {
				store, err := Open(input, WithRebuild())
				if store != nil {
					_ = store.Close()
					t.Fatalf("Open(%q) accepted a newer schema", input)
				}
				requireSchemaTooNew(t, err, currentSchemaVersion)
			}
			var version int
			if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 999 {
				t.Fatalf("URI probe changed version: %d, %v", version, err)
			}
		})
	}
}

func TestSchemaDowngradeProbeRejectsDuplicateModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.sqlite")
	_ = futureSchemaFixture(t, path, "wal")
	for _, query := range []string{"mode=memory&mode=rw", "mode=rw&mode=memory", "mode=memory&mode=memory"} {
		if err := checkSchemaDowngrade(schemaFixtureURI(t, path, query), currentSchemaVersion); err == nil {
			t.Errorf("ambiguous modes bypassed probe: %s", query)
		}
	}
}

func TestSchemaProbeAndWriterUseSameFilename(t *testing.T) {
	for _, name := range schemaFixtureFilenames() {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			db, err := sql.Open("sqlite", sqliteWriterDSN(path))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var sequence int
			var schema, actual string
			if err := db.QueryRow("PRAGMA database_list").Scan(&sequence, &schema, &actual); err != nil {
				t.Fatal(err)
			}
			requestedInfo, requestedErr := os.Stat(path)
			actualInfo, actualErr := os.Stat(actual)
			if requestedErr != nil || actualErr != nil || !os.SameFile(requestedInfo, actualInfo) {
				t.Fatalf("writer target=%q requested=%q; stat errors=%v/%v", actual, path, actualErr, requestedErr)
			}
		})
	}
}

func TestSchemaDowngradeSharedMemoryRecheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory-only.sqlite")
	uri := schemaFixtureURI(t, path, "mode=memory&cache=shared")
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE future_marker (value TEXT); INSERT INTO future_marker VALUES ('retained'); PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(uri, WithRebuild())
	if store != nil {
		_ = store.Close()
		t.Fatal("Open returned a newer shared-memory store")
	}
	requireSchemaTooNew(t, err, currentSchemaVersion)
	var marker string
	if err := db.QueryRow("SELECT value FROM future_marker").Scan(&marker); err != nil || marker != "retained" {
		t.Fatalf("memory schema was altered: marker=%q err=%v", marker, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("shared-memory URI touched disk: %v", err)
	}
}

func TestSchemaDowngradeProbeReadFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite")
	if err := checkSchemaDowngrade(path, currentSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("probe created a missing store: %v", err)
	}
	corrupt := []byte("not a SQLite database")
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkSchemaDowngrade(path, currentSchemaVersion); err == nil {
		t.Fatal("probe accepted unreadable schema metadata")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(corrupt, after) {
		t.Fatalf("failed probe altered input: %q, %v", after, err)
	}
	if err := checkSchemaDowngrade(t.TempDir(), currentSchemaVersion); err == nil {
		t.Fatal("probe accepted a directory as a database")
	}
}

// Each iteration receives a fresh future-schema fixture outside the timer.
// preserved_database_bytes/op is an outcome count, not physical disk traffic.
func BenchmarkSchemaNewerOpen(b *testing.B) {
	for _, rebuild := range []bool{false, true} {
		b.Run(fmt.Sprintf("rebuild=%t", rebuild), func(b *testing.B) {
			var preserved, refused int
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				path := filepath.Join(b.TempDir(), "future.sqlite")
				db := futureSchemaFixture(b, path, "delete")
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					b.Fatal(err)
				}
				var options []Option
				if rebuild {
					options = []Option{WithRebuild()}
				}
				b.StartTimer()
				opened, openErr := Open(path, options...)
				if opened != nil {
					_ = opened.Close()
				}
				b.StopTimer()
				if openErr != nil {
					refused++
				}
				after, err := os.ReadFile(path)
				if err != nil {
					b.Fatal(err)
				}
				if bytes.Equal(before, after) {
					preserved++
				}
				if openErr != nil && !strings.Contains(openErr.Error(), "schema") {
					b.Fatal(openErr)
				}
			}
			b.ReportMetric(float64(preserved)/float64(b.N), "preserved_database_bytes/op")
			b.ReportMetric(float64(refused)/float64(b.N), "refusals/op")
		})
	}
}
