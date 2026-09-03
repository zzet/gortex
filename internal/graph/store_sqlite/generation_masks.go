package store_sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The ownership-mask accessors.
//
// Every call here binds the handle's generation, exactly like a payload
// sidecar: a mask read returns only this generation's claims, and a mask write
// stamps them with this generation. Reading through a base handle is legal and
// returns nothing — the base corpus owns what it carries and states no claims.
// WRITING through a base handle is refused: a base-generation mask would be a
// claim about the only layer that has nothing beneath it, so it can only be a
// caller that forgot to derive a handle.
//
// Mask writes are not payload mutations. They add no node, edge or sidecar row,
// so they do not invalidate an analysis generation; they take the same mutation
// gate every durable write in this package takes, and nothing more.

// OwnershipMode is what a generation claims about a masked path or source. The
// two vocabularies below differ: a file may be replaced or deleted, while an
// edge source may only be replaced.
type OwnershipMode string

const (
	// OwnershipReplace claims the generation's own payload supersedes the
	// layer below, including when that payload is empty.
	OwnershipReplace OwnershipMode = "replace"
	// OwnershipDelete claims the path is gone and nothing below shows through.
	OwnershipDelete OwnershipMode = "delete"
)

var fileOwnershipModes = []OwnershipMode{OwnershipReplace, OwnershipDelete}

// edgeSourceOwnershipModes is deliberately narrower than fileOwnershipModes:
// withdrawing a source's edges without replacing them is what an empty
// replacement already says, so there is no delete marker to write.
var edgeSourceOwnershipModes = []OwnershipMode{OwnershipReplace}

// ProducerState is how complete one producer's contribution to a generation is.
type ProducerState string

const (
	// ProducerStateComplete means the producer finished and its payload is whole.
	ProducerStateComplete ProducerState = "complete"
	// ProducerStateIncomplete means the producer finished but its payload is
	// partial, so an absence it leaves behind is not a fact.
	ProducerStateIncomplete ProducerState = "incomplete"
	// ProducerStateBuilding means the producer is still running.
	ProducerStateBuilding ProducerState = "building"
	// ProducerStateUnavailable means the producer could not run at all.
	ProducerStateUnavailable ProducerState = "unavailable"
	// ProducerStateDisabledByConfig means the producer was switched off, so its
	// absence is intended rather than a failure.
	ProducerStateDisabledByConfig ProducerState = "disabled_by_config"
)

var producerStates = []ProducerState{
	ProducerStateComplete,
	ProducerStateIncomplete,
	ProducerStateBuilding,
	ProducerStateUnavailable,
	ProducerStateDisabledByConfig,
}

// Mask errors. A caller can tell a misuse of the base handle from a malformed
// value from a generation whose masks contradict its payload without matching
// on strings.
var (
	// ErrMasksAtBaseGeneration means a mask write was attempted through a
	// handle pinned to the base generation, where a mask has no meaning.
	ErrMasksAtBaseGeneration = errors.New("store_sqlite: ownership masks require a derived view generation")

	// ErrGenerationMaskInvalidValue means a mask write carried a value outside
	// its vocabulary, or left a required identifier empty.
	ErrGenerationMaskInvalidValue = errors.New("store_sqlite: invalid generation mask value")

	// ErrGenerationMaskIntegrity means a generation's masks contradict the
	// payload it carries at the same generation.
	ErrGenerationMaskIntegrity = errors.New("store_sqlite: generation mask contradicts its payload")
)

// FileMask is one file-level ownership claim.
type FileMask struct {
	RepoPrefix string
	FilePath   string
	Mode       OwnershipMode
}

// EdgeSourceMask is one source node whose outgoing edge set the generation
// replaces without owning the node's file.
type EdgeSourceMask struct {
	SourceID string
	Mode     OwnershipMode
}

// ProducerCompleteness is one producer's contribution state for a generation.
type ProducerCompleteness struct {
	Producer string
	State    ProducerState
	Reason   string
}

// generationMaskChunk bounds rows per multi-row INSERT. The widest mask row
// binds 4 host parameters, so 200 rows = 800 params, under SQLite's
// conservative 999 default.
const generationMaskChunk = 200

// generationMaskViolationLimit caps how many contradictions one integrity
// report names. A wholly broken generation must not build an unbounded error.
const generationMaskViolationLimit = 8

// requireDerivedGeneration rejects a mask write made through a base handle.
func (s *Store) requireDerivedGeneration() error {
	if s.viewGen == baseViewGeneration {
		return fmt.Errorf("%w: handle is pinned to generation %d", ErrMasksAtBaseGeneration, s.viewGen)
	}
	return nil
}

// requireMaskValue rejects a value outside its vocabulary.
func requireMaskValue[T ~string](field string, value T, allowed []T) error {
	if validCatalogValue(value, allowed) {
		return nil
	}
	return fmt.Errorf("%w: %s %q", ErrGenerationMaskInvalidValue, field, string(value))
}

// requireMaskID rejects an empty identifier, which would otherwise become a
// real row nothing can address.
func requireMaskID(field, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s must not be empty", ErrGenerationMaskInvalidValue, field)
	}
	return nil
}

// SetFileMasks upserts file-level ownership claims for this generation,
// chunked under the host-parameter limit and applied in one transaction.
// Idempotent on the (view_gen, repo_prefix, file_path) primary key; empty input
// is a no-op that still refuses a base handle, so a caller cannot discover the
// misuse only once it has rows.
func (s *Store) SetFileMasks(masks []FileMask) error {
	if err := s.requireDerivedGeneration(); err != nil {
		return err
	}
	for _, mask := range masks {
		if err := requireMaskID("file_path", mask.FilePath); err != nil {
			return err
		}
		if err := requireMaskValue("ownership_mode", mask.Mode, fileOwnershipModes); err != nil {
			return err
		}
	}
	return s.writeMaskRows(`INSERT OR REPLACE INTO generation_file_masks
  (view_gen, repo_prefix, file_path, ownership_mode) VALUES `,
		len(masks), func(i int) []any {
			return []any{s.viewGen, masks[i].RepoPrefix, masks[i].FilePath, string(masks[i].Mode)}
		})
}

// FileMasks returns every file-level claim this generation makes, ordered by
// the primary key so the read walks the table in index order.
func (s *Store) FileMasks() ([]FileMask, error) {
	rows, err := s.db.Query(`
SELECT repo_prefix, file_path, ownership_mode
  FROM generation_file_masks WHERE view_gen = ?
 ORDER BY repo_prefix, file_path`, s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileMask{}
	for rows.Next() {
		var mask FileMask
		var mode string
		if err := rows.Scan(&mask.RepoPrefix, &mask.FilePath, &mode); err != nil {
			return nil, err
		}
		mask.Mode = OwnershipMode(mode)
		out = append(out, mask)
	}
	return out, rows.Err()
}

// FileMaskFor reads one claim through the whole primary key. The bool is false
// when this generation makes no claim about the path, which is the common case:
// the masks are sparse and an unmentioned path is inherited.
func (s *Store) FileMaskFor(repoPrefix, filePath string) (OwnershipMode, bool, error) {
	var mode string
	err := s.db.QueryRow(`
SELECT ownership_mode FROM generation_file_masks
 WHERE view_gen = ? AND repo_prefix = ? AND file_path = ?`,
		s.viewGen, repoPrefix, filePath).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return OwnershipMode(mode), true, nil
}

// SetNodeTombstones records node identities this generation removes without
// claiming their whole file. Idempotent on (view_gen, node_id).
func (s *Store) SetNodeTombstones(nodeIDs []string) error {
	if err := s.requireDerivedGeneration(); err != nil {
		return err
	}
	for _, nodeID := range nodeIDs {
		if err := requireMaskID("node_id", nodeID); err != nil {
			return err
		}
	}
	return s.writeMaskRows(`INSERT OR REPLACE INTO generation_node_tombstones (view_gen, node_id) VALUES `,
		len(nodeIDs), func(i int) []any {
			return []any{s.viewGen, nodeIDs[i]}
		})
}

// NodeTombstones returns every node identity this generation removes.
func (s *Store) NodeTombstones() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT node_id FROM generation_node_tombstones WHERE view_gen = ? ORDER BY node_id`, s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			return nil, err
		}
		out = append(out, nodeID)
	}
	return out, rows.Err()
}

// SetEdgeSourceMasks upserts edge-set replacement markers for this generation.
// Idempotent on (view_gen, source_id).
func (s *Store) SetEdgeSourceMasks(masks []EdgeSourceMask) error {
	if err := s.requireDerivedGeneration(); err != nil {
		return err
	}
	for _, mask := range masks {
		if err := requireMaskID("source_id", mask.SourceID); err != nil {
			return err
		}
		if err := requireMaskValue("ownership_mode", mask.Mode, edgeSourceOwnershipModes); err != nil {
			return err
		}
	}
	return s.writeMaskRows(`INSERT OR REPLACE INTO generation_edge_sources
  (view_gen, source_id, ownership_mode) VALUES `,
		len(masks), func(i int) []any {
			return []any{s.viewGen, masks[i].SourceID, string(masks[i].Mode)}
		})
}

// EdgeSourceMasks returns every edge-set replacement marker this generation
// carries.
func (s *Store) EdgeSourceMasks() ([]EdgeSourceMask, error) {
	rows, err := s.db.Query(
		`SELECT source_id, ownership_mode FROM generation_edge_sources WHERE view_gen = ? ORDER BY source_id`,
		s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EdgeSourceMask{}
	for rows.Next() {
		var mask EdgeSourceMask
		var mode string
		if err := rows.Scan(&mask.SourceID, &mode); err != nil {
			return nil, err
		}
		mask.Mode = OwnershipMode(mode)
		out = append(out, mask)
	}
	return out, rows.Err()
}

// SetProducerState records one producer's contribution state for this
// generation. One row per producer, replaced by each report.
func (s *Store) SetProducerState(row ProducerCompleteness) error {
	return s.SetProducerStates([]ProducerCompleteness{row})
}

// SetProducerStates records a generation's producer states atomically. The
// batch form avoids opening one SQLite write transaction per producer while a
// derived view is being prepared.
func (s *Store) SetProducerStates(rows []ProducerCompleteness) error {
	if err := s.requireDerivedGeneration(); err != nil {
		return err
	}
	for _, row := range rows {
		if err := requireMaskID("producer", row.Producer); err != nil {
			return err
		}
		if err := requireMaskValue("state", row.State, producerStates); err != nil {
			return err
		}
	}
	return s.writeMaskRows(`INSERT OR REPLACE INTO generation_producer_completeness
  (view_gen, producer, state, reason) VALUES `,
		len(rows), func(i int) []any {
			row := rows[i]
			return []any{s.viewGen, row.Producer, string(row.State), row.Reason}
		})
}

// ProducerStates returns every producer's contribution state for this
// generation.
func (s *Store) ProducerStates() ([]ProducerCompleteness, error) {
	rows, err := s.db.Query(
		`SELECT producer, state, reason FROM generation_producer_completeness WHERE view_gen = ? ORDER BY producer`,
		s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProducerCompleteness{}
	for rows.Next() {
		var row ProducerCompleteness
		var state string
		if err := rows.Scan(&row.Producer, &state, &row.Reason); err != nil {
			return nil, err
		}
		row.State = ProducerState(state)
		out = append(out, row)
	}
	return out, rows.Err()
}

// writeMaskRows applies one multi-row INSERT per chunk inside a single
// transaction. row(i) returns the bound values for row i, all of the same
// width, so the VALUES fragment is built once. Empty input opens no
// transaction.
func (s *Store) writeMaskRows(insert string, total int, row func(i int) []any) error {
	if total == 0 {
		return nil
	}
	width := len(row(0))
	placeholders := "(?" + strings.Repeat(", ?", width-1) + ")"

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < total; start += generationMaskChunk {
		end := min(start+generationMaskChunk, total)
		var stmt strings.Builder
		stmt.WriteString(insert)
		args := make([]any, 0, (end-start)*width)
		for i := start; i < end; i++ {
			if i > start {
				stmt.WriteByte(',')
			}
			stmt.WriteString(placeholders)
			args = append(args, row(i)...)
		}
		if _, err := tx.Exec(stmt.String(), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ValidateGenerationMasks checks this generation's file masks against the
// payload the same generation carries.
//
// The rule, and why it is the cheapest sound one available. A generation
// records the files it processed in the files sidecar — one row per file it
// read, carrying a node count that may legitimately be zero — its symbols in
// nodes, the call sites it extracted in edges, and its indexed documents in
// the content docid map. All four key the file by the repo-prefixed graph
// path (see persistFileMeta in internal/indexer), so one value probes them
// all:
//
//   - a 'replace' mask is valid when the generation has a files row for the
//     path, OR at least one node at that path. The files row IS the explicit
//     empty-file marker the rule needs — "I read this file and it produced no
//     symbols" is already expressible as a row with node_count 0 — so replacing
//     a file with nothing needs no extra marker table. The node probe keeps the
//     rule sound for a producer that writes payload without the optional files
//     sidecar.
//   - a 'delete' mask is valid when the generation carries NOTHING at the
//     path: no files row, no node, no edge whose call site sits there, and no
//     content document indexed at it. A generation that carries a file and
//     simultaneously claims to have deleted it is self-contradictory, and the
//     two claims cannot both be served.
//
// The edge and content probes belong to the delete arm alone. An edge or a
// document at a path is payload the delete claim contradicts, but neither is
// the generation claiming the FILE — a re-emitted edge set names its source
// symbols, not the file it re-derived them from — so a replace mask still
// needs a files row or a node behind it.
//
// The probes are indexed: files on the leading columns of its (view_gen,
// repo_prefix, file_path) primary key; nodes on nodes_by_file, which is a
// COVERING index here because a WITHOUT ROWID index entry carries the primary
// key — view_gen included — so the generation filter costs no table lookup;
// content docids on content_fts_rowid_by_file, whose leading columns are
// exactly (view_gen, file_path). edges_by_file leads with file_path and
// carries no generation, so that probe seeks the path and filters the
// generation over the few edges standing at it — a seek, not a scan, and only
// for the masks that claim a deletion. The whole check is one set-oriented
// query returning only the violating rows, capped at
// generationMaskViolationLimit.
//
// The CTE is deliberately left un-materialized: SQLite flattens it, so mask
// rows stream and the LIMIT stops the walk at the first few contradictions
// instead of probing every mask first. The price is that `covered` is
// evaluated once per arm of the OR, which is two indexed point seeks.
//
// A base handle has no masks, so this reports nothing rather than refusing.
func (s *Store) ValidateGenerationMasks() error {
	rows, err := s.db.Query(`
WITH masked(repo_prefix, file_path, ownership_mode, covered) AS (
    SELECT m.repo_prefix, m.file_path, m.ownership_mode,
           EXISTS (SELECT 1 FROM files AS f
                    WHERE f.view_gen = m.view_gen
                      AND f.repo_prefix = m.repo_prefix
                      AND f.file_path = m.file_path)
        OR EXISTS (SELECT 1 FROM nodes AS n
                    WHERE n.file_path = m.file_path AND n.view_gen = m.view_gen)
      FROM generation_file_masks AS m
     WHERE m.view_gen = ?
)
SELECT repo_prefix, file_path, ownership_mode FROM masked
 WHERE (ownership_mode = ? AND covered = 0)
    OR (ownership_mode = ? AND (covered = 1
        OR EXISTS (SELECT 1 FROM edges AS e
                    WHERE e.file_path = masked.file_path AND e.view_gen = ?)
        OR EXISTS (SELECT 1 FROM content_fts_rowid AS c
                    WHERE c.view_gen = ? AND c.file_path = masked.file_path)))
 ORDER BY repo_prefix, file_path
 LIMIT ?`,
		s.viewGen, string(OwnershipReplace), string(OwnershipDelete),
		s.viewGen, s.viewGen, generationMaskViolationLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var repoPrefix, filePath, mode string
		if err := rows.Scan(&repoPrefix, &filePath, &mode); err != nil {
			return err
		}
		reason := "no payload row at this generation"
		if OwnershipMode(mode) == OwnershipDelete {
			reason = "payload rows still present at this generation"
		}
		violations = append(violations, fmt.Sprintf("%s mask on %s/%s: %s", mode, repoPrefix, filePath, reason))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf("%w: generation %d: %s", ErrGenerationMaskIntegrity, s.viewGen, strings.Join(violations, "; "))
}
