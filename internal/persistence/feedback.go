package persistence

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/pathkey"
)

const (
	feedbackFile   = "feedback.gob.gz"
	maxFeedbackCap = 500
)

// FeedbackEntry records one agent feedback event about a context call.
type FeedbackEntry struct {
	Timestamp time.Time
	Task      string   // task description from the context call
	Useful    []string // symbol IDs the agent found useful
	NotNeeded []string // symbol IDs returned but not needed
	Missing   []string // symbol IDs that should have been included
	Source    string   // "smart_context" or "prefetch_context"
	// Keywords is the task's keyword cluster (derived from Task at
	// record time). Feedback is scored only against entries whose
	// keyword cluster overlaps the querying task's, so a symbol marked
	// useful for one task does not contaminate an unrelated one. A nil
	// Keywords (legacy entry, or a keyword-less task) is treated as
	// matching any query for backward compatibility.
	Keywords []string
}

// FeedbackStore holds all feedback entries for a single repo.
// Persisted separately from the graph snapshot (repo-scoped, not commit-scoped).
type FeedbackStore struct {
	Version  string
	RepoPath string
	Entries  []FeedbackEntry
}

// RepoCacheKey produces a filesystem-safe directory name from repo path alone
// (no commit hash). Used for data that persists across commits, like feedback.
//
// The path is folded to Unicode NFC before hashing so a repo whose path
// contains non-ASCII characters keys to the same directory whether the
// caller passes a decomposed (macOS) or precomposed (Linux / git) form.
func RepoCacheKey(repoPath string) string {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	h := sha256.Sum256([]byte(pathkey.Normalize(abs)))
	return hex.EncodeToString(h[:6]) + "_latest"
}

// FeedbackDir returns the full directory path for feedback storage.
func FeedbackDir(cacheDir, repoPath string) string {
	return filepath.Join(cacheDir, RepoCacheKey(repoPath))
}

// LoadFeedback reads a feedback store from disk. Returns an empty store if
// the file does not exist (not an error — cold start is normal).
func LoadFeedback(dir string) (*FeedbackStore, error) {
	path := filepath.Join(dir, feedbackFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &FeedbackStore{}, nil
		}
		return nil, fmt.Errorf("persistence: open feedback: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("persistence: gzip reader: %w", err)
	}
	defer gz.Close()

	var store FeedbackStore
	if err := gob.NewDecoder(gz).Decode(&store); err != nil {
		return nil, fmt.Errorf("persistence: gob decode feedback: %w", err)
	}
	return &store, nil
}

// SaveFeedback writes a feedback store to disk with gob+gzip compression.
// Entries are trimmed to maxFeedbackCap before writing (oldest removed).
func SaveFeedback(dir string, store *FeedbackStore) error {
	// Trim oldest entries if over cap.
	if len(store.Entries) > maxFeedbackCap {
		store.Entries = store.Entries[len(store.Entries)-maxFeedbackCap:]
	}

	if err := persistDir(dir); err != nil {
		return fmt.Errorf("persistence: mkdir feedback: %w", err)
	}

	path := filepath.Join(dir, feedbackFile)
	f, err := persistFile(path)
	if err != nil {
		return fmt.Errorf("persistence: create feedback: %w", err)
	}

	gz := gzip.NewWriter(f)
	enc := gob.NewEncoder(gz)

	if err := enc.Encode(store); err != nil {
		_ = gz.Close()
		_ = f.Close()
		return fmt.Errorf("persistence: gob encode feedback: %w", err)
	}

	if err := gz.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("persistence: gzip close feedback: %w", err)
	}

	return f.Close()
}
