package store_sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// legacySidecar is one payload sidecar exactly as schema v14 wrote it: no
// view_gen column anywhere, and secondary indexes that lead with repo_prefix
// or file_path. Spelled out literally rather than derived from the current
// registry so the fixture keeps describing v14 even as the live schema moves
// on — a downgrade computed from viewGenSidecars would silently track it and
// stop testing the migration at all.
type legacySidecar struct {
	body    string
	indexes []string
}

var legacySidecarShapes = map[string]legacySidecar{
	"file_mtimes": {body: ` (
    repo_prefix TEXT NOT NULL,
    file_path   TEXT NOT NULL,
    mtime_ns    INTEGER NOT NULL,
    PRIMARY KEY (repo_prefix, file_path)
) WITHOUT ROWID`},
	"repo_index_state": {body: ` (
    repo_prefix        TEXT PRIMARY KEY,
    indexed_sha        TEXT NOT NULL DEFAULT '',
    dirty              INTEGER NOT NULL DEFAULT 0,
    indexed_at         INTEGER NOT NULL DEFAULT 0,
    workspace_fp       TEXT NOT NULL DEFAULT '',
    node_count         INTEGER NOT NULL DEFAULT 0,
    edge_count         INTEGER NOT NULL DEFAULT 0,
    extractor_versions TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID`},
	"enrichment_state": {body: ` (
    repo_prefix  TEXT NOT NULL,
    provider     TEXT NOT NULL,
    indexed_sha  TEXT NOT NULL DEFAULT '',
    completed_at INTEGER NOT NULL DEFAULT 0,
    coverage     REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_prefix, provider)
) WITHOUT ROWID`},
	"contract_state": {body: ` (
    repo_prefix    TEXT PRIMARY KEY,
    indexed_sha    TEXT NOT NULL DEFAULT '',
    completed_at   INTEGER NOT NULL DEFAULT 0,
    contract_count INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID`},
	"clone_shingles": {
		body: ` (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    shingles    BLOB,
    signature   TEXT,
    token_count INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID`,
		indexes: []string{`CREATE INDEX clone_shingles_by_repo ON clone_shingles(repo_prefix, node_id)`},
	},
	"clone_corpus_state": {body: ` (
    repo_prefix TEXT PRIMARY KEY
) WITHOUT ROWID`},
	"constant_values": {
		body: ` (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT '',
    value       TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID`,
		indexes: []string{`CREATE INDEX constant_values_by_file ON constant_values(repo_prefix, file_path)`},
	},
	"semantic_binding_types": {body: ` (
    repo_prefix TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL,
    line        INTEGER NOT NULL DEFAULT 0,
    name        TEXT NOT NULL DEFAULT '',
    type_name   TEXT NOT NULL,
    PRIMARY KEY (repo_prefix, file_path, line, name)
) WITHOUT ROWID`},
	"files": {
		body: ` (
    repo_prefix  TEXT NOT NULL DEFAULT '',
    file_path    TEXT NOT NULL,
    content_hash TEXT NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    node_count   INTEGER NOT NULL DEFAULT 0,
    errors       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (repo_prefix, file_path)
) WITHOUT ROWID`,
		indexes: []string{`CREATE INDEX files_with_errors ON files(repo_prefix) WHERE errors <> ''`},
	},
	"ref_facts": {
		body: ` (
    repo_prefix TEXT NOT NULL DEFAULT '',
    from_id     TEXT NOT NULL,
    to_id       TEXT NOT NULL,
    kind        TEXT NOT NULL,
    ref_name    TEXT NOT NULL DEFAULT '',
    line        INTEGER NOT NULL DEFAULT 0,
    origin      TEXT NOT NULL DEFAULT '',
    tier        TEXT NOT NULL DEFAULT '',
    candidates  TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT '',
    lang        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (repo_prefix, from_id, to_id, kind, line)
) WITHOUT ROWID`,
		indexes: []string{
			`CREATE INDEX ref_facts_by_file ON ref_facts(repo_prefix, file_path)`,
			`CREATE INDEX ref_facts_by_target ON ref_facts(repo_prefix, to_id)`,
		},
	},
	"vectors": {
		body: ` (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    parent_id   TEXT NOT NULL DEFAULT '',
    dims        INTEGER NOT NULL,
    vec         BLOB NOT NULL
) WITHOUT ROWID`,
		indexes: []string{`CREATE INDEX vectors_by_repo ON vectors(repo_prefix, node_id)`},
	},
	"churn_enrichment": {
		body: ` (
    node_id        TEXT PRIMARY KEY,
    repo_prefix    TEXT NOT NULL DEFAULT '',
    commit_count   INTEGER NOT NULL DEFAULT 0,
    age_days       INTEGER NOT NULL DEFAULT 0,
    churn_rate     REAL NOT NULL DEFAULT 0,
    last_author    TEXT NOT NULL DEFAULT '',
    last_commit_at TEXT NOT NULL DEFAULT '',
    head_sha       TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    computed_at    TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID`,
		indexes: []string{`CREATE INDEX churn_by_repo ON churn_enrichment(repo_prefix) WHERE repo_prefix <> ''`},
	},
	"coverage_enrichment": {
		body: ` (
    node_id      TEXT PRIMARY KEY,
    repo_prefix  TEXT NOT NULL DEFAULT '',
    coverage_pct REAL NOT NULL DEFAULT 0,
    num_stmt     INTEGER NOT NULL DEFAULT 0,
    hit          INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID`,
		indexes: []string{`CREATE INDEX coverage_by_repo ON coverage_enrichment(repo_prefix) WHERE repo_prefix <> ''`},
	},
	"release_enrichment": {
		body: ` (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    added_in    TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID`,
		indexes: []string{`CREATE INDEX release_by_repo ON release_enrichment(repo_prefix) WHERE repo_prefix <> ''`},
	},
	"blame_enrichment": {
		body: ` (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    commit_sha  TEXT NOT NULL DEFAULT '',
    email       TEXT NOT NULL DEFAULT '',
    ts          INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID`,
		indexes: []string{`CREATE INDEX blame_by_repo ON blame_enrichment(repo_prefix) WHERE repo_prefix <> ''`},
	},
	"symbol_fts_state": {body: ` (
    repo_prefix  TEXT PRIMARY KEY,
    normalization TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID`},
	"symbol_fts_rowid": {
		body: ` (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    fts_rowid   INTEGER NOT NULL
) WITHOUT ROWID`,
		indexes: []string{
			`CREATE UNIQUE INDEX symbol_fts_rowid_by_rowid ON symbol_fts_rowid(fts_rowid)`,
			`CREATE INDEX symbol_fts_rowid_by_repo ON symbol_fts_rowid(repo_prefix, fts_rowid)`,
		},
	},
	"content_fts_rowid": {
		body: ` (
    fts_rowid   INTEGER PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID`,
		indexes: []string{
			`CREATE INDEX content_fts_rowid_by_repo_file ON content_fts_rowid(repo_prefix, file_path, fts_rowid)`,
			`CREATE INDEX content_fts_rowid_by_file ON content_fts_rowid(file_path, fts_rowid)`,
		},
	},
}

const sidecarSeedRepo = "repo"

// seedSidecarRows writes one representative row into every generation-keyed
// sidecar through the store's public capability surface, plus the graph nodes
// the FTS pair joins back to.
func seedSidecarRows(t *testing.T, store *Store) {
	t.Helper()

	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha", FilePath: "repo/a.go", RepoPrefix: sidecarSeedRepo},
		{ID: "repo/a.go::Beta", Kind: graph.KindFunction, Name: "Beta", FilePath: "repo/a.go", RepoPrefix: sidecarSeedRepo},
		{ID: "repo/a.go::Gamma", Kind: graph.KindConstant, Name: "Gamma", FilePath: "repo/a.go", RepoPrefix: sidecarSeedRepo},
	}, nil)

	mustNoErr(t, "file mtimes", store.BulkSetFileMtimes(sidecarSeedRepo, map[string]int64{"a.go": 11}))
	mustNoErr(t, "repo index state", store.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: sidecarSeedRepo, IndexedSHA: "sha-1", Dirty: true, IndexedAt: 42,
		WorkspaceFP: "fp-1", NodeCount: 3, EdgeCount: 0, ExtractorVersions: `{"go":1}`,
	}))
	mustNoErr(t, "enrichment state", store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: sidecarSeedRepo, Provider: "gopls", IndexedSHA: "sha-1", CompletedAt: 43, Coverage: 0.5,
	}))
	mustNoErr(t, "contract state", store.SetContractState(graph.ContractState{
		RepoPrefix: sidecarSeedRepo, IndexedSHA: "sha-1", CompletedAt: 44, ContractCount: 2,
	}))
	mustNoErr(t, "clone corpus", store.BulkSetCloneCorpus(sidecarSeedRepo, []graph.CloneCorpusRow{{
		NodeID: "repo/a.go::Alpha", RepoPrefix: sidecarSeedRepo,
		Shingles: []uint64{7, 8}, Signature: "sig-1", TokenCount: 9, Finalized: true,
	}}))
	mustNoErr(t, "constant values", store.BulkSetConstantValues(sidecarSeedRepo, []graph.ConstantValueRow{{
		NodeID: "repo/a.go::Gamma", FilePath: "repo/a.go", Value: "const-1",
	}}))
	mustNoErr(t, "semantic bindings", store.ReplaceSemanticBindingTypes(sidecarSeedRepo, []graph.SemanticBindingType{{
		Site:     graph.SemanticBindingSite{RepoPrefix: sidecarSeedRepo, FilePath: "repo/a.go", Line: 3, Name: "x"},
		TypeName: "string",
	}}))
	mustNoErr(t, "file metas", store.SetFileMetas(sidecarSeedRepo, []graph.FileMetaRow{{
		FilePath: "repo/a.go", ContentHash: "hash-a", Size: 12, NodeCount: 3, Errors: "[1]",
	}}))
	mustNoErr(t, "ref facts", store.BulkSetRefFacts(sidecarSeedRepo, []graph.RefFact{{
		FromID: "repo/a.go::Alpha", ToID: "repo/a.go::Beta", Kind: "calls", RefName: "Beta",
		Line: 5, Origin: "ast_resolved", Tier: "ast", Candidates: []string{"repo/a.go::Beta"},
		FilePath: "repo/a.go", Lang: "go",
	}}))
	mustNoErr(t, "embedding", store.UpsertEmbedding("repo/a.go::Alpha", []float32{0.25, 0.5}))
	mustNoErr(t, "churn", store.BulkSetChurn(sidecarSeedRepo, []graph.ChurnEnrichment{{
		NodeID: "repo/a.go::Alpha", CommitCount: 4, AgeDays: 5, ChurnRate: 0.8,
		LastAuthor: "ann", LastCommitAt: "2020-01-01T00:00:00Z", HeadSHA: "sha-1",
		Branch: "main", ComputedAt: "2020-01-02T00:00:00Z",
	}}))
	mustNoErr(t, "coverage", store.BulkSetCoverage(sidecarSeedRepo, []graph.CoverageEnrichment{{
		NodeID: "repo/a.go::Alpha", CoveragePct: 66.5, NumStmt: 10, Hit: 7,
	}}))
	mustNoErr(t, "releases", store.BulkSetReleases(sidecarSeedRepo, []graph.ReleaseEnrichment{{
		NodeID: "repo/a.go::Alpha", AddedIn: "v1.2.3",
	}}))
	mustNoErr(t, "blame", store.BulkSetBlame(sidecarSeedRepo, []graph.BlameEnrichment{{
		NodeID: "repo/a.go::Alpha", Commit: "sha-1", Email: "ann@example.com", Timestamp: 99,
	}}))
	mustNoErr(t, "symbol fts state", store.SetSymbolFTSNormalization(sidecarSeedRepo, "identifier"))
	mustNoErr(t, "symbol fts", store.BulkUpsertSymbolFTS(sidecarSeedRepo, []graph.SymbolFTSItem{
		{NodeID: "repo/a.go::Alpha", Tokens: "alpha sidecarseedtoken"},
		{NodeID: "repo/a.go::Beta", Tokens: "beta sidecarseedtoken"},
	}))
	mustNoErr(t, "content fts", store.AppendContent(sidecarSeedRepo, []graph.ContentFTSItem{{
		NodeID: "repo/doc.md::0", FilePath: "repo/doc.md", Ordinal: 0, Body: "sidecarcontenttoken in a document",
	}}))
	mustNoErr(t, "clone corpus marker", store.MarkCloneCorpusInitialized(sidecarSeedRepo))
}

func mustNoErr(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed %s: %v", what, err)
	}
}

// downgradeSidecarsToV14 rewrites every generation-keyed sidecar back to its
// v14 shape, keeping only the base-generation rows, and stamps the file at
// user_version 14. The result is a store the shipped v14 build could have
// written.
func downgradeSidecarsToV14(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, sidecar := range viewGenSidecars {
		legacy, ok := legacySidecarShapes[sidecar.table]
		if !ok {
			t.Fatalf("no v14 fixture shape for sidecar %q", sidecar.table)
		}
		old := sidecar.table + "_v14"
		execDDL(t, db, `CREATE TABLE `+old+legacy.body)
		execDDL(t, db, `INSERT INTO `+old+` (`+sidecar.columns+`)
SELECT `+sidecar.columns+` FROM `+sidecar.table+` WHERE view_gen = 0`)
		execDDL(t, db, `DROP TABLE `+sidecar.table)
		execDDL(t, db, `ALTER TABLE `+old+` RENAME TO `+sidecar.table)
		for _, ddl := range legacy.indexes {
			execDDL(t, db, ddl)
		}
	}
	execDDL(t, db, `PRAGMA user_version = 14`)
}

func execDDL(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

// sidecarSchemaObjects returns the normalized CREATE text of every
// generation-keyed sidecar table and index, keyed by object name. Quotes and
// whitespace are normalized because a table produced by ALTER TABLE … RENAME
// carries a quoted name and the fresh-store one does not.
func sidecarSchemaObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	tables := make(map[string]struct{}, len(viewGenSidecars))
	for _, sidecar := range viewGenSidecars {
		tables[sidecar.table] = struct{}{}
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

func rowViewGens(t *testing.T, db *sql.DB, table string) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT view_gen FROM ` + table)
	if err != nil {
		t.Fatalf("read %s view generations: %v", table, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var gen int64
		if err := rows.Scan(&gen); err != nil {
			t.Fatalf("scan %s view generation: %v", table, err)
		}
		out = append(out, gen)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s view generations: %v", table, err)
	}
	return out
}

func scalarInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// TestSchemaV14StoreGainsSidecarViewGenerationKeys is the backward-compatibility
// proof for v15: a store whose payload sidecars were written before view_gen
// existed is re-keyed on its next Open, every row survives in generation 0, the
// FTS docid joins still resolve, no rebuild is signalled, and the resulting
// shape is byte-identical (modulo quoting) to a store created fresh by this
// build.
func TestSchemaV14StoreGainsSidecarViewGenerationKeys(t *testing.T) {
	if currentSchemaVersion < 15 {
		t.Fatalf("currentSchemaVersion = %d, want >= 15 for the sidecar view generation keys", currentSchemaVersion)
	}
	var step *schemaMigration
	for i := range schemaMigrations {
		if schemaMigrations[i].version == 15 {
			step = &schemaMigrations[i]
			break
		}
	}
	if step == nil || step.rebuild || step.inPlace == nil {
		t.Fatalf("v15 migration = %+v, want a registered in-place step", step)
	}

	path := filepath.Join(t.TempDir(), "pre-sidecar-view-gen.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	seedSidecarRows(t, seed)
	fresh := sidecarSchemaObjects(t, seed.writerDB)
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) { downgradeSidecarsToV14(t, db) })

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("reopen v14 store: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	if migrated.NeedsRebuild() {
		t.Fatal("re-keying sidecars in place must not signal a wipe/reindex")
	}
	if version, err := readUserVersion(migrated.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("post-migration user_version = %d (err %v), want %d", version, err, currentSchemaVersion)
	}

	// Every sidecar kept exactly its seeded rows, all at the base generation.
	for _, sidecar := range viewGenSidecars {
		gens := rowViewGens(t, migrated.writerDB, sidecar.table)
		if len(gens) == 0 {
			t.Fatalf("%s lost every row across the migration", sidecar.table)
		}
		for _, gen := range gens {
			if gen != 0 {
				t.Fatalf("%s row landed at view_gen %d, want 0", sidecar.table, gen)
			}
		}
	}

	// Payload spot-checks through the public read surface.
	if got := migrated.LoadFileMtimes(sidecarSeedRepo); got["a.go"] != 11 {
		t.Fatalf("file mtimes after migration = %v, want a.go -> 11", got)
	}
	state, ok, err := migrated.GetRepoIndexState(sidecarSeedRepo)
	if err != nil || !ok || state.IndexedSHA != "sha-1" || state.NodeCount != 3 {
		t.Fatalf("repo index state after migration = %+v (ok %v, err %v)", state, ok, err)
	}
	metas, err := migrated.FileMetasForRepo(sidecarSeedRepo)
	if err != nil || len(metas) != 1 || metas[0].ContentHash != "hash-a" {
		t.Fatalf("file metas after migration = %+v (err %v)", metas, err)
	}
	facts, err := migrated.LoadRefFactsByFiles(sidecarSeedRepo, nil)
	if err != nil || len(facts) != 1 || facts[0].ToID != "repo/a.go::Beta" {
		t.Fatalf("ref facts after migration = %+v (err %v)", facts, err)
	}
	if vectors := migrated.GetEmbeddings([]string{"repo/a.go::Alpha"}); len(vectors) != 1 {
		t.Fatalf("embeddings after migration = %v, want the seeded vector", vectors)
	}
	if rows := migrated.ChurnRows(sidecarSeedRepo); len(rows) != 1 || rows[0].LastAuthor != "ann" {
		t.Fatalf("churn rows after migration = %+v", rows)
	}
	if mode, ok, err := migrated.GetSymbolFTSNormalization(sidecarSeedRepo); err != nil || !ok || mode != "identifier" {
		t.Fatalf("symbol FTS normalization after migration = %q (ok %v, err %v)", mode, ok, err)
	}
	if initialized, err := migrated.CloneCorpusInitialized(sidecarSeedRepo); err != nil || !initialized {
		t.Fatalf("clone corpus marker after migration = %v (err %v)", initialized, err)
	}

	// The FTS docid joins survived: both maps still resolve every document.
	if got := scalarInt(t, migrated.writerDB, `
SELECT COUNT(*) FROM symbol_fts AS f
JOIN symbol_fts_rowid AS o ON o.fts_rowid = f.rowid AND o.node_id = f.node_id
WHERE o.view_gen = 0`); got != 2 {
		t.Fatalf("symbol docid pairs after migration = %d, want 2", got)
	}
	if got := scalarInt(t, migrated.writerDB, `
SELECT COUNT(*) FROM content_fts AS f
JOIN content_fts_rowid AS o ON o.fts_rowid = f.rowid
WHERE o.view_gen = 0`); got != 1 {
		t.Fatalf("content docid pairs after migration = %d, want 1", got)
	}
	hits, err := migrated.SearchSymbols("sidecarseedtoken", 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("symbol search after migration returned %d hits (err %v), want 2", len(hits), err)
	}
	contentHits, err := migrated.SearchContent("sidecarcontenttoken", sidecarSeedRepo, 10)
	if err != nil || len(contentHits) != 1 {
		t.Fatalf("content search after migration returned %d hits (err %v), want 1", len(contentHits), err)
	}

	// The migrated shape matches a store this build creates from scratch.
	got := sidecarSchemaObjects(t, migrated.writerDB)
	if len(got) != len(fresh) {
		t.Fatalf("migrated store has %d sidecar schema objects, fresh store has %d", len(got), len(fresh))
	}
	names := make([]string, 0, len(fresh))
	for name := range fresh {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if got[name] != fresh[name] {
			t.Fatalf("sidecar object %q differs after migration:\n migrated: %s\n    fresh: %s", name, got[name], fresh[name])
		}
	}
}

// TestSchemaSidecarViewGenerationMigrationIsIdempotent covers the case the
// column probe exists for: schemaSQL runs before the migration steps, so a
// store whose sidecars were created by this build already carry view_gen when
// the v15 step runs. Re-running it must be a no-op rather than a duplicate
// table or a lost row.
func TestSchemaSidecarViewGenerationMigrationIsIdempotent(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "idempotent-sidecar-view-gen.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	seedSidecarRows(t, store)

	for i := 0; i < 2; i++ {
		tx, err := store.writerDB.Begin()
		if err != nil {
			t.Fatalf("begin migration tx %d: %v", i, err)
		}
		if err := addSidecarViewGenerationKeys(tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("addSidecarViewGenerationKeys run %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit migration tx %d: %v", i, err)
		}
	}

	for _, sidecar := range viewGenSidecars {
		if gens := rowViewGens(t, store.writerDB, sidecar.table); len(gens) == 0 {
			t.Fatalf("%s lost its rows across repeated migration runs", sidecar.table)
		}
	}
	if got := migratedSidecarLeftovers(t, store.writerDB); len(got) > 0 {
		t.Fatalf("repeated migration runs left rebuild tables behind: %v", got)
	}
}

func migratedSidecarLeftovers(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE type = 'table' AND name LIKE '%_view_gen_rebuild'`)
	if err != nil {
		t.Fatalf("scan for rebuild leftovers: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan leftover name: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate leftovers: %v", err)
	}
	return out
}

// TestSidecarUpsertsRetargetConflictToViewGeneration proves the write path
// follows the new primary keys: writing the same logical row twice through the
// public API still leaves one row, carrying the second write's payload. Before
// the conflict clauses were retargeted the second write either duplicated the
// row or failed the constraint.
func TestSidecarUpsertsRetargetConflictToViewGeneration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "sidecar-upsert.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha", FilePath: "repo/a.go", RepoPrefix: sidecarSeedRepo},
	}, nil)

	mustNoErr(t, "first mtime", store.SetFileMtime(sidecarSeedRepo, "a.go", 1))
	mustNoErr(t, "second mtime", store.SetFileMtime(sidecarSeedRepo, "a.go", 2))
	if got := store.LoadFileMtimes(sidecarSeedRepo); len(got) != 1 || got["a.go"] != 2 {
		t.Fatalf("file mtimes after re-upsert = %v, want one row at 2", got)
	}

	for _, sha := range []string{"sha-1", "sha-2"} {
		mustNoErr(t, "repo index state", store.SetRepoIndexState(graph.RepoIndexState{
			RepoPrefix: sidecarSeedRepo, IndexedSHA: sha, NodeCount: 1,
		}))
	}
	if got := scalarInt(t, store.writerDB, `SELECT COUNT(*) FROM repo_index_state`); got != 1 {
		t.Fatalf("repo_index_state rows after re-upsert = %d, want 1", got)
	}
	if state, ok, err := store.GetRepoIndexState(sidecarSeedRepo); err != nil || !ok || state.IndexedSHA != "sha-2" {
		t.Fatalf("repo index state after re-upsert = %+v (ok %v, err %v)", state, ok, err)
	}

	for _, mode := range []string{"raw", "identifier"} {
		mustNoErr(t, "symbol fts state", store.SetSymbolFTSNormalization(sidecarSeedRepo, mode))
	}
	if got := scalarInt(t, store.writerDB, `SELECT COUNT(*) FROM symbol_fts_state`); got != 1 {
		t.Fatalf("symbol_fts_state rows after re-upsert = %d, want 1", got)
	}
	if mode, ok, err := store.GetSymbolFTSNormalization(sidecarSeedRepo); err != nil || !ok || mode != "identifier" {
		t.Fatalf("symbol FTS normalization after re-upsert = %q (ok %v, err %v)", mode, ok, err)
	}

	for _, tokens := range []int{5, 6} {
		mustNoErr(t, "clone corpus", store.BulkSetCloneCorpus(sidecarSeedRepo, []graph.CloneCorpusRow{{
			NodeID: "repo/a.go::Alpha", RepoPrefix: sidecarSeedRepo,
			Shingles: []uint64{1}, Signature: fmt.Sprintf("sig-%d", tokens), TokenCount: tokens, Finalized: true,
		}}))
	}
	page, err := store.CloneCorpusPage(sidecarSeedRepo, "", 10)
	if err != nil || len(page) != 1 || page[0].TokenCount != 6 || page[0].Signature != "sig-6" {
		t.Fatalf("clone corpus after re-upsert = %+v (err %v)", page, err)
	}
}

// TestAtGenerationSidecarWriteIsInvisibleToBaseHandle is the read-scoping
// proof: a derived handle's sidecar write lands in its own generation and the
// base handle cannot see it, while the base corpus stays visible to both.
// Deliberately limited to a payload sidecar — nodes and edges are not re-keyed
// yet, so a graph write through a derived handle proves nothing.
func TestAtGenerationSidecarWriteIsInvisibleToBaseHandle(t *testing.T) {
	base, err := Open(filepath.Join(t.TempDir(), "generation-scope.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer base.Close()

	mustNoErr(t, "base mtime", base.SetFileMtime(sidecarSeedRepo, "base.go", 1))

	derived := base.AtGeneration(1)
	if derived == nil || derived.ViewGeneration() != 1 {
		t.Fatalf("AtGeneration(1) = %v, want a handle pinned to generation 1", derived)
	}
	mustNoErr(t, "derived mtime", derived.SetFileMtime(sidecarSeedRepo, "derived.go", 2))

	var gen int64
	if err := base.writerDB.QueryRow(
		`SELECT view_gen FROM file_mtimes WHERE repo_prefix = ? AND file_path = ?`,
		sidecarSeedRepo, "derived.go",
	).Scan(&gen); err != nil {
		t.Fatalf("read derived row generation: %v", err)
	}
	if gen != 1 {
		t.Fatalf("derived handle wrote at view_gen %d, want 1", gen)
	}

	baseRows := base.LoadFileMtimes(sidecarSeedRepo)
	if _, ok := baseRows["derived.go"]; ok {
		t.Fatalf("base handle sees the derived generation's row: %v", baseRows)
	}
	if baseRows["base.go"] != 1 {
		t.Fatalf("base handle lost its own row: %v", baseRows)
	}

	derivedRows := derived.LoadFileMtimes(sidecarSeedRepo)
	if derivedRows["derived.go"] != 2 {
		t.Fatalf("derived handle cannot read its own row: %v", derivedRows)
	}
	if _, ok := derivedRows["base.go"]; ok {
		t.Fatalf("derived handle sees the base generation's row: %v", derivedRows)
	}
}
