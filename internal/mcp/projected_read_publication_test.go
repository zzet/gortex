package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type heldReadProjectionExtractor struct {
	base    parser.Extractor
	hold    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (e *heldReadProjectionExtractor) Language() string     { return "go" }
func (e *heldReadProjectionExtractor) Extensions() []string { return []string{".go"} }
func (e *heldReadProjectionExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	if e.hold.CompareAndSwap(true, false) {
		close(e.entered)
		<-e.release
	}
	return e.base.Extract(path, raw)
}

func TestProjectedReadCannotPublishOverNewerReadVersion(t *testing.T) {
	for _, mode := range []string{"changed bytes", "same bytes", "newer version already retired"} {
		t.Run(mode, func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			extractor := &heldReadProjectionExtractor{base: languages.NewGoExtractor(), entered: make(chan struct{}), release: make(chan struct{})}
			registry := parser.NewRegistry()
			registry.Register(extractor)
			s, root := setupTestServerWithRegistry(t, registry)
			path := filepath.Join(root, "main.go")
			first := []byte("package main\nfunc AlphaOne() {}\n")
			second := []byte("package main\nfunc AlphaTwo() {}\n")
			if mode == "same bytes" {
				second = first
			}
			require.NoError(t, os.WriteFile(path, first, 0o644))
			stamp, err := os.Lstat(path)
			require.NoError(t, err)
			extractor.hold.Store(true)
			var release sync.Once
			unblock := func() { release.Do(func() { close(extractor.release) }) }
			t.Cleanup(unblock)
			type result struct {
				entry *projectionEntry
				err   error
			}
			finished := make(chan result, 1)
			go func() {
				entry, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
				finished <- result{entry, err}
			}()
			joined := false
			defer func() {
				unblock()
				if !joined {
					select {
					case <-finished:
					case <-time.After(time.Second):
						t.Error("owned read did not join during cleanup")
					}
				}
			}()
			select {
			case <-extractor.entered:
			case <-time.After(time.Second):
				t.Fatal("first owned extraction never entered")
			}
			// In-place equal-length replacement retains inode and restores the
			// stamp, exposing a stat-only completion check and nil-pointer ABA.
			require.NoError(t, os.WriteFile(path, second, 0o644))
			require.NoError(t, os.Chtimes(path, stamp.ModTime(), stamp.ModTime()))
			newer, err := s.projectCurrentTrackedFile(t.Context(), context.Background(), path, "main.go", "main.go")
			require.NoError(t, err)
			require.NotNil(t, newer)
			if mode == "newer version already retired" {
				pendingFileProjections.drop(newer)
			}
			unblock()
			var older result
			select {
			case older = <-finished:
				joined = true
			case <-time.After(time.Second):
				t.Fatal("owned extraction did not finish after release")
			}
			if mode == "newer version already retired" {
				require.Nil(t, liveEntry(pendingFileProjections, path), "retirement must not let old bytes resurrect through a nil-pointer ABA")
			} else {
				require.Same(t, newer, liveEntry(pendingFileProjections, path), "a completed old read cannot replace the newer immutable snapshot")
			}
			if mode != "same bytes" {
				require.Error(t, older.err, "a parsed source whose bytes moved must refuse even when inode, size and mtime match")
				require.Nil(t, older.entry)
			}
		})
	}
}

func TestProjectedReadKeepsSuspectInvalidationAfterBoundedReuse(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	raw := []byte("package main\nfunc Changed() {}\n")
	require.NoError(t, os.WriteFile(path, raw, 0o644))
	entry, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.NotNil(t, entry)
	pendingFileProjections.markSuspect(path)
	current, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.Same(t, entry, current)
	require.True(t, entry.suspect.Load(), "a checked request cannot consume a watcher's invalidation for other readers/ACKs")
}

func TestProjectedReadReplacementDoesNotBorrowPhysicalSequence(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	first := []byte("package main\nfunc AlphaOne() {}\n")
	second := []byte("package main\nfunc AlphaTwo() {}\n")
	require.NoError(t, os.WriteFile(path, first, 0o644))
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	projection, err := s.indexer.ProjectSource(t.Context(), path, first)
	require.NoError(t, err)
	// Model an already admitted physical sequence using actual checked bytes.
	// The read-through replacement must not mint or borrow that execution ID.
	physical := newProjectionEntry(path, s.indexer.RepoPrefix(), projection, first, 7, stamp)
	require.NoError(t, pendingFileProjections.publish(physical))
	require.NoError(t, os.WriteFile(path, second, 0o644))
	require.NoError(t, os.Chtimes(path, stamp.info.ModTime(), stamp.info.ModTime()))
	current, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.NotNil(t, current)
	require.NotSame(t, physical, current)
	require.Zero(t, current.commitSeq)
	require.EqualValues(t, 7, physical.commitSeq)
	require.True(t, physical.suspect.Load())
	require.NotNil(t, current.layer.NodeByID("main.go::AlphaTwo"))
	require.Nil(t, current.layer.NodeByID("main.go::AlphaOne"))
}
