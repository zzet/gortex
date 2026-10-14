package graph

import (
	"sort"
)

// This file implements the FileMtime* capability family on MemoryStore
// (issue davalillo/gortex#26): the in-RAM sidecar that lets the memory
// backend seed warm-restart reconcile exactly the way the sqlite backend
// seeds it from its file_mtimes table. Rows are per repo prefix:
// repo-relative file path -> mtime in nanoseconds.
//
// Durability contract: the sidecar is NOT journaled. It is persisted by the
// snapshot dump (serverstack.snapshotMemoryStore) and restored by the
// snapshot load (serverstack.openMemoryBackend). A crash therefore rolls
// mtimes back to the last snapshot — the next boot sees those files as
// changed and re-reconciles them, which costs time but can never produce a
// wrong graph (ChangedSinceMtimes never skips a file that actually changed).

var (
	_ FileMtimeReader   = (*MemoryStore)(nil)
	_ FileMtimeWriter   = (*MemoryStore)(nil)
	_ FileMtimeReplacer = (*MemoryStore)(nil)
	_ FileMtimeDeleter  = (*MemoryStore)(nil)
)

// FileMtimePrefixes lists the repo prefixes that currently hold mtime rows,
// sorted. The snapshot dump iterates it to persist the sidecar.
func (m *MemoryStore) FileMtimePrefixes() []string {
	m.mtimeMu.Lock()
	defer m.mtimeMu.Unlock()
	out := make([]string, 0, len(m.mtimes))
	for prefix, rows := range m.mtimes {
		if len(rows) > 0 {
			out = append(out, prefix)
		}
	}
	sort.Strings(out)
	return out
}

// LoadFileMtimes returns a fresh copy of the repo's recorded mtimes, nil for
// "no data" — the FileMtimeReader contract, mirroring
// store_sqlite.Store.LoadFileMtimes.
func (m *MemoryStore) LoadFileMtimes(repoPrefix string) map[string]int64 {
	m.mtimeMu.Lock()
	defer m.mtimeMu.Unlock()
	rows := m.mtimes[repoPrefix]
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string]int64, len(rows))
	for path, mtime := range rows {
		out[path] = mtime
	}
	return out
}

// BulkSetFileMtimes upserts every (filePath -> mtimeNs) entry for one repo
// prefix — the FileMtimeWriter incremental path.
func (m *MemoryStore) BulkSetFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if len(mtimes) == 0 {
		return nil
	}
	m.mtimeMu.Lock()
	defer m.mtimeMu.Unlock()
	if m.mtimes == nil {
		m.mtimes = make(map[string]map[string]int64)
	}
	rows := m.mtimes[repoPrefix]
	if rows == nil {
		rows = make(map[string]int64, len(mtimes))
		m.mtimes[repoPrefix] = rows
	}
	for path, mtime := range mtimes {
		rows[path] = mtime
	}
	return nil
}

// ReplaceFileMtimes persists the AUTHORITATIVE full mtime set for a repo
// prefix, dropping rows for files no longer present — the FileMtimeReplacer
// full-census path. Empty input is a no-op: it must never wipe a repo's
// mtimes from an empty census (same contract as store_sqlite).
func (m *MemoryStore) ReplaceFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if len(mtimes) == 0 {
		return nil
	}
	rows := make(map[string]int64, len(mtimes))
	for path, mtime := range mtimes {
		rows[path] = mtime
	}
	m.mtimeMu.Lock()
	defer m.mtimeMu.Unlock()
	if m.mtimes == nil {
		m.mtimes = make(map[string]map[string]int64)
	}
	m.mtimes[repoPrefix] = rows
	return nil
}

// DeleteFileMtimes drops the rows for a set of repo-relative file paths —
// the FileMtimeDeleter incremental path. Empty input is a no-op.
func (m *MemoryStore) DeleteFileMtimes(repoPrefix string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	m.mtimeMu.Lock()
	defer m.mtimeMu.Unlock()
	rows := m.mtimes[repoPrefix]
	if len(rows) == 0 {
		return nil
	}
	for _, path := range paths {
		delete(rows, path)
	}
	if len(rows) == 0 {
		delete(m.mtimes, repoPrefix)
	}
	return nil
}

// snapshotFileMtimes returns an owned copy of the whole sidecar for the
// snapshot dump. Serverstack-internal helper surface; not part of the Store
// capability family.
func (m *MemoryStore) snapshotFileMtimes() map[string]map[string]int64 {
	m.mtimeMu.Lock()
	defer m.mtimeMu.Unlock()
	if len(m.mtimes) == 0 {
		return nil
	}
	out := make(map[string]map[string]int64, len(m.mtimes))
	for prefix, rows := range m.mtimes {
		if len(rows) == 0 {
			continue
		}
		copyRows := make(map[string]int64, len(rows))
		for path, mtime := range rows {
			copyRows[path] = mtime
		}
		out[prefix] = copyRows
	}
	return out
}
