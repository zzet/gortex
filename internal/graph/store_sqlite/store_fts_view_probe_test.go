package store_sqlite

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProbeSymbolFTSViewGenerationRowsCost times one MATCH over a view's
// generations against one MATCH per generation on a store copy. An operator
// probe: skipped unless GORTEX_FTS_VIEW_PROBE_STORE names a private copy;
// GORTEX_FTS_VIEW_PROBE_GENS the generations, GORTEX_FTS_VIEW_PROBE_TERMS the
// prefix terms.
func TestProbeSymbolFTSViewGenerationRowsCost(t *testing.T) {
	path := os.Getenv("GORTEX_FTS_VIEW_PROBE_STORE")
	if path == "" {
		t.Skip("set GORTEX_FTS_VIEW_PROBE_STORE to a private store copy")
	}
	t.Setenv("GORTEX_SQLITE_ROW_COUNTERS", "0")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	var gens []int64
	for _, g := range splitProbeList(os.Getenv("GORTEX_FTS_VIEW_PROBE_GENS")) {
		v, err := strconv.ParseInt(g, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		gens = append(gens, v)
	}
	for _, term := range splitProbeList(os.Getenv("GORTEX_FTS_VIEW_PROBE_TERMS")) {
		match := `tokens : "` + strings.ReplaceAll(term, `"`, `""`) + `" *`
		for rep := 0; rep < 2; rep++ {
			start := time.Now()
			rows, over, err := s.SymbolFTSViewGenerationRows(ctx, gens, match, 50000)
			if err != nil {
				t.Fatal(err)
			}
			one := time.Since(start)
			n := 0
			for _, r := range rows {
				n += len(r)
			}
			start = time.Now()
			m := 0
			for _, g := range gens {
				var after int64
				for {
					page, next, err := s.SymbolFTSGenerationRows(ctx, g, match, after, 1000)
					if err != nil {
						t.Fatal(err)
					}
					m += len(page)
					if next == 0 || m > 200000 {
						break
					}
					after = next
				}
			}
			per := time.Since(start)
			var total int64
			noteStart := time.Now()
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, match).Scan(&total); err != nil {
				t.Fatal(err)
			}
			t.Logf("PROBE term=%q rep=%d table_matches=%d (count %s) view_rows=%d over=%v one_match=%s per_generation=%s rows=%d",
				term, rep, total, time.Since(noteStart).Round(time.Millisecond), n, over, one.Round(time.Millisecond), per.Round(time.Millisecond), m)
		}
	}
}

// splitProbeList splits a comma-separated list and drops empty items.
func splitProbeList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
