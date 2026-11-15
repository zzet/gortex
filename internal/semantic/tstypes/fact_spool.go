package tstypes

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	_ "modernc.org/sqlite"
)

// A page is deliberately capped by both files and encoded bytes. Facts from a
// single source file are indivisible, but source parsing already rejects files
// larger than maxFileBytes, so even that exception has a hard upstream bound.
// The byte cap charges only the paged class's payloads; the imports side-fetch
// that hydrates every non-imports page rides on top, so peak page bytes can
// modestly exceed it.
const (
	tstypesFactPageFiles = 32
	tstypesFactPageBytes = 4 << 20
	tstypesSQLChunkRows  = 64
)

type factSpool struct {
	db   *sql.DB
	path string
}

type factPageStats struct {
	Class      factClass
	Files      int
	Facts      int
	Bytes      int
	CacheNodes int
	CacheEdges int
	CacheNames int
}

type stagedResolvedAlias struct {
	typeID  string
	alias   string
	traitID string
	method  string
}

func newFactSpool() (*factSpool, error) {
	file, err := os.CreateTemp("", "gortex-tstypes-facts-*.sqlite")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=OFF;
PRAGMA synchronous=OFF;
PRAGMA cache_size=-4096;
CREATE TABLE files (
  file_path TEXT PRIMARY KEY,
  repo_prefix TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE file_facts (
  class INTEGER NOT NULL,
  file_path TEXT NOT NULL,
  repo_prefix TEXT NOT NULL,
  payload BLOB NOT NULL,
  PRIMARY KEY (class, file_path)
) WITHOUT ROWID;
CREATE TABLE resolved_aliases (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  type_id TEXT NOT NULL,
  alias TEXT NOT NULL,
  trait_id TEXT NOT NULL,
  method TEXT NOT NULL
);
CREATE INDEX idx_resolved_alias_type ON resolved_aliases(type_id, seq);`); err != nil {
		_ = db.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &factSpool{db: db, path: path}, nil
}

func (s *factSpool) close() {
	if s == nil {
		return
	}
	if s.db != nil {
		_ = s.db.Close()
	}
	_ = os.Remove(s.path)
	_ = os.Remove(s.path + "-journal")
	_ = os.Remove(s.path + "-wal")
	_ = os.Remove(s.path + "-shm")
}

type encodedCall struct {
	Line              int          `json:"line"`
	Method            string       `json:"method"`
	RecvType          string       `json:"recv_type,omitempty"`
	RecvPendingCallee string       `json:"recv_pending_callee,omitempty"`
	RecvCallTypeArg   string       `json:"recv_call_type_arg,omitempty"`
	RecvIdent         string       `json:"recv_ident,omitempty"`
	RecvChain         *encodedCall `json:"recv_chain,omitempty"`
	Inferred          bool         `json:"inferred,omitempty"`
	ArgCount          int          `json:"arg_count,omitempty"`
	ArgKnown          bool         `json:"arg_known,omitempty"`
	Owner             string       `json:"owner,omitempty"`
}

type encodedSuper struct {
	TypeName  string         `json:"type_name"`
	SuperName string         `json:"super_name"`
	Kind      graph.EdgeKind `json:"kind,omitempty"`
	Line      int            `json:"line,omitempty"`
}

type encodedMeta struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Owner string `json:"owner,omitempty"`
	Name  string `json:"name,omitempty"`
	Line  int    `json:"line,omitempty"`
}

type encodedAlias struct {
	TypeName string `json:"type_name"`
	Alias    string `json:"alias"`
	Trait    string `json:"trait,omitempty"`
	Method   string `json:"method"`
	Line     int    `json:"line,omitempty"`
}

func encodeCallFact(in *callFact) *encodedCall {
	if in == nil {
		return nil
	}
	return &encodedCall{
		Line: in.line, Method: in.method, RecvType: in.recvType,
		RecvPendingCallee: in.recvPendingCallee, RecvCallTypeArg: in.recvCallTypeArg,
		RecvIdent: in.recvIdent, RecvChain: encodeCallFact(in.recvChain),
		Inferred: in.inferred, ArgCount: in.argCount, ArgKnown: in.argKnown,
		Owner: in.owner,
	}
}

func decodeCallFact(in *encodedCall) *callFact {
	if in == nil {
		return nil
	}
	return &callFact{
		line: in.Line, method: in.Method, recvType: in.RecvType,
		recvPendingCallee: in.RecvPendingCallee, recvCallTypeArg: in.RecvCallTypeArg,
		recvIdent: in.RecvIdent, recvChain: decodeCallFact(in.RecvChain),
		inferred: in.Inferred, argCount: in.ArgCount, argKnown: in.ArgKnown,
		owner: in.Owner,
	}
}

// factClass partitions one file's facts by the phase that applies them.
// Values are stable spool row keys — the temp spool never outlives a pass,
// so renumbering is safe across builds but pointless.
type factClass int

const (
	classImports factClass = iota
	classSupers
	classMetas
	classAliases
	classCalls

	factClassCount = iota // 5 — auto-tracks any class added above
)

// marshalClassPayloads encodes one file's facts as per-class JSON arrays,
// omitting empty classes entirely — a file with no aliases contributes no
// aliases row, which is what lets a phase skip files wholesale.
func marshalClassPayloads(facts *fileFacts) (map[factClass][]byte, error) {
	out := make(map[factClass][]byte, 5)
	put := func(class factClass, v any, n int) error {
		if n == 0 {
			return nil
		}
		blob, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out[class] = blob
		return nil
	}
	if err := put(classImports, facts.imports, len(facts.imports)); err != nil {
		return nil, err
	}
	wireSupers := make([]encodedSuper, 0, len(facts.supers))
	for _, fact := range facts.supers {
		wireSupers = append(wireSupers, encodedSuper{fact.typeName, fact.superName, fact.kind, fact.line})
	}
	if err := put(classSupers, wireSupers, len(wireSupers)); err != nil {
		return nil, err
	}
	wireMetas := make([]encodedMeta, 0, len(facts.metas))
	for _, fact := range facts.metas {
		wireMetas = append(wireMetas, encodedMeta{fact.key, fact.value, fact.owner, fact.name, fact.line})
	}
	if err := put(classMetas, wireMetas, len(wireMetas)); err != nil {
		return nil, err
	}
	wireAliases := make([]encodedAlias, 0, len(facts.aliases))
	for _, fact := range facts.aliases {
		wireAliases = append(wireAliases, encodedAlias{fact.typeName, fact.alias, fact.trait, fact.method, fact.line})
	}
	if err := put(classAliases, wireAliases, len(wireAliases)); err != nil {
		return nil, err
	}
	wireCalls := make([]encodedCall, 0, len(facts.calls))
	for i := range facts.calls {
		wireCalls = append(wireCalls, *encodeCallFact(&facts.calls[i]))
	}
	if err := put(classCalls, wireCalls, len(wireCalls)); err != nil {
		return nil, err
	}
	return out, nil
}

// unmarshalClassPayload appends one class's decoded facts onto facts.
func unmarshalClassPayload(facts *fileFacts, class factClass, payload []byte) error {
	switch class {
	case classImports:
		return json.Unmarshal(payload, &facts.imports)
	case classSupers:
		var wire []encodedSuper
		if err := json.Unmarshal(payload, &wire); err != nil {
			return err
		}
		for _, fact := range wire {
			facts.supers = append(facts.supers, superFact{fact.TypeName, fact.SuperName, fact.Kind, fact.Line})
		}
	case classMetas:
		var wire []encodedMeta
		if err := json.Unmarshal(payload, &wire); err != nil {
			return err
		}
		for _, fact := range wire {
			facts.metas = append(facts.metas, metaFact{fact.Key, fact.Value, fact.Owner, fact.Name, fact.Line})
		}
	case classAliases:
		var wire []encodedAlias
		if err := json.Unmarshal(payload, &wire); err != nil {
			return err
		}
		for _, fact := range wire {
			facts.aliases = append(facts.aliases, aliasFact{fact.TypeName, fact.Alias, fact.Trait, fact.Method, fact.Line})
		}
	case classCalls:
		var wire []encodedCall
		if err := json.Unmarshal(payload, &wire); err != nil {
			return err
		}
		for i := range wire {
			facts.calls = append(facts.calls, *decodeCallFact(&wire[i]))
		}
	default:
		return fmt.Errorf("unknown fact class %d", class)
	}
	return nil
}

type stagedFileFacts struct {
	facts    *fileFacts
	payloads map[factClass][]byte
	bytes    int
}

func stageFileFacts(facts *fileFacts) (stagedFileFacts, error) {
	payloads, err := marshalClassPayloads(facts)
	if err != nil {
		return stagedFileFacts{}, err
	}
	total := 0
	for _, payload := range payloads {
		total += len(payload)
	}
	return stagedFileFacts{facts: facts, payloads: payloads, bytes: total}, nil
}

// appendFiles performs one bounded transaction for a writer page; it is never
// called once per parser worker or source file. Each record lands as one
// files row plus a file_facts row per non-empty class.
func (s *factSpool) appendFiles(records []stagedFileFacts) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for start := 0; start < len(records); start += tstypesSQLChunkRows {
		end := min(start+tstypesSQLChunkRows, len(records))
		fileValues := make([]string, 0, end-start)
		fileArgs := make([]any, 0, (end-start)*2)
		classValues := make([]string, 0, (end-start)*factClassCount)
		classArgs := make([]any, 0, (end-start)*factClassCount*4)
		for _, record := range records[start:end] {
			if record.facts == nil {
				continue
			}
			fileValues = append(fileValues, "(?,?)")
			fileArgs = append(fileArgs, record.facts.file, record.facts.repoPrefix)
			for class, payload := range record.payloads {
				classValues = append(classValues, "(?,?,?,?)")
				classArgs = append(classArgs, int(class), record.facts.file, record.facts.repoPrefix, payload)
			}
		}
		if len(fileValues) > 0 {
			query := `INSERT INTO files(file_path,repo_prefix) VALUES ` + strings.Join(fileValues, ",") + `
ON CONFLICT(file_path) DO UPDATE SET repo_prefix=excluded.repo_prefix`
			if _, err := tx.Exec(query, fileArgs...); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if len(classValues) > 0 {
			query := `INSERT INTO file_facts(class,file_path,repo_prefix,payload) VALUES ` + strings.Join(classValues, ",") + `
ON CONFLICT(class,file_path) DO UPDATE SET repo_prefix=excluded.repo_prefix,payload=excluded.payload`
			if _, err := tx.Exec(query, classArgs...); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

// pageClass reads a deterministic keyset page of one fact class. Every
// returned fileFacts carries that class plus the file's imports — buildIndex
// needs the import map in every phase. Files without facts of the class have
// no row and are skipped wholesale.
func (s *factSpool) pageClass(ctx context.Context, class factClass, after string) ([]*fileFacts, string, factPageStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file_path,repo_prefix,payload FROM file_facts
WHERE class = ? AND file_path > ? ORDER BY file_path LIMIT ?`, int(class), after, tstypesFactPageFiles)
	if err != nil {
		return nil, after, factPageStats{}, err
	}
	page := make([]*fileFacts, 0, tstypesFactPageFiles)
	stats := factPageStats{Class: class}
	last := after
	for rows.Next() {
		var filePath, repoPrefix string
		var payload []byte
		if err := rows.Scan(&filePath, &repoPrefix, &payload); err != nil {
			_ = rows.Close()
			return nil, last, stats, err
		}
		if len(page) > 0 && stats.Bytes+len(payload) > tstypesFactPageBytes {
			break
		}
		facts := &fileFacts{file: filePath, repoPrefix: repoPrefix}
		if err := unmarshalClassPayload(facts, class, payload); err != nil {
			_ = rows.Close()
			return nil, last, stats, fmt.Errorf("decode facts for %s: %w", filePath, err)
		}
		page = append(page, facts)
		last = filePath
		stats.Files++
		stats.Bytes += len(payload)
	}
	// Rows.Close reports the driver's close error, not the iteration error
	// (rs.lasterr), so a failure part-way through the scan is invisible
	// without Err. The empty page that produced would read as "class
	// exhausted" to the driver loop in provider_stream.go, silently dropping
	// the rest of this phase's facts. The break above is not an error, so
	// Err stays nil on the byte-budget path.
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, last, stats, err
	}
	if err := rows.Close(); err != nil {
		return nil, last, stats, err
	}
	if class != classImports && len(page) > 0 {
		if err := s.attachImports(ctx, page); err != nil {
			return nil, last, stats, err
		}
	}
	for _, facts := range page {
		stats.Facts += len(facts.imports) + len(facts.calls) + len(facts.supers) + len(facts.metas) + len(facts.aliases)
	}
	return page, last, stats, nil
}

// attachImports hydrates the page files' imports rows in one bounded IN query.
func (s *factSpool) attachImports(ctx context.Context, page []*fileFacts) error {
	byFile := make(map[string]*fileFacts, len(page))
	args := make([]any, 0, len(page)+1)
	args = append(args, int(classImports))
	for _, facts := range page {
		byFile[facts.file] = facts
		args = append(args, facts.file)
	}
	placeholders := strings.Repeat(",?", len(page))[1:]
	rows, err := s.db.QueryContext(ctx, `SELECT file_path,payload FROM file_facts
WHERE class = ? AND file_path IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var filePath string
		var payload []byte
		if err := rows.Scan(&filePath, &payload); err != nil {
			return err
		}
		if facts := byFile[filePath]; facts != nil {
			if err := unmarshalClassPayload(facts, classImports, payload); err != nil {
				return fmt.Errorf("decode imports for %s: %w", filePath, err)
			}
		}
	}
	return rows.Err()
}

// pageFiles lists staged files as bare stubs for the coverage walk — no
// payload touched anywhere.
func (s *factSpool) pageFiles(ctx context.Context, after string) ([]*fileFacts, string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file_path,repo_prefix FROM files
WHERE file_path > ? ORDER BY file_path LIMIT ?`, after, tstypesFactPageFiles)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	page := make([]*fileFacts, 0, tstypesFactPageFiles)
	last := after
	for rows.Next() {
		var filePath, repoPrefix string
		if err := rows.Scan(&filePath, &repoPrefix); err != nil {
			return nil, last, err
		}
		page = append(page, &fileFacts{file: filePath, repoPrefix: repoPrefix})
		last = filePath
	}
	return page, last, rows.Err()
}

func (s *factSpool) appendAliases(records []stagedResolvedAlias) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for start := 0; start < len(records); start += tstypesSQLChunkRows {
		end := start + tstypesSQLChunkRows
		if end > len(records) {
			end = len(records)
		}
		values := make([]string, end-start)
		args := make([]any, 0, (end-start)*4)
		for i, record := range records[start:end] {
			values[i] = "(?,?,?,?)"
			args = append(args, record.typeID, record.alias, record.traitID, record.method)
		}
		if _, err := tx.Exec(`INSERT INTO resolved_aliases(type_id,alias,trait_id,method) VALUES `+
			strings.Join(values, ","), args...); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *factSpool) aliasesForTypeIDs(ctx context.Context, ids []string) ([]stagedResolvedAlias, error) {
	ids = uniqueSortedIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	const chunk = 400
	var out []stagedResolvedAlias
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		values := strings.Repeat(",?", end-start)[1:]
		args := make([]any, end-start)
		for i, id := range ids[start:end] {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT type_id,alias,trait_id,method FROM resolved_aliases
WHERE type_id IN (`+values+`) ORDER BY type_id,seq`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var record stagedResolvedAlias
			if err := rows.Scan(&record.typeID, &record.alias, &record.traitID, &record.method); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, record)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].typeID < out[j].typeID })
	return out, nil
}
