package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// generationMaskSchemaObjects returns the normalized CREATE text of every mask
// table, keyed by object name. Whitespace and quoting are normalized so a
// migrated shape and a fresh one compare on structure alone.
func generationMaskSchemaObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	tables := make(map[string]struct{}, len(generationMaskTables))
	for _, mask := range generationMaskTables {
		tables[mask.table] = struct{}{}
	}
	rows, err := db.Query(`SELECT name, tbl_name, sql FROM sqlite_schema WHERE sql IS NOT NULL`)
	if err != nil {
		t.Fatalf("read sqlite_schema: %v", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, table, ddl string
		if err := rows.Scan(&name, &table, &ddl); err != nil {
			t.Fatalf("scan sqlite_schema: %v", err)
		}
		if _, ok := tables[table]; !ok {
			continue
		}
		out[name] = strings.Join(strings.Fields(strings.ReplaceAll(ddl, `"`, "")), " ")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_schema: %v", err)
	}
	return out
}

// TestSchemaV17StoreGainsGenerationMaskTables is the backward-compatibility
// proof for v18: a store written before the ownership masks existed gains every
// mask table on its next Open, keeps its graph rows, signals no rebuild, and
// ends up shaped exactly like a store this build creates from scratch.
func TestSchemaV17StoreGainsGenerationMaskTables(t *testing.T) {
	if currentSchemaVersion < 18 {
		t.Fatalf("currentSchemaVersion = %d, want >= 18 for the generation masks", currentSchemaVersion)
	}
	var step *schemaMigration
	for i := range schemaMigrations {
		if schemaMigrations[i].version == 18 {
			step = &schemaMigrations[i]
			break
		}
	}
	if step == nil || step.rebuild || step.inPlace == nil {
		t.Fatalf("v18 migration = %+v, want a registered in-place step", step)
	}

	path := filepath.Join(t.TempDir(), "pre-generation-masks.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	seed.AddBatch([]*graph.Node{{
		ID: "repo/a.go::Legacy", Kind: graph.KindFunction, Name: "Legacy",
		FilePath: "repo/a.go", RepoPrefix: maskTestRepo,
	}}, nil)
	fresh := generationMaskSchemaObjects(t, seed.writerDB)
	if len(fresh) != len(generationMaskTables) {
		t.Fatalf("fresh store has %d mask schema objects, want %d", len(fresh), len(generationMaskTables))
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	// Recreate the exact pre-mask shape: the graph exists, the mask tables do
	// not, and the file is stamped at the version before they shipped.
	withRawDB(t, path, func(db *sql.DB) {
		for _, mask := range generationMaskTables {
			execDDL(t, db, `DROP TABLE IF EXISTS `+mask.table)
		}
		execDDL(t, db, `PRAGMA user_version = 17`)
	})

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("reopen pre-mask store: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	if migrated.NeedsRebuild() {
		t.Fatal("an additive mask upgrade must not signal a wipe/reindex")
	}
	if version, err := readUserVersion(migrated.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("post-migration user_version = %d (err %v), want %d", version, err, currentSchemaVersion)
	}
	if migrated.GetNode("repo/a.go::Legacy") == nil {
		t.Fatal("existing graph rows must survive the in-place mask upgrade")
	}
	for _, mask := range generationMaskTables {
		if !hasTable(t, migrated.writerDB, mask.table) {
			t.Fatalf("migrated store is missing mask table %s", mask.table)
		}
	}

	// The upgraded store is usable, not merely present.
	derived := migrated.AtGeneration(1)
	if err := derived.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/a.go", Mode: OwnershipReplace},
	}); err != nil {
		t.Fatalf("write to migrated mask tables: %v", err)
	}
	if got, err := derived.FileMasks(); err != nil || len(got) != 1 {
		t.Fatalf("read from migrated mask tables = %+v (err %v)", got, err)
	}

	// The migrated shape matches a store this build creates from scratch.
	got := generationMaskSchemaObjects(t, migrated.writerDB)
	if len(got) != len(fresh) {
		t.Fatalf("migrated store has %d mask schema objects, fresh store has %d", len(got), len(fresh))
	}
	names := make([]string, 0, len(fresh))
	for name := range fresh {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if got[name] != fresh[name] {
			t.Fatalf("mask object %q differs after migration:\n migrated: %s\n    fresh: %s", name, got[name], fresh[name])
		}
	}
}

// TestGenerationMaskMigrationIsIdempotent covers the ordering the step lives
// under: schemaSQL runs before the migration steps, so on a fresh store the
// mask tables already exist when v18 runs. Re-running it must keep the rows
// rather than recreate the tables empty.
func TestGenerationMaskMigrationIsIdempotent(t *testing.T) {
	store := openMaskStore(t)
	derived := store.AtGeneration(1)
	if err := derived.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/a.go", Mode: OwnershipReplace},
	}); err != nil {
		t.Fatalf("seed mask: %v", err)
	}
	if err := derived.SetNodeTombstones([]string{"repo/a.go::Gone"}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}

	for i := 0; i < 2; i++ {
		tx, err := store.writerDB.Begin()
		if err != nil {
			t.Fatalf("begin migration tx %d: %v", i, err)
		}
		if err := createGenerationMaskTables(tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("createGenerationMaskTables run %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit migration tx %d: %v", i, err)
		}
	}

	masks, err := derived.FileMasks()
	if err != nil || len(masks) != 1 || masks[0].Mode != OwnershipReplace {
		t.Fatalf("file masks after repeated migration runs = %+v (err %v)", masks, err)
	}
	tombstones, err := derived.NodeTombstones()
	if err != nil || len(tombstones) != 1 {
		t.Fatalf("tombstones after repeated migration runs = %v (err %v)", tombstones, err)
	}
}
