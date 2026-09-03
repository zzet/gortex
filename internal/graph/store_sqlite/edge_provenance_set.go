package store_sqlite

import (
	"database/sql"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	provenanceSelectChunkSize = 180 // 180*5 + 1 generation = 901 host parameters.
	provenanceUpdateChunkSize = 140 // 140*7 + 1 generation = 981 host parameters.
)

type provenanceFinalState struct {
	key    sqliteEdgeIdentity
	origin string
	tier   string
}

// selectEdgeOriginsTx reads the stored origin that decides whether each update
// is a real promotion. It binds the same generation the UPDATE below does: a
// stored origin read from another generation would either skip a promotion this
// corpus needs or count one the UPDATE cannot apply.
func selectEdgeOriginsTx(tx *sql.Tx, viewGen int64, updates []graph.EdgeProvenanceUpdate) (map[sqliteEdgeIdentity]string, int, error) {
	unique := make(map[sqliteEdgeIdentity]struct{}, len(updates))
	keys := make([]sqliteEdgeIdentity, 0, len(updates))
	for _, update := range updates {
		if update.Edge == nil {
			continue
		}
		key := sqliteIdentityForEdge(update.Edge)
		if _, exists := unique[key]; exists {
			continue
		}
		unique[key] = struct{}{}
		keys = append(keys, key)
	}
	origins := make(map[sqliteEdgeIdentity]string, len(keys))
	statements := 0
	for start := 0; start < len(keys); start += provenanceSelectChunkSize {
		end := minInt(start+provenanceSelectChunkSize, len(keys))
		chunk := keys[start:end]
		var values strings.Builder
		args := make([]any, 0, len(chunk)*5+1)
		for i, key := range chunk {
			if i > 0 {
				values.WriteByte(',')
			}
			values.WriteString(`(?,?,?,?,?)`)
			args = append(args, key.from, key.to, string(key.kind), key.filePath, key.line)
		}
		args = append(args, viewGen)
		rows, err := tx.Query(`WITH wanted(from_id,to_id,kind,file_path,line) AS (VALUES `+values.String()+`)
SELECT e.from_id, e.to_id, e.kind, e.file_path, e.line, e.origin
FROM wanted AS w
JOIN edges AS e
  ON e.from_id = w.from_id AND e.to_id = w.to_id AND e.kind = w.kind
 AND e.file_path = w.file_path AND e.line = w.line AND e.view_gen = ?`, args...)
		if err != nil {
			return nil, statements, err
		}
		statements++
		for rows.Next() {
			var key sqliteEdgeIdentity
			var kind, origin string
			if err := rows.Scan(&key.from, &key.to, &kind, &key.filePath, &key.line, &origin); err != nil {
				_ = rows.Close()
				return nil, statements, err
			}
			key.kind = graph.EdgeKind(kind)
			origins[key] = origin
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, statements, err
		}
		if err := rows.Close(); err != nil {
			return nil, statements, err
		}
	}
	return origins, statements, nil
}

func updateEdgeProvenanceTx(tx *sql.Tx, viewGen int64, states []provenanceFinalState) (int, error) {
	statements := 0
	for start := 0; start < len(states); start += provenanceUpdateChunkSize {
		end := minInt(start+provenanceUpdateChunkSize, len(states))
		chunk := states[start:end]
		var values strings.Builder
		args := make([]any, 0, len(chunk)*7+1)
		for i, state := range chunk {
			if i > 0 {
				values.WriteByte(',')
			}
			values.WriteString(`(?,?,?,?,?,?,?)`)
			args = append(args, state.origin, state.tier,
				state.key.from, state.key.to, string(state.key.kind), state.key.filePath, state.key.line)
		}
		args = append(args, viewGen)
		_, err := tx.Exec(`WITH updates(origin,tier,from_id,to_id,kind,file_path,line) AS (VALUES `+values.String()+`)
UPDATE edges AS e
SET origin = u.origin, tier = u.tier
FROM updates AS u
WHERE e.from_id = u.from_id AND e.to_id = u.to_id AND e.kind = u.kind
  AND e.file_path = u.file_path AND e.line = u.line
  AND e.view_gen = ?
  AND (e.origin IS NOT u.origin OR e.tier IS NOT u.tier)`, args...)
		statements++
		if err != nil {
			return statements, err
		}
	}
	return statements, nil
}

func (s *Store) setEdgeProvenanceBatchSetOriented(batch []graph.EdgeProvenanceUpdate) (totalChanged, statements int, err error) {
	if len(batch) == 0 {
		return 0, 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	for start := 0; start < len(batch); start += reindexChunkSize {
		end := minInt(start+reindexChunkSize, len(batch))
		chunk := batch[start:end]
		tx, err := s.beginWrite()
		if err != nil {
			return totalChanged, statements, err
		}
		committed := false
		func() {
			defer func() {
				if !committed {
					_ = tx.Rollback()
				}
			}()

			origins, selected, selectErr := selectEdgeOriginsTx(tx, s.viewGen, chunk)
			statements += selected
			if selectErr != nil {
				err = selectErr
				return
			}
			positions := make(map[sqliteEdgeIdentity]int, len(origins))
			final := make([]provenanceFinalState, 0, len(origins))
			chunkChanged := 0
			for _, update := range chunk {
				edge := update.Edge
				if edge == nil {
					continue
				}
				key := sqliteIdentityForEdge(edge)
				storedOrigin, found := origins[key]
				if !found || storedOrigin == update.NewOrigin {
					continue
				}
				newTier := edge.Tier
				if newTier != "" {
					newTier = graph.ResolvedBy(update.NewOrigin)
				}
				state := provenanceFinalState{key: key, origin: update.NewOrigin, tier: newTier}
				if pos, exists := positions[key]; exists {
					final[pos] = state
				} else {
					positions[key] = len(final)
					final = append(final, state)
				}
				origins[key] = update.NewOrigin
				edge.Origin = update.NewOrigin
				if edge.Tier != "" {
					edge.Tier = newTier
				}
				chunkChanged++
			}
			if chunkChanged == 0 {
				if commitErr := tx.Commit(); commitErr != nil {
					err = commitErr
					return
				}
				committed = true
				return
			}
			analysisInvalidated := s.analysisGenerationPresent
			if analysisInvalidated {
				if invalidateErr := invalidateAnalysisGenerationTx(tx); invalidateErr != nil {
					err = invalidateErr
					return
				}
			}
			updated, updateErr := updateEdgeProvenanceTx(tx, s.viewGen, final)
			statements += updated
			if updateErr != nil {
				err = updateErr
				return
			}
			if commitErr := tx.Commit(); commitErr != nil {
				err = commitErr
				return
			}
			committed = true
			if analysisInvalidated {
				s.analysisGenerationPresent = false
			}
			s.edgeIdentityRevs.Add(int64(chunkChanged))
			s.finishAnalysisMutationLocked(true)
			totalChanged += chunkChanged
		}()
		if err != nil {
			return totalChanged, statements, err
		}
	}
	return totalChanged, statements, nil
}
