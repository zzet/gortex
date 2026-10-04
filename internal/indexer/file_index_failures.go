package indexer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"go.uber.org/zap"
)

// FileIndexFailure records an unsuccessful indexing attempt separately from
// the last successful file snapshot.
type FileIndexFailure = graph.FileIndexFailure

// LoadFileIndexFailures reads failures from the selected graph view. An absent
// capability has no failure ledger; it must never fall back to another view.
func LoadFileIndexFailures(g graph.Reader, repoPrefix string) []FileIndexFailure {
	rows, _ := LoadFileIndexFailuresWithError(g, repoPrefix)
	return rows
}

// LoadFileIndexFailuresWithError lets health consumers distinguish unavailable
// failure state from a healthy, empty ledger.
func LoadFileIndexFailuresWithError(g graph.Reader, repoPrefix string) ([]FileIndexFailure, error) {
	reader, ok := g.(graph.FileIndexFailureReader)
	if !ok {
		return nil, nil
	}
	return reader.FileIndexFailuresForRepo(repoPrefix)
}

type fileIndexFailureState struct {
	mu               sync.Mutex
	loaded           bool
	loadErr          error
	dirty            bool
	permissionWarned bool
	rows             map[string]FileIndexFailure
	errors           map[string]error
	cleared          map[string]struct{}
}

// clearRecoveredParseErrors runs once after a completed incremental mutation,
// not once per file. The final failed set wins over preliminary read or walk
// successes, and only explicitly recovered/deleted paths lose old diagnostics.
func (idx *Indexer) clearRecoveredParseErrors(successful, failed, deleted []string) {
	if len(successful)+len(deleted) == 0 {
		return
	}
	key := func(path string) string {
		if !filepath.IsAbs(path) {
			path = filepath.Join(idx.rootPath, filepath.FromSlash(path))
		}
		return idx.relKey(path)
	}
	recovered := make(map[string]struct{}, len(successful)+len(deleted))
	for _, path := range successful {
		recovered[key(path)] = struct{}{}
	}
	for _, path := range deleted {
		recovered[key(path)] = struct{}{}
	}
	for _, path := range failed {
		delete(recovered, key(path))
	}
	if len(recovered) == 0 {
		return
	}
	idx.parseErrorsMu.Lock()
	defer idx.parseErrorsMu.Unlock()
	retained := idx.parseErrors[:0]
	for _, failure := range idx.parseErrors {
		if _, ok := recovered[key(failure.FilePath)]; !ok {
			retained = append(retained, failure)
		}
	}
	clear(idx.parseErrors[len(retained):])
	idx.parseErrors = retained
}

// The repository mutation lane serializes passes. The state mutex also covers
// full-index parse workers; persistence happens once after those workers join.
func (idx *Indexer) loadFileIndexFailuresLocked() {
	state := &idx.fileIndexFailures
	if state.loaded {
		return
	}
	state.rows = make(map[string]FileIndexFailure)
	state.errors = make(map[string]error)
	state.cleared = make(map[string]struct{})
	state.loaded = true
	if reader, ok := idx.graph.(graph.FileIndexFailureReader); ok {
		rows, err := reader.FileIndexFailuresForRepo(idx.repoPrefix)
		if err != nil {
			state.loadErr = err
			idx.logger.Warn("indexer: loading file failures failed", zap.String("repo", idx.repoPrefix), zap.Error(err))
			return
		}
		for _, row := range rows {
			state.rows[row.Path] = row
		}
	}
}

func (idx *Indexer) loadFileIndexFailures() {
	idx.fileIndexFailures.mu.Lock()
	defer idx.fileIndexFailures.mu.Unlock()
	state := &idx.fileIndexFailures
	if state.loaded && state.loadErr != nil {
		// Retry only at a pass boundary. A failed initial load must not turn
		// every file into a store read or discard newly observed failures.
		if reader, ok := idx.graph.(graph.FileIndexFailureReader); ok {
			rows, err := reader.FileIndexFailuresForRepo(idx.repoPrefix)
			if err == nil {
				for _, row := range rows {
					_, cleared := state.cleared[row.Path]
					if _, observed := state.rows[row.Path]; !observed && !cleared {
						state.rows[row.Path] = row
					}
				}
				state.loadErr = nil
				clear(state.cleared)
			}
		}
	}
	idx.loadFileIndexFailuresLocked()
}

func (idx *Indexer) noteFileIndexFailure(path string, err error) {
	graphPath := idx.prefixPath(idx.relKey(path))
	state := &idx.fileIndexFailures
	state.mu.Lock()
	defer state.mu.Unlock()
	idx.loadFileIndexFailuresLocked()
	if err == nil {
		if state.loadErr != nil {
			state.cleared[graphPath] = struct{}{}
			state.dirty = true
		}
		if _, exists := state.rows[graphPath]; exists {
			delete(state.rows, graphPath)
			delete(state.errors, graphPath)
			state.dirty = true
		}
		return
	}
	delete(state.cleared, graphPath)
	row := FileIndexFailure{
		Path: graphPath, Error: err.Error(), PermissionDenied: errors.Is(err, os.ErrPermission),
		RepoPrefix: idx.repoPrefix, WorkspaceID: idx.workspaceID, ProjectID: idx.projectID,
	}
	if old, exists := state.rows[graphPath]; !exists || old != row {
		state.rows[graphPath] = row
		state.dirty = true
	}
	state.errors[graphPath] = err
}

func (idx *Indexer) fileIndexFailureError(path string) error {
	graphPath := idx.prefixPath(idx.relKey(path))
	state := &idx.fileIndexFailures
	state.mu.Lock()
	defer state.mu.Unlock()
	idx.loadFileIndexFailuresLocked()
	if err := state.errors[graphPath]; err != nil {
		return err
	}
	if row, exists := state.rows[graphPath]; exists {
		if row.PermissionDenied {
			return fmt.Errorf("%s: %w", row.Error, os.ErrPermission)
		}
		return errors.New(row.Error)
	}
	return errFileVersionChanged
}

func (idx *Indexer) fileIndexFailurePaths() []string {
	state := &idx.fileIndexFailures
	state.mu.Lock()
	defer state.mu.Unlock()
	idx.loadFileIndexFailuresLocked()
	paths := make([]string, 0, len(state.rows))
	for path := range state.rows {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func (idx *Indexer) pruneMissingFileIndexFailures() {
	for _, graphPath := range idx.fileIndexFailurePaths() {
		relPath, ok := idx.graphPathRelKey(graphPath)
		if !ok {
			continue
		}
		path := filepath.Join(idx.rootPath, filepath.FromSlash(relPath))
		if src := idx.contentSource(); src != nil {
			if _, err := src.Stat(relPath); errors.Is(err, source.ErrNotInSource) {
				idx.noteFileIndexFailure(path, nil)
			}
			continue
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			idx.noteFileIndexFailure(path, nil)
		}
	}
}

func (idx *Indexer) flushFileIndexFailures() {
	// Drain the silent-zero pending set first, inside the same lock the
	// persistence below reads: the notes must be in state.rows before the
	// rows slice is built, and this runs after the pass's version
	// receipts, so nothing clears them again this pass.
	idx.flushSilentZeroNotes()
	state := &idx.fileIndexFailures
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.loaded {
		return
	}
	rows := make([]FileIndexFailure, 0, len(state.rows))
	permissionCount := 0
	var example FileIndexFailure
	for _, row := range state.rows {
		rows = append(rows, row)
		if row.PermissionDenied {
			permissionCount++
			if example.Path == "" || row.Path < example.Path {
				example = row
			}
		}
	}
	if state.dirty && state.loadErr != nil {
		idx.logger.Warn("indexer: file failure state unavailable; retaining pending updates",
			zap.String("repo", idx.repoPrefix), zap.Error(state.loadErr))
	} else if state.dirty {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
		if writer, ok := idx.graph.(graph.FileIndexFailureWriter); ok {
			if err := writer.ReplaceFileIndexFailures(idx.repoPrefix, rows); err != nil {
				idx.logger.Warn("indexer: persisting file failures failed", zap.String("repo", idx.repoPrefix), zap.Error(err))
				return
			}
			state.dirty = false
		}
	}
	if permissionCount == 0 {
		state.permissionWarned = false
	} else if !state.permissionWarned {
		hint := "Check file and directory permissions for the account running Gortex, then reindex."
		if runtime.GOOS == "darwin" {
			hint = "Check System Settings > Privacy & Security > Full Disk Access for the app or service running Gortex, then restart it and reindex."
		}
		idx.logger.Warn("indexer: repository indexing incomplete due to filesystem permissions",
			zap.String("repo", idx.repoPrefix), zap.Int("unreadable_files", permissionCount),
			zap.String("file", example.Path), zap.String("error", example.Error), zap.String("hint", hint))
		state.permissionWarned = true
	}
}
