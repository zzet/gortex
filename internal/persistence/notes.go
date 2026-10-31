package persistence

import (
	"compress/gzip"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	notesFile   = "notes.gob.gz"
	maxNotesCap = 5000
)

// NoteEntry is a single session-memory note. Notes are not graph
// nodes; they live alongside the graph as a separate, persistent
// side-store. A note is created via the `save_note` tool, surfaced
// via `query_notes`, and folded into the per-session digest emitted
// by `distill_session`.
//
// Auto-links capture the symbol IDs the agent (or the auto-linker)
// determined to be referenced by the note body. Because they are
// stored explicitly, queries by symbol stay O(notes) without
// re-tokenising every body on every call.
type NoteEntry struct {
	ID          string
	Timestamp   time.Time
	UpdatedAt   time.Time
	SessionID   string   // MCP session that created the note ("" for shared/embedded session)
	ClientName  string   // MCP clientInfo.name at create time (claude-code / cursor / ...)
	Body        string   // free-form text the agent wrote
	SymbolID    string   // primary attached symbol (optional)
	FilePath    string   // primary attached file (optional)
	RepoPrefix  string   // repo prefix derived from session scope or attached symbol/file
	WorkspaceID string   // workspace boundary; queries scope by this
	ProjectID   string   // project sub-boundary
	Tags        []string // free-form labels — "decision", "bug", "todo", ...
	AutoLinks   []string // symbol IDs referenced by the body (auto-detected + explicit links)
	Pinned      bool     // pinned notes are never evicted by the cap
}

// NoteStore is the persisted shape: a versioned, repo-scoped list of
// entries. Same persistence shape as FeedbackStore so the cache
// directory layout stays consistent.
type NoteStore struct {
	Version  string
	RepoPath string
	Entries  []NoteEntry
}

// NotesDir resolves the cache directory holding the notes file for
// the given repo. Mirrors FeedbackDir so the two side-stores share
// a per-repo cache subdirectory.
func NotesDir(cacheDir, repoPath string) string {
	return filepath.Join(cacheDir, RepoCacheKey(repoPath))
}

// LoadNotes reads a note store from disk. Returns an empty store
// when the file does not exist (cold start is normal).
func LoadNotes(dir string) (*NoteStore, error) {
	path := filepath.Join(dir, notesFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &NoteStore{}, nil
		}
		return nil, fmt.Errorf("persistence: open notes: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("persistence: gzip reader notes: %w", err)
	}
	defer gz.Close()

	var store NoteStore
	if err := gob.NewDecoder(gz).Decode(&store); err != nil {
		return nil, fmt.Errorf("persistence: gob decode notes: %w", err)
	}
	return &store, nil
}

// SaveNotes writes the store to disk with gob+gzip. Trimming
// honours pinned notes: when the entry count exceeds maxNotesCap,
// the oldest non-pinned entries are dropped first.
func SaveNotes(dir string, store *NoteStore) error {
	if len(store.Entries) > maxNotesCap {
		store.Entries = trimNotes(store.Entries, maxNotesCap)
	}

	if err := persistDir(dir); err != nil {
		return fmt.Errorf("persistence: mkdir notes: %w", err)
	}

	path := filepath.Join(dir, notesFile)
	f, err := persistFile(path)
	if err != nil {
		return fmt.Errorf("persistence: create notes: %w", err)
	}

	gz := gzip.NewWriter(f)
	enc := gob.NewEncoder(gz)

	if err := enc.Encode(store); err != nil {
		_ = gz.Close()
		_ = f.Close()
		return fmt.Errorf("persistence: gob encode notes: %w", err)
	}

	if err := gz.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("persistence: gzip close notes: %w", err)
	}

	return f.Close()
}

// trimNotes drops the oldest non-pinned entries until len(out) <= cap.
// Order is preserved so callers iterate chronologically. If pinned
// entries alone exceed cap, every pinned entry is still retained
// — the cap is a soft ceiling for the unpinned tail.
func trimNotes(in []NoteEntry, cap int) []NoteEntry {
	if len(in) <= cap {
		return in
	}
	excess := len(in) - cap

	// Walk forward dropping non-pinned entries until we have shed
	// `excess` of them, then return the tail.
	out := make([]NoteEntry, 0, cap)
	dropped := 0
	for _, e := range in {
		if dropped < excess && !e.Pinned {
			dropped++
			continue
		}
		out = append(out, e)
	}
	return out
}
