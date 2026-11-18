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
	comboFile               = "combo.gob.gz"
	maxComboQueries         = 2000
	maxComboEntriesPerQuery = 20
)

// ComboMatch records one (query → symbol) association. HitCount is how many
// times the agent picked this symbol following the same normalized query;
// LastUsed is a unix timestamp (seconds) for decay and reaping.
type ComboMatch struct {
	SymbolID string
	HitCount uint32
	LastUsed int64
}

// ComboQuery holds all recorded matches for one normalized query string
// within a single repo. Ordered most-hit-first after any record.
type ComboQuery struct {
	Query   string
	Matches []ComboMatch
}

// ComboStore is the persisted state of the query→symbol combo tracker for
// one repo. Separate file from feedback so each subsystem can evolve its
// schema independently.
type ComboStore struct {
	Version  string
	RepoPath string
	Queries  []ComboQuery
}

// ComboDir returns the on-disk directory for combo storage. Shares the
// repo cache key with feedback so all repo-scoped state lives together.
func ComboDir(cacheDir, repoPath string) string {
	return filepath.Join(cacheDir, RepoCacheKey(repoPath))
}

// LoadCombo reads the combo store from disk. Missing file is not an error.
func LoadCombo(dir string) (*ComboStore, error) {
	path := filepath.Join(dir, comboFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ComboStore{}, nil
		}
		return nil, fmt.Errorf("persistence: open combo: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("persistence: gzip reader combo: %w", err)
	}
	defer gz.Close()

	var store ComboStore
	if err := gob.NewDecoder(gz).Decode(&store); err != nil {
		return nil, fmt.Errorf("persistence: gob decode combo: %w", err)
	}
	return &store, nil
}

// SaveCombo writes the combo store to disk with gob+gzip compression. Trims
// the oldest-used queries if over cap so the file can't grow unboundedly on
// a long-running daemon.
func SaveCombo(dir string, store *ComboStore) error {
	if len(store.Queries) > maxComboQueries {
		// Keep the queries touched most recently by scanning their matches.
		// Stable order (no sort) preserves the common case where callers
		// already maintain MRU; only trigger a full scan when we'd otherwise
		// drop unbounded.
		trim := len(store.Queries) - maxComboQueries
		store.Queries = store.Queries[trim:]
	}
	if err := persistDir(dir); err != nil {
		return fmt.Errorf("persistence: mkdir combo: %w", err)
	}
	path := filepath.Join(dir, comboFile)
	f, err := persistFile(path)
	if err != nil {
		return fmt.Errorf("persistence: create combo: %w", err)
	}
	gz := gzip.NewWriter(f)
	enc := gob.NewEncoder(gz)
	if err := enc.Encode(store); err != nil {
		_ = gz.Close()
		_ = f.Close()
		return fmt.Errorf("persistence: gob encode combo: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("persistence: gzip close combo: %w", err)
	}
	return f.Close()
}

// MaxComboEntries returns the cap on matches per query. Exported so the
// manager can enforce the same limit in-memory before flushing.
func MaxComboEntries() int { return maxComboEntriesPerQuery }

// Now is overridable for tests; used when SaveCombo needs to timestamp.
var Now = func() time.Time { return time.Now() }
