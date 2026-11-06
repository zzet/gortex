//go:build performance

package store_sqlite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"
)

// Use a stopped disposable daemon's database (or a SQLite backup), never a
// live database copied with filesystem tools. This opens it read-only, without
// invoking Open's migrations or background maintenance.
func vectorScanFixture(tb testing.TB) (*Store, [][]float32) {
	tb.Helper()
	path := os.Getenv("GORTEX_VECTOR_FIXTURE")
	if path == "" {
		tb.Skip("set GORTEX_VECTOR_FIXTURE to a disposable local-embedding database")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&" + sqliteBusyPragma + "&" + sqlitePerConnectionPragmas() + "&_pragma=query_only(1)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	var cacheSize, mmapSize int64
	if err := db.QueryRow("PRAGMA cache_size").Scan(&cacheSize); err != nil {
		tb.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA mmap_size").Scan(&mmapSize); err != nil {
		tb.Fatal(err)
	}
	if cacheSize != -32768 || mmapSize != sqliteMmapBytes() {
		tb.Fatalf("unexpected connection settings: cache_size=%d mmap_size=%d", cacheSize, mmapSize)
	}
	tb.Logf("production connection settings: cache_size=%d mmap_size=%d", cacheSize, mmapSize)
	var gen int64
	var dims, count int
	err = db.QueryRow(`SELECT view_gen, dims, count(*) FROM vectors GROUP BY view_gen, dims ORDER BY count(*) DESC LIMIT 1`).Scan(&gen, &dims, &count)
	if err != nil {
		tb.Fatal(err)
	}
	if dims != 384 || count < 100 {
		tb.Fatalf("need real local vectors: gen=%d dims=%d count=%d", gen, dims, count)
	}
	rows, err := db.Query(`SELECT vec FROM vectors WHERE view_gen=? AND dims=? ORDER BY node_id LIMIT 32`, gen, dims)
	if err != nil {
		tb.Fatal(err)
	}
	var queries [][]float32
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			tb.Fatal(err)
		}
		queries = append(queries, decodeVec(blob))
	}
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	_ = rows.Close()
	if queryPath := os.Getenv("GORTEX_VECTOR_QUERIES"); queryPath != "" {
		data, err := os.ReadFile(queryPath)
		if err != nil {
			tb.Fatal(err)
		}
		var extra struct {
			Vectors [][]float32 `json:"vectors"`
		}
		if err := json.Unmarshal(data, &extra); err != nil {
			tb.Fatal(err)
		}
		for _, q := range extra.Vectors {
			if len(q) != dims || norm(q) == 0 {
				tb.Fatal("invalid query embedding")
			}
			queries = append(queries, q)
		}
	}
	tb.Logf("generation=%d dimensions=%d vectors=%d queries=%d", gen, dims, count, len(queries))
	return &Store{storeCore: &storeCore{db: db}, viewGen: gen}, queries
}

// Run separately from the timing processes: integrity checks walk the database
// and would change their cache state. Package init registers the production
// path collations on this driver, including their Unicode/path identity rules.
func TestLocalVectorFixtureIntegrity(t *testing.T) {
	s, _ := vectorScanFixture(t)
	var queryOnly, folded, exact int
	if err := s.db.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Fatalf("query_only=%d err=%v", queryOnly, err)
	}
	if err := s.db.QueryRow(`SELECT
		'/Repository/A' COLLATE gortex_path_folded = '/repository/a',
		'/Repository/A' COLLATE gortex_path_exact = '/repository/a'`).Scan(&folded, &exact); err != nil || folded != 1 || exact != 0 {
		t.Fatalf("production collations: folded=%d exact=%d err=%v", folded, exact, err)
	}
	for _, pragma := range []string{"PRAGMA quick_check", "PRAGMA integrity_check"} {
		rows, err := s.db.Query(pragma)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for rows.Next() {
			var result string
			if err := rows.Scan(&result); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			count++
			if result != "ok" {
				t.Errorf("%s: %s", pragma, result)
			}
		}
		if err := rows.Err(); err != nil {
			t.Error(err)
		}
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
		if count != 1 {
			t.Errorf("%s returned %d rows, want one ok", pragma, count)
		}
		t.Logf("%s completed (%d result rows)", pragma, count)
	}
}

func vectorScanLimit(tb testing.TB) int {
	tb.Helper()
	limit := 658 // Backend limit 10 (unscoped public limit 5): 8x plus 578 chunks.
	if raw := os.Getenv("GORTEX_VECTOR_LIMIT"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			tb.Fatalf("invalid GORTEX_VECTOR_LIMIT %q", raw)
		}
		limit = n
	}
	tb.Logf("vector scan limit=%d", limit)
	return limit
}

func BenchmarkLocalVectorScan(b *testing.B) {
	s, queries := vectorScanFixture(b)
	limit := vectorScanLimit(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits, err := s.SimilarTo(queries[i%len(queries)], limit)
		if err != nil || len(hits) != limit {
			b.Fatalf("hits=%d err=%v", len(hits), err)
		}
	}
}

func TestLocalVectorScanEvidence(t *testing.T) {
	s, queries := vectorScanFixture(t)
	limit := vectorScanLimit(t)
	for _, q := range queries {
		if _, err := s.SimilarTo(q, limit); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	digest := sha256.New()
	var latencies []time.Duration
	for round := 0; round < 3; round++ {
		for _, q := range queries {
			start := time.Now()
			hits, err := s.SimilarTo(q, limit)
			latencies = append(latencies, time.Since(start))
			if err != nil || len(hits) != limit {
				t.Fatalf("hits=%d err=%v", len(hits), err)
			}
			for _, hit := range hits {
				_, _ = fmt.Fprintf(digest, "%s\x00%s\x00%016x\n", hit.NodeID, hit.ParentID, math.Float64bits(hit.Distance))
			}
		}
	}
	runtime.ReadMemStats(&after)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("queries=%d digest=%x p50_ns=%d p95_ns=%d allocated=%d mallocs=%d gc=%d", len(latencies), digest.Sum(nil), latencies[len(latencies)/2], latencies[len(latencies)*95/100], after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, after.NumGC-before.NumGC)
}
