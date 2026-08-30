package store_sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	_ "modernc.org/sqlite"
)

// withRawDB opens a bare *sql.DB on path, runs fn, and closes it — used to
// simulate an on-disk store written by an older/newer build (set user_version,
// insert rows) without going through Open's reconciliation.
func withRawDB(t *testing.T, path string, fn func(db *sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	fn(db)
}

func nodeCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&n); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	return n
}

// TestOpenStampsFreshDB: a brand-new on-disk store is stamped to the current
// schema version.
func TestOpenStampsFreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	defer s.Close()
	if v, err := readUserVersion(s.db); err != nil || v != currentSchemaVersion {
		t.Fatalf("fresh user_version = %d (err %v), want %d", v, err, currentSchemaVersion)
	}
}

func TestOpenV8DropsUnusedSemanticPendingIndexInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s.AddBatch([]*graph.Node{{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"}}, nil)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`CREATE INDEX nodes_semantic_pending ON nodes(repo_prefix) WHERE semantic_type IS NULL`); err != nil {
			t.Fatalf("create legacy index: %v", err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 8`); err != nil {
			t.Fatalf("stamp v8: %v", err)
		}
	})

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("open v8 store: %v", err)
	}
	defer migrated.Close()
	var obsolete int
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'nodes_semantic_pending'`).Scan(&obsolete); err != nil {
		t.Fatalf("probe obsolete index: %v", err)
	}
	if obsolete != 0 {
		t.Fatalf("obsolete semantic pending index count = %d, want 0", obsolete)
	}
	if migrated.NodeCount() != 1 || migrated.NeedsRebuild() {
		t.Fatalf("v8 migration changed graph or requested rebuild: nodes=%d rebuild=%v", migrated.NodeCount(), migrated.NeedsRebuild())
	}
}

// TestOpenPreVersionStoreRequiresRebuild: once a rebuild boundary ships, an
// existing user_version=0 graph cannot be confused with a brand-new empty DB.
// The default open preserves it and returns ErrSchemaRebuildRequired; the
// exclusive daemon path wipes it and reports NeedsRebuild.
func TestOpenPreVersionStoreRequiresRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	// Create the store, then simulate a pre-versioning DB: a row + user_version 0.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('n1','func','Foo','f.go')`); err != nil {
			t.Fatalf("seed node: %v", err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
			t.Fatalf("reset user_version: %v", err)
		}
	})

	if _, err := Open(path); !errors.Is(err, ErrSchemaRebuildRequired) {
		t.Fatalf("reopen old DB error = %v, want ErrSchemaRebuildRequired", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if n := nodeCount(t, db); n != 1 {
			t.Fatalf("refused rebuild changed node count to %d, want 1", n)
		}
	})

	s2, err := Open(path, WithRebuild())
	if err != nil {
		t.Fatalf("exclusive rebuild: %v", err)
	}
	defer s2.Close()
	if !s2.NeedsRebuild() {
		t.Fatal("rebuilt pre-version store did not report NeedsRebuild")
	}
	if n := nodeCount(t, s2.db); n != 0 {
		t.Fatalf("node count after integrity rebuild = %d, want 0", n)
	}
}

// TestOpenRebuildsNewerDB: a store written by a NEWER build (user_version above
// current) cannot be trusted, so Open drops and rebuilds it — the data is gone
// and the version is re-stamped to current. Proves the wipe path (and that the
// -wal/-shm companions are cleared along with the main file).
func TestOpenRebuildsNewerDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('n1','func','Foo','f.go')`); err != nil {
			t.Fatalf("seed node: %v", err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil { // a future version this binary doesn't know
			t.Fatalf("set future user_version: %v", err)
		}
	})

	s2, err := Open(path, WithRebuild()) // simulate the daemon: holds the lock, may rebuild
	if err != nil {
		t.Fatalf("reopen newer DB: %v", err)
	}
	defer s2.Close()
	if v, _ := readUserVersion(s2.db); v != currentSchemaVersion {
		t.Fatalf("user_version after rebuild = %d, want %d", v, currentSchemaVersion)
	}
	if n := nodeCount(t, s2.db); n != 0 {
		t.Fatalf("node count after rebuild = %d, want 0 (newer DB must be wiped)", n)
	}
}

// TestOpenRefusesWipeWithoutOptIn: the default Open must NOT destroy an
// incompatible on-disk database. Without WithRebuild it returns
// ErrSchemaRebuildRequired and leaves the file (and its rows) intact, so a
// caller that does not hold the store lock cannot silently corrupt a store
// another process may have open.
func TestOpenRefusesWipeWithoutOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := s.writerDB.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('n1','func','Foo','f.go')`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
			t.Fatalf("set future version: %v", err)
		}
	})

	if _, err := Open(path); !errors.Is(err, ErrSchemaRebuildRequired) {
		t.Fatalf("Open without WithRebuild = %v, want ErrSchemaRebuildRequired", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if n := nodeCount(t, db); n != 1 {
			t.Fatalf("node count = %d after a refused wipe, want 1 (the file must be untouched)", n)
		}
		var v int
		if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
			t.Fatalf("read user_version: %v", err)
		}
		if v != 999 {
			t.Fatalf("user_version = %d after a refused wipe, want 999 (unchanged)", v)
		}
	})
}

// TestPlanSchemaMigration covers the pure decision logic, including the
// in-place vs rebuild dispatch a future currentSchemaVersion=2 would exercise.
func TestPlanSchemaMigration(t *testing.T) {
	inPlace := schemaMigration{version: 2, name: "add-index", inPlace: func(*sql.Tx) error { return nil }}
	rebuild := schemaMigration{version: 2, name: "typed-column", rebuild: true}

	cases := []struct {
		name            string
		stored, current int
		migs            []schemaMigration
		wantWipe        bool
		wantStamp       bool
		wantInPlace     int
	}{
		{"up to date", 1, 1, nil, false, false, 0},
		{"fresh at v1 baseline-stamps", 0, 1, nil, false, true, 0},
		{"newer DB rebuilds", 2, 1, nil, true, true, 0},
		{"v0 with only in-place pending upgrades in place, no wipe", 0, 2, []schemaMigration{inPlace}, false, true, 1},
		{"v0 with a pending rebuild wipes", 0, 2, []schemaMigration{rebuild}, true, true, 0},
		{"v1->v2 in-place", 1, 2, []schemaMigration{inPlace}, false, true, 1},
		{"v1->v2 rebuild", 1, 2, []schemaMigration{rebuild}, true, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planSchemaMigrationWith(c.stored, c.current, c.migs)
			if got.wipe != c.wantWipe || got.stamp != c.wantStamp || len(got.inPlace) != c.wantInPlace {
				t.Fatalf("plan(%d->%d) = {wipe:%v stamp:%v inPlace:%d}, want {wipe:%v stamp:%v inPlace:%d}",
					c.stored, c.current, got.wipe, got.stamp, len(got.inPlace), c.wantWipe, c.wantStamp, c.wantInPlace)
			}
		})
	}
}

func TestOpenV2RequiresTopologyIntegrityRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := s.writerDB.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('n1','func','Foo','f.go')`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
			t.Fatalf("set v2: %v", err)
		}
	})

	if _, err := Open(path); !errors.Is(err, ErrSchemaRebuildRequired) {
		t.Fatalf("default v2 open error = %v, want ErrSchemaRebuildRequired", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if v, err := readUserVersion(db); err != nil || v != 2 {
			t.Fatalf("refused v2 version = %d (err %v), want 2", v, err)
		}
		if n := nodeCount(t, db); n != 1 {
			t.Fatalf("refused v2 rebuild changed node count to %d, want 1", n)
		}
	})

	rebuilt, err := Open(path, WithRebuild())
	if err != nil {
		t.Fatalf("exclusive v2 rebuild: %v", err)
	}
	defer rebuilt.Close()
	if !rebuilt.NeedsRebuild() {
		t.Fatal("v2 integrity rebuild did not report NeedsRebuild")
	}
	if v, err := readUserVersion(rebuilt.db); err != nil || v != currentSchemaVersion {
		t.Fatalf("rebuilt version = %d (err %v), want %d", v, err, currentSchemaVersion)
	}
	if n := nodeCount(t, rebuilt.db); n != 0 {
		t.Fatalf("rebuilt v2 node count = %d, want 0", n)
	}
}

// TestOpenWithoutQualIndexPreservesDuplicateQualNames covers legacy or damaged
// stores whose qualified-name index is absent. Reopening recreates the lookup
// index without rewriting valid duplicate identities or requiring a rebuild.
func TestOpenWithoutQualIndexPreservesDuplicateQualNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`DROP INDEX nodes_by_qual`); err != nil {
			t.Fatalf("drop qualified-name index: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO nodes (id, kind, name, qual_name, file_path) VALUES
			('a', 'function', 'Run', 'pkg.Run', 'a.go'),
			('b', 'function', 'Run', 'pkg.Run', 'b.go')`); err != nil {
			t.Fatalf("seed duplicate qualified names: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO edges (from_id, to_id, kind, file_path, line) VALUES
			('a', 'b', 'calls', 'a.go', 1)`); err != nil {
			t.Fatalf("seed edge between duplicate nodes: %v", err)
		}
	})

	repaired, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy store for in-place repair: %v", err)
	}
	if repaired.NeedsRebuild() {
		t.Fatal("qualified-name repair unexpectedly requested a rebuild")
	}
	if n := nodeCount(t, repaired.db); n != 2 {
		t.Fatalf("node count after repair = %d, want 2", n)
	}
	var edges int
	if err := repaired.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE from_id = 'a' AND to_id = 'b'`).Scan(&edges); err != nil || edges != 1 {
		t.Fatalf("preserved edge count = %d (err %v), want 1", edges, err)
	}
	var aQual, bQual string
	if err := repaired.db.QueryRow(`SELECT qual_name FROM nodes WHERE id = 'a'`).Scan(&aQual); err != nil {
		t.Fatalf("read deterministic keeper: %v", err)
	}
	if err := repaired.db.QueryRow(`SELECT qual_name FROM nodes WHERE id = 'b'`).Scan(&bQual); err != nil {
		t.Fatalf("read repaired duplicate: %v", err)
	}
	if aQual != "pkg.Run" || bQual != "pkg.Run" {
		t.Fatalf("qualified names after reopen = a:%q b:%q, want both %q", aQual, bQual, "pkg.Run")
	}
	var uniqueIndex int
	if err := repaired.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'nodes_by_qual'`).Scan(&uniqueIndex); err != nil || uniqueIndex != 1 {
		t.Fatalf("qualified-name index count = %d (err %v), want 1", uniqueIndex, err)
	}
	if err := repaired.Close(); err != nil {
		t.Fatalf("close repaired store: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("warm reopen after repair: %v", err)
	}
	defer reopened.Close()
	if n := nodeCount(t, reopened.db); n != 2 {
		t.Fatalf("node count after warm reopen = %d, want 2", n)
	}
}

func TestOpenV7RelaxesQualNameUniquenessInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	if _, err := seed.writerDB.Exec(`INSERT INTO nodes
		(id, kind, name, qual_name, file_path, repo_prefix)
		VALUES ('repo-a/k8s::ClusterRole::_default::prometheus', 'resource', 'prometheus',
		        'ClusterRole/prometheus', 'repo-a/deploy/rbac.yaml', 'repo-a')`); err != nil {
		t.Fatalf("seed first resource: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`DROP INDEX nodes_by_qual`); err != nil {
			t.Fatalf("drop current qualified-name index: %v", err)
		}
		if _, err := db.Exec(`CREATE UNIQUE INDEX nodes_by_qual ON nodes(qual_name) WHERE qual_name <> ''`); err != nil {
			t.Fatalf("restore v7 qualified-name index: %v", err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 7`); err != nil {
			t.Fatalf("stamp v7: %v", err)
		}
	})

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("open v7 store: %v", err)
	}
	defer migrated.Close()
	if migrated.NeedsRebuild() {
		t.Fatal("qualified-name index migration unexpectedly requested a rebuild")
	}
	if _, err := migrated.writerDB.Exec(`INSERT INTO nodes
		(id, kind, name, qual_name, file_path, repo_prefix)
		VALUES ('repo-b/k8s::ClusterRole::_default::prometheus', 'resource', 'prometheus',
		        'ClusterRole/prometheus', 'repo-b/manifests/roles.yaml', 'repo-b')`); err != nil {
		t.Fatalf("insert duplicate qualified name after v7 migration: %v", err)
	}

	var duplicates int
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE qual_name = 'ClusterRole/prometheus'`).Scan(&duplicates); err != nil || duplicates != 2 {
		t.Fatalf("duplicate qualified-name rows = %d (err %v), want 2", duplicates, err)
	}
	var unique int
	if err := migrated.db.QueryRow(`SELECT "unique" FROM pragma_index_list('nodes') WHERE name = 'nodes_by_qual'`).Scan(&unique); err != nil || unique != 0 {
		t.Fatalf("nodes_by_qual unique flag = %d (err %v), want 0", unique, err)
	}
	if version, err := readUserVersion(migrated.db); err != nil || version != currentSchemaVersion {
		t.Fatalf("migrated user_version = %d (err %v), want %d", version, err, currentSchemaVersion)
	}
}

// TestApplyInPlaceMigrations: steps run in order and commit; a failing step
// rolls the whole transaction back.
func TestApplyInPlaceMigrations(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "m.sqlite")
		withRawDB(t, path, func(db *sql.DB) {
			step := schemaMigration{version: 2, name: "mk", inPlace: func(tx *sql.Tx) error {
				_, err := tx.Exec(`CREATE TABLE marker (x TEXT)`)
				return err
			}}
			if err := applyInPlaceMigrations(db, []schemaMigration{step}); err != nil {
				t.Fatalf("apply: %v", err)
			}
			var name string
			if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='marker'`).Scan(&name); err != nil {
				t.Fatalf("marker table not created: %v", err)
			}
		})
	})

	t.Run("rollback on failure preserves cause and rolls back every step", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "m.sqlite")
		withRawDB(t, path, func(db *sql.DB) {
			// Two steps in one batch: the first creates table A, the second
			// creates B then fails. Both must roll back, proving the steps
			// share a single transaction.
			stepA := schemaMigration{version: 2, name: "make-a", inPlace: func(tx *sql.Tx) error {
				_, err := tx.Exec(`CREATE TABLE a (x TEXT)`)
				return err
			}}
			stepB := schemaMigration{version: 3, name: "boom", inPlace: func(tx *sql.Tx) error {
				if _, err := tx.Exec(`CREATE TABLE b (x TEXT)`); err != nil {
					return err
				}
				return sql.ErrConnDone // synthetic failure after a partial write
			}}
			err := applyInPlaceMigrations(db, []schemaMigration{stepA, stepB})
			if err == nil {
				t.Fatal("expected applyInPlaceMigrations to surface the step error")
			}
			if !errors.Is(err, sql.ErrConnDone) {
				t.Fatalf("error should wrap the step's cause; got %v", err)
			}
			if !strings.Contains(err.Error(), "v3") || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("error should name the failing migration (v3/boom); got %q", err.Error())
			}
			for _, tbl := range []string{"a", "b"} {
				var name string
				e := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
				if e != sql.ErrNoRows {
					t.Fatalf("table %q should have rolled back (shared transaction), got name=%q err=%v", tbl, name, e)
				}
			}
		})
	})
}

// TestOpenAtCurrentVersionIsNoOp covers the highest-frequency path — every
// daemon restart reopens an up-to-date store. It must be a no-op that
// preserves data; an off-by-one to wipe here would destroy the cache on every
// restart.
func TestOpenAtCurrentVersionIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	withRawSeed := func(db *sql.DB) {
		if _, err := db.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('n1','func','Foo','f.go')`); err != nil {
			t.Fatalf("seed node: %v", err)
		}
	}
	withRawSeed(s.writerDB)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen at current version: %v", err)
	}
	defer s2.Close()
	if v, _ := readUserVersion(s2.db); v != currentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", v, currentSchemaVersion)
	}
	if n := nodeCount(t, s2.db); n != 1 {
		t.Fatalf("node count = %d, want 1 (a no-op reopen must NOT wipe)", n)
	}
	if s2.NeedsRebuild() {
		t.Fatal("a no-op reopen must not signal NeedsRebuild")
	}
}

// TestOpenWithInPlaceMigration drives the in-place arm end-to-end through the
// real Open composition (via the openWith seam): an older store at version 1
// is upgraded to version 2 by a registered in-place step that runs AFTER
// schemaSQL, the step's effect is visible, the existing data survives, and the
// version is stamped.
func TestOpenWithInPlaceMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	// Create a store with a row, then knock it back to the v1 baseline so the
	// openWith below drives the v1->v2 in-place arm (a fresh Open now stamps the
	// current version, which is >= 2).
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := s.writerDB.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('n1','func','Foo','f.go')`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
			t.Fatalf("reset to v1 baseline: %v", err)
		}
	})

	// An in-place v2 step that depends on the base schema (an index on a
	// nodes column) — proving it runs after schemaSQL/ensureNodeColumns.
	ran := false
	v2 := schemaMigration{version: 2, name: "idx-language", inPlace: func(tx *sql.Tx) error {
		ran = true
		_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS test_nodes_by_language ON nodes(language)`)
		return err
	}}

	s2, err := openWith(path, 2, []schemaMigration{v2}, false) // in-place never wipes
	if err != nil {
		t.Fatalf("openWith v2 in-place: %v", err)
	}
	defer s2.Close()
	if !ran {
		t.Fatal("the in-place migration step did not run")
	}
	if v, _ := readUserVersion(s2.db); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	if n := nodeCount(t, s2.db); n != 1 {
		t.Fatalf("node count = %d, want 1 (in-place upgrade must preserve data)", n)
	}
	var name string
	if err := s2.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='test_nodes_by_language'`).Scan(&name); err != nil {
		t.Fatalf("in-place index not created: %v", err)
	}
	if s2.NeedsRebuild() {
		t.Fatal("an in-place upgrade must not signal NeedsRebuild")
	}
}

// TestOpenWithInPlaceFailureDoesNotStamp: a failing in-place step makes Open
// return an error and leaves the stored version unchanged, so the next open
// retries the upgrade rather than treating it as done.
func TestOpenWithInPlaceFailureDoesNotStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A fresh Open now stamps the current version (>= 2); knock it back to the
	// v1 baseline so openWith drives the v1->v2 arm and the failing step runs.
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
			t.Fatalf("reset to v1 baseline: %v", err)
		}
	})

	boom := schemaMigration{version: 2, name: "boom", inPlace: func(*sql.Tx) error {
		return sql.ErrConnDone
	}}
	if _, err := openWith(path, 2, []schemaMigration{boom}, false); err == nil {
		t.Fatal("expected openWith to fail when an in-place step errors")
	}

	withRawDB(t, path, func(db *sql.DB) {
		var v int
		if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
			t.Fatalf("read user_version: %v", err)
		}
		if v != 1 {
			t.Fatalf("user_version = %d after a failed migration, want 1 (unstamped, so the next open retries)", v)
		}
	})
}

// A schema version is the durable completion marker for the entire migration
// tail, not just for its table rewrites. Inject a failure in the mandatory
// analysis invalidation after the v19 step commits: Open must leave v18 stamped
// so the next process retries instead of trusting an old-shape analysis cache.
func TestOpenMigrationDoesNotStampBeforeAnalysisInvalidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close current store: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`
INSERT INTO analysis_generations (
    format_version, build_revision, created_at_unix, state,
    node_count, community_count, process_count, concept_count
) VALUES (1, 1, 1, 1, 0, 0, 0, 0);
INSERT INTO analysis_active_generation(slot, generation_id)
VALUES (1, last_insert_rowid());
CREATE TRIGGER fail_schema_migration_analysis_invalidation
BEFORE UPDATE OF state ON analysis_generations
WHEN NEW.state = 2
BEGIN
    SELECT RAISE(ABORT, 'injected analysis invalidation failure');
END;
PRAGMA user_version = 18;`); err != nil {
			t.Fatalf("seed v18 crash-boundary fixture: %v", err)
		}
	})

	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "injected analysis invalidation failure") {
		t.Fatalf("Open error = %v, want injected analysis invalidation failure", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		var version, active, ready int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM analysis_active_generation`).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM analysis_generations WHERE state = 1`).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if version != 18 || active != 1 || ready != 1 {
			t.Fatalf("failed tail left version=%d active=%d ready=%d, want 18/1/1", version, active, ready)
		}
		if _, err := db.Exec(`DROP TRIGGER fail_schema_migration_analysis_invalidation`); err != nil {
			t.Fatalf("remove injected failure: %v", err)
		}
	})

	retried, err := Open(path)
	if err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	defer retried.Close()
	var version, active, stale int
	if err := retried.writerDB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := retried.writerDB.QueryRow(`SELECT COUNT(*) FROM analysis_active_generation`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := retried.writerDB.QueryRow(`SELECT COUNT(*) FROM analysis_generations WHERE state = 2`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion || active != 0 || stale != 1 {
		t.Fatalf("retry left version=%d active=%d stale=%d, want %d/0/1", version, active, stale, currentSchemaVersion)
	}
}

// TestOpenWithMemoryUnderWipePlanStampsWithoutError: an in-memory store under a
// plan that would wipe an on-disk DB must not attempt a file removal — it is
// always fresh and simply stamps the current version.
func TestOpenWithMemoryUnderWipePlanStampsWithoutError(t *testing.T) {
	rebuildV2 := schemaMigration{version: 2, name: "typed-col", rebuild: true}
	// stored==0, current==2, a pending rebuild => plan.wipe==true; the memory
	// guard must skip the wipe and stamp anyway.
	s, err := openWith(":memory:", 2, []schemaMigration{rebuildV2}, false) // memory never wipes
	if err != nil {
		t.Fatalf("openWith :memory: under wipe plan: %v", err)
	}
	defer s.Close()
	if v, _ := readUserVersion(s.db); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	if s.NeedsRebuild() {
		t.Fatal(":memory: must never report a wipe (nothing to remove)")
	}
}

// TestNeedsRebuildSignalAfterWipe: a store written by a newer build is wiped on
// open and reports NeedsRebuild so the daemon forces a full re-index.
func TestNeedsRebuildSignalAfterWipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
			t.Fatalf("set future version: %v", err)
		}
	})
	s2, err := Open(path, WithRebuild()) // daemon-equivalent: lock held, rebuild permitted
	if err != nil {
		t.Fatalf("reopen newer DB: %v", err)
	}
	defer s2.Close()
	if !s2.NeedsRebuild() {
		t.Fatal("a wiped store must report NeedsRebuild so the daemon re-indexes")
	}
}

// TestSchemaMigrationsWellFormed asserts the shipped registry is valid and that
// the validator rejects the dangerous misconfigurations — above all, bumping
// currentSchemaVersion without appending a matching migration.
func TestSchemaMigrationsWellFormed(t *testing.T) {
	if err := validateSchemaMigrations(currentSchemaVersion, schemaMigrations); err != nil {
		t.Fatalf("shipped registry is invalid: %v", err)
	}

	inPlace := func(*sql.Tx) error { return nil }
	bad := []struct {
		name    string
		current int
		migs    []schemaMigration
	}{
		{"bumped version with no migration", 2, nil},
		{"highest below current", 3, []schemaMigration{{version: 2, name: "x", rebuild: true}}},
		{"both strategies set", 2, []schemaMigration{{version: 2, name: "x", rebuild: true, inPlace: inPlace}}},
		{"neither strategy set", 2, []schemaMigration{{version: 2, name: "x"}}},
		{"not strictly ascending", 3, []schemaMigration{{version: 2, name: "a", rebuild: true}, {version: 2, name: "b", rebuild: true}}},
		{"v1 entry (baseline is implicit)", 1, []schemaMigration{{version: 1, name: "a", rebuild: true}}},
	}
	for _, c := range bad {
		if err := validateSchemaMigrations(c.current, c.migs); err == nil {
			t.Errorf("%s: expected a validation error, got nil", c.name)
		}
	}
}

func TestOpenV5CompactsResolverEdgeIndexesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolver-index-v5.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	// Seed OWNED nodes. A file-backed node with an empty repo_prefix is
	// single-repo-mode residue, which the v7 migration in this same chain
	// purges by design — an unprefixed fixture would be deleted out from
	// under the assertions below.
	if _, err := s.writerDB.Exec(`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES
		('repo/a.go::source', 'function', 'source', 'repo/a.go', 'repo'),
		('repo/b.go::target', 'function', 'target', 'repo/b.go', 'repo')`); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if _, err := s.writerDB.Exec(`INSERT INTO edges (from_id, to_id, kind, file_path, line) VALUES
		('repo/a.go::source', 'unresolved::target', 'calls', 'repo/a.go', 1),
		('repo/a.go::source', 'repo/b.go::target', 'calls', 'repo/a.go', 2)`); err != nil {
		t.Fatalf("seed edges: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	// Recreate the v5 shapes: one dense Boolean frontier index plus the
	// one-shot global Go receiver index. The v6 migration must replace/drop
	// them without wiping graph rows.
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`DROP INDEX IF EXISTS edges_by_unresolved`); err != nil {
			t.Fatalf("drop current unresolved index: %v", err)
		}
		if _, err := db.Exec(`CREATE INDEX edges_by_unresolved ON edges(is_unresolved)`); err != nil {
			t.Fatalf("create v5 dense unresolved index: %v", err)
		}
		if _, err := db.Exec(`CREATE INDEX edges_go_member_receiver ON edges(member_receiver_dir, member_receiver, from_id, to_id, id) WHERE kind = 'member_of' AND member_receiver IS NOT NULL AND member_receiver_dir IS NOT NULL`); err != nil {
			t.Fatalf("create v5 receiver index: %v", err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 5`); err != nil {
			t.Fatalf("stamp v5: %v", err)
		}
	})

	assertCompacted := func(store *Store) {
		t.Helper()
		if store.NeedsRebuild() {
			t.Fatal("mechanical v5 index migration unexpectedly requested a rebuild")
		}
		var partial int
		if err := store.db.QueryRow(`SELECT partial FROM pragma_index_list('edges') WHERE name = 'edges_by_unresolved'`).Scan(&partial); err != nil || partial != 1 {
			t.Fatalf("edges_by_unresolved partial=%d err=%v, want 1,nil", partial, err)
		}
		var ddl string
		if err := store.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'edges_by_unresolved'`).Scan(&ddl); err != nil {
			t.Fatalf("read unresolved index DDL: %v", err)
		}
		if !strings.Contains(strings.ToLower(ddl), "where is_unresolved = 1") {
			t.Fatalf("unresolved index is not frontier-partial: %s", ddl)
		}
		var obsolete, edges int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'edges_go_member_receiver'`).Scan(&obsolete); err != nil || obsolete != 0 {
			t.Fatalf("obsolete receiver index count=%d err=%v, want 0,nil", obsolete, err)
		}
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&edges); err != nil || edges != 2 {
			t.Fatalf("edge count after in-place migration=%d err=%v, want 2,nil", edges, err)
		}
		plan := strings.ToLower(queryPlan(t, store, `SELECT id FROM edges WHERE id > ? AND is_unresolved = 1 ORDER BY id`, 0))
		if !strings.Contains(plan, "edges_by_unresolved") || strings.Contains(plan, "scan edges") {
			t.Fatalf("partial unresolved query plan is not indexed:\n%s", plan)
		}
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("migrate v5 store: %v", err)
	}
	assertCompacted(s)
	if v, err := readUserVersion(s.db); err != nil || v != currentSchemaVersion {
		t.Fatalf("migrated user_version=%d err=%v, want %d,nil", v, err, currentSchemaVersion)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	// A normal warm reopen must preserve the compact shape and data without
	// repeating or undoing the migration.
	s, err = Open(path)
	if err != nil {
		t.Fatalf("warm reopen migrated store: %v", err)
	}
	defer s.Close()
	assertCompacted(s)
}

func TestOpenBackfillsSyntheticNodeRepoPrefixes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		rows := []string{
			"repo-a::module::go:net/http",
			"repo-a::stdlib::net/http::Get",
			"repo-b::builtin::len",
			"repo-b::external_call::example.com/pkg::Run",
			"dep::example.com/shared::Run",
			"external::example.com/shared::Run",
			"module::go:shared",
			"ordinary::node",
		}
		for _, id := range rows {
			if _, err := db.Exec(`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES (?, 'function', ?, '', '')`, id, id); err != nil {
				t.Fatalf("seed %q: %v", id, err)
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
			t.Fatalf("reset user_version: %v", err)
		}
	})

	s, err = Open(path)
	if err != nil {
		t.Fatalf("open v4 store: %v", err)
	}
	defer s.Close()

	want := map[string]string{
		"repo-a::module::go:net/http":                 "repo-a",
		"repo-a::stdlib::net/http::Get":               "repo-a",
		"repo-b::builtin::len":                        "repo-b",
		"repo-b::external_call::example.com/pkg::Run": "repo-b",
		"dep::example.com/shared::Run":                "",
		"external::example.com/shared::Run":           "",
		"module::go:shared":                           "",
		"ordinary::node":                              "",
	}
	rows, err := s.db.Query(`SELECT id, repo_prefix FROM nodes`)
	if err != nil {
		t.Fatalf("query migrated rows: %v", err)
	}
	defer rows.Close()
	seen := make(map[string]string, len(want))
	for rows.Next() {
		var id, repo string
		if err := rows.Scan(&id, &repo); err != nil {
			t.Fatalf("scan migrated row: %v", err)
		}
		seen[id] = repo
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migrated rows: %v", err)
	}
	for id, wantRepo := range want {
		if got := seen[id]; got != wantRepo {
			t.Errorf("repo_prefix for %q = %q, want %q", id, got, wantRepo)
		}
	}
	if v, err := readUserVersion(s.db); err != nil || v != currentSchemaVersion {
		t.Fatalf("user_version = %d (err %v), want %d", v, err, currentSchemaVersion)
	}
}

func TestOpenV4AddsCloneCorpusProjectionInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`DROP INDEX IF EXISTS clone_shingles_by_repo`); err != nil {
			t.Fatalf("drop clone index: %v", err)
		}
		if _, err := db.Exec(`DROP TABLE clone_shingles`); err != nil {
			t.Fatalf("drop clone table: %v", err)
		}
		if _, err := db.Exec(`CREATE TABLE clone_shingles (
node_id TEXT PRIMARY KEY, repo_prefix TEXT NOT NULL DEFAULT '', shingles BLOB
) WITHOUT ROWID`); err != nil {
			t.Fatalf("create v4 clone table: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO clone_shingles(node_id, repo_prefix, shingles) VALUES ('repo::f', 'repo', X'0100000000000000')`); err != nil {
			t.Fatalf("seed v4 clone row: %v", err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
			t.Fatalf("set v4: %v", err)
		}
	})

	store, err = Open(path)
	if err != nil {
		t.Fatalf("open v4 store: %v", err)
	}
	defer store.Close()
	page, err := store.CloneCorpusPage("repo", "", 10)
	if err != nil {
		t.Fatalf("read migrated clone projection: %v", err)
	}
	if len(page) != 1 || page[0].NodeID != "repo::f" || len(page[0].Shingles) != 1 || page[0].Shingles[0] != 1 {
		t.Fatalf("migrated clone row = %#v, want preserved v4 shingle row", page)
	}
	if page[0].Finalized || page[0].TokenCount != 0 {
		t.Fatalf("migrated clone defaults = finalized:%v tokens:%d, want pending/0", page[0].Finalized, page[0].TokenCount)
	}
	var indexCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='clone_shingles_by_repo'`).Scan(&indexCount); err != nil {
		t.Fatalf("query clone repo index: %v", err)
	}
	if indexCount != 1 {
		t.Fatalf("clone repo index count = %d, want 1", indexCount)
	}
}

// TestOpenDedupesFnValuePlaceholders drives the shipped v2 migration through the
// real Open composition: a store knocked back to the v1 baseline with duplicate
// fn-value placeholder edges is deduped in place on reopen — one survivor per
// (from_id, to_id), the MIN(id) row kept — while a distinct placeholder, a
// resolved edge, and an ordinary unresolved stub are untouched, and the version
// stamps to current. Covers both the bare and the multi-repo COPY-rewrite form.
func TestOpenDedupesFnValuePlaceholders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	// Seed edges by explicit id so the MIN(id) survivors are predictable. The
	// is_unresolved column is generated, so it is omitted from the INSERT.
	ins := `INSERT INTO edges (id, from_id, to_id, kind, file_path, line) VALUES (?,?,?,?,?,?)`
	seed := []struct {
		id       int
		from, to string
		kind     string
		line     int
	}{
		// Duplicate bare placeholders: same (from,to), distinct lines. Keep id 1.
		{1, "a", "unresolved::fnvalue::handler", "references", 10},
		{2, "a", "unresolved::fnvalue::handler", "references", 20},
		{3, "a", "unresolved::fnvalue::handler", "references", 30},
		// A distinct placeholder (different name) — must survive untouched.
		{4, "a", "unresolved::fnvalue::other", "references", 10},
		// Duplicate multi-repo COPY-rewrite placeholders — exercises the
		// is_unresolved infix branch of the migration. Keep id 5.
		{5, "b", "r::unresolved::fnvalue::handler", "references", 10},
		{6, "b", "r::unresolved::fnvalue::handler", "references", 20},
		// A resolved edge and an ordinary unresolved stub — never touched.
		{7, "a", "b", "calls", 1},
		{8, "a", "unresolved::Foo", "calls", 1},
	}
	for _, r := range seed {
		if _, err := s.writerDB.Exec(ins, r.id, r.from, r.to, r.kind, "f.go", r.line); err != nil {
			t.Fatalf("seed edge %d: %v", r.id, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Knock the store back to the v1 baseline so the reopen runs the v2 dedup.
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
			t.Fatalf("reset to v1 baseline: %v", err)
		}
	})

	// Exercise the historical v1->v2 in-place migration in isolation. The
	// shipped v3 boundary intentionally rebuilds every v2-or-older graph.
	s2, err := openWith(path, 2, schemaMigrations[:1], false)
	if err != nil {
		t.Fatalf("reopen for dedup: %v", err)
	}
	defer s2.Close()

	if v, _ := readUserVersion(s2.db); v != 2 {
		t.Fatalf("user_version after dedup = %d, want 2", v)
	}

	present := func(id int) bool {
		var n int
		if err := s2.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count id %d: %v", id, err)
		}
		return n == 1
	}
	// Bare-form dedup keeps the MIN(id) survivor and drops the rest.
	if !present(1) || present(2) || present(3) {
		t.Fatalf("bare dedup wrong: want keep 1 / drop 2,3; got 1=%v 2=%v 3=%v", present(1), present(2), present(3))
	}
	// A distinct placeholder pair survives.
	if !present(4) {
		t.Fatal("distinct fn-value placeholder (id 4) was wrongly deleted")
	}
	// Multi-repo infix dedup keeps the MIN(id) survivor and drops the rest.
	if !present(5) || present(6) {
		t.Fatalf("multi-repo dedup wrong: want keep 5 / drop 6; got 5=%v 6=%v", present(5), present(6))
	}
	// A resolved edge and an ordinary unresolved stub must be untouched.
	if !present(7) {
		t.Fatal("resolved edge (id 7) must survive the placeholder dedup")
	}
	if !present(8) {
		t.Fatal("ordinary unresolved stub (id 8) must survive the placeholder dedup")
	}
}
