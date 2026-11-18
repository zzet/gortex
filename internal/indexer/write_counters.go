package indexer

import (
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// These are process-interval activity counters, not exclusive per-edit costs.
// Overlapping patches/background writers contribute to each enclosing interval;
// nested phases must not be summed with their parent totals.
func writeCounterFields(prefix string, d store_sqlite.VFSIOSplit) []zap.Field {
	fields := []zap.Field{
		zap.Int64(prefix+"wal_bytes", d.WALBytes()),
		zap.Int64(prefix+"main_bytes", d.WriterMainWriteBytes+d.OtherMainWriteBytes),
		zap.Bool(prefix+"wal_bytes_reset", d.WALReset),
		zap.Int64(prefix+"rows", d.Rows.Total()), zap.Int64(prefix+"txns", d.Transactions),
		zap.Int64(prefix+"node_rows", d.Rows.Nodes), zap.Int64(prefix+"edge_rows", d.Rows.Edges),
		zap.Int64(prefix+"symbol_fts_rows", d.Rows.SymbolFTS), zap.Int64(prefix+"content_fts_rows", d.Rows.ContentFTS),
	}
	// Suppress small rebind noise; piggyback on the existing phase/patch line.
	r := d.Reindex
	if r.Updated+r.Deleted+r.Inserted >= 100 {
		reason := "none"
		if r.Generic > 0 {
			reason = "not_uniform_namespace_conversion"
		}
		if r.ReceiptFallback > 0 {
			if r.Generic > 0 {
				reason = "not_uniform_namespace_conversion,receipt_reverse_conversion"
			} else {
				reason = "receipt_reverse_conversion"
			}
		}
		fields = append(fields, zap.Int64(prefix+"reindex_updated", r.Updated), zap.Int64(prefix+"reindex_deleted", r.Deleted), zap.Int64(prefix+"reindex_inserted", r.Inserted), zap.Bool(prefix+"reindex_fast_path", r.FastPath > 0 && r.Generic == 0 && r.ReceiptFallback == 0), zap.String(prefix+"reindex_fallback", reason))
	}
	return fields
}

type patchWriteLog struct{ split store_sqlite.VFSIOSplit }

func (m patchWriteLog) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	for _, f := range writeCounterFields("", m.split) {
		f.AddTo(enc)
	}
	return nil
}

func (idx *Indexer) writeCounterMark() store_sqlite.VFSIOMark {
	if idx.writeCounters != nil {
		return idx.writeCounters()
	}
	return store_sqlite.WriteMark()
}
func (mi *MultiIndexer) writeCounterMark() store_sqlite.VFSIOMark {
	if source, ok := mi.graph.(interface{ WriteMark() store_sqlite.VFSIOMark }); ok {
		return source.WriteMark()
	}
	return store_sqlite.WriteMark()
}
func (idx *Indexer) startReconcilePhase(logger *zap.Logger, repo, phase string, fields ...zap.Field) *reconcilePhaseTiming {
	return startReconcilePhaseWithWrites(idx.writeCounterMark, logger, repo, phase, fields...)
}
func (mi *MultiIndexer) startReconcilePhase(logger *zap.Logger, repo, phase string, fields ...zap.Field) *reconcilePhaseTiming {
	return startReconcilePhaseWithWrites(mi.writeCounterMark, logger, repo, phase, fields...)
}
