package mcp

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestBatchProjectionVerificationSharesOneDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prepared := map[string]*preparedFileProjection{
			"a": {entry: &projectionEntry{}}, "b": {entry: &projectionEntry{}}, "c": {entry: &projectionEntry{}},
		}
		receipt := batchTransactionReceipt{Files: []batchTransactionFile{{Path: "a"}, {Path: "b"}, {Path: "c"}}}
		calls := 0
		start := time.Now()
		members := verifyBatchProjectionCohort(t.Context(), receipt, prepared, 7, func(ctx context.Context, p *preparedFileProjection, seq uint64, sha string) (*projectionEntry, error) {
			calls++
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.Equal(t, start.Add(150*time.Millisecond), deadline)
			select {
			case <-time.After(100 * time.Millisecond):
				return p.entry, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		require.Nil(t, members, "deadline cannot publish the verified prefix alone")
		require.Equal(t, 2, calls, "no fresh budget for later members")
		require.Equal(t, 150*time.Millisecond, time.Since(start))
	})
}

type batchProjectionOverlapExtractor struct {
	parser.Extractor
	active   atomic.Bool
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (e *batchProjectionOverlapExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	if e.active.Load() {
		e.entered <- struct{}{}
		<-e.release
		defer func() { e.finished <- struct{}{} }()
	}
	return e.Extractor.Extract(path, raw)
}

func TestBatchProjectionPreparationOverlapsRealExtractions(t *testing.T) {
	extractor := &batchProjectionOverlapExtractor{Extractor: languages.NewGoExtractor(), entered: make(chan struct{}, 2), release: make(chan struct{}), finished: make(chan struct{}, 2)}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	s, root := setupTestServerWithRegistry(t, registry)
	a, b := filepath.Join(root, "main.go"), filepath.Join(root, "other.go")
	ctx, ack, _ := s.withEditAcknowledgement(t.Context())
	ctx, unlock, err := acquireMutationPaths(ctx, []string{a, b})
	require.NoError(t, err)
	defer unlock()
	buffers := map[string]*batchFileBuffer{
		a: {absPath: a, relPath: "main.go", original: []byte("package main\nfunc helper() {}\n"), content: []byte("package main\nfunc ParallelFirst() {}\n"), existsBefore: true, existsAfter: true},
		b: {absPath: b, relPath: "other.go", content: []byte("package main\nfunc ParallelSecond() {}\n"), existsAfter: true},
	}
	extractor.active.Store(true)
	released := false
	defer func() {
		if !released {
			close(extractor.release)
		}
	}()
	result := make(chan map[string]*preparedFileProjection, 1)
	go func() { result <- s.prepareBatchProjections(ctx, buffers, []string{a, b}) }()
	for range 2 {
		select {
		case <-extractor.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("both native extractions did not enter")
		}
	}
	close(extractor.release)
	released = true
	var got map[string]*preparedFileProjection
	select {
	case got = <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("batch preparation did not join")
	}
	for range 2 {
		select {
		case <-extractor.finished:
		case <-time.After(2 * time.Second):
			t.Fatal("owned native extraction did not finish")
		}
	}
	require.NotNil(t, got[a])
	require.NotNil(t, got[b])
	require.NotNil(t, got[a].entry, "a serial build expired before its sibling could enter")
	require.NotNil(t, got[b].entry)
	ack.mu.Lock()
	defer ack.mu.Unlock()
	require.EqualValues(t, 1, ack.laps.data[editAckParseGate].started)
	require.EqualValues(t, 1, ack.laps.data[editAckParseGate].completed)
	require.False(t, ack.laps.overlap, "concurrent files share one wall lap, never nested per-file laps")
	require.False(t, ack.laps.data[editAckParseGate].invalid)
	require.Equal(t, "source_projection_parse_gate", ack.parserScope)
}
