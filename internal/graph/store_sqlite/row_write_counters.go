package store_sqlite

import (
	"database/sql"
	"sync/atomic"
)

// RowWriteCounts counts directly changed rows in the four graph payload tables.
// Statement RowsAffected excludes trigger writes (including generation census
// triggers). Like VFS counters, these are process-wide activity, not committed
// cardinality: successful statements in a later rolled-back transaction count.
// INSERT ... RETURNING helpers add their returned-row census once per statement.
type RowWriteCounts struct {
	Nodes      int64 `json:"nodes"`
	Edges      int64 `json:"edges"`
	SymbolFTS  int64 `json:"symbol_fts"`
	ContentFTS int64 `json:"content_fts"`
}

func (r RowWriteCounts) Total() int64 { return r.Nodes + r.Edges + r.SymbolFTS + r.ContentFTS }
func (r RowWriteCounts) Since(p RowWriteCounts) RowWriteCounts {
	return RowWriteCounts{r.Nodes - p.Nodes, r.Edges - p.Edges, r.SymbolFTS - p.SymbolFTS, r.ContentFTS - p.ContentFTS}
}

var nodeRowsWritten, edgeRowsWritten, symbolFTSRowsWritten, contentFTSRowsWritten atomic.Int64

func rowWriteMark() RowWriteCounts {
	return RowWriteCounts{nodeRowsWritten.Load(), edgeRowsWritten.Load(), symbolFTSRowsWritten.Load(), contentFTSRowsWritten.Load()}
}
func countStatementRows(counter *atomic.Int64, result sql.Result, err error) {
	if err == nil && result != nil {
		if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows > 0 {
			counter.Add(rows)
		}
	}
}
func countNodeWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&nodeRowsWritten, result, err)
	return result, err
}
func countEdgeWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&edgeRowsWritten, result, err)
	return result, err
}
func countSymbolFTSWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&symbolFTSRowsWritten, result, err)
	return result, err
}
func countContentFTSWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&contentFTSRowsWritten, result, err)
	return result, err
}

// ReindexWriteCounts is committed set-reindex work over an interval. Generic
// simulation is reported separately from the receipt-safety fallback so a log
// never calls all delete/insert work an in-place conversion.
type ReindexWriteCounts struct {
	Updated, Deleted, Inserted         int64
	FastPath, Generic, ReceiptFallback int64
}

var reindexUpdated, reindexDeleted, reindexInserted, reindexFast, reindexGeneric, reindexReceipt atomic.Int64

func reindexWriteMark() ReindexWriteCounts {
	return ReindexWriteCounts{reindexUpdated.Load(), reindexDeleted.Load(), reindexInserted.Load(), reindexFast.Load(), reindexGeneric.Load(), reindexReceipt.Load()}
}
func (r ReindexWriteCounts) Since(p ReindexWriteCounts) ReindexWriteCounts {
	return ReindexWriteCounts{r.Updated - p.Updated, r.Deleted - p.Deleted, r.Inserted - p.Inserted, r.FastPath - p.FastPath, r.Generic - p.Generic, r.ReceiptFallback - p.ReceiptFallback}
}
func noteReindexWrites(s sqliteReindexSetStats) {
	reindexUpdated.Add(int64(s.updatedRows))
	reindexDeleted.Add(int64(s.deletedRows))
	reindexInserted.Add(int64(s.insertedRows))
	reindexFast.Add(int64(s.fastPath))
	reindexGeneric.Add(int64(s.genericFallback))
	reindexReceipt.Add(int64(s.receiptFallback))
}

// noteTableRows is for existing helpers whose payload table is a parameter.
// Sidecars, FTS ownership maps and catalog tables intentionally contribute zero.
func noteTableRows(table string, result sql.Result, err error) {
	switch table {
	case "nodes":
		countStatementRows(&nodeRowsWritten, result, err)
	case "edges":
		countStatementRows(&edgeRowsWritten, result, err)
	case "symbol_fts":
		countStatementRows(&symbolFTSRowsWritten, result, err)
	case "content_fts":
		countStatementRows(&contentFTSRowsWritten, result, err)
	}
}
