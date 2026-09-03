package store_sqlite

import "testing"

func TestBulkCheckpointCadenceStaysCoarseAfterIndexSeal(t *testing.T) {
	for _, indexesDeferred := range []bool{true, false} {
		s := &Store{storeCore: &storeCore{bulkIndexesDeferred: indexesDeferred}}
		nodeRows, edgeRows := s.bulkCheckpointIntervalsLocked()
		if nodeRows != bulkCheckpointNodeInterval || edgeRows != bulkCheckpointEdgeInterval {
			t.Fatalf("indexesDeferred=%t: checkpoint intervals=(%d, %d), want (%d, %d)",
				indexesDeferred, nodeRows, edgeRows,
				bulkCheckpointNodeInterval, bulkCheckpointEdgeInterval)
		}
	}
}
