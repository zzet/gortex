package persistence

import (
	"compress/gzip"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
)

const (
	keywordFile             = "keyword.gob.gz"
	maxKeywords             = 4000
	maxKeywordEntriesPerKey = 10
)

// KeywordMatch records one (keyword -> symbol) association. HitCount
// is how many times the agent picked this symbol following a query
// that contained the keyword; LastUsed is a unix timestamp (seconds)
// for decay and reaping. Structurally identical to ComboMatch but
// kept as a distinct type so the two stores' schemas can diverge.
type KeywordMatch struct {
	SymbolID string
	HitCount uint32
	LastUsed int64
	// MissCount is the implicit-negative tally: how many times the
	// agent was shown this symbol for a query carrying the keyword but
	// skipped over it to pick a lower-ranked result. The per-keyword
	// boost nets HitCount-MissCount, so a symbol the agent keeps passing
	// over loses its learned boost. A zero value (legacy data) is the
	// pre-negative-signal behaviour. Decays on the same clock as hits.
	MissCount uint32
}

// KeywordAssoc holds all recorded matches for one query keyword
// within a single repo. Ordered most-hit-first after any record.
type KeywordAssoc struct {
	Keyword string
	Matches []KeywordMatch
}

// KeywordStore is the persisted per-keyword association index for one
// repo. Where ComboStore keys on the whole normalized query,
// KeywordStore keys on each surviving query token -- so a new task
// with overlapping keywords but different phrasing still inherits the
// associations its keywords earned. Separate file from combo and
// feedback so each subsystem's schema evolves independently.
type KeywordStore struct {
	Version  string
	RepoPath string
	Keywords []KeywordAssoc
}

// KeywordDir returns the on-disk directory for keyword storage.
// Shares the repo cache key with combo / feedback so all repo-scoped
// state lives together.
func KeywordDir(cacheDir, repoPath string) string {
	return filepath.Join(cacheDir, RepoCacheKey(repoPath))
}

// LoadKeyword reads the keyword store from disk. A missing file is
// not an error -- it yields an empty store.
func LoadKeyword(dir string) (*KeywordStore, error) {
	path := filepath.Join(dir, keywordFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &KeywordStore{}, nil
		}
		return nil, fmt.Errorf("persistence: open keyword: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("persistence: gzip reader keyword: %w", err)
	}
	defer gz.Close()

	var store KeywordStore
	if err := gob.NewDecoder(gz).Decode(&store); err != nil {
		return nil, fmt.Errorf("persistence: gob decode keyword: %w", err)
	}
	return &store, nil
}

// SaveKeyword writes the keyword store with gob+gzip compression.
// Trims the oldest keywords if over cap so the file cannot grow
// unboundedly on a long-running daemon.
func SaveKeyword(dir string, store *KeywordStore) error {
	if len(store.Keywords) > maxKeywords {
		trim := len(store.Keywords) - maxKeywords
		store.Keywords = store.Keywords[trim:]
	}
	if err := persistDir(dir); err != nil {
		return fmt.Errorf("persistence: mkdir keyword: %w", err)
	}
	path := filepath.Join(dir, keywordFile)
	f, err := persistFile(path)
	if err != nil {
		return fmt.Errorf("persistence: create keyword: %w", err)
	}
	gz := gzip.NewWriter(f)
	enc := gob.NewEncoder(gz)
	if err := enc.Encode(store); err != nil {
		_ = gz.Close()
		_ = f.Close()
		return fmt.Errorf("persistence: gob encode keyword: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("persistence: gzip close keyword: %w", err)
	}
	return f.Close()
}

// MaxKeywordEntries returns the cap on matches per keyword. Tighter
// than MaxComboEntries -- a single keyword is a coarser key than a
// whole query, so its match list is held shorter to stay precise.
func MaxKeywordEntries() int { return maxKeywordEntriesPerKey }
