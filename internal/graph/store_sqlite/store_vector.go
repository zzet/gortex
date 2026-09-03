package store_sqlite

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion that the SQLite Store satisfies the optional
// engine-native vector-search capability.
var _ graph.VectorSearcher = (*Store)(nil)

// errInvalidDims is returned by BuildVectorIndex for a negative width.
var errInvalidDims = errors.New("store_sqlite: invalid vector dims")

// Vector design (pure-Go, zero CGo)
//
// modernc.org/sqlite is a pure-Go SQLite that cannot load C extensions,
// so sqlite-vec / sqlite-vector are off the table — and staying CGo-free
// is the whole point of this backend. Embeddings are persisted as a
// little-endian float32 BLOB in the `vectors` table; the win over the
// daemon's in-process HNSW fallback is durability: vectors survive a
// restart instead of being recomputed.
//
// Queries use an exact brute-force cosine top-k: SimilarTo streams every
// stored vector, scores it against the query, and keeps the best `limit`
// in a bounded max-heap. This is O(N) per query but fully correct,
// deterministic, and holds no extra Store state (the Store struct lives
// in store.go and cannot be edited here). An on-Store HNSW cache is a
// future optimisation; for the corpus sizes this backend targets the
// exact path is the simplest thing that is verifiably right.
//
// BuildVectorIndex only validates/records intent — there is no separate
// index structure to build, since SimilarTo computes over the table
// directly.

// vectorChunk bounds rows per multi-row INSERT in BulkUpsertEmbeddings and
// ReplaceVectorCorpus. Six host parameters per row, SQLite's conservative
// default limit is 999; 160 leaves headroom.
const vectorChunk = 160

// encodeVec serialises a float32 slice to a little-endian BLOB
// (4 bytes per element).
func encodeVec(vec []float32) []byte {
	b := make([]byte, len(vec)*4)
	for i, f := range vec {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeVec is the inverse of encodeVec. A BLOB whose length is not a
// multiple of 4 yields nil (corrupt row); callers skip nil vectors.
func decodeVec(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

// UpsertEmbedding persists one node's embedding, replacing any prior
// vector for that node ID.
func (s *Store) UpsertEmbedding(nodeID string, vec []float32) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.execActiveWriteLocked(context.Background(),
		`INSERT OR REPLACE INTO vectors (view_gen, node_id, repo_prefix, parent_id, dims, vec) VALUES (?, ?, ?, '', ?, ?)`,
		s.viewGen, nodeID, graph.RepoPrefixOfID(nodeID), len(vec), encodeVec(vec),
	)
	return err
}

// BulkUpsertEmbeddings persists many embeddings in a single transaction,
// chunked under SQLite's host-parameter limit. Idempotent on NodeID.
// Empty input is a no-op.
func (s *Store) BulkUpsertEmbeddings(items []graph.VectorItem) error {
	if len(items) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(items); start += vectorChunk {
		end := start + vectorChunk
		if end > len(items) {
			end = len(items)
		}
		batch := items[start:end]

		args := make([]any, 0, len(batch)*5)
		stmt := make([]byte, 0, 96+len(batch)*20)
		stmt = append(stmt, "INSERT OR REPLACE INTO vectors (view_gen, node_id, repo_prefix, parent_id, dims, vec) VALUES "...)
		for i, it := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, '', ?, ?)"...)
			args = append(args, s.viewGen, it.NodeID, graph.RepoPrefixOfID(it.NodeID), len(it.Vec), encodeVec(it.Vec))
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// BuildVectorIndex finalises the vector index. Because SimilarTo scores
// over the `vectors` table directly there is no separate structure to
// populate; this validates the declared width is positive and is
// otherwise a no-op (idempotent, safe to call repeatedly).
func (s *Store) BuildVectorIndex(dims int) error {
	if dims < 0 {
		return errInvalidDims
	}
	return nil
}

// GetEmbeddings reads back the stored vectors for an explicit set of
// node IDs in one batched scan. It does not rank — it is the read side
// of the post-rerank cosine-refinement stage, which needs the raw
// vectors (not a distance) to score against a freshly embedded query.
//
// IDs with no stored vector (or a corrupt BLOB) are simply absent from
// the returned map; empty input yields an empty map. A query error
// yields whatever was decoded so far rather than failing the caller —
// the refinement stage treats a thin result as "score what we have",
// never as a hard error, so a transient read can never regress search.
// The IN-list is chunked under SQLite's host-parameter limit.
func (s *Store) GetEmbeddings(ids []string) map[string][]float32 {
	out := make(map[string][]float32, len(ids))
	if len(ids) == 0 {
		return out
	}

	for start := 0; start < len(ids); start += vectorChunk {
		end := start + vectorChunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]

		stmt := make([]byte, 0, 48+len(batch)*2)
		stmt = append(stmt, "SELECT node_id, vec FROM vectors WHERE view_gen = ? AND node_id IN ("...)
		args := make([]any, 0, len(batch)+1)
		args = append(args, s.viewGen)
		for i, id := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, id)
		}
		stmt = append(stmt, ')')

		rows, err := s.db.Query(string(stmt), args...)
		if err != nil {
			// Return what we have; the refinement stage is best-effort.
			return out
		}
		for rows.Next() {
			var (
				id   string
				blob sql.RawBytes
			)
			if err := rows.Scan(&id, &blob); err != nil {
				_ = rows.Close()
				return out
			}
			if vec := decodeVec(blob); vec != nil {
				out[id] = vec
			}
		}
		// Best-effort by construction (see the Query error above): a short
		// batch costs recall in the refinement stage, never correctness.
		_ = rows.Err()
		_ = rows.Close()
	}

	return out
}

// SimilarTo returns up to `limit` stored vectors closest to the query
// under cosine distance, ordered by ascending distance (most similar
// first). Vectors whose length differs from the query are skipped — a
// dimension mismatch can't be meaningfully scored.
func (s *Store) SimilarTo(vec []float32, limit int) ([]graph.VectorHit, error) {
	if limit <= 0 || len(vec) == 0 {
		return nil, nil
	}

	qNorm := norm(vec)
	if qNorm == 0 {
		return nil, nil
	}

	rows, err := s.db.Query(`SELECT node_id, parent_id, vec FROM vectors WHERE view_gen = ? AND dims = ?`, s.viewGen, len(vec))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Max-heap keyed on distance: the root is the *worst* kept hit, so a
	// candidate better than the root evicts it. This keeps the heap at
	// `limit` and yields an exact top-k.
	h := &hitHeap{}
	for rows.Next() {
		var id, parentID string
		var blob sql.RawBytes
		if err := rows.Scan(&id, &parentID, &blob); err != nil {
			return nil, err
		}
		cand := decodeVec(blob)
		if len(cand) != len(vec) {
			continue
		}
		cNorm := norm(cand)
		if cNorm == 0 {
			continue
		}
		dist := cosineDistance(vec, cand, qNorm, cNorm)

		if h.Len() < limit {
			heap.Push(h, graph.VectorHit{NodeID: id, ParentID: parentID, Distance: dist})
		} else if dist < (*h)[0].Distance {
			(*h)[0] = graph.VectorHit{NodeID: id, ParentID: parentID, Distance: dist}
			heap.Fix(h, 0)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Drain the max-heap (largest distance first) then reverse so the
	// result is ascending by distance (most similar first).
	out := make([]graph.VectorHit, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(h).(graph.VectorHit)
	}
	return out, nil
}

// norm returns the Euclidean norm (L2) of v as a float64.
func norm(v []float32) float64 {
	var sum float64
	for _, f := range v {
		d := float64(f)
		sum += d * d
	}
	return math.Sqrt(sum)
}

// cosineDistance returns 1 - cosine_similarity(a, b), given precomputed
// norms. Lower = more similar; identical direction → ~0, orthogonal → 1,
// opposite → 2. a and b are assumed equal length and non-zero norm.
func cosineDistance(a, b []float32, aNorm, bNorm float64) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	sim := dot / (aNorm * bNorm)
	// Guard against tiny floating-point overshoot past ±1.
	if sim > 1 {
		sim = 1
	} else if sim < -1 {
		sim = -1
	}
	return 1 - sim
}

// hitHeap is a max-heap of VectorHit ordered by Distance: Less reports
// the *larger* distance as "less" so the root is the worst-kept hit.
type hitHeap []graph.VectorHit

func (h hitHeap) Len() int           { return len(h) }
func (h hitHeap) Less(i, j int) bool { return h[i].Distance > h[j].Distance }
func (h hitHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *hitHeap) Push(x any)        { *h = append(*h, x.(graph.VectorHit)) }
func (h *hitHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}
