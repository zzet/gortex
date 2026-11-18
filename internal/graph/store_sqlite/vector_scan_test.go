package store_sqlite

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
)

func TestSimilarToScanOwnsScratchAndSkipsMalformedRows(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "vectors.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE vectors (view_gen INTEGER, node_id TEXT, parent_id TEXT, dims INTEGER, vec BLOB NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	const dims = 384
	rng := rand.New(rand.NewSource(17))
	corpus := make(map[string][]float32)
	malformedBase := make([]float32, dims+1)
	malformedBase[0] = 1
	encoded := encodeVec(malformedBase)
	insertInvalid := func(group int) {
		t.Helper()
		// Nonzero oversized/trailing-byte rows must be rejected by the width
		// checks, not accidentally filtered by the zero-norm check.
		for i, blob := range [][]byte{[]byte{}, {1, 2, 3}, encoded[:4], encoded[:dims*4+1], encoded[:dims*4+3], encoded, make([]byte, dims*4)} {
			if _, err := db.Exec(`INSERT INTO vectors VALUES (7, ?, '', ?, ?)`, fmt.Sprintf("invalid%d/%d", group, i), dims, blob); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 40; i++ {
		if i%8 == 0 {
			insertInvalid(i) // Before the first valid row and interleaved thereafter.
		}
		v := make([]float32, dims)
		for j := range v {
			v[j] = float32(rng.NormFloat64())
			switch i {
			case 38:
				v[j] *= 1e-30
			case 39:
				v[j] *= 1e30
			}
		}
		id := fmt.Sprintf("node%02d", i)
		corpus[id] = v
		if _, err := db.Exec(`INSERT INTO vectors VALUES (7, ?, ?, ?, ?)`, id, "parent/"+id, dims, encodeVec(v)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO vectors VALUES (8, 'other-generation', '', ?, ?)`, dims, encodeVec(corpus["node00"])); err != nil {
		t.Fatal(err)
	}
	s := &Store{storeCore: &storeCore{db: db}, viewGen: 7}
	for _, tc := range []struct {
		query []float32
		limit int
	}{
		{nil, 20}, {[]float32{}, 20}, {make([]float32, dims), 20},
		{corpus["node00"], 0}, {corpus["node00"], -1}, {[]float32{1}, 20},
	} {
		hits, err := s.SimilarTo(tc.query, tc.limit)
		if err != nil || len(hits) != 0 {
			t.Fatalf("empty query result: hits=%d err=%v", len(hits), err)
		}
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 10; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			index := worker
			if worker >= 8 {
				index = worker + 30 // Tiny and large magnitudes as queries too.
			}
			query := append([]float32(nil), corpus[fmt.Sprintf("node%02d", index)]...)
			original := append([]float32(nil), query...)
			var ranked []string
			for id := range corpus {
				ranked = append(ranked, id)
			}
			distance := func(id string) float64 { return cosineDistance(query, corpus[id], norm(query), norm(corpus[id])) }
			sort.Slice(ranked, func(i, j int) bool { return distance(ranked[i]) < distance(ranked[j]) })
			for _, limit := range []int{1, 7, 40, 100} {
				hits, err := s.SimilarTo(query, limit)
				if err != nil || len(hits) != min(limit, len(corpus)) {
					t.Errorf("hits=%d err=%v", len(hits), err)
					return
				}
				for i, hit := range hits {
					if hit.NodeID != ranked[i] {
						t.Errorf("limit %d hit %d = %q, want %q", limit, i, hit.NodeID, ranked[i])
					}
					v, ok := corpus[hit.NodeID]
					if !ok {
						t.Errorf("unexpected hit %q", hit.NodeID)
						return
					}
					want := cosineDistance(query, v, norm(query), norm(v))
					if math.Float64bits(hit.Distance) != math.Float64bits(want) || hit.ParentID != "parent/"+hit.NodeID {
						t.Errorf("wrong score/parent for %q", hit.NodeID)
					}
					if i > 0 && hits[i-1].Distance > hit.Distance {
						t.Error("hits out of order")
					}
				}
				if !reflect.DeepEqual(query, original) {
					t.Error("query modified")
				}
			}
		}()
	}
	wg.Wait()
}

func TestSimilarToScanPreservesTieEviction(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ties.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE vectors (view_gen INTEGER, node_id TEXT, parent_id TEXT, dims INTEGER, vec BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, v := range [][]float32{{0, 1}, {1, 1}, {1, 1}, {1, 0}, {1, 1}} {
		if _, err := db.Exec(`INSERT INTO vectors VALUES (7, ?, '', 2, ?)`, fmt.Sprint(i), encodeVec(v)); err != nil {
			t.Fatal(err)
		}
	}
	s := &Store{storeCore: &storeCore{db: db}, viewGen: 7}
	hits, err := s.SimilarTo([]float32{1, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Pin the original heap behavior for this rowid-ordered table: an equal
	// candidate never replaces the current worst hit, including after eviction.
	if len(hits) != 2 || hits[0].NodeID != "3" || hits[1].NodeID != "1" {
		t.Fatalf("tie/eviction order changed: %v", hits)
	}
}
