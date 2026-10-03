package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

const shadowEvictBatchRows = 256

// EvictRepoForShadowReplacement retires mutable base or private building
// payload rows before a successfully parsed shadow is drained. The caller must own the repository's
// replacement lane and source/output receipt until the replacement completes.
// This is deliberately not the atomic EvictRepo/EvictFiles API: cancellation
// returns the counts already committed and the caller must abandon publication.
// Positive handles require explicit managed admission: each transaction verifies
// the payload remains unsealed and its catalog reservation is still building.
func (s *Store) EvictRepoForShadowReplacement(ctx context.Context, repo string) (nodesRemoved, edgesRemoved int, retErr error) {
	if !s.SupportsShadowReplacementRetirement() {
		return 0, 0, fmt.Errorf("shadow replacement requires a mutable base or managed building payload")
	}
	if s.viewGen > 0 {
		// Refuse ready/retired/sealed reservations before even taking a payload
		// snapshot. Every subsequent chunk repeats admission in its own tx.
		if _, err := s.shadowEvictStep(ctx, func(*sql.Tx) (int, *sqliteMutationReceiptAccumulator, error) { return 0, nil, nil }); err != nil {
			return 0, 0, err
		}
	}
	// Cross-repository resolution can create incoming edges. Fence this view while
	// the frozen retirement set is consumed; derived checkout resolver lanes are
	// independent. Do not hold writeMu while waiting for this fence or reading.
	mu := s.ResolveMutex()
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}

	var ids []string
	doomed := make(map[string]struct{})
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM nodes WHERE repo_prefix = ? AND view_gen = ?`, repo, s.viewGen)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
		doomed[id] = struct{}{}
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, 0, err
	}

	// Retire successful replacement provenance before the first partial graph commit.
	// A canceled replacement must not be reused as a clean dedicated base.
	if _, err := s.shadowEvictStep(ctx, func(tx *sql.Tx) (int, *sqliteMutationReceiptAccumulator, error) {
		result, err := tx.ExecContext(ctx, `DELETE FROM repo_index_state WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repo)
		if err != nil {
			return 0, nil, err
		}
		n, err := result.RowsAffected()
		return int(n), newSQLiteMutationReceiptAccumulator(), err
	}); err != nil {
		return 0, 0, err
	}
	// Physical AUTOINCREMENT keys provide a bounded scan independent of the
	// optional endpoint indexes. Scan actual rows across all generations, then
	// filter in Go: a generation predicate before LIMIT could skip an unbounded
	// positive-generation population. Catch up newly appended sibling edges in
	// the same writer transaction before each node deletion. This generation's endpoint
	// rebinds normally use ResolveMutex; the endpoint revision also catches
	// direct rebind APIs that update a physical row behind the cursor. Raw
	// sibling inserts receive fresh keys.
	var cursor int64
	endpointClock := s.shadowEndpointRevision()
	endpointRevision := endpointClock.Load()
	for position := 0; len(ids) > 0; {
		var pageRows, nodeStep, edgeStep int
		nextCursor := cursor
		_, err := s.shadowEvictStep(ctx, func(tx *sql.Tx) (int, *sqliteMutationReceiptAccumulator, error) {
			if current := endpointClock.Load(); current != endpointRevision {
				cursor = 0
				nextCursor = 0
				endpointRevision = current
			}
			rows, err := tx.QueryContext(ctx, `SELECT id, view_gen, from_id, to_id FROM edges WHERE id > ? ORDER BY id LIMIT 256`, cursor)
			if err != nil {
				return 0, nil, err
			}
			var edgeKeys []int64
			var identityKeys []string
			for rows.Next() {
				var id, generation int64
				var from, to string
				if err = rows.Scan(&id, &generation, &from, &to); err != nil {
					break
				}
				pageRows++
				nextCursor = id
				if generation != s.viewGen {
					continue
				}
				_, fromDoomed := doomed[from]
				_, toDoomed := doomed[to]
				if fromDoomed || toDoomed {
					edgeKeys = append(edgeKeys, id)
					if fromDoomed {
						identityKeys = append(identityKeys, from)
					}
					if toDoomed {
						identityKeys = append(identityKeys, to)
					}
				}
			}
			if err == nil {
				err = rows.Err()
			}
			closeErr := rows.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				return 0, nil, err
			}
			var nodeKeys []string
			if pageRows < shadowEvictBatchRows && position < len(ids) {
				nodeKeys = ids[position:min(position+shadowEvictBatchRows, len(ids))]
				identityKeys = append(identityKeys, nodeKeys...)
			}
			delta := newSQLiteMutationReceiptAccumulator()
			if len(identityKeys) > 0 && s.hasActiveMutationReceiptsLocked() {
				keys, err := json.Marshal(identityKeys)
				if err != nil {
					return 0, nil, err
				}
				rows, err := tx.QueryContext(ctx, `SELECT id,kind,name,qual_name,file_path FROM nodes WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND view_gen=? AND repo_prefix=?`, string(keys), s.viewGen, repo)
				if err != nil {
					return 0, nil, err
				}
				for rows.Next() {
					var id, kind, name, qual, file string
					if err = rows.Scan(&id, &kind, &name, &qual, &file); err != nil {
						break
					}
					recordSQLiteEvictedNode(delta, id, kind, name, qual, file)
				}
				if err == nil {
					err = rows.Err()
				}
				closeErr := rows.Close()
				if err == nil {
					err = closeErr
				}
				if err != nil {
					return 0, nil, err
				}
			}
			if len(edgeKeys) > 0 {
				keys, err := json.Marshal(edgeKeys)
				if err != nil {
					return 0, nil, err
				}
				result, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE id IN (SELECT CAST(value AS INTEGER) FROM json_each(?)) AND view_gen=?`, string(keys), s.viewGen)
				if err != nil {
					return 0, nil, err
				}
				n, err := result.RowsAffected()
				if err != nil {
					return 0, nil, err
				}
				edgeStep = int(n)
			}
			if len(nodeKeys) > 0 {
				keys, err := json.Marshal(nodeKeys)
				if err != nil {
					return 0, nil, err
				}
				result, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND view_gen=? AND repo_prefix=?`, string(keys), s.viewGen, repo)
				if err != nil {
					return 0, nil, err
				}
				n, err := result.RowsAffected()
				if err != nil {
					return 0, nil, err
				}
				nodeStep = int(n)
			}
			return nodeStep + edgeStep, delta, nil
		})
		if err != nil {
			return nodesRemoved, edgesRemoved, err
		}
		cursor = nextCursor
		nodesRemoved += nodeStep
		edgesRemoved += edgeStep
		if pageRows < shadowEvictBatchRows {
			if position >= len(ids) {
				break
			}
			position = min(position+shadowEvictBatchRows, len(ids))
		}
	}
	// Keep the same sidecar scope as atomic repository eviction, including a
	// repository with no remaining nodes. Each tuple is its table's primary key.
	for _, statement := range []string{
		`DELETE FROM semantic_binding_types WHERE (view_gen, repo_prefix, file_path, line, name) IN (SELECT view_gen, repo_prefix, file_path, line, name FROM semantic_binding_types WHERE view_gen = ? AND repo_prefix = ? LIMIT 256)`,
		`DELETE FROM file_index_failures WHERE (view_gen, repo_prefix, file_path) IN (SELECT view_gen, repo_prefix, file_path FROM file_index_failures WHERE view_gen = ? AND repo_prefix = ? LIMIT 256)`,
	} {
		for {
			n, err := s.shadowEvictStep(ctx, func(tx *sql.Tx) (int, *sqliteMutationReceiptAccumulator, error) {
				result, err := tx.ExecContext(ctx, statement, s.viewGen, repo)
				if err != nil {
					return 0, nil, err
				}
				n, err := result.RowsAffected()
				return int(n), newSQLiteMutationReceiptAccumulator(), err
			})
			if err != nil {
				return nodesRemoved, edgesRemoved, err
			}
			if n == 0 {
				break
			}
		}
	}
	return nodesRemoved, edgesRemoved, nil
}

// shadowEvictStep releases the writer gate after every bounded transaction.
// A queued checkout writer gets its turn before the next step; announced writes
// waiting on a coordinator must not prevent retirement itself from progressing.
func (s *Store) shadowEvictStep(ctx context.Context, mutate func(*sql.Tx) (int, *sqliteMutationReceiptAccumulator, error)) (int, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return 0, err
	}
	defer s.writeMu.Unlock()
	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // committed transactions have nothing left to roll back
	n, delta, err := mutate(tx)
	if err != nil {
		return 0, err
	}
	invalidated := n > 0 && s.analysisGenerationPresent
	if invalidated {
		if err := s.invalidateAnalysisViewTx(tx); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if invalidated {
		s.analysisGenerationPresent = s.analysisLatchRemaining
	}
	s.finishAnalysisMutationLocked(n > 0)
	if n > 0 {
		s.mergeMutationReceiptLocked(delta)
	}
	return n, nil
}

// Endpoint-rewrite families call this after a successful transaction, still
// holding writeMu. Some families also delete collisions/change edge kind, so
// their changed flag is conservative; ordinary append and attribute writes do
// not touch this generation's clock. It is a retirement fence, not provenance.
func (s *Store) noteEdgeEndpointRewriteLocked(changed bool) {
	if changed {
		s.shadowEndpointRevision().Add(1)
	}
}

// SupportsShadowReplacementRetirement reports only the handle capability.
// Actual positive lifecycle admission is freshly checked by each transaction;
// a true result never authorizes writing a ready, retired or sealed payload.
func (s *Store) SupportsShadowReplacementRetirement() bool {
	return s != nil && (s.viewGen == 0 || s.managedPayloadGeneration)
}

func (s *Store) shadowEndpointRevision() *atomic.Uint64 {
	if s.viewGen == 0 {
		return &s.baseEdgeEndpointRevision
	}
	return &s.payloadSealFor(s.viewGen).endpointRevision
}
