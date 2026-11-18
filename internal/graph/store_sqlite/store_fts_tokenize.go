package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// SymbolFTSTokenize returns the terms SQLite's FTS5 indexes for each text
// of the symbol_fts tokens column, in token order: the exact output of the
// tokenizer symbol_fts uses (FTS5's default, unicode61 with its case folding
// and diacritic removal), for text the search package cannot tokenize itself
// (non-ASCII). It runs FTS5 on a private in-memory database — the same SQLite
// library and the same default tokenizer, and no read or write of the store —
// and reads each text's terms back through an fts5vocab instance table.
func SymbolFTSTokenize(ctx context.Context, texts []string) ([][]string, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// The column set mirrors symbol_fts: only "tokens" is indexed, with the
	// table's default tokenizer.
	for _, ddl := range []string{
		`CREATE VIRTUAL TABLE t USING fts5(node_id UNINDEXED, repo_prefix UNINDEXED, tokens)`,
		`CREATE VIRTUAL TABLE v USING fts5vocab(t, instance)`,
	} {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return nil, fmt.Errorf("fts tokenize: %w", err)
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	for i, text := range texts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO t(rowid, node_id, repo_prefix, tokens) VALUES (?, '', '', ?)`, i+1, text); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out := make([][]string, len(texts))
	rows, err := conn.QueryContext(ctx, `SELECT doc, term FROM v ORDER BY doc, offset`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var doc int64
		var term string
		if err := rows.Scan(&doc, &term); err != nil {
			return nil, err
		}
		if doc >= 1 && doc <= int64(len(texts)) {
			out[doc-1] = append(out[doc-1], term)
		}
	}
	return out, rows.Err()
}
