package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	_ "modernc.org/sqlite"
)

// Small walks never open a database. A large walk spills compact values into
// a private temporary artifact, not the application store. Calls open and
// close it independently; no connection or transaction is cached across calls.
type symbolPageStorage struct {
	memory *symbolPageMemory
	path   string
}
type symbolPageMemory struct {
	discovered map[string]bool
	pending    []symbolPageCandidate
	pages      map[int]symbolPageReplay
	bytes      int
}
type symbolPageTransaction struct {
	storage        *symbolPageStorage
	draft          *symbolPageMemory
	database       *sql.DB
	tx             *sql.Tx
	total, pending int
}

func newSymbolPageStorage() *symbolPageStorage {
	return &symbolPageStorage{memory: &symbolPageMemory{discovered: map[string]bool{}, pages: map[int]symbolPageReplay{}}}
}
func (storage *symbolPageStorage) close() {
	if storage.path != "" {
		_ = os.RemoveAll(filepath.Dir(storage.path))
		storage.path = ""
	}
}
func openSymbolPageArtifact(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "PRAGMA synchronous=OFF; PRAGMA cache_size=-1024"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
func candidateBytes(candidate symbolPageCandidate) int {
	return len(candidate.id) + len(candidate.node) + len(candidate.row) + len(candidate.rank) + 128
}
func (storage *symbolPageStorage) spill(ctx context.Context) error {
	if storage.path != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "gortex-symbol-pages-")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "sequence.sqlite")
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dir)
		}
	}()
	db, err := openSymbolPageArtifact(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, `CREATE TABLE candidates(id TEXT PRIMARY KEY, ordinal INTEGER NOT NULL UNIQUE, node BLOB, row BLOB, rank BLOB, emitted INTEGER NOT NULL); CREATE INDEX pending_candidates ON candidates(emitted,ordinal); CREATE TABLE pages(position INTEGER PRIMARY KEY, result BLOB NOT NULL, nodes BLOB NOT NULL)`); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	pending := make(map[string]bool, len(storage.memory.pending))
	for _, candidate := range storage.memory.pending {
		pending[candidate.id] = true
	}
	var retired []string
	for id := range storage.memory.discovered {
		if !pending[id] {
			retired = append(retired, id)
		}
	}
	sort.Strings(retired)
	ordinal := 0
	for _, id := range retired {
		if _, err = tx.ExecContext(ctx, "INSERT INTO candidates(id,ordinal,emitted) VALUES(?,?,1)", id, ordinal); err != nil {
			return err
		}
		ordinal++
	}
	for _, candidate := range storage.memory.pending {
		if _, err = tx.ExecContext(ctx, "INSERT INTO candidates VALUES(?,?,?,?,?,0)", candidate.id, ordinal, []byte(candidate.node), []byte(candidate.row), []byte(candidate.rank)); err != nil {
			return err
		}
		ordinal++
	}
	for position, replay := range storage.memory.pages {
		result, marshalErr := json.Marshal(replay.result)
		if marshalErr != nil {
			return marshalErr
		}
		nodes, marshalErr := json.Marshal(replay.nodes)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO pages VALUES(?,?,?)", position, result, nodes); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	storage.path = path
	storage.memory = nil
	success = true
	return nil
}
func (storage *symbolPageStorage) replay(ctx context.Context, position int) (symbolPageReplay, bool, error) {
	if storage.path == "" {
		replay, ok := storage.memory.pages[position]
		return replay, ok, nil
	}
	db, err := openSymbolPageArtifact(ctx, storage.path)
	if err != nil {
		return symbolPageReplay{}, false, err
	}
	defer db.Close()
	var result, nodes []byte
	err = db.QueryRowContext(ctx, "SELECT result,nodes FROM pages WHERE position=?", position).Scan(&result, &nodes)
	if err == sql.ErrNoRows {
		return symbolPageReplay{}, false, nil
	}
	if err != nil {
		return symbolPageReplay{}, false, err
	}
	replay := symbolPageReplay{result: &mcplib.CallToolResult{}}
	if err = json.Unmarshal(result, replay.result); err != nil {
		return symbolPageReplay{}, false, err
	}
	if err = json.Unmarshal(nodes, &replay.nodes); err != nil {
		return symbolPageReplay{}, false, err
	}
	return replay, true, nil
}
func (storage *symbolPageStorage) begin(ctx context.Context, candidates []symbolPageCandidate) (*symbolPageTransaction, error) {
	if storage.path == "" {
		added := 0
		for _, candidate := range candidates {
			if !storage.memory.discovered[candidate.id] {
				added += candidateBytes(candidate)
			}
		}
		if storage.memory.bytes+added > symbolPageSequenceBytes {
			if err := storage.spill(ctx); err != nil {
				return nil, err
			}
		}
	}
	stage := &symbolPageTransaction{storage: storage}
	if storage.path == "" {
		memory := storage.memory
		stage.draft = &symbolPageMemory{discovered: make(map[string]bool, len(memory.discovered)+len(candidates)), pending: append([]symbolPageCandidate(nil), memory.pending...), pages: memory.pages, bytes: memory.bytes}
		for id := range memory.discovered {
			stage.draft.discovered[id] = true
		}
		for _, candidate := range candidates {
			if stage.draft.discovered[candidate.id] {
				continue
			}
			stage.draft.discovered[candidate.id] = true
			stage.draft.pending = append(stage.draft.pending, candidate)
			stage.draft.bytes += candidateBytes(candidate)
		}
		stage.total = len(stage.draft.discovered)
		stage.pending = len(stage.draft.pending)
		return stage, nil
	}
	db, err := openSymbolPageArtifact(ctx, storage.path)
	if err != nil {
		return nil, err
	}
	stage.database = db
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	stage.tx = tx
	success := false
	defer func() {
		if !success {
			stage.rollback()
		}
	}()
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM candidates").Scan(&stage.total); err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		result, insertErr := tx.ExecContext(ctx, "INSERT OR IGNORE INTO candidates VALUES(?,?,?,?,?,0)", candidate.id, stage.total, []byte(candidate.node), []byte(candidate.row), []byte(candidate.rank))
		if insertErr != nil {
			return nil, insertErr
		}
		n, insertErr := result.RowsAffected()
		if insertErr != nil {
			return nil, insertErr
		}
		stage.total += int(n)
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM candidates WHERE emitted=0").Scan(&stage.pending); err != nil {
		return nil, err
	}
	success = true
	return stage, nil
}
func (stage *symbolPageTransaction) page(ctx context.Context, limit int) ([]symbolPageCandidate, error) {
	if stage.tx == nil {
		return stage.draft.pending[:min(limit, len(stage.draft.pending))], nil
	}
	rows, err := stage.tx.QueryContext(ctx, "SELECT id,node,row,rank FROM candidates WHERE emitted=0 ORDER BY ordinal LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []symbolPageCandidate
	for rows.Next() {
		var candidate symbolPageCandidate
		var node, row, rank []byte
		if err = rows.Scan(&candidate.id, &node, &row, &rank); err != nil {
			return nil, err
		}
		candidate.node, candidate.row, candidate.rank = node, row, rank
		page = append(page, candidate)
	}
	return page, rows.Err()
}
func (stage *symbolPageTransaction) commit(ctx context.Context, position int, replay symbolPageReplay, returned []symbolPageCandidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := json.Marshal(replay.result)
	if err != nil {
		return err
	}
	nodes, err := json.Marshal(replay.nodes)
	if err != nil {
		return err
	}
	if stage.tx == nil && stage.draft.bytes+len(result)+len(nodes) > symbolPageSequenceBytes {
		// Spill only committed history, then stage this call again. Prior cursor
		// replay remains usable if the filesystem or cancellation rejects the spill.
		if err = stage.storage.spill(ctx); err != nil {
			return err
		}
		next, err := stage.storage.begin(ctx, stage.draft.pending)
		if err != nil {
			return err
		}
		defer next.rollback()
		return next.commit(ctx, position, replay, returned)
	}
	if stage.tx == nil {
		seen := make(map[string]bool, len(returned))
		for _, candidate := range returned {
			seen[candidate.id] = true
		}
		kept := make([]symbolPageCandidate, 0, len(stage.draft.pending)-len(returned))
		for _, candidate := range stage.draft.pending {
			if !seen[candidate.id] {
				kept = append(kept, candidate)
			}
		}
		pages := make(map[int]symbolPageReplay, len(stage.draft.pages)+1)
		for index, value := range stage.draft.pages {
			pages[index] = value
		}
		pages[position] = replay
		stage.draft.pending = kept
		stage.draft.pages = pages
		stage.draft.bytes += len(result) + len(nodes)
		stage.storage.memory = stage.draft
		return nil
	}
	for _, candidate := range returned {
		if _, err = stage.tx.ExecContext(ctx, "UPDATE candidates SET emitted=1,node=NULL,row=NULL,rank=NULL WHERE id=?", candidate.id); err != nil {
			return err
		}
	}
	if _, err = stage.tx.ExecContext(ctx, "INSERT INTO pages VALUES(?,?,?)", position, result, nodes); err != nil {
		return err
	}
	return stage.tx.Commit()
}
func (stage *symbolPageTransaction) rollback() {
	if stage.tx != nil {
		_ = stage.tx.Rollback()
	}
	if stage.database != nil {
		_ = stage.database.Close()
	}
}
