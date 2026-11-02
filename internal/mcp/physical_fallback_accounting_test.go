package mcp

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

type physicalFallbackSource struct {
	source.ContentSource
	reads atomic.Int32
}

func (s *physicalFallbackSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	rc, meta, err := s.ContentSource.Open(path)
	if err != nil || path != "main.go" || s.reads.Add(1) != 2 {
		return rc, meta, err
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, meta, err
	}
	data = append(data, []byte("\n// refresh source moved on\n")...)
	meta.Size = int64(len(data))
	return io.NopCloser(bytes.NewReader(data)), meta, nil
}

type physicalFallbackExtractor struct {
	parser.Extractor
	calls atomic.Int32
}

func (e *physicalFallbackExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	e.calls.Add(1)
	return e.Extractor.Extract(path, raw)
}

func TestPhysicalFallbackAccountingRequiresOriginalDiskCommit(t *testing.T) {
	for _, physical := range []bool{true, false} {
		name := "read_built_without_physical_owner"
		if physical {
			name = "actual_physical_commit"
		}
		t.Run(name, func(t *testing.T) {
			extractor := &physicalFallbackExtractor{Extractor: languages.NewGoExtractor()}
			registry := parser.NewRegistry()
			registry.Register(extractor)
			s, root := setupTestServerWithRegistry(t, registry)
			s.editAckMode = config.EditAckLocal
			watcher, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
			require.NoError(t, err)
			require.NoError(t, watcher.Start([]string{t.TempDir()}))
			s.SetWatcher(watcher)
			t.Cleanup(func() { require.NoError(t, watcher.Stop()); s.DrainBackground(); s.indexer.Close() })
			fs, err := source.NewFilesystemSource(root)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, fs.Close()) })
			changing := &physicalFallbackSource{ContentSource: fs}
			s.indexer.SetContentSource(changing)
			extractor.calls.Store(0)
			ctx := canonicalTimingContext()
			path := filepath.Join(root, "main.go")
			content := []byte("package main\nfunc AfterFallback() {}\n")
			var record *mutationCommitRecord
			if physical {
				record = commitTimingFixture(t, s, ctx, path, string(content))
				require.Positive(t, record.commitSeq)
			} else {
				// Same bytes and real queued graph work are not an MCP disk commit.
				require.NoError(t, os.WriteFile(path, content, 0600))
				require.Nil(t, s.committedPublicationRecord(ctx, path))
			}
			admission := s.admitSourceMutations(ctx, path)
			require.NotNil(t, admission.receipt)
			outcome := s.awaitSourceMutations(ctx, admission)
			require.NoError(t, outcome.Err)
			require.Eventually(t, func() bool {
				admission.receipt.mu.RLock()
				defer admission.receipt.mu.RUnlock()
				return admission.receipt.completed && admission.receipt.result.Err == nil
			}, 5*time.Second, time.Millisecond)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, content, after)
			require.GreaterOrEqual(t, changing.reads.Load(), int32(3))
			require.GreaterOrEqual(t, extractor.calls.Load(), int32(2), "the successful prepared extraction must be followed by actual fallback extraction")
			if !physical {
				require.Nil(t, s.committedPublicationRecord(ctx, path), "queued graph success cannot mint physical sequence/owner")
				return
			}
			publication := s.openCommittedPublicationRecord(ctx, record)
			require.NotNil(t, publication)
			record.recordGraph(s.awaitSourceMutations(ctx, admission))
			var window indexer.EditTimingWindow
			require.Eventually(t, func() bool {
				status := s.mutationStatusPayload(record)
				window, _ = status["phase_timing"].(indexer.EditTimingWindow)
				return window.PatchAccounting != nil
			}, 5*time.Second, time.Millisecond)
			p := window.PatchAccounting
			require.Equal(t, publication.Snapshot().Ticket, p.AttemptTicket)
			require.Zero(t, p.AttemptRetry)
			require.Equal(t, "accepted_winning_physical_attempt", p.Scope)
			require.True(t, p.PartitionComplete, "real physical fallback must expose a complete actual P partition")
			require.NotNil(t, window.PatchWallMS)
			require.NotNil(t, p.UnattributedMS)
			require.LessOrEqual(t, *p.UnattributedMS, *window.PatchWallMS/20)
		})
	}
}
