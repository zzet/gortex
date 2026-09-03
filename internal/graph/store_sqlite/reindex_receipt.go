package store_sqlite

import (
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// sqliteReindexReceipt records only newly inserted unresolved edges. Removing
// an unresolved edge, or replacing one with a resolved edge, cannot create work
// for the final resolver catch-up. The recorder is built per transaction and
// published only after that transaction commits.
type sqliteReindexReceipt struct {
	delta        *sqliteMutationReceiptAccumulator
	sourceNodes  map[string]sqliteMutationNodeIdentity
	sourcesExact bool
}

// prepareSQLiteReindexReceiptTx preloads source identities in bounded SQL
// batches for unresolved edges whose own file_path is empty. It is a no-op when
// no receipt window is active, keeping the normal reindex path read-free.
//
// The set of entries preloaded here must stay exactly the set
// sqliteReindexMutations turns into writes. An entry that produces a write
// without a preloaded source node records an edge with no exact file, which
// widens the incremental resolution frontier for the whole receipt window — so
// an entry the mutation builder stopped skipping must stop being skipped here
// too.
func (s *Store) prepareSQLiteReindexReceiptTx(tx *sql.Tx, batch []graph.EdgeReindex) *sqliteReindexReceipt {
	if !s.hasActiveMutationReceiptsLocked() {
		return nil
	}

	receipt := &sqliteReindexReceipt{
		delta:        newSQLiteMutationReceiptAccumulator(),
		sourcesExact: true,
	}
	ids := make([]string, 0, len(batch))
	for _, reindex := range batch {
		edge := reindex.Edge
		if edge == nil || edge.FilePath != "" || !graph.IsUnresolvedTarget(edge.To) {
			continue
		}
		ids = append(ids, edge.From)
	}

	var err error
	receipt.sourceNodes, err = mutationNodeIdentitiesTx(tx, s.viewGen, ids)
	if err != nil {
		receipt.sourcesExact = false
		receipt.sourceNodes = make(map[string]sqliteMutationNodeIdentity)
	}
	return receipt
}

// recordInserted records the resolution frontier created by one successful
// INSERT. INSERT OR IGNORE collisions are deliberately excluded: deleting the
// stale identity while an equivalent destination row already exists creates no
// new unresolved work.
func (r *sqliteReindexReceipt) recordInserted(edge *graph.Edge, inserted bool) {
	if r == nil || edge == nil || !inserted || !graph.IsUnresolvedTarget(edge.To) {
		return
	}

	file := edge.FilePath
	if file == "" {
		if source, found := r.sourceNodes[edge.From]; found {
			file = source.filePath
		} else if !r.sourcesExact {
			r.delta.complete = false
		}
	}
	recordSQLiteAddedEdge(r.delta, edge, file)
}

func (s *Store) publishSQLiteReindexReceiptLocked(receipt *sqliteReindexReceipt) {
	if receipt == nil {
		return
	}
	s.mergeMutationReceiptLocked(receipt.delta)
}
