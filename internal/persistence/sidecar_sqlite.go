package persistence

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/sqliteconn"
)

// SidecarStore is the SQLite-backed side-store for the agent's
// non-graph knowledge: session notes, cross-session development
// memories, saved scopes, and repository notebooks. It is a SEPARATE
// database file from the graph store, with its own path and lifetime, so
// notes/memories/scopes/notebooks survive a graph store that is thrown
// away or rebuilt from scratch.
//
// The file lives at <DataDir>/sidecar.sqlite by default (see
// DefaultSidecarPath); tests and the per-repo `gortex mcp` subprocess
// can point it at a cache-dir-local path for isolation.
//
// Rows are scoped by repo_key (the same RepoCacheKey hash the gob.gz
// layout used as a directory name) so a single sidecar file holds the
// notes/memories/notebooks of every repo the daemon serves. Scopes are
// global (no repo_key) — they were never per-repo.
//
// The managers in internal/mcp keep their in-memory slice + scorers
// unchanged; this store only swaps the persistence layer: load rows
// into the slice on open, write rows on each mutation, trim via a
// bounded DELETE.
type SidecarStore struct {
	db *sql.DB
	// writeMu serialises mutations. SQLite serialises writers
	// internally; mirroring that on the Go side turns SQLITE_BUSY
	// contention into clean lock-wait.
	writeMu sync.Mutex
}

// sidecarSchema is the idempotent base shape of the side-store. OpenSidecar
// runs it on every open, before runMigrations, and it is never gated on a
// schema version — it guarantees every table and original-shape index
// exists, which the column-only migrations in sidecar_migrate.go build on.
//
// Invariant: an index declared here may reference only columns present in its
// table's ORIGINAL create shape. Any column added later, and any index that
// depends on it, belongs in a migration (after that column's ALTER) — never
// here, or it will abort the whole batch with "no such column" on a database
// created before the column existed.
const sidecarSchema = `
CREATE TABLE IF NOT EXISTS notes (
	id            TEXT NOT NULL,
	repo_key      TEXT NOT NULL,
	session_id    TEXT NOT NULL DEFAULT '',
	client_name   TEXT NOT NULL DEFAULT '',
	body          TEXT NOT NULL DEFAULT '',
	symbol_id     TEXT NOT NULL DEFAULT '',
	file_path     TEXT NOT NULL DEFAULT '',
	repo_prefix   TEXT NOT NULL DEFAULT '',
	workspace_id  TEXT NOT NULL DEFAULT '',
	project_id    TEXT NOT NULL DEFAULT '',
	tags          TEXT NOT NULL DEFAULT '[]',
	auto_links    TEXT NOT NULL DEFAULT '[]',
	pinned        INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL DEFAULT 0,
	updated_at    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (repo_key, id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_notes_session   ON notes (repo_key, session_id);
CREATE INDEX IF NOT EXISTS idx_notes_workspace ON notes (repo_key, workspace_id, project_id);
CREATE INDEX IF NOT EXISTS idx_notes_updated   ON notes (repo_key, updated_at DESC);

CREATE TABLE IF NOT EXISTS memories (
	id            TEXT NOT NULL,
	repo_key      TEXT NOT NULL,
	kind          TEXT NOT NULL DEFAULT '',
	source        TEXT NOT NULL DEFAULT '',
	body          TEXT NOT NULL DEFAULT '',
	title         TEXT NOT NULL DEFAULT '',
	confidence    REAL NOT NULL DEFAULT 0,
	importance    INTEGER NOT NULL DEFAULT 0,
	author_agent  TEXT NOT NULL DEFAULT '',
	symbol_ids    TEXT NOT NULL DEFAULT '[]',
	file_paths    TEXT NOT NULL DEFAULT '[]',
	auto_links    TEXT NOT NULL DEFAULT '[]',
	tags          TEXT NOT NULL DEFAULT '[]',
	workspace_id  TEXT NOT NULL DEFAULT '',
	project_id    TEXT NOT NULL DEFAULT '',
	repo_prefix   TEXT NOT NULL DEFAULT '',
	pinned        INTEGER NOT NULL DEFAULT 0,
	superseded_by TEXT NOT NULL DEFAULT '',
	access_count  INTEGER NOT NULL DEFAULT 0,
	last_accessed INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL DEFAULT 0,
	updated_at    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (repo_key, id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_memories_workspace ON memories (repo_key, workspace_id, project_id);
CREATE INDEX IF NOT EXISTS idx_memories_updated   ON memories (repo_key, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_memories_kind      ON memories (repo_key, kind);

CREATE TABLE IF NOT EXISTS scopes (
	name        TEXT NOT NULL PRIMARY KEY,
	description TEXT NOT NULL DEFAULT '',
	repos       TEXT NOT NULL DEFAULT '[]',
	paths       TEXT NOT NULL DEFAULT '[]'
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS notebooks (
	id          TEXT NOT NULL,
	repo_key    TEXT NOT NULL,
	title       TEXT NOT NULL DEFAULT '',
	body        TEXT NOT NULL DEFAULT '',
	tags        TEXT NOT NULL DEFAULT '[]',
	symbol_ids  TEXT NOT NULL DEFAULT '[]',
	used_count  INTEGER NOT NULL DEFAULT 0,
	last_used   INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL DEFAULT 0,
	updated_at  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (repo_key, id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_notebooks_updated ON notebooks (repo_key, updated_at DESC);

CREATE TABLE IF NOT EXISTS migration_marks (
	repo_key TEXT NOT NULL,
	kind     TEXT NOT NULL,
	done_at  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (repo_key, kind)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS suppressions (
	repo_key     TEXT NOT NULL,
	identity_key TEXT NOT NULL,
	rule         TEXT NOT NULL DEFAULT '',
	category     TEXT NOT NULL DEFAULT '',
	file         TEXT NOT NULL DEFAULT '',
	symbol_id    TEXT NOT NULL DEFAULT '',
	reason       TEXT NOT NULL DEFAULT '',
	author       TEXT NOT NULL DEFAULT '',
	hit_count    INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL DEFAULT 0,
	last_hit     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (repo_key, identity_key)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_supp_updated ON suppressions (repo_key, last_hit DESC);

CREATE TABLE IF NOT EXISTS savings_events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	ts          INTEGER NOT NULL DEFAULT 0,
	session_id  TEXT NOT NULL DEFAULT '',
	tool        TEXT NOT NULL DEFAULT '',
	repo        TEXT NOT NULL DEFAULT '',
	language    TEXT NOT NULL DEFAULT '',
	model       TEXT NOT NULL DEFAULT '',
	client      TEXT NOT NULL DEFAULT '',
	returned    INTEGER NOT NULL DEFAULT 0,
	saved       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_savings_events_ts     ON savings_events (ts);
CREATE INDEX IF NOT EXISTS idx_savings_events_tool   ON savings_events (tool, ts);
-- idx_savings_events_model / idx_savings_events_client index the model and
-- client columns, which were added to savings_events after its original
-- shape. They are created by a schema migration (sidecar_migrate.go), not
-- here: a pre-existing table won't gain those columns from CREATE TABLE IF
-- NOT EXISTS, so an index referencing them here would abort the whole
-- schema batch with "no such column" on an older database.

CREATE TABLE IF NOT EXISTS savings_totals (
	bucket   TEXT NOT NULL PRIMARY KEY,
	saved    INTEGER NOT NULL DEFAULT 0,
	returned INTEGER NOT NULL DEFAULT 0,
	calls    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS savings_meta (
	key   TEXT NOT NULL PRIMARY KEY,
	value INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS cli_events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	ts         INTEGER NOT NULL DEFAULT 0,
	session_id TEXT NOT NULL DEFAULT '',
	verb       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_cli_events_ts      ON cli_events (ts);
CREATE INDEX IF NOT EXISTS idx_cli_events_session ON cli_events (session_id, ts);

CREATE TABLE IF NOT EXISTS promoted_tools (
	repo_key        TEXT NOT NULL,
	tool            TEXT NOT NULL,
	workspace_id    TEXT NOT NULL DEFAULT '',
	promoted_epoch  INTEGER NOT NULL DEFAULT 0,
	last_used_epoch INTEGER NOT NULL DEFAULT 0,
	use_count       INTEGER NOT NULL DEFAULT 0,
	updated_at      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (repo_key, tool)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS tool_surface_meta (
	repo_key      TEXT NOT NULL PRIMARY KEY,
	session_epoch INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
`

// DefaultSidecarPath is the canonical location of the side-store DB:
// <DataDir>/sidecar.sqlite (~/.gortex/sidecar.sqlite by default). An
// absolute $XDG_DATA_HOME relocates it under that tree, same as the
// graph store and models.
func DefaultSidecarPath(dataDir string) string {
	return filepath.Join(dataDir, "sidecar.sqlite")
}

// ---------------------------------------------------------------------------
// Process-shared sidecar cache.
//
// A single sidecar file may back several managers (notes + memories +
// notebooks + scopes for one repo, plus every other repo a daemon
// serves). Opening one *sql.DB per manager would multiply the pool and
// risk lock contention, so stores are cached by absolute path and
// reused. Tests that pass distinct temp paths get distinct handles.
// ---------------------------------------------------------------------------

var (
	sidecarMu    sync.Mutex
	sidecarCache = map[string]*SidecarStore{}
)

// OpenSidecar opens (or creates) the sidecar DB at path, reusing an
// already-open handle for the same absolute path. An empty path yields
// (nil, nil): callers treat a nil store as "in-memory only, no disk"
// — the behaviour the gob.gz managers had when their cache dir was
// empty.
func OpenSidecar(path string) (*SidecarStore, error) {
	if path == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	sidecarMu.Lock()
	defer sidecarMu.Unlock()
	if st, ok := sidecarCache[abs]; ok {
		return st, nil
	}

	if dir := filepath.Dir(abs); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("persistence: mkdir sidecar dir: %w", err)
		}
	}

	// Same WAL + synchronous=NORMAL + busy_timeout tradeoff the graph
	// store_sqlite backend uses for write-heavy embedded workloads.
	// journal_size_limit caps the -wal high-water mark so it can't ratchet
	// up unbounded under steady writes with ever-present readers (same WAL
	// growth class the graph store guards against).
	//
	// busy_timeout is listed FIRST, before journal_mode: modernc applies
	// _pragma settings in DSN order, and the very first connection to a DB
	// created in rollback-journal mode runs PRAGMA journal_mode=WAL, which
	// takes a brief EXCLUSIVE lock to convert the file. Unlike the graph store
	// (one process), the sidecar is opened concurrently by the daemon, every
	// per-repo `gortex mcp` subprocess, and the CLI — so that conversion races.
	// Ordering busy_timeout first installs the busy handler before any later
	// pragma or BEGIN, so write-lock contention blocks-and-retries instead of
	// failing fast (set after journal_mode the timeout would still be 0 during
	// the conversion). The journal-mode CONVERSION itself is NOT covered by the
	// busy handler — SQLite returns an immediate SQLITE_BUSY for a mode change
	// while another connection has the file open — so withSidecarBusyRetry in
	// OpenSidecar is the backstop for that residual race.
	//
	// _txlock=immediate makes db.Begin() emit BEGIN IMMEDIATE so a write
	// transaction takes the reserved lock at BEGIN. Both runBaseSchema (the
	// CREATE ... IF NOT EXISTS batch) and runMigrations depend on this so
	// several processes opening the same DB at once serialise on busy_timeout
	// instead of racing on an un-retryable lock-promotion SQLITE_BUSY.
	// runMigrations additionally reads PRAGMA user_version inside the
	// transaction, and a plain
	// DEFERRED begin would not hold the write lock at that point — two
	// processes opening a stale DB at once could both pass the version gate,
	// and the loser would hit an un-retryable SQLITE_BUSY on lock upgrade
	// (busy_timeout does not retry a deferred-to-write promotion). With
	// IMMEDIATE the loser blocks on busy_timeout at BEGIN, then re-reads the
	// bumped version and skips cleanly. Harmless for the savings write
	// transactions (already serialised in-process by writeMu); only changes
	// when their write lock is taken, not whether.
	dsn := abs + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(OFF)&_pragma=journal_size_limit(67108864)&_txlock=immediate"
	// Apply the policy to every physical connection, including replacements.
	// A late implicit checkpoint in db.Close would otherwise outlive the
	// graph backend's close. Committed WAL is recovered on the next open.
	db, err := sqliteconn.Open(dsn)
	if err != nil {
		return nil, fmt.Errorf("persistence: open sidecar: %w", err)
	}
	// busy_timeout does not cover the rollback->WAL conversion the first opener
	// performs, so wrap the lock-taking schema + migration steps in a bounded
	// BUSY/LOCKED retry (see withSidecarBusyRetry). Both steps are idempotent.
	if err := withSidecarBusyRetry(func() error { return runBaseSchema(db) }); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("persistence: sidecar schema: %w", err)
	}
	// Bring databases created by older builds forward in place: add columns
	// and column-dependent indexes that CREATE TABLE IF NOT EXISTS cannot add
	// to a pre-existing table. Forward-only, additive, and safe when several
	// gortex processes open the same file at once (see runMigrations).
	if err := withSidecarBusyRetry(func() error { return runMigrations(db) }); err != nil {
		_ = db.Close()
		return nil, err
	}

	st := &SidecarStore{db: db}
	sidecarCache[abs] = st
	return st, nil
}

// Close closes the underlying *sql.DB and drops it from the shared
// cache. Every physical handle is actually closed, without an implicit final
// WAL checkpoint. Committed WAL remains part of the database and is recovered
// on reopen. The daemon owns this boundary after sidecar users have drained.
func (s *SidecarStore) Close() error {
	if s == nil {
		return nil
	}
	sidecarMu.Lock()
	for k, v := range sidecarCache {
		if v == s {
			delete(sidecarCache, k)
		}
	}
	sidecarMu.Unlock()
	return s.db.Close()
}

// CloseSidecar closes the cached sidecar rooted at path, if one is open, and
// drops it from the shared cache. Unlike OpenSidecar(path).Close() — the idiom
// a test otherwise has to reach for — it never creates the file or its parent
// directory when nothing was open, so a caller can release a path
// unconditionally without leaving a database behind.
//
// The daemon holds its sidecar for the process lifetime; this exists for
// teardown, where a still-open handle keeps the file locked on Windows and
// makes an otherwise-passing test fail in RemoveAll.
func CloseSidecar(path string) error {
	if path == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	// Evict under the same lock that found it. Releasing between the lookup
	// and the close would let a concurrent OpenSidecar hand out the handle
	// this call is about to close; dropping the entry first makes that lookup
	// miss and open a fresh one instead. Close still runs outside the lock —
	// it takes sidecarMu itself to clear any other keys pointing at the same
	// store.
	sidecarMu.Lock()
	st, ok := sidecarCache[abs]
	if ok {
		delete(sidecarCache, abs)
	}
	sidecarMu.Unlock()
	if !ok || st == nil {
		return nil
	}
	return st.Close()
}

// ---------------------------------------------------------------------------
// JSON helpers for []string columns.
// ---------------------------------------------------------------------------

func encodeStrings(in []string) string {
	if len(in) == 0 {
		return "[]"
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func decodeStrings(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// unixOrZero converts a time to a UTC unix-nano stamp; the zero time
// maps to 0 so a NULL/absent value round-trips back to the zero time.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// ---------------------------------------------------------------------------
// Migration bookkeeping.
// ---------------------------------------------------------------------------

// migrationDone reports whether a one-shot legacy import has already
// run for (repoKey, kind). Idempotency guard for the gob.gz/json/md →
// sqlite import.
func (s *SidecarStore) migrationDone(repoKey, kind string) bool {
	var n int
	row := s.db.QueryRow(`SELECT COUNT(1) FROM migration_marks WHERE repo_key = ? AND kind = ?`, repoKey, kind)
	if err := row.Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func (s *SidecarStore) markMigrated(repoKey, kind string) {
	_, _ = s.db.Exec(`INSERT OR REPLACE INTO migration_marks (repo_key, kind, done_at) VALUES (?,?,?)`,
		repoKey, kind, time.Now().UTC().UnixNano())
}

// countRows returns the number of rows for a repo_key in the given
// table — used to guard "sqlite already has rows" before importing.
func (s *SidecarStore) countRows(table, repoKey string) int {
	var n int
	row := s.db.QueryRow(`SELECT COUNT(1) FROM `+table+` WHERE repo_key = ?`, repoKey)
	if err := row.Scan(&n); err != nil {
		return 0
	}
	return n
}

// ===========================================================================
// Notes
// ===========================================================================

// LoadNotesRows reads every note for a repo_key, oldest-first (the
// managers append-load into a chronological slice).
func (s *SidecarStore) LoadNotesRows(repoKey string) ([]NoteEntry, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, client_name, body, symbol_id, file_path,
		       repo_prefix, workspace_id, project_id, tags, auto_links,
		       pinned, created_at, updated_at
		FROM notes WHERE repo_key = ?
		ORDER BY created_at ASC, id ASC`, repoKey)
	if err != nil {
		return nil, fmt.Errorf("persistence: query notes: %w", err)
	}
	defer rows.Close()

	var out []NoteEntry
	for rows.Next() {
		var (
			e                    NoteEntry
			tags, links          string
			pinned               int
			createdAt, updatedAt int64
		)
		if err := rows.Scan(&e.ID, &e.SessionID, &e.ClientName, &e.Body, &e.SymbolID,
			&e.FilePath, &e.RepoPrefix, &e.WorkspaceID, &e.ProjectID, &tags, &links,
			&pinned, &createdAt, &updatedAt); err != nil {
			return out, fmt.Errorf("persistence: scan note: %w", err)
		}
		e.Tags = decodeStrings(tags)
		e.AutoLinks = decodeStrings(links)
		e.Pinned = pinned != 0
		e.Timestamp = fromUnix(createdAt)
		e.UpdatedAt = fromUnix(updatedAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpsertNote writes (or replaces) a single note row.
func (s *SidecarStore) UpsertNote(repoKey string, e NoteEntry) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	pinned := 0
	if e.Pinned {
		pinned = 1
	}
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO notes
		(id, repo_key, session_id, client_name, body, symbol_id, file_path,
		 repo_prefix, workspace_id, project_id, tags, auto_links, pinned,
		 created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, repoKey, e.SessionID, e.ClientName, e.Body, e.SymbolID, e.FilePath,
		e.RepoPrefix, e.WorkspaceID, e.ProjectID, encodeStrings(e.Tags),
		encodeStrings(e.AutoLinks), pinned, unixOrZero(e.Timestamp), unixOrZero(e.UpdatedAt))
	if err != nil {
		return fmt.Errorf("persistence: upsert note: %w", err)
	}
	return nil
}

// DeleteNote removes a single note row. Missing rows are not errors.
func (s *SidecarStore) DeleteNote(repoKey, id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`DELETE FROM notes WHERE repo_key = ? AND id = ?`, repoKey, id)
	return err
}

// TrimNotes enforces the soft cap: when the repo_key holds more than
// cap notes, the oldest non-pinned notes are deleted first until the
// count is within cap (pinned notes are never deleted). Mirrors the
// gob.gz trimNotes semantics as a bounded DELETE.
func (s *SidecarStore) TrimNotes(repoKey string, cap int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM notes WHERE repo_key = ?`, repoKey).Scan(&total); err != nil {
		return err
	}
	if total <= cap {
		return nil
	}
	excess := total - cap
	// Delete the oldest non-pinned notes first.
	_, err := s.db.Exec(`
		DELETE FROM notes
		WHERE repo_key = ? AND pinned = 0 AND id IN (
			SELECT id FROM notes
			WHERE repo_key = ? AND pinned = 0
			ORDER BY created_at ASC, id ASC
			LIMIT ?
		)`, repoKey, repoKey, excess)
	return err
}

// ===========================================================================
// Memories
// ===========================================================================

// LoadMemoriesRows reads every memory for a repo_key, oldest-first.
func (s *SidecarStore) LoadMemoriesRows(repoKey string) ([]MemoryEntry, error) {
	rows, err := s.db.Query(`
		SELECT id, kind, source, body, title, confidence, importance,
		       author_agent, symbol_ids, file_paths, auto_links, tags,
		       workspace_id, project_id, repo_prefix, pinned, superseded_by,
		       access_count, last_accessed, created_at, updated_at
		FROM memories WHERE repo_key = ?
		ORDER BY created_at ASC, id ASC`, repoKey)
	if err != nil {
		return nil, fmt.Errorf("persistence: query memories: %w", err)
	}
	defer rows.Close()

	var out []MemoryEntry
	for rows.Next() {
		var (
			e                              MemoryEntry
			syms, files, links, tags       string
			confidence                     float64
			pinned                         int
			accessCount                    int64
			lastAccessed, created, updated int64
		)
		if err := rows.Scan(&e.ID, &e.Kind, &e.Source, &e.Body, &e.Title, &confidence,
			&e.Importance, &e.AuthorAgent, &syms, &files, &links, &tags,
			&e.WorkspaceID, &e.ProjectID, &e.RepoPrefix, &pinned, &e.SupersededBy,
			&accessCount, &lastAccessed, &created, &updated); err != nil {
			return out, fmt.Errorf("persistence: scan memory: %w", err)
		}
		e.Confidence = float32(confidence)
		e.SymbolIDs = decodeStrings(syms)
		e.FilePaths = decodeStrings(files)
		e.AutoLinks = decodeStrings(links)
		e.Tags = decodeStrings(tags)
		e.Pinned = pinned != 0
		e.AccessCount = uint64(accessCount)
		e.LastAccessed = fromUnix(lastAccessed)
		e.Timestamp = fromUnix(created)
		e.UpdatedAt = fromUnix(updated)
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpsertMemory writes (or replaces) a single memory row.
func (s *SidecarStore) UpsertMemory(repoKey string, e MemoryEntry) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	pinned := 0
	if e.Pinned {
		pinned = 1
	}
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO memories
		(id, repo_key, kind, source, body, title, confidence, importance,
		 author_agent, symbol_ids, file_paths, auto_links, tags, workspace_id,
		 project_id, repo_prefix, pinned, superseded_by, access_count,
		 last_accessed, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, repoKey, e.Kind, e.Source, e.Body, e.Title, float64(e.Confidence),
		e.Importance, e.AuthorAgent, encodeStrings(e.SymbolIDs), encodeStrings(e.FilePaths),
		encodeStrings(e.AutoLinks), encodeStrings(e.Tags), e.WorkspaceID, e.ProjectID,
		e.RepoPrefix, pinned, e.SupersededBy, int64(e.AccessCount),
		unixOrZero(e.LastAccessed), unixOrZero(e.Timestamp), unixOrZero(e.UpdatedAt))
	if err != nil {
		return fmt.Errorf("persistence: upsert memory: %w", err)
	}
	return nil
}

// DeleteMemory removes a single memory row. Missing rows are not errors.
func (s *SidecarStore) DeleteMemory(repoKey, id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`DELETE FROM memories WHERE repo_key = ? AND id = ?`, repoKey, id)
	return err
}

// TrimMemories enforces the soft cap with the two-pass policy the
// gob.gz trimMemories used: first shed non-pinned importance<=2 rows,
// then (if still over cap) shed the oldest non-pinned rows. Pinned
// rows are never deleted.
func (s *SidecarStore) TrimMemories(repoKey string, cap int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM memories WHERE repo_key = ?`, repoKey).Scan(&total); err != nil {
		return err
	}
	if total <= cap {
		return nil
	}
	excess := total - cap

	// Pass 1: oldest non-pinned, low-importance (<=2) rows.
	res, err := s.db.Exec(`
		DELETE FROM memories
		WHERE repo_key = ? AND pinned = 0 AND importance <= 2 AND id IN (
			SELECT id FROM memories
			WHERE repo_key = ? AND pinned = 0 AND importance <= 2
			ORDER BY created_at ASC, id ASC
			LIMIT ?
		)`, repoKey, repoKey, excess)
	if err != nil {
		return err
	}
	dropped, _ := res.RowsAffected()
	remaining := excess - int(dropped)
	if remaining <= 0 {
		return nil
	}

	// Pass 2: oldest non-pinned rows regardless of importance.
	_, err = s.db.Exec(`
		DELETE FROM memories
		WHERE repo_key = ? AND pinned = 0 AND id IN (
			SELECT id FROM memories
			WHERE repo_key = ? AND pinned = 0
			ORDER BY created_at ASC, id ASC
			LIMIT ?
		)`, repoKey, repoKey, remaining)
	return err
}

// ===========================================================================
// Scopes (global — no repo_key)
// ===========================================================================

// ScopeRow mirrors the SavedScope shape without importing the mcp
// package. The mcp scopeStore converts between this and SavedScope.
type ScopeRow struct {
	Name        string
	Description string
	Repos       []string
	Paths       []string
}

// LoadScopes reads every saved scope, name-sorted.
func (s *SidecarStore) LoadScopes() ([]ScopeRow, error) {
	rows, err := s.db.Query(`SELECT name, description, repos, paths FROM scopes ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("persistence: query scopes: %w", err)
	}
	defer rows.Close()

	var out []ScopeRow
	for rows.Next() {
		var (
			r            ScopeRow
			repos, paths string
		)
		if err := rows.Scan(&r.Name, &r.Description, &repos, &paths); err != nil {
			return out, fmt.Errorf("persistence: scan scope: %w", err)
		}
		r.Repos = decodeStrings(repos)
		r.Paths = decodeStrings(paths)
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertScope writes (or replaces) a single scope row.
func (s *SidecarStore) UpsertScope(r ScopeRow) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO scopes (name, description, repos, paths)
		VALUES (?,?,?,?)`,
		r.Name, r.Description, encodeStrings(r.Repos), encodeStrings(r.Paths))
	if err != nil {
		return fmt.Errorf("persistence: upsert scope: %w", err)
	}
	return nil
}

// DeleteScope removes a scope by name. Missing rows are not errors.
func (s *SidecarStore) DeleteScope(name string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`DELETE FROM scopes WHERE name = ?`, name)
	return err
}

// ScopeCount returns the number of saved scopes — used to guard the
// legacy scopes.json import.
func (s *SidecarStore) ScopeCount() int {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM scopes`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// ===========================================================================
// Notebooks
// ===========================================================================

// NotebookRow is the persisted notebook shape. SymbolIDs is carried
// for forward-compatibility (the markdown layout never had it, but the
// schema reserves the column); the mcp notebookEntry maps onto this.
type NotebookRow struct {
	ID        string
	Title     string
	Body      string
	Tags      []string
	SymbolIDs []string
	UsedCount uint64
	LastUsed  time.Time
	Created   time.Time
	Updated   time.Time
}

// LoadNotebookRows reads every notebook entry for a repo_key,
// newest-first by Updated.
func (s *SidecarStore) LoadNotebookRows(repoKey string) ([]NotebookRow, error) {
	rows, err := s.db.Query(`
		SELECT id, title, body, tags, symbol_ids, used_count, last_used,
		       created_at, updated_at
		FROM notebooks WHERE repo_key = ?
		ORDER BY updated_at DESC, id ASC`, repoKey)
	if err != nil {
		return nil, fmt.Errorf("persistence: query notebooks: %w", err)
	}
	defer rows.Close()

	var out []NotebookRow
	for rows.Next() {
		var (
			r                          NotebookRow
			tags, syms                 string
			usedCount                  int64
			lastUsed, created, updated int64
		)
		if err := rows.Scan(&r.ID, &r.Title, &r.Body, &tags, &syms, &usedCount,
			&lastUsed, &created, &updated); err != nil {
			return out, fmt.Errorf("persistence: scan notebook: %w", err)
		}
		r.Tags = decodeStrings(tags)
		r.SymbolIDs = decodeStrings(syms)
		r.UsedCount = uint64(usedCount)
		r.LastUsed = fromUnix(lastUsed)
		r.Created = fromUnix(created)
		r.Updated = fromUnix(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetNotebookRow reads a single notebook entry by id, or (zero, false).
func (s *SidecarStore) GetNotebookRow(repoKey, id string) (NotebookRow, bool) {
	row := s.db.QueryRow(`
		SELECT id, title, body, tags, symbol_ids, used_count, last_used,
		       created_at, updated_at
		FROM notebooks WHERE repo_key = ? AND id = ?`, repoKey, id)
	var (
		r                          NotebookRow
		tags, syms                 string
		usedCount                  int64
		lastUsed, created, updated int64
	)
	if err := row.Scan(&r.ID, &r.Title, &r.Body, &tags, &syms, &usedCount,
		&lastUsed, &created, &updated); err != nil {
		return NotebookRow{}, false
	}
	r.Tags = decodeStrings(tags)
	r.SymbolIDs = decodeStrings(syms)
	r.UsedCount = uint64(usedCount)
	r.LastUsed = fromUnix(lastUsed)
	r.Created = fromUnix(created)
	r.Updated = fromUnix(updated)
	return r, true
}

// UpsertNotebook writes (or replaces) a single notebook row.
func (s *SidecarStore) UpsertNotebook(repoKey string, r NotebookRow) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO notebooks
		(id, repo_key, title, body, tags, symbol_ids, used_count, last_used,
		 created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.ID, repoKey, r.Title, r.Body, encodeStrings(r.Tags), encodeStrings(r.SymbolIDs),
		int64(r.UsedCount), unixOrZero(r.LastUsed), unixOrZero(r.Created), unixOrZero(r.Updated))
	if err != nil {
		return fmt.Errorf("persistence: upsert notebook: %w", err)
	}
	return nil
}

// DeleteNotebook removes a notebook entry. Missing rows are not errors.
func (s *SidecarStore) DeleteNotebook(repoKey, id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`DELETE FROM notebooks WHERE repo_key = ? AND id = ?`, repoKey, id)
	return err
}

// NotebookCutoff deletes notebook rows whose effective freshness stamp
// (LastUsed, falling back to Updated when never used) is older than
// cutoff. Mirrors the markdown TTL pruner as a bounded DELETE. Returns
// the deleted ids so the caller can mirror the prune elsewhere if
// needed.
func (s *SidecarStore) NotebookPrune(repoKey string, cutoff time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	c := unixOrZero(cutoff)
	if c == 0 {
		return nil
	}
	// effective = last_used when non-zero, else created/updated.
	_, err := s.db.Exec(`
		DELETE FROM notebooks
		WHERE repo_key = ?
		  AND (CASE WHEN last_used > 0 THEN last_used ELSE updated_at END) < ?`, repoKey, c)
	return err
}

// ===========================================================================
// Suppressions (durable per-repo false-positive review filter)
// ===========================================================================

// SuppressionEntry is one durable review-finding suppression row, keyed by
// (repo_key, IdentityKey) — a finding silenced permanently for a repo until it
// is explicitly removed. The IdentityKey is the line-drift-stable identity the
// review layer computes; the remaining fields are denormalised context so a
// listing is human-legible without re-deriving the finding.
type SuppressionEntry struct {
	IdentityKey string
	Rule        string
	Category    string
	File        string
	SymbolID    string
	Reason      string
	Author      string
	HitCount    int64
	Created     time.Time
	LastHit     time.Time
}

// UpsertSuppression writes (or replaces) a single suppression row. A replace
// preserves the existing HitCount/Created when the caller leaves them zero and
// the row already exists is NOT done here — the row is replaced wholesale, so
// callers that want to keep counters read first or use BumpSuppressionHit.
func (s *SidecarStore) UpsertSuppression(repoKey string, e SuppressionEntry) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO suppressions
		(repo_key, identity_key, rule, category, file, symbol_id, reason, author,
		 hit_count, created_at, last_hit)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		repoKey, e.IdentityKey, e.Rule, e.Category, e.File, e.SymbolID, e.Reason,
		e.Author, e.HitCount, unixOrZero(e.Created), unixOrZero(e.LastHit))
	if err != nil {
		return fmt.Errorf("persistence: upsert suppression: %w", err)
	}
	return nil
}

// LoadSuppression reads a single suppression row by identity key. The bool is
// false when no row exists (a clean miss, never an error for the caller).
func (s *SidecarStore) LoadSuppression(repoKey, identityKey string) (SuppressionEntry, bool) {
	row := s.db.QueryRow(`
		SELECT identity_key, rule, category, file, symbol_id, reason, author,
		       hit_count, created_at, last_hit
		FROM suppressions WHERE repo_key = ? AND identity_key = ?`, repoKey, identityKey)
	var (
		e                SuppressionEntry
		created, lastHit int64
	)
	if err := row.Scan(&e.IdentityKey, &e.Rule, &e.Category, &e.File, &e.SymbolID,
		&e.Reason, &e.Author, &e.HitCount, &created, &lastHit); err != nil {
		return SuppressionEntry{}, false
	}
	e.Created = fromUnix(created)
	e.LastHit = fromUnix(lastHit)
	return e, true
}

// LoadSuppressions reads every suppression row for a repo, most-recently-hit
// first (created-then-key tiebreak for rows never hit), matching idx_supp_updated.
func (s *SidecarStore) LoadSuppressions(repoKey string) ([]SuppressionEntry, error) {
	rows, err := s.db.Query(`
		SELECT identity_key, rule, category, file, symbol_id, reason, author,
		       hit_count, created_at, last_hit
		FROM suppressions WHERE repo_key = ?
		ORDER BY last_hit DESC, created_at DESC, identity_key ASC`, repoKey)
	if err != nil {
		return nil, fmt.Errorf("persistence: query suppressions: %w", err)
	}
	defer rows.Close()

	var out []SuppressionEntry
	for rows.Next() {
		var (
			e                SuppressionEntry
			created, lastHit int64
		)
		if err := rows.Scan(&e.IdentityKey, &e.Rule, &e.Category, &e.File, &e.SymbolID,
			&e.Reason, &e.Author, &e.HitCount, &created, &lastHit); err != nil {
			return out, fmt.Errorf("persistence: scan suppression: %w", err)
		}
		e.Created = fromUnix(created)
		e.LastHit = fromUnix(lastHit)
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteSuppression removes a single suppression row. Missing rows are not errors.
func (s *SidecarStore) DeleteSuppression(repoKey, identityKey string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`DELETE FROM suppressions WHERE repo_key = ? AND identity_key = ?`, repoKey, identityKey)
	return err
}

// BumpSuppressionHit records a suppression match: it increments hit_count and
// stamps last_hit to now for the row, if it exists. A missing row is a no-op
// (not an error) — IsSuppressed returned false for it, so there was nothing to
// bump. The write is a single guarded UPDATE.
func (s *SidecarStore) BumpSuppressionHit(repoKey, identityKey string, at time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`
		UPDATE suppressions
		SET hit_count = hit_count + 1, last_hit = ?
		WHERE repo_key = ? AND identity_key = ?`, unixOrZero(at), repoKey, identityKey)
	return err
}

// ===========================================================================
// Legacy migration importers
// ===========================================================================

// MigrateLegacyNotes imports a legacy notes.gob.gz for repoKey when the
// sqlite table is empty for that scope, then renames the legacy file to
// *.bak. Idempotent: guarded by a migration mark and an empty-table
// check. legacyDir is the gob.gz directory (NotesDir result).
func (s *SidecarStore) MigrateLegacyNotes(repoKey, legacyDir string) error {
	if legacyDir == "" || s.migrationDone(repoKey, "notes") || s.countRows("notes", repoKey) > 0 {
		return nil
	}
	loaded, err := LoadNotes(legacyDir)
	if err != nil || loaded == nil || len(loaded.Entries) == 0 {
		s.markMigrated(repoKey, "notes")
		return nil
	}
	for _, e := range loaded.Entries {
		if e.Timestamp.IsZero() {
			e.Timestamp = time.Now().UTC()
		}
		if e.UpdatedAt.IsZero() {
			e.UpdatedAt = e.Timestamp
		}
		if err := s.UpsertNote(repoKey, e); err != nil {
			return err
		}
	}
	s.markMigrated(repoKey, "notes")
	renameLegacy(filepath.Join(legacyDir, notesFile))
	return nil
}

// MigrateLegacyMemories imports a legacy memories.gob.gz for repoKey.
func (s *SidecarStore) MigrateLegacyMemories(repoKey, legacyDir string) error {
	if legacyDir == "" || s.migrationDone(repoKey, "memories") || s.countRows("memories", repoKey) > 0 {
		return nil
	}
	loaded, err := LoadMemories(legacyDir)
	if err != nil || loaded == nil || len(loaded.Entries) == 0 {
		s.markMigrated(repoKey, "memories")
		return nil
	}
	for _, e := range loaded.Entries {
		if e.Timestamp.IsZero() {
			e.Timestamp = time.Now().UTC()
		}
		if e.UpdatedAt.IsZero() {
			e.UpdatedAt = e.Timestamp
		}
		if err := s.UpsertMemory(repoKey, e); err != nil {
			return err
		}
	}
	s.markMigrated(repoKey, "memories")
	renameLegacy(filepath.Join(legacyDir, memoriesFile))
	return nil
}

// MigrateLegacyScopes imports a legacy scopes.json when the scopes
// table is empty, then renames the file to *.bak. Idempotent.
func (s *SidecarStore) MigrateLegacyScopes(legacyPath string) error {
	if legacyPath == "" || s.migrationDone("global", "scopes") || s.ScopeCount() > 0 {
		return nil
	}
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		s.markMigrated("global", "scopes")
		return nil
	}
	type legacyScope struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Repos       []string `json:"repos"`
		Paths       []string `json:"paths"`
	}
	var legacy []legacyScope
	if json.Unmarshal(data, &legacy) != nil {
		s.markMigrated("global", "scopes")
		return nil
	}
	for _, sc := range legacy {
		if sc.Name == "" {
			continue
		}
		if err := s.UpsertScope(ScopeRow(sc)); err != nil {
			return err
		}
	}
	s.markMigrated("global", "scopes")
	renameLegacy(legacyPath)
	return nil
}

// MigrateLegacyNotebook imports markdown notebook files under
// legacyDir/<id>.md into the sqlite notebooks table for repoKey, then
// renames each imported file to <id>.md.bak. importMD parses one file's
// contents into a NotebookRow. Idempotent.
func (s *SidecarStore) MigrateLegacyNotebook(repoKey, legacyDir string, importMD func(id, contents string) (NotebookRow, bool)) error {
	if legacyDir == "" || importMD == nil || s.migrationDone(repoKey, "notebook") || s.countRows("notebooks", repoKey) > 0 {
		return nil
	}
	entries, err := os.ReadDir(legacyDir)
	if err != nil {
		s.markMigrated(repoKey, "notebook")
		return nil
	}
	imported := make([]string, 0, len(entries))
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || filepath.Ext(name) != ".md" {
			continue
		}
		full := filepath.Join(legacyDir, name)
		contents, rerr := os.ReadFile(full)
		if rerr != nil {
			continue
		}
		id := name[:len(name)-len(".md")]
		row, ok := importMD(id, string(contents))
		if !ok {
			continue
		}
		row.ID = id
		if err := s.UpsertNotebook(repoKey, row); err != nil {
			return err
		}
		imported = append(imported, full)
	}
	s.markMigrated(repoKey, "notebook")
	sort.Strings(imported)
	for _, full := range imported {
		renameLegacy(full)
	}
	return nil
}

// renameLegacy renames a legacy file to <file>.bak. Best-effort —
// never deletes; a missing file or rename failure is silently
// ignored so a migration that already moved the file stays idempotent.
func renameLegacy(path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	_ = os.Rename(path, path+".bak")
}
