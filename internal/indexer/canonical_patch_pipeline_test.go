package indexer

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
)

func beginRealPatchObservation(t *testing.T) (*PublicationPhaseRecord, *PublicationStamps) {
	t.Helper()
	r := newPointPublication(t, "real-core")
	require.True(t, r.bindCanonicalTicket(1, time.Now()))
	s := &PublicationStamps{patchLaps: &canonicalPatchLaps{}}
	s.markAt(PublicationPatchExecutionStarted, time.Now())
	return r, s
}

func finishRealPatchObservation(r *PublicationPhaseRecord, s *PublicationStamps) PublicationPhaseSnapshot {
	s.markAt(PublicationPatchExecutionFinished, time.Now())
	r.Absorb(s)
	r.Mark(PublicationTicketCompleted)
	return r.Snapshot()
}

func TestCanonicalPatchRealChunksAggregateWithoutChargingInterleavedWork(t *testing.T) {
	root := t.TempDir()
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(root)
	var paths []string
	for i := range incrementalBatchFiles + 1 {
		path := filepath.Join(root, fmt.Sprintf("f%02d.go", i))
		writeTestFile(t, path, fmt.Sprintf("package p\nfunc Before%02d() {}\n", i))
		paths = append(paths, path)
	}
	_, err := idx.Index(root)
	require.NoError(t, err)
	for i, path := range paths {
		bumpMtime(t, path, fmt.Sprintf("package p\nfunc After%02d() { After%02d() }\n", i, (i+1)%len(paths)))
	}
	r, s := beginRealPatchObservation(t)
	observedInterleavedGap := false
	idx.affectedByDeltaHook = func(string, []string) {
		if observedInterleavedGap {
			return
		}
		s.mu.Lock()
		parseEnd := s.patchLaps.laps[patchParse].last
		s.mu.Unlock()
		require.False(t, parseEnd.IsZero(), "the real apply hook follows a completed parse chunk")
		require.Eventually(t, func() bool { return time.Now().After(parseEnd) }, 5*time.Second, time.Millisecond,
			"observe an actual interleaved apply interval before the next parse chunk")
		observedInterleavedGap = true
	}
	var bookkeepingHooks []string
	idx.incrementalCatchupHook = func(kind string, _ []string) {
		if kind != "dataflow" && kind != "ref_facts" {
			return
		}
		s.mu.Lock()
		active := s.patchLaps.active
		bookkeeping := s.patchLaps.laps[patchBookkeeping]
		s.mu.Unlock()
		require.Equal(t, 1, active, "actual follow-up work must own one disjoint interval")
		require.Equal(t, bookkeeping.done+1, bookkeeping.begun, kind)
		bookkeepingHooks = append(bookkeepingHooks, kind)
	}
	result, err := idx.incrementalWatcherPaths(root, paths, incrementalPathMode{
		detectDeletions: true, forceExplicitFiles: true, publication: s,
	})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	for i, path := range paths {
		caller := nodeByName(t, idx, filepath.Base(path), fmt.Sprintf("After%02d", i))
		next := (i + 1) % len(paths)
		callee := nodeByName(t, idx, filepath.Base(paths[next]), fmt.Sprintf("After%02d", next))
		require.Equal(t, callee.ID, callEdge(t, idx, caller.ID).To, "the actual deferred resolver must bind the new cross-file call")
	}
	snapshot := finishRealPatchObservation(r, s)
	evidence := snapshot.patchLaps
	require.NotNil(t, evidence)
	require.False(t, evidence.invalid)
	require.Equal(t, []string{"dataflow", "ref_facts"}, bookkeepingHooks, "the actual follow-up passes must still run once in order")
	require.Equal(t, uint64(1), evidence.laps[patchBookkeeping].done)
	require.Equal(t, evidence.laps[patchBookkeeping].begun, evidence.laps[patchBookkeeping].done)
	require.GreaterOrEqual(t, evidence.laps[patchParse].done, uint64(2), "must execute actual repeated chunks")
	require.GreaterOrEqual(t, evidence.laps[patchSearch].done, uint64(2))
	require.GreaterOrEqual(t, evidence.laps[patchApply].done, uint64(4), "apply is split at each real search boundary")
	parse := evidence.laps[patchParse]
	require.True(t, observedInterleavedGap)
	require.Less(t, parse.totalNS, parse.lastNS-parse.firstNS, "the first→last parse envelope includes intervening apply/search and must not be used as parse time")
	var sum int64
	timings := snapshot.EditTimings()
	require.NotContains(t, timings.Milliseconds, "bookkeeping", "private evidence does not silently change the requested lap contract")
	for i, name := range canonicalPatchLapNames {
		v := evidence.laps[i]
		require.Equal(t, v.begun, v.done, name)
		require.Equal(t, "measured", timings.States[name], name)
		require.Equal(t, float64(v.totalNS)/1e6, *timings.Milliseconds[name], name)
		sum += v.totalNS
	}
	offsets := phaseOffsets(snapshot)
	require.LessOrEqual(t, sum, offsets[PublicationPatchExecutionFinished]-offsets[PublicationPatchExecutionStarted])
	require.NotNil(t, timings.Window.PublicationUnattributedWallMS)
}

// This controlled source changes only the prepared refresh read. It forces the
// real legacy combined fallback through an existing source seam, without a
// production knob or timing-dependent filesystem race. All later reads serve
// the actual owned file bytes again.
type refreshMismatchSource struct {
	source.ContentSource
	reads atomic.Int32
}

func (s *refreshMismatchSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	rc, meta, err := s.ContentSource.Open(path)
	if err != nil || path != "edit.go" || s.reads.Add(1) != 2 {
		return rc, meta, err
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, meta, err
	}
	data = append(data, []byte("\n// prepared refresh moved on\n")...)
	meta.Size = int64(len(data))
	return io.NopCloser(bytes.NewReader(data)), meta, nil
}

func TestCanonicalPatchRealCombinedFallbackCannotPublishPartialLapTotals(t *testing.T) {
	path, idx, _ := pointPublicationFixture(t)
	fs, err := source.NewFilesystemSource(filepath.Dir(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fs.Close()) })
	changing := &refreshMismatchSource{ContentSource: fs}
	idx.SetContentSource(changing)
	t.Cleanup(func() { idx.SetContentSource(nil) })
	r, s := beginRealPatchObservation(t)
	_, _, reparsed, failed, _ := idx.reindexIncrementalChunk([]string{path}, &reparsePendingEnrichmentBatch{
		deferResolverCatchup: true, publication: s,
	}, false)
	require.Empty(t, failed)
	require.Contains(t, reparsed, path)
	require.GreaterOrEqual(t, changing.reads.Load(), int32(3), "speculative read, changed refresh and actual fallback must all run")
	require.NotNil(t, nodeByName(t, idx, "edit.go", "After"), "the fallback must actually parse and apply")
	snapshot := finishRealPatchObservation(r, s)
	for _, lap := range []canonicalPatchLap{patchParse, patchApply, patchSearch} {
		require.True(t, snapshot.patchLaps.laps[lap].incomplete)
		name := canonicalPatchLapNames[lap]
		require.Nil(t, snapshot.EditTimings().Milliseconds[name])
		require.Equal(t, "not_observed", snapshot.EditTimings().States[name])
	}
	require.FileExists(t, path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(data), "prepared refresh moved on", "the diagnostic source must not change actual disk content")
}
