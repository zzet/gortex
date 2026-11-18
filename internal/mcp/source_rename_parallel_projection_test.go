package mcp

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type renameParallelExtractor struct {
	base        parser.Extractor
	enabled     atomic.Bool
	active, max atomic.Int32
	entered     chan struct{}
	release     chan struct{}
}

func (e *renameParallelExtractor) Language() string     { return "go" }
func (e *renameParallelExtractor) Extensions() []string { return []string{".go"} }
func (e *renameParallelExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	if e.enabled.Load() {
		active := e.active.Add(1)
		defer e.active.Add(-1)
		for prior := e.max.Load(); active > prior; prior = e.max.Load() {
			if e.max.CompareAndSwap(prior, active) {
				break
			}
		}
		e.entered <- struct{}{}
		<-e.release
	}
	return e.base.Extract(path, raw)
}

func TestRenameBuildsBoundedParallelProjectionsInsideOneAcknowledgementLap(t *testing.T) {
	t.Setenv("GORTEX_EDIT_PARSE_GATE", "1")
	extractor := &renameParallelExtractor{base: languages.NewGoExtractor(), entered: make(chan struct{}, 3), release: make(chan struct{})}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	s, root := setupTestServerWithRegistry(t, registry)
	var edits []renameEdit
	var paths []string
	for _, name := range []string{"first.go", "second.go", "third.go"} {
		path := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(path, []byte("package main\nfunc Old() {}\n"), 0o644))
		paths = append(paths, path)
		edits = append(edits, renameEdit{File: name, Line: 2, OldText: "func Old() {}", NewText: "func New() {}"})
	}
	_, err := s.indexer.IncrementalReindexPaths(root, paths)
	require.NoError(t, err)
	held, releasePaths, err := acquireMutationPaths(t.Context(), paths)
	require.NoError(t, err)
	defer releasePaths()
	ctx, ack, _ := s.withEditAcknowledgement(held)
	extractor.enabled.Store(true)
	unblock := sync.OnceFunc(func() { close(extractor.release) })
	defer unblock()
	done := make(chan struct{})
	var writes []*renameFileWrite
	var planErr error
	go func() { defer close(done); writes, planErr = s.planRenameWrites(ctx, edits, false) }()
	t.Cleanup(func() {
		unblock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("owned projection workers remain during cleanup")
		}
	})
	select {
	case <-extractor.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first real projection extractor did not enter")
	}
	parallel := false
	select {
	case <-extractor.entered:
		parallel = true
	case <-time.After(time.Second):
	}
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("owned projection workers did not join after release")
	}
	require.NoError(t, planErr)
	require.Len(t, writes, 3)
	for _, write := range writes {
		require.NotNil(t, write.projection.entry, "bounded parallel preparation must finish each checked candidate, not escape through a timed-out first parse")
	}
	assert.True(t, parallel, "a second real projection must begin before the first can finish")
	assert.EqualValues(t, 2, extractor.max.Load(), "parallelism is bounded rather than one goroutine per file")
	ack.finishActual()
	ack.finishReply(mcplib.NewToolResultText("planned without writes"), nil)
	timing := ack.snapshot().ParseGate
	assert.EqualValues(t, 1, timing.Started, "parallel preparation has one actual wall lap, never overlapping additive per-file laps")
	assert.EqualValues(t, 1, timing.Completed)
	assert.Equal(t, "measured", timing.State)
	for _, path := range paths {
		raw, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		require.Equal(t, "package main\nfunc Old() {}\n", string(raw))
	}
}
