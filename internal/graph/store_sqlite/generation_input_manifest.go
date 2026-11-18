package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
)

// The admitted-input manifest store surface. See the table comments beside
// generationInputManifestTableBody in schema.go for what the manifest records
// and why it exists.
//
// Writes go through the same gate every payload write does (beginWriteContext:
// the generation's seal plus, for a managed generation, the catalog state), so
// a manifest can only be written while its generation is building and is
// sealed with the payload by PublishPayloadGeneration. Reads are allowed at any
// state — a published parent's manifest is exactly what the next build reads.

// InputManifestVersion is the manifest format this build writes. A reader
// treats a manifest of any other version as unusable, never as empty.
const InputManifestVersion = 1

// InputManifestEntryState is what the sample found at one path.
type InputManifestEntryState string

const (
	// InputManifestPresent: the path exists in the working tree with content
	// that differs from HEAD (or HEAD lacks it).
	InputManifestPresent InputManifestEntryState = "present"
	// InputManifestAbsent: the path is gone from the working tree.
	InputManifestAbsent InputManifestEntryState = "absent"
	// InputManifestOpaque: the path is a symlink, submodule or other entry
	// whose content the sampler does not hash.
	InputManifestOpaque InputManifestEntryState = "opaque"
	// InputManifestHeadEqual: the path was dirty in git's view but its bytes
	// and mode equal HEAD's.
	InputManifestHeadEqual InputManifestEntryState = "head_equal"
)

var inputManifestEntryStates = []InputManifestEntryState{
	InputManifestPresent, InputManifestAbsent, InputManifestOpaque, InputManifestHeadEqual,
}

// InputManifestAdmission is the builder's admission verdict for one path.
type InputManifestAdmission string

const (
	InputManifestAdmitted        InputManifestAdmission = "admitted"
	InputManifestExcluded        InputManifestAdmission = "excluded"
	InputManifestUnknownLanguage InputManifestAdmission = "unknown_language"
	InputManifestOversized       InputManifestAdmission = "oversized"
	InputManifestUnreadable      InputManifestAdmission = "unreadable"
	InputManifestNotApplicable   InputManifestAdmission = "not_applicable"
)

var inputManifestAdmissions = []InputManifestAdmission{
	InputManifestAdmitted, InputManifestExcluded, InputManifestUnknownLanguage,
	InputManifestOversized, InputManifestUnreadable, InputManifestNotApplicable,
}

// InputManifestEntry is one path of a generation's admitted-input manifest.
// FilePath is git-relative and canonical (path.Clean, forward slashes), the
// same spelling as the builder's change set.
type InputManifestEntry struct {
	FilePath      string
	State         InputManifestEntryState
	Admission     InputManifestAdmission
	Mode          string
	ContentSHA256 string
}

// InputManifestMeta declares a generation's manifest complete. EntryCount is
// the number of entry rows the manifest carries; WriteInputManifest stamps it
// from the entries it writes and refuses a caller value that disagrees.
type InputManifestMeta struct {
	ManifestVersion int
	IsFull          bool
	EntryCount      int
	PolicyDigest    string
}

// Manifest errors, distinguishable without matching on strings.
var (
	// ErrInputManifestAtBaseGeneration means a manifest was written or read
	// through a handle pinned to the base generation, which has no sample.
	ErrInputManifestAtBaseGeneration = errors.New("store_sqlite: input manifests require a derived view generation")

	// ErrInputManifestInvalid means a manifest write carried a value outside
	// its vocabulary, a non-canonical or duplicate path, or inconsistent meta.
	ErrInputManifestInvalid = errors.New("store_sqlite: invalid input manifest")

	// ErrInputManifestIncomplete means a stored manifest's entry rows disagree
	// with its meta row, so it must not be used as a planning input.
	ErrInputManifestIncomplete = errors.New("store_sqlite: input manifest entries disagree with its meta")
)

// inputManifestChunk bounds rows per multi-row INSERT: the entry row binds 6
// host parameters, so 150 rows = 900 params, under SQLite's conservative 999.
const inputManifestChunk = 150

func (s *Store) requireManifestGeneration() error {
	if s.viewGen == baseViewGeneration {
		return fmt.Errorf("%w: handle is pinned to generation %d", ErrInputManifestAtBaseGeneration, s.viewGen)
	}
	return nil
}

// canonicalManifestPath reports whether p is a git-relative, already-cleaned
// path: not empty, not absolute, not escaping the root, no backslashes, and
// equal to its own path.Clean.
func canonicalManifestPath(p string) bool {
	if p == "" || p == "." || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return false
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	return path.Clean(p) == p
}

func validateInputManifest(meta InputManifestMeta, entries []InputManifestEntry) error {
	if meta.ManifestVersion != InputManifestVersion {
		return fmt.Errorf("%w: manifest_version %d, this build writes %d",
			ErrInputManifestInvalid, meta.ManifestVersion, InputManifestVersion)
	}
	if meta.PolicyDigest == "" {
		return fmt.Errorf("%w: policy_digest must not be empty", ErrInputManifestInvalid)
	}
	if meta.EntryCount != 0 && meta.EntryCount != len(entries) {
		return fmt.Errorf("%w: entry_count %d but %d entries", ErrInputManifestInvalid, meta.EntryCount, len(entries))
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if !canonicalManifestPath(entry.FilePath) {
			return fmt.Errorf("%w: file_path %q is not a canonical git-relative path", ErrInputManifestInvalid, entry.FilePath)
		}
		if _, dup := seen[entry.FilePath]; dup {
			return fmt.Errorf("%w: duplicate file_path %q", ErrInputManifestInvalid, entry.FilePath)
		}
		seen[entry.FilePath] = struct{}{}
		if !validCatalogValue(entry.State, inputManifestEntryStates) {
			return fmt.Errorf("%w: entry_state %q", ErrInputManifestInvalid, string(entry.State))
		}
		if !validCatalogValue(entry.Admission, inputManifestAdmissions) {
			return fmt.Errorf("%w: admission %q", ErrInputManifestInvalid, string(entry.Admission))
		}
	}
	return nil
}

// WriteInputManifest records this generation's admitted-input manifest: every
// entry row plus the one meta row, in one write transaction through the payload
// write gate. A second call while the generation is still building replaces
// the whole manifest (entries and meta), so a retried build step cannot leave a
// mixture. It is refused on the base handle (ErrInputManifestAtBaseGeneration)
// and once the generation is published or retiring (ErrPayloadGenerationSealed).
// An empty entry set is a valid manifest (a clean sample) and still writes the
// meta row. A zero ManifestVersion means InputManifestVersion; EntryCount may
// be zero (stamped from len(entries)) or must equal len(entries).
func (s *Store) WriteInputManifest(ctx context.Context, meta InputManifestMeta, entries []InputManifestEntry) error {
	if err := s.requireManifestGeneration(); err != nil {
		return err
	}
	if meta.ManifestVersion == 0 {
		meta.ManifestVersion = InputManifestVersion
	}
	if err := validateInputManifest(meta, entries); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	if _, err := tx.ExecContext(ctx, `DELETE FROM generation_input_manifest WHERE view_gen = ?`, s.viewGen); err != nil {
		return fmt.Errorf("store_sqlite: clear input manifest at generation %d: %w", s.viewGen, err)
	}
	const insert = `INSERT INTO generation_input_manifest
  (view_gen, file_path, entry_state, admission, mode, content_sha256) VALUES `
	const placeholders = "(?, ?, ?, ?, ?, ?)"
	for start := 0; start < len(entries); start += inputManifestChunk {
		end := min(start+inputManifestChunk, len(entries))
		var stmt strings.Builder
		stmt.WriteString(insert)
		args := make([]any, 0, (end-start)*6)
		for i := start; i < end; i++ {
			if i > start {
				stmt.WriteByte(',')
			}
			stmt.WriteString(placeholders)
			entry := entries[i]
			args = append(args, s.viewGen, entry.FilePath, string(entry.State), string(entry.Admission),
				entry.Mode, entry.ContentSHA256)
		}
		if _, err := tx.ExecContext(ctx, stmt.String(), args...); err != nil {
			return fmt.Errorf("store_sqlite: write input manifest at generation %d: %w", s.viewGen, err)
		}
	}
	isFull := 0
	if meta.IsFull {
		isFull = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO generation_input_manifest_meta
  (view_gen, manifest_version, is_full, entry_count, policy_digest) VALUES (?, ?, ?, ?, ?)`,
		s.viewGen, meta.ManifestVersion, isFull, len(entries), meta.PolicyDigest); err != nil {
		return fmt.Errorf("store_sqlite: write input manifest meta at generation %d: %w", s.viewGen, err)
	}
	return tx.Commit()
}

// InputManifest reads this generation's manifest in one read transaction. The
// bool is false when the generation has no meta row (a generation published
// before manifests existed, or one whose manifest was never written): its
// entries, if any, are not a manifest. A meta row whose entry count disagrees
// with the stored rows is reported as ErrInputManifestIncomplete. Entries are
// ordered by file_path. Refused on the base handle.
func (s *Store) InputManifest(ctx context.Context) (InputManifestMeta, []InputManifestEntry, bool, error) {
	if err := s.requireManifestGeneration(); err != nil {
		return InputManifestMeta{}, nil, false, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return InputManifestMeta{}, nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only transaction

	var meta InputManifestMeta
	var isFull int
	err = tx.QueryRowContext(ctx, `
SELECT manifest_version, is_full, entry_count, policy_digest
  FROM generation_input_manifest_meta WHERE view_gen = ?`, s.viewGen).
		Scan(&meta.ManifestVersion, &isFull, &meta.EntryCount, &meta.PolicyDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return InputManifestMeta{}, nil, false, nil
	}
	if err != nil {
		return InputManifestMeta{}, nil, false, err
	}
	meta.IsFull = isFull != 0

	rows, err := tx.QueryContext(ctx, `
SELECT file_path, entry_state, admission, mode, content_sha256
  FROM generation_input_manifest WHERE view_gen = ?
 ORDER BY file_path`, s.viewGen)
	if err != nil {
		return InputManifestMeta{}, nil, false, err
	}
	defer rows.Close()
	entries := make([]InputManifestEntry, 0, meta.EntryCount)
	for rows.Next() {
		var entry InputManifestEntry
		var state, admission string
		if err := rows.Scan(&entry.FilePath, &state, &admission, &entry.Mode, &entry.ContentSHA256); err != nil {
			return InputManifestMeta{}, nil, false, err
		}
		entry.State = InputManifestEntryState(state)
		entry.Admission = InputManifestAdmission(admission)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return InputManifestMeta{}, nil, false, err
	}
	if len(entries) != meta.EntryCount {
		return InputManifestMeta{}, nil, false, fmt.Errorf("%w: generation %d meta names %d entries, %d stored",
			ErrInputManifestIncomplete, s.viewGen, meta.EntryCount, len(entries))
	}
	return meta, entries, true, nil
}
