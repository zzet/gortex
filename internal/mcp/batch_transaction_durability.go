package mcp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/zzet/gortex/internal/platform"
)

// batchDurabilityOps is a narrow test seam around the filesystem operations
// that define an atomic batch transaction's crash-durability boundaries.
// Production always uses the defaults returned by batchDurability.
type batchDurabilityOps struct {
	writeFile     func(string, []byte, os.FileMode) error
	syncDirectory func(string) error
	removeFile    func(string) error
}

func (s *Server) batchDurability() batchDurabilityOps {
	ops := batchDurabilityOps{
		writeFile:     durableAtomicWriteFile,
		syncDirectory: syncBatchDirectory,
		removeFile:    platform.RemoveFile,
	}
	if s == nil || s.batchDurabilityOverride == nil {
		return ops
	}
	if override := s.batchDurabilityOverride; override.writeFile != nil {
		ops.writeFile = override.writeFile
	}
	if override := s.batchDurabilityOverride; override.syncDirectory != nil {
		ops.syncDirectory = override.syncDirectory
	}
	if override := s.batchDurabilityOverride; override.removeFile != nil {
		ops.removeFile = override.removeFile
	}
	return ops
}

// durableAtomicWriteFile makes the new inode durable before publishing it with
// rename. The containing directory is deliberately synced by the transaction
// coordinator, which deduplicates directory syncs across every file in a batch.
func durableAtomicWriteFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".gortex-batch-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()

	if n, writeErr := tmp.Write(content); writeErr != nil {
		return fmt.Errorf("write temporary file: %w", writeErr)
	} else if n != len(content) {
		return fmt.Errorf("write temporary file: %w", io.ErrShortWrite)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary file mode: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	closed = true
	if err := platform.ReplaceFile(tmpPath, path); err != nil {
		return fmt.Errorf("replace target file: %w", err)
	}
	return nil
}

// syncBatchDirectories preserves first-seen order while syncing each distinct
// directory exactly once. Callers can therefore express ordering constraints
// (for example transaction directory before its parent) without paying one
// directory fsync per edited file.
func (s *Server) syncBatchDirectories(dirs ...string) error {
	ops := s.batchDurability()
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		clean := filepath.Clean(dir)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		if err := ops.syncDirectory(clean); err != nil {
			return fmt.Errorf("sync directory %s: %w", clean, err)
		}
	}
	return nil
}

func batchFileDirectories(files []batchTransactionFile) []string {
	dirs := make([]string, 0, len(files))
	for _, file := range files {
		dirs = append(dirs, filepath.Dir(file.Path))
	}
	return dirs
}

func batchPathDirectories(paths []string) []string {
	dirs := make([]string, 0, len(paths))
	for _, path := range paths {
		dirs = append(dirs, filepath.Dir(path))
	}
	return dirs
}

// Directory handles cannot be flushed portably on Windows. The file itself is
// still flushed before rename there; Unix-family hosts additionally persist the
// directory entry. Unsupported directory fsync errors are returned rather than
// silently weakening durability.
func syncBatchDirectory(path string) error {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" || runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	return errors.Join(syncErr, closeErr)
}
