package store_sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ReadRepoIndexStates opens the SQLite store at path read-only and returns
// every repo_index_state freshness row keyed by repo_prefix.
//
// It is a deliberately lightweight side door for read-only callers (notably
// `gortex repos`) that must inspect index freshness WITHOUT going through
// Open — which runs schema migrations, alters columns, starts a checkpoint
// goroutine, and (on a version mismatch) can refuse to open or rebuild the
// file. None of that is appropriate for a status command that may run while
// a daemon holds the same store open.
//
// The connection is query-only and inherits the database's existing journal
// mode, so it reads safely alongside a running daemon (WAL permits concurrent
// readers).
//
// Exactly two conditions mean "nothing recorded yet" and yield an empty map
// with a nil error: no store file at all, and a store that predates the
// repo_index_state table. Every other failure — a corrupt or truncated
// database, a permission error, a schema the query cannot run against — is
// returned. Collapsing those into an empty map made a broken store
// indistinguishable from an unindexed one, so `gortex repos` reported success
// and told the user their repos had never been indexed.
func ReadRepoIndexStates(path string) (map[string]graph.RepoIndexState, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return map[string]graph.RepoIndexState{}, nil
		}
		return nil, fmt.Errorf("stat sqlite store %q: %w", path, err)
	}

	// query_only blocks accidental writes; busy_timeout keeps a brief read
	// from erroring out if the daemon happens to hold the write lock for a
	// moment. We deliberately do NOT set journal_mode — forcing it could try
	// to switch the live database's mode; inheriting the on-disk WAL mode is
	// exactly what a concurrent reader wants.
	dsn := path + "?_pragma=busy_timeout(2000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite store %q: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// view_gen = 0 is spelled out rather than inherited from a Store handle:
	// this door never opens one, and every caller of it wants the base corpus
	// a plain index writes, not whatever derived view a daemon happens to be
	// serving. A store that predates the column holds only base rows, so the
	// fallback below reads exactly the same set.
	rows, err := db.Query(`
SELECT repo_prefix, indexed_sha, dirty, indexed_at, workspace_fp, node_count, edge_count, extractor_versions
  FROM repo_index_state WHERE view_gen = 0`)
	if err != nil && isMissingColumnErr(err, "view_gen") {
		rows, err = db.Query(`
SELECT repo_prefix, indexed_sha, dirty, indexed_at, workspace_fp, node_count, edge_count, extractor_versions
  FROM repo_index_state`)
	}
	if err != nil {
		// A store written before the repo_index_state table existed has
		// nothing recorded yet. Anything else — most importantly a file that
		// is not a database — is a real failure the caller must see.
		if isMissingTableErr(err) {
			return map[string]graph.RepoIndexState{}, nil
		}
		return nil, fmt.Errorf("read repo_index_state from %q: %w", path, err)
	}
	defer rows.Close()

	out := map[string]graph.RepoIndexState{}
	for rows.Next() {
		var st graph.RepoIndexState
		var dirty int
		if err := rows.Scan(&st.RepoPrefix, &st.IndexedSHA, &dirty, &st.IndexedAt,
			&st.WorkspaceFP, &st.NodeCount, &st.EdgeCount, &st.ExtractorVersions); err != nil {
			return nil, fmt.Errorf("scan repo_index_state: %w", err)
		}
		st.Dirty = dirty != 0
		out[st.RepoPrefix] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repo_index_state: %w", err)
	}
	return out, nil
}

// isMissingTableErr reports whether err is SQLite refusing a query because the
// table does not exist. String-matched because the driver reports it as a
// generic "SQL logic error" with the detail only in the message; the query
// above names exactly one table, so this cannot mistake some other absence for
// it. A corrupt file reports "file is not a database" and is deliberately not
// covered here.
func isMissingTableErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// isMissingColumnErr reports whether err is SQLite refusing a query because
// the named column does not exist. It exists so this door still reads a store
// stamped before the view generation column shipped: such a store carries only
// base-generation rows, so dropping the predicate reads the same set rather
// than reporting the repos as never indexed. String-matched for the same
// reason as isMissingTableErr.
func isMissingColumnErr(err error, column string) bool {
	return err != nil && strings.Contains(err.Error(), "no such column: "+column)
}
