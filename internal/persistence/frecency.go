package persistence

import (
	"compress/gzip"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
)

const (
	frecencyFile        = "frecency.gob.gz"
	maxFrecencyAccesses = 16 // entries per symbol; matches FFF's bounded deque
	maxFrecencySymbols  = 10000
)

// FrecencyAccesses is the bounded, ordered (oldest→newest) list of unix
// timestamps (seconds) at which one symbol was consumed. Bounded because
// the decay formula already weights recent accesses far more heavily than
// old ones — beyond ~16 entries, additional history contributes noise.
type FrecencyAccesses struct {
	SymbolID string
	Times    []int64
}

// FrecencyStore holds all per-symbol access histories for a single repo.
type FrecencyStore struct {
	Version  string
	RepoPath string
	Symbols  []FrecencyAccesses
}

// MaxFrecencyAccesses returns the per-symbol access-history cap.
func MaxFrecencyAccesses() int { return maxFrecencyAccesses }

// FrecencyDir returns the on-disk directory for frecency storage.
func FrecencyDir(cacheDir, repoPath string) string {
	return filepath.Join(cacheDir, RepoCacheKey(repoPath))
}

// LoadFrecency reads the frecency store from disk.
func LoadFrecency(dir string) (*FrecencyStore, error) {
	path := filepath.Join(dir, frecencyFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &FrecencyStore{}, nil
		}
		return nil, fmt.Errorf("persistence: open frecency: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("persistence: gzip reader frecency: %w", err)
	}
	defer gz.Close()

	var store FrecencyStore
	if err := gob.NewDecoder(gz).Decode(&store); err != nil {
		return nil, fmt.Errorf("persistence: gob decode frecency: %w", err)
	}
	return &store, nil
}

// SaveFrecency writes the frecency store to disk. Trims the oldest-touched
// symbols if over cap so the file can't grow unboundedly.
func SaveFrecency(dir string, store *FrecencyStore) error {
	if len(store.Symbols) > maxFrecencySymbols {
		trim := len(store.Symbols) - maxFrecencySymbols
		store.Symbols = store.Symbols[trim:]
	}
	if err := persistDir(dir); err != nil {
		return fmt.Errorf("persistence: mkdir frecency: %w", err)
	}
	path := filepath.Join(dir, frecencyFile)
	f, err := persistFile(path)
	if err != nil {
		return fmt.Errorf("persistence: create frecency: %w", err)
	}
	gz := gzip.NewWriter(f)
	enc := gob.NewEncoder(gz)
	if err := enc.Encode(store); err != nil {
		_ = gz.Close()
		_ = f.Close()
		return fmt.Errorf("persistence: gob encode frecency: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("persistence: gzip close frecency: %w", err)
	}
	return f.Close()
}
