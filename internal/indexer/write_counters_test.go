package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestPatchWriteLogKeepsCapturedEnd(t *testing.T) {
	_, store := newSQLiteIndexer(t)
	before := store.WriteMark()
	store.AddBatch([]*graph.Node{{ID: "patch", Name: "Patch", Kind: graph.KindFunction}}, nil)
	d := store.WriteMark().Since(before)
	require.Equal(t, int64(1), d.Rows.Nodes)
	core, logs := observer.New(zap.InfoLevel)
	zap.New(core).Info("graph patch complete", zap.Inline(patchWriteLog{split: d}))
	// observer evaluates Inline lazily through ContextMap. Both repeated
	// formatting and unrelated writes must leave the captured interval intact.
	for _, id := range []string{"later-one", "later-two"} {
		store.AddBatch([]*graph.Node{{ID: id, Name: id, Kind: graph.KindFunction}}, nil)
		fields := logs.All()[0].ContextMap()
		require.Equal(t, d.Rows.Total(), fields["rows"])
		require.Equal(t, d.WALBytes(), fields["wal_bytes"])
		require.Equal(t, d.WriterMainWriteBytes+d.OtherMainWriteBytes, fields["main_bytes"])
		require.Equal(t, d.Transactions, fields["txns"])
	}
}

func TestReindexWriteFieldsThresholdAndFallback(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	logger.Info("small", writeCounterFields("phase_", store_sqlite.VFSIOSplit{Reindex: store_sqlite.ReindexWriteCounts{Updated: 99, FastPath: 1}})...)
	require.NotContains(t, logs.All()[0].ContextMap(), "phase_reindex_updated")
	logger.Info("large", writeCounterFields("phase_", store_sqlite.VFSIOSplit{Reindex: store_sqlite.ReindexWriteCounts{Updated: 100, Deleted: 2, Inserted: 2, FastPath: 1, ReceiptFallback: 1}})...)
	fields := logs.All()[1].ContextMap()
	require.Equal(t, int64(100), fields["phase_reindex_updated"])
	require.Equal(t, int64(2), fields["phase_reindex_deleted"])
	require.Equal(t, int64(2), fields["phase_reindex_inserted"])
	require.Equal(t, false, fields["phase_reindex_fast_path"])
	require.Equal(t, "receipt_reverse_conversion", fields["phase_reindex_fallback"])
	require.Len(t, logs.All(), 2, "detail remains inline; no extra info events")
}

func TestDeltaWriterWriteCounterSource(t *testing.T) {
	// The builder supplies its store handle's sampler separately from the
	// composed graph. DeltaWriter need not expose a store/VFS capability.
	want := store_sqlite.VFSIOMark{Available: true, Rows: store_sqlite.RowWriteCounts{Nodes: 123}, Transactions: 7}
	calls := 0
	idx := &Indexer{
		graph: graph.NewDeltaWriter(graph.New(), nil),
		writeCounters: func() store_sqlite.VFSIOMark {
			calls++
			return want
		},
	}
	require.Equal(t, want, idx.writeCounterMark())
	require.Equal(t, 1, calls, "sample the injected underlying store exactly once")
}
