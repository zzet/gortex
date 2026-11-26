package indexer

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

// writerWriteFixture indexes files into g under a fresh root and returns a
// watcher whose mutations are admitted explicitly.
func writerWriteFixture(t *testing.T, g graph.Store, files map[string]string) (string, *Indexer, *Watcher) {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		writeTestFile(t, filepath.Join(root, name), content)
	}
	idx := newTestIndexer(g)
	t.Cleanup(idx.Close)
	idx.SetRootPath(root)
	_, err := idx.Index(root)
	require.NoError(t, err)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	w.degradedNoFsnotify = true
	t.Cleanup(func() {
		if !w.mutationAdmissionStopped() {
			require.NoError(t, w.Stop())
		}
	})
	return root, idx, w
}

// writerWriteOf is the accepted physical attempt's writer_write object, read
// back through JSON the way mutation_status renders phase_timing.
func writerWriteOf(t *testing.T, record *PublicationPhaseRecord) map[string]any {
	t.Helper()
	require.Eventually(t, func() bool { return record.Snapshot().attemptWindow != nil }, 5*time.Second, time.Millisecond)
	raw, err := json.Marshal(record.Snapshot().EditTimings().Window)
	require.NoError(t, err)
	var window map[string]any
	require.NoError(t, json.Unmarshal(raw, &window))
	accounting, _ := window["patch_accounting"].(map[string]any)
	writer, ok := accounting["writer_write"].(map[string]any)
	require.True(t, ok, "patch_accounting carries writer_write: %s", raw)
	return writer
}

func writerNumber(t *testing.T, writer map[string]any, key string) float64 {
	t.Helper()
	v, ok := writer[key].(float64)
	require.True(t, ok, "%s is a measured number: %v", key, writer)
	return v
}

// requireMeasuredWriterWrite checks the invariants of one measured reading
// against its own attempt's patch window.
func requireMeasuredWriterWrite(t *testing.T, name string, record *PublicationPhaseRecord, writer map[string]any) {
	t.Helper()
	require.Equal(t, "measured", writer["state"], "%s: %v", name, writer)
	require.Nil(t, writer["reason"], name)
	require.GreaterOrEqual(t, writerNumber(t, writer, "holds"), 1.0, name)
	require.Positive(t, writerNumber(t, writer, "xwrite_bytes"), name)
	xwrite := writerNumber(t, writer, "xwrite_ms")
	require.InDelta(t, xwrite, writerNumber(t, writer, "main_xwrite_ms")+writerNumber(t, writer, "wal_xwrite_ms"), 1e-6, name)
	require.LessOrEqual(t, xwrite, writerNumber(t, writer, "hold_ms"), "%s: xWrite runs inside the holds", name)
	require.Equal(t, "derived_lap_started", writer["derived_cut"], name)
	require.LessOrEqual(t, writerNumber(t, writer, "before_derived_xwrite_ms"), xwrite, name)
	wall := record.Snapshot().EditTimings().Window.PatchWallMS
	require.NotNil(t, wall, name)
	first, last := writerNumber(t, writer, "first_hold_start_offset_ms"), writerNumber(t, writer, "last_hold_end_offset_ms")
	require.GreaterOrEqual(t, first, 0.0, name)
	require.LessOrEqual(t, first, last, name)
	require.LessOrEqual(t, last, *wall, "%s: owned holds lie inside the attempt", name)
}

// Each edit's receipt reports the SQLite writer time of the write-gate holds
// its own serving attempt took. Alone, an attempt's holds are the store's only
// writes while it patches. With a second editor's attempt open and an
// unrelated writer committing a larger transaction while the first attempt is
// held mid-patch, neither receipt counts the other's or the unrelated writes.
func TestEditWriterWriteBelongsToServingAttempt(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "writer.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root, idx, w := writerWriteFixture(t, store, map[string]string{
		"small.go": "package p\n\nfunc Small() {}\n",
		"large.go": "package p\n\nfunc Large() {}\n",
	})
	// Armed after the quiet edit: holds the first attempt at its resolver.
	var armed atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var holdOnce sync.Once
	idx.incrementalCatchupHook = func(kind string, _ []string) {
		if kind == "resolve" && armed.Load() {
			holdOnce.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	// Signals once the second editor's attempt runs inside its attribution.
	secondOpen := make(chan struct{})
	var secondOnce sync.Once
	w.pointMutationClaimed = func(path string) {
		if armed.Load() && filepath.Base(path) == "small.go" {
			secondOnce.Do(func() { close(secondOpen) })
		}
	}
	recorder := NewPublicationPhaseRecorder(8, 4)
	t.Cleanup(func() { runtime.KeepAlive(recorder) })
	edit := func(name, content string) (*PublicationPhaseRecord, *MutationTicket) {
		t.Helper()
		path := filepath.Join(root, name)
		bumpMtime(t, path, content)
		record := recorder.Begin("", name+fmt.Sprint(time.Now().UnixNano()), PublicationSourceMCPEdit, time.Now())
		ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
		require.NoError(t, err)
		return record, ticket
	}

	quiet, ticket := edit("small.go", "package p\n\nfunc Small() {}\n\nfunc Quiet() { Small() }\n")
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	quietWriter := writerWriteOf(t, quiet)
	if quietWriter["state"] == "unavailable" && quietWriter["reason"] == "vfs_write_hooks_unavailable" {
		t.Skip("SQLite VFS write hooks are not installed in this process")
	}
	requireMeasuredWriterWrite(t, "quiet", quiet, quietWriter)
	require.Zero(t, writerNumber(t, quietWriter, "unowned_store_holds"),
		"alone, every store write while the attempt patched was the attempt's own: %v", quietWriter)

	armed.Store(true)
	var body strings.Builder
	body.WriteString("package p\n\nfunc Large() {}\n")
	for i := range 200 {
		fmt.Fprintf(&body, "func Large%03d() { Small(); Large() }\n", i)
	}
	runStart := store_sqlite.WriteMark()
	first, firstTicket := edit("large.go", body.String())
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first attempt did not reach its held resolver boundary")
	}
	second, secondTicket := edit("small.go", "package p\n\nfunc Small() {}\n\nfunc SmallToo() { Small() }\n")
	select {
	case <-secondOpen:
	case <-time.After(10 * time.Second):
		t.Fatal("the second attempt did not start while the first was held")
	}

	// An unrelated writer commits while the first attempt is held and the
	// second is open; nothing else writes meanwhile, so the process-wide
	// writer counters measure its size.
	nodes := make([]*graph.Node, 0, 3000)
	for i := range cap(nodes) {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("unrelated/u.go::U%04d", i), Kind: graph.KindFunction, Name: fmt.Sprintf("U%04d", i),
			FilePath: "unrelated/u.go", StartLine: i + 1, EndLine: i + 1, Language: "go", Meta: map[string]any{"doc": strings.Repeat("x", 512)}})
	}
	mark := store_sqlite.WriteMark()
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.AddBatch(nodes, nil)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the unrelated writer did not commit while the first attempt was held")
	}
	unrelated := store_sqlite.WriteMark().Since(mark)
	unrelatedBytes := float64(unrelated.WriterMainWriteBytes + unrelated.WriterWALWriteBytes)
	require.Positive(t, unrelatedBytes, "the unrelated batch wrote through the writer connection")
	unblock()
	for _, ticket := range []*MutationTicket{firstTicket, secondTicket} {
		require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	}

	firstWriter, secondWriter := writerWriteOf(t, first), writerWriteOf(t, second)
	run := store_sqlite.WriteMark().Since(runStart)
	runBytes := float64(run.WriterMainWriteBytes + run.WriterWALWriteBytes)
	owned := writerNumber(t, firstWriter, "xwrite_bytes") + writerNumber(t, secondWriter, "xwrite_bytes")
	t.Logf("writer bytes: run=%.0f first+second=%.0f unrelated=%.0f; first=%v second=%v", runBytes, owned, unrelatedBytes, firstWriter, secondWriter)
	require.LessOrEqual(t, owned+unrelatedBytes, runBytes, "attributed writes never exceed what the writer wrote")
	requireMeasuredWriterWrite(t, "first", first, firstWriter)
	requireMeasuredWriterWrite(t, "second", second, secondWriter)
	for name, writer := range map[string]map[string]any{"first": firstWriter, "second": secondWriter} {
		require.Less(t, writerNumber(t, writer, "xwrite_bytes"), unrelatedBytes, "%s never counts the unrelated commit", name)
	}
	require.GreaterOrEqual(t, writerNumber(t, firstWriter, "unowned_store_holds"), 1.0, "the unrelated commit overlapped the first attempt")
	require.Greater(t, writerNumber(t, firstWriter, "xwrite_bytes"), writerNumber(t, secondWriter, "xwrite_bytes"),
		"each receipt keeps its own attempt's writes")
}

// An attempt whose graph cannot attribute SQLite writer time reports why,
// with every number null, never a measured zero.
func TestEditWriterWriteUnavailableIsNull(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		open         func(*testing.T) graph.Store
	}{
		{"in_memory_graph", "not_a_sqlite_store", func(*testing.T) graph.Store { return graph.New() }},
		{"sqlite_without_writer_connection", "no_dedicated_writer_connection", func(t *testing.T) graph.Store {
			s, err := store_sqlite.Open(":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, w := writerWriteFixture(t, tc.open(t), map[string]string{"edit.go": "package p\n\nfunc Before() {}\n"})
			path := filepath.Join(root, "edit.go")
			bumpMtime(t, path, "package p\n\nfunc After() {}\n")
			record := newPointPublication(t, tc.name)
			ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
			require.NoError(t, err)
			require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
			writer := writerWriteOf(t, record)
			require.Equal(t, "unavailable", writer["state"])
			require.Equal(t, tc.reason, writer["reason"])
			for _, key := range []string{"xwrite_ms", "main_xwrite_ms", "wal_xwrite_ms", "xwrite_calls", "xwrite_bytes", "holds", "hold_ms",
				"first_hold_start_offset_ms", "last_hold_end_offset_ms", "before_derived_xwrite_ms", "before_derived_hold_ms", "unowned_store_holds"} {
				value, present := writer[key]
				require.True(t, present, "%s is rendered as null, not omitted", key)
				require.Nil(t, value, key)
			}
		})
	}
}
