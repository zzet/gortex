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
	memoriesFile   = "memories.gob.gz"
	maxMemoriesCap = 10000
)

// MemoryEntry is a single cross-session development memory. Unlike
// NoteEntry (per-session scratchpad), MemoryEntry has no SessionID
// — every memory is workspace-wide and durable across sessions.
//
// Memories accumulate over time and compound the longer a team uses
// Gortex: every recorded invariant, gotcha, decision, or convention
// becomes discoverable by every future agent in the same workspace.
//
// Memories are surfaced when:
//   - an anchor symbol / file enters the agent's working set
//     (via surface_memories)
//   - the agent explicitly queries by symbol / file / tag / text
//     (via query_memories)
type MemoryEntry struct {
	ID           string
	Timestamp    time.Time
	UpdatedAt    time.Time
	LastAccessed time.Time
	AccessCount  uint64
	Body         string   // free-form text
	Title        string   // short caption (one-liner)
	Kind         string   // invariant | constraint | convention | gotcha | decision | incident | reference
	Source       string   // manual | distilled | incident | review
	Confidence   float32  // 0..1 — how sure we are this still holds
	Importance   int      // 1..5 — operator-assigned weight
	AuthorAgent  string   // mcp clientInfo.name
	SymbolIDs    []string // primary symbol anchors
	FilePaths    []string // primary file anchors
	AutoLinks    []string // additional referenced symbol IDs (auto-detected)
	Tags         []string // free-form labels
	WorkspaceID  string
	ProjectID    string
	RepoPrefix   string
	Pinned       bool   // pinned memories are never evicted and float to top
	SupersededBy string // ID of newer memory that replaces this one
}

// MemoryStore is the persisted shape: a versioned, repo-scoped list
// of entries. Mirrors NoteStore so the on-disk layout stays
// consistent.
type MemoryStore struct {
	Version  string
	RepoPath string
	Entries  []MemoryEntry
}

// MemoriesDir resolves the cache directory holding the memories
// file for the given repo. Shares the per-repo cache directory
// with notes / feedback / combo / frecency.
func MemoriesDir(cacheDir, repoPath string) string {
	return filepath.Join(cacheDir, RepoCacheKey(repoPath))
}

// LoadMemories reads a memory store from disk. Returns an empty
// store when the file does not exist (cold start is normal).
func LoadMemories(dir string) (*MemoryStore, error) {
	path := filepath.Join(dir, memoriesFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &MemoryStore{}, nil
		}
		return nil, fmt.Errorf("persistence: open memories: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("persistence: gzip reader memories: %w", err)
	}
	defer gz.Close()

	var store MemoryStore
	if err := gob.NewDecoder(gz).Decode(&store); err != nil {
		return nil, fmt.Errorf("persistence: gob decode memories: %w", err)
	}
	return &store, nil
}

// SaveMemories writes the store to disk with gob+gzip. Trimming
// honours pinned memories: when the entry count exceeds
// maxMemoriesCap, the lowest-importance / non-pinned entries are
// dropped first; if more shedding is still needed, the oldest
// non-pinned entries go next.
func SaveMemories(dir string, store *MemoryStore) error {
	if len(store.Entries) > maxMemoriesCap {
		store.Entries = trimMemories(store.Entries, maxMemoriesCap)
	}

	if err := persistDir(dir); err != nil {
		return fmt.Errorf("persistence: mkdir memories: %w", err)
	}

	path := filepath.Join(dir, memoriesFile)
	f, err := persistFile(path)
	if err != nil {
		return fmt.Errorf("persistence: create memories: %w", err)
	}

	gz := gzip.NewWriter(f)
	enc := gob.NewEncoder(gz)

	if err := enc.Encode(store); err != nil {
		_ = gz.Close()
		_ = f.Close()
		return fmt.Errorf("persistence: gob encode memories: %w", err)
	}

	if err := gz.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("persistence: gzip close memories: %w", err)
	}

	return f.Close()
}

// trimMemories drops entries until len(out) <= cap. Two passes:
//   - first, shed non-pinned entries with importance <= 2
//   - then, if still over cap, shed oldest non-pinned regardless
//
// Pinned entries are always retained, even if the resulting slice
// exceeds cap (the cap is a soft ceiling for the prunable tail).
func trimMemories(in []MemoryEntry, cap int) []MemoryEntry {
	if len(in) <= cap {
		return in
	}
	excess := len(in) - cap

	out := make([]MemoryEntry, 0, len(in))
	dropped := 0
	for _, e := range in {
		if dropped < excess && !e.Pinned && e.Importance <= 2 {
			dropped++
			continue
		}
		out = append(out, e)
	}

	if len(out) > cap {
		excess = len(out) - cap
		next := make([]MemoryEntry, 0, len(out))
		dropped = 0
		for _, e := range out {
			if dropped < excess && !e.Pinned {
				dropped++
				continue
			}
			next = append(next, e)
		}
		out = next
	}
	return out
}
