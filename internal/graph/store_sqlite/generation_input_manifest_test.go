package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func manifestFixtureEntries() []InputManifestEntry {
	// Deliberately unsorted: the read returns file_path order.
	return []InputManifestEntry{
		{FilePath: "pkg/z.go", State: InputManifestPresent, Admission: InputManifestAdmitted, Mode: "100644", ContentSHA256: "sha-z"},
		{FilePath: "pkg/a.go", State: InputManifestAbsent, Admission: InputManifestNotApplicable},
		{FilePath: "vendor/link", State: InputManifestOpaque, Admission: InputManifestExcluded, Mode: "120000"},
		{FilePath: "pkg/undo.go", State: InputManifestHeadEqual, Admission: InputManifestAdmitted, Mode: "100755", ContentSHA256: "sha-head"},
		{FilePath: "assets/big.bin", State: InputManifestPresent, Admission: InputManifestOversized, Mode: "100644", ContentSHA256: "sha-big"},
	}
}

func sortedManifestEntries(entries []InputManifestEntry) []InputManifestEntry {
	out := append([]InputManifestEntry(nil), entries...)
	sort.Slice(out, func(i, j int) bool { return out[i].FilePath < out[j].FilePath })
	return out
}

func manifestRowCounts(t *testing.T, store *Store, generationID int64) (entries, meta int) {
	t.Helper()
	return countAtGeneration(t, store, "generation_input_manifest", generationID),
		countAtGeneration(t, store, "generation_input_manifest_meta", generationID)
}

func beginManifestGeneration(t *testing.T) (*Store, int64, *Store) {
	t.Helper()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	return store, generationID, handle
}

// TestInputManifestRoundTrip writes a manifest through a building
// generation's handle and reads it back — before publish through the same
// handle, after publish through a fresh one — and pins the refusals of the
// write-side validation.
func TestInputManifestRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)

	if meta, entries, found, err := handle.InputManifest(ctx); err != nil || found || len(entries) != 0 || meta != (InputManifestMeta{}) {
		t.Fatalf("manifest before any write = %+v, %v, %v, %v; want absent", meta, entries, found, err)
	}

	entries := manifestFixtureEntries()
	meta := InputManifestMeta{IsFull: true, PolicyDigest: "policy-1"}
	if err := handle.WriteInputManifest(ctx, meta, entries); err != nil {
		t.Fatalf("WriteInputManifest: %v", err)
	}
	wantMeta := InputManifestMeta{ManifestVersion: InputManifestVersion, IsFull: true, EntryCount: len(entries), PolicyDigest: "policy-1"}
	gotMeta, gotEntries, found, err := handle.InputManifest(ctx)
	if err != nil || !found {
		t.Fatalf("InputManifest = found %v, err %v", found, err)
	}
	if gotMeta != wantMeta {
		t.Fatalf("meta = %+v, want %+v", gotMeta, wantMeta)
	}
	if want := sortedManifestEntries(entries); !reflect.DeepEqual(gotEntries, want) {
		t.Fatalf("entries = %+v, want %+v", gotEntries, want)
	}

	// A second write while building replaces the whole manifest: no stale
	// entry from the first write survives, and the meta follows.
	delta := []InputManifestEntry{
		{FilePath: "pkg/z.go", State: InputManifestPresent, Admission: InputManifestAdmitted, Mode: "100644", ContentSHA256: "sha-z2"},
	}
	if err := handle.WriteInputManifest(ctx, InputManifestMeta{ManifestVersion: InputManifestVersion, EntryCount: 1, PolicyDigest: "policy-1"}, delta); err != nil {
		t.Fatalf("WriteInputManifest replace: %v", err)
	}
	gotMeta, gotEntries, found, err = handle.InputManifest(ctx)
	if err != nil || !found || gotMeta.IsFull || gotMeta.EntryCount != 1 || !reflect.DeepEqual(gotEntries, delta) {
		t.Fatalf("replaced manifest = %+v, %+v, %v, %v", gotMeta, gotEntries, found, err)
	}

	// Refused writes change nothing.
	for name, bad := range map[string]struct {
		meta    InputManifestMeta
		entries []InputManifestEntry
	}{
		"empty_policy":    {InputManifestMeta{}, nil},
		"future_version":  {InputManifestMeta{ManifestVersion: InputManifestVersion + 1, PolicyDigest: "p"}, nil},
		"count_mismatch":  {InputManifestMeta{EntryCount: 3, PolicyDigest: "p"}, delta},
		"absolute_path":   {InputManifestMeta{PolicyDigest: "p"}, []InputManifestEntry{{FilePath: "/abs.go", State: InputManifestPresent, Admission: InputManifestAdmitted}}},
		"unclean_path":    {InputManifestMeta{PolicyDigest: "p"}, []InputManifestEntry{{FilePath: "pkg/../x.go", State: InputManifestPresent, Admission: InputManifestAdmitted}}},
		"escaping_path":   {InputManifestMeta{PolicyDigest: "p"}, []InputManifestEntry{{FilePath: "../x.go", State: InputManifestPresent, Admission: InputManifestAdmitted}}},
		"backslash_path":  {InputManifestMeta{PolicyDigest: "p"}, []InputManifestEntry{{FilePath: `pkg\x.go`, State: InputManifestPresent, Admission: InputManifestAdmitted}}},
		"duplicate_path":  {InputManifestMeta{PolicyDigest: "p"}, []InputManifestEntry{delta[0], delta[0]}},
		"bad_state":       {InputManifestMeta{PolicyDigest: "p"}, []InputManifestEntry{{FilePath: "x.go", State: "modified", Admission: InputManifestAdmitted}}},
		"bad_admission":   {InputManifestMeta{PolicyDigest: "p"}, []InputManifestEntry{{FilePath: "x.go", State: InputManifestPresent, Admission: "maybe"}}},
		"empty_file_path": {InputManifestMeta{PolicyDigest: "p"}, []InputManifestEntry{{State: InputManifestPresent, Admission: InputManifestAdmitted}}},
	} {
		if err := handle.WriteInputManifest(ctx, bad.meta, bad.entries); !errors.Is(err, ErrInputManifestInvalid) {
			t.Fatalf("%s: WriteInputManifest = %v, want %v", name, err, ErrInputManifestInvalid)
		}
	}
	if _, got, _, err := handle.InputManifest(ctx); err != nil || !reflect.DeepEqual(got, delta) {
		t.Fatalf("manifest after refused writes = %+v, %v", got, err)
	}

	// Large enough to cross the multi-row INSERT chunk several times.
	many := make([]InputManifestEntry, 0, 3*inputManifestChunk+7)
	for i := 0; i < cap(many); i++ {
		many = append(many, InputManifestEntry{
			FilePath: fmt.Sprintf("pkg/f%04d.go", i), State: InputManifestPresent,
			Admission: InputManifestAdmitted, Mode: "100644", ContentSHA256: fmt.Sprintf("sha-%d", i),
		})
	}
	if err := handle.WriteInputManifest(ctx, InputManifestMeta{IsFull: true, PolicyDigest: "policy-2"}, many); err != nil {
		t.Fatalf("WriteInputManifest many: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 2000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}
	gotMeta, gotEntries, found, err = store.AtGeneration(generationID).InputManifest(ctx)
	if err != nil || !found || gotMeta.EntryCount != len(many) || gotMeta.PolicyDigest != "policy-2" || !gotMeta.IsFull {
		t.Fatalf("published manifest meta = %+v, %v, %v", gotMeta, found, err)
	}
	if !reflect.DeepEqual(gotEntries, many) {
		t.Fatalf("published manifest holds %d entries, want the %d written", len(gotEntries), len(many))
	}

	// A meta row whose entries went missing is not a manifest.
	if _, err := store.writerDB.Exec(`DELETE FROM generation_input_manifest WHERE view_gen = ? AND file_path = ?`,
		generationID, many[0].FilePath); err != nil {
		t.Fatalf("drop one entry: %v", err)
	}
	if _, _, found, err := store.AtGeneration(generationID).InputManifest(ctx); !errors.Is(err, ErrInputManifestIncomplete) || found {
		t.Fatalf("torn manifest = found %v, err %v; want %v", found, err, ErrInputManifestIncomplete)
	}
}

// TestInputManifestSealedAfterPublish proves the manifest rides the payload
// write gate: once published, neither the builder's handle nor a fresh one
// can write it, while reading it stays open.
func TestInputManifestSealedAfterPublish(t *testing.T) {
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)
	entries := manifestFixtureEntries()
	if err := handle.WriteInputManifest(ctx, InputManifestMeta{IsFull: true, PolicyDigest: "policy-1"}, entries); err != nil {
		t.Fatalf("WriteInputManifest: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 3000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}
	late := []InputManifestEntry{{FilePath: "late.go", State: InputManifestPresent, Admission: InputManifestAdmitted}}
	for name, target := range map[string]*Store{"builder_handle": handle, "fresh_handle": store.AtGeneration(generationID)} {
		if err := target.WriteInputManifest(ctx, InputManifestMeta{PolicyDigest: "policy-late"}, late); !errors.Is(err, ErrPayloadGenerationSealed) {
			t.Fatalf("%s: WriteInputManifest after publish = %v, want %v", name, err, ErrPayloadGenerationSealed)
		}
		meta, got, found, err := target.InputManifest(ctx)
		if err != nil || !found || meta.PolicyDigest != "policy-1" || !reflect.DeepEqual(got, sortedManifestEntries(entries)) {
			t.Fatalf("%s: manifest after refused write = %+v, %+v, %v, %v", name, meta, got, found, err)
		}
	}
	if n, m := manifestRowCounts(t, store, generationID); n != len(entries) || m != 1 {
		t.Fatalf("rows after refused writes = %d entries, %d meta; want %d, 1", n, m, len(entries))
	}
}

// TestInputManifestRetiredWithGeneration proves retirement sweeps both
// manifest tables, and that the sweep enumeration names them.
func TestInputManifestRetiredWithGeneration(t *testing.T) {
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)
	writePayloadOverlay(t, handle)
	entries := manifestFixtureEntries()
	if err := handle.WriteInputManifest(ctx, InputManifestMeta{IsFull: true, PolicyDigest: "policy-1"}, entries); err != nil {
		t.Fatalf("WriteInputManifest: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 4000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}

	swept := map[string]bool{}
	for _, table := range payloadSweepTables() {
		swept[table] = true
	}
	for _, table := range []string{"generation_input_manifest", "generation_input_manifest_meta"} {
		if !swept[table] {
			t.Fatalf("payloadSweepTables does not name %s", table)
		}
	}

	if n, m := manifestRowCounts(t, store, generationID); n != len(entries) || m != 1 {
		t.Fatalf("rows before retire = %d entries, %d meta", n, m)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("RetirePayloadGeneration: %v", err)
	}
	if n, m := manifestRowCounts(t, store, generationID); n != 0 || m != 0 {
		t.Fatalf("rows after retire = %d entries, %d meta; want none", n, m)
	}
	if _, found, err := store.Catalog().GetViewGeneration(ctx, generationID); err != nil || found {
		t.Fatalf("catalog row after retire = %v, %v", found, err)
	}
}

// TestInputManifestRefusedOnBaseGeneration proves the base corpus, which has
// no sample, can neither write nor read a manifest.
func TestInputManifestRefusedOnBaseGeneration(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	entries := manifestFixtureEntries()
	for name, base := range map[string]*Store{"store": store, "at_zero": store.AtGeneration(baseViewGeneration)} {
		if err := base.WriteInputManifest(ctx, InputManifestMeta{IsFull: true, PolicyDigest: "p"}, entries); !errors.Is(err, ErrInputManifestAtBaseGeneration) {
			t.Fatalf("%s: WriteInputManifest = %v, want %v", name, err, ErrInputManifestAtBaseGeneration)
		}
		// An empty write is refused too, so a misuse cannot hide until rows exist.
		if err := base.WriteInputManifest(ctx, InputManifestMeta{PolicyDigest: "p"}, nil); !errors.Is(err, ErrInputManifestAtBaseGeneration) {
			t.Fatalf("%s: empty WriteInputManifest = %v, want %v", name, err, ErrInputManifestAtBaseGeneration)
		}
		if _, _, found, err := base.InputManifest(ctx); !errors.Is(err, ErrInputManifestAtBaseGeneration) || found {
			t.Fatalf("%s: InputManifest = found %v, err %v; want %v", name, found, err, ErrInputManifestAtBaseGeneration)
		}
	}
	var total int
	if err := store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM generation_input_manifest) + (SELECT COUNT(*) FROM generation_input_manifest_meta)`).Scan(&total); err != nil {
		t.Fatalf("count manifest rows: %v", err)
	}
	if total != 0 {
		t.Fatalf("base refusals left %d manifest rows", total)
	}
}

// inputManifestSchemaObjects returns the normalized CREATE text of every
// manifest schema object, keyed by name.
func inputManifestSchemaObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	tables := map[string]struct{}{}
	for _, table := range generationInputManifestTables {
		tables[table.table] = struct{}{}
	}
	rows, err := db.Query(`SELECT name, tbl_name, sql FROM sqlite_schema WHERE sql IS NOT NULL`)
	if err != nil {
		t.Fatalf("read sqlite_schema: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, table, ddl string
		if err := rows.Scan(&name, &table, &ddl); err != nil {
			t.Fatalf("scan sqlite_schema: %v", err)
		}
		if _, ok := tables[table]; ok {
			out[name] = normalizeManifestDDL(ddl)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_schema: %v", err)
	}
	return out
}

func normalizeManifestDDL(ddl string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(ddl, `"`, "")), " ")
}

// TestSchema29MigrationAddsInputManifestTables is the backward-compatibility
// proof for v29: a store stamped 28 without the manifest tables gains both on
// its next Open in place (no rebuild, rows kept), ends up with DDL identical to
// a fresh store's, and the new tables are usable.
func TestSchema29MigrationAddsInputManifestTables(t *testing.T) {
	if currentSchemaVersion < 29 {
		t.Fatalf("currentSchemaVersion = %d, want >= 29 for the input manifest", currentSchemaVersion)
	}
	var step *schemaMigration
	for i := range schemaMigrations {
		if schemaMigrations[i].version == 29 {
			step = &schemaMigrations[i]
			break
		}
	}
	if step == nil || step.rebuild || step.inPlace == nil {
		t.Fatalf("v29 migration = %+v, want a registered in-place step", step)
	}

	path := filepath.Join(t.TempDir(), "pre-input-manifest.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	seed.AddBatch([]*graph.Node{{
		ID: "repo/a.go::Legacy", Kind: graph.KindFunction, Name: "Legacy",
		FilePath: "repo/a.go", RepoPrefix: "repo",
	}}, nil)
	fresh := inputManifestSchemaObjects(t, seed.writerDB)
	if len(fresh) != len(generationInputManifestTables) {
		t.Fatalf("fresh store has %d manifest schema objects, want %d: %v", len(fresh), len(generationInputManifestTables), fresh)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		for _, table := range generationInputManifestTables {
			execDDL(t, db, `DROP TABLE IF EXISTS `+table.table)
		}
		execDDL(t, db, `PRAGMA user_version = 28`)
	})

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("reopen v28 store: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })
	if migrated.NeedsRebuild() {
		t.Fatal("an additive manifest upgrade must not signal a wipe/reindex")
	}
	if version, err := readUserVersion(migrated.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("post-migration user_version = %d (err %v), want %d", version, err, currentSchemaVersion)
	}
	if migrated.GetNode("repo/a.go::Legacy") == nil {
		t.Fatal("existing graph rows must survive the in-place manifest upgrade")
	}
	got := inputManifestSchemaObjects(t, migrated.writerDB)
	if !reflect.DeepEqual(got, fresh) {
		t.Fatalf("migrated manifest DDL differs from fresh:\n migrated: %v\n    fresh: %v", got, fresh)
	}

	derived := migrated.AtGeneration(7)
	entries := []InputManifestEntry{{FilePath: "repo/a.go", State: InputManifestPresent, Admission: InputManifestAdmitted, Mode: "100644", ContentSHA256: "sha"}}
	if err := derived.WriteInputManifest(context.Background(), InputManifestMeta{IsFull: true, PolicyDigest: "p"}, entries); err != nil {
		t.Fatalf("write migrated manifest: %v", err)
	}
	if _, read, found, err := derived.InputManifest(context.Background()); err != nil || !found || !reflect.DeepEqual(read, entries) {
		t.Fatalf("read migrated manifest = %+v, %v, %v", read, found, err)
	}

	// Re-running the step on a populated store keeps the rows.
	tx, err := migrated.writerDB.Begin()
	if err != nil {
		t.Fatalf("begin rerun: %v", err)
	}
	if err := createGenerationInputManifestTables(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("rerun v29 step: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rerun: %v", err)
	}
	if _, read, found, err := derived.InputManifest(context.Background()); err != nil || !found || len(read) != 1 {
		t.Fatalf("manifest after rerun = %+v, %v, %v", read, found, err)
	}
}
