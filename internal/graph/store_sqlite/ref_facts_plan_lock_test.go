package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// refFactFixtureGenerations are the generations the ref-fact fixture writes:
// the base and one derived generation, so a read or delete that leaks across
// generations shows.
var refFactFixtureGenerations = []int64{0, 5}

// writeRefFactFixture writes, per generation, 60 files of 12 facts each for
// two repositories. Targets repeat across files, so a by-target read spans
// several files.
func writeRefFactFixture(t *testing.T, s *Store) {
	t.Helper()
	for _, generation := range refFactFixtureGenerations {
		for _, repo := range []string{"repo", "other"} {
			var facts []graph.RefFact
			for f := 0; f < 60; f++ {
				file := fmt.Sprintf("%s/pkg/f%02d.go", repo, f)
				for n := 0; n < 12; n++ {
					facts = append(facts, graph.RefFact{
						FromID:     fmt.Sprintf("%s::Fn%d", file, n%4),
						ToID:       fmt.Sprintf("%s/pkg/f%02d.go::T%d", repo, (f+n)%60, n%3),
						Kind:       []string{"calls", "references"}[n%2],
						RefName:    fmt.Sprintf("T%d", n%3),
						Line:       n + 1 + int(generation),
						Origin:     "ast_resolved",
						Tier:       "ast",
						FilePath:   file,
						Lang:       "go",
						Candidates: []string{fmt.Sprintf("c%d", n)},
					})
				}
			}
			if err := s.AtGeneration(generation).BulkSetRefFacts(repo, facts); err != nil {
				t.Fatalf("write ref facts generation %d repo %s: %v", generation, repo, err)
			}
		}
	}
}

// legacyRefFacts is the reference reader: the plain `IN (…)` form the
// file- and target-driven joins replaced, one query per chunk. Its rows and
// their order are what callers saw before.
func legacyRefFacts(t *testing.T, s *Store, repo, column string, keys []string) []graph.RefFact {
	t.Helper()
	var out []graph.RefFact
	for start := 0; start < len(keys); start += refFactChunk {
		end := min(start+refFactChunk, len(keys))
		chunk := keys[start:end]
		args := []any{s.viewGen, repo}
		for _, key := range chunk {
			args = append(args, key)
		}
		rows, err := s.db.Query(`SELECT from_id, to_id, kind, ref_name, line, origin, tier, candidates, file_path, lang
FROM ref_facts WHERE view_gen = ? AND repo_prefix = ? AND `+column+` IN (`+inPlaceholders(len(chunk))+`)
ORDER BY from_id, to_id, kind, line`, args...)
		if err != nil {
			t.Fatalf("legacy read: %v", err)
		}
		for rows.Next() {
			var f graph.RefFact
			var cand string
			if err := rows.Scan(&f.FromID, &f.ToID, &f.Kind, &f.RefName, &f.Line, &f.Origin, &f.Tier, &cand, &f.FilePath, &f.Lang); err != nil {
				t.Fatalf("legacy scan: %v", err)
			}
			f.RepoPrefix = repo
			f.Candidates = decodeCandidates(cand)
			out = append(out, f)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("legacy rows: %v", err)
		}
		_ = rows.Close()
	}
	return out
}

// TestRefFactReadsMatchTheLegacyInLists pins that the file- and
// target-driven reads return exactly the rows, in exactly the order, of the
// `IN (…)` form they replaced — duplicates, missing keys, a chunk boundary and
// both generations included — and that the per-file delete removes exactly
// the named files' facts of its own generation.
func TestRefFactReadsMatchTheLegacyInLists(t *testing.T) {
	s, _ := openTempStore(t)
	writeRefFactFixture(t, s)
	var files, targets []string
	for f := 0; f < 90; f++ { // 90 > refFactChunk: two chunks
		files = append(files, fmt.Sprintf("repo/pkg/f%02d.go", f%70)) // f60..f69 are absent; f00..f19 repeat
		targets = append(targets, fmt.Sprintf("repo/pkg/f%02d.go::T%d", f%70, f%4))
	}
	files = append(files, "repo/pkg/f03.go", "")
	for _, generation := range refFactFixtureGenerations {
		h := s.AtGeneration(generation)
		got, err := h.LoadRefFactsByFiles("repo", files)
		if err != nil {
			t.Fatalf("generation %d: by files: %v", generation, err)
		}
		// The legacy IN list deduplicated within a chunk only; compare chunk
		// by chunk exactly as it read.
		want := legacyRefFacts(t, h, "repo", "file_path", files)
		if len(want) == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("generation %d: by files: %d rows, legacy %d rows (equal=%v)", generation, len(got), len(want), reflect.DeepEqual(got, want))
		}
		byTarget, err := h.LoadRefFactsByTargets("repo", targets)
		if err != nil {
			t.Fatalf("generation %d: by targets: %v", generation, err)
		}
		legacy := map[string][]graph.RefFact{}
		for _, f := range legacyRefFacts(t, h, "repo", "to_id", targets) {
			legacy[f.FilePath] = append(legacy[f.FilePath], f)
		}
		if len(legacy) == 0 || !reflect.DeepEqual(byTarget, legacy) {
			t.Fatalf("generation %d: by targets: %d files, legacy %d files", generation, len(byTarget), len(legacy))
		}
	}

	// Delete two files on generation 5; generation 0 and the other
	// repository keep every row.
	before0, _ := s.AtGeneration(0).LoadRefFactsByFiles("repo", nil)
	beforeOther, _ := s.AtGeneration(5).LoadRefFactsByFiles("other", nil)
	before5, _ := s.AtGeneration(5).LoadRefFactsByFiles("repo", nil)
	if err := s.AtGeneration(5).DeleteRefFactsByFiles("repo", []string{"repo/pkg/f01.go", "repo/pkg/f07.go"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after5, _ := s.AtGeneration(5).LoadRefFactsByFiles("repo", nil)
	if len(after5) != len(before5)-24 {
		t.Fatalf("delete removed %d rows, want 24", len(before5)-len(after5))
	}
	for _, f := range after5 {
		if f.FilePath == "repo/pkg/f01.go" || f.FilePath == "repo/pkg/f07.go" {
			t.Fatalf("deleted file still has fact %+v", f)
		}
	}
	after0, _ := s.AtGeneration(0).LoadRefFactsByFiles("repo", nil)
	afterOther, _ := s.AtGeneration(5).LoadRefFactsByFiles("other", nil)
	if !reflect.DeepEqual(before0, after0) || !reflect.DeepEqual(beforeOther, afterOther) {
		t.Fatalf("delete leaked outside generation 5 / repo")
	}
}

// TestRefFactPlansLockedAcrossStatisticsRegimes locks the ref-fact reads and
// the per-file delete to their per-key index seeks under three statistics
// regimes: none, the live store's rows (sqlite_stat1 carries nodes/edges rows
// and none for ref_facts — the regime in which the plain IN list took the
// primary key's (view_gen, repo_prefix) prefix and visited every fact of the
// repository), and an honest ANALYZE of the fixture.
func TestRefFactPlansLockedAcrossStatisticsRegimes(t *testing.T) {
	s, _ := openTempStore(t)
	writeRefFactFixture(t, s)
	assertLocked := func(t *testing.T, ctx context.Context, conn *sql.Conn) {
		t.Helper()
		badPrefix := "PRIMARY KEY (view_gen=? AND repo_prefix=?)"
		for _, generation := range refFactFixtureGenerations {
			files := `["repo/pkg/f01.go","repo/pkg/f02.go"]`
			plan := strings.Join(explainOnConn(t, ctx, conn, refFactsByFilesSQL(true), files, generation, "repo"), "\n")
			if !strings.Contains(plan, "SCAN f") || !strings.Contains(plan, "INDEX ref_facts_by_file (view_gen=? AND repo_prefix=? AND file_path=?)") || strings.Contains(plan, badPrefix) {
				t.Errorf("generation %d: by-files plan is not a per-file seek:\n%s", generation, plan)
			}
			targets := `["repo/pkg/f01.go::T1","repo/pkg/f02.go::T2"]`
			plan = strings.Join(explainOnConn(t, ctx, conn, refFactsByTargetsSQL(true), targets, generation, "repo"), "\n")
			if !strings.Contains(plan, "SCAN t") || !strings.Contains(plan, "INDEX ref_facts_by_target (view_gen=? AND repo_prefix=? AND to_id=?)") || strings.Contains(plan, badPrefix) {
				t.Errorf("generation %d: by-targets plan is not a per-target seek:\n%s", generation, plan)
			}
			del := refFactsDeleteByFilesPrefix(true) + inPlaceholders(2) + ")"
			plan = strings.Join(explainOnConn(t, ctx, conn, del, generation, "repo", "repo/pkg/f01.go", "repo/pkg/f02.go"), "\n")
			if !strings.Contains(plan, "INDEX ref_facts_by_file (view_gen=? AND repo_prefix=? AND file_path=?)") || strings.Contains(plan, badPrefix) {
				t.Errorf("generation %d: delete plan is not a per-file seek:\n%s", generation, plan)
			}
		}
	}
	t.Run("no_stats", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			assertLocked(t, ctx, conn)
		})
	})
	t.Run("live_store_rows", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("create sqlite_stat1: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
				t.Fatalf("clear stat rows: %v", err)
			}
			for _, row := range receiverMutationCallStatRows {
				if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES (?, ?, ?)`, row[0], row[1], row[2]); err != nil {
					t.Fatalf("insert stat row %v: %v", row, err)
				}
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE sqlite_schema`); err != nil {
				t.Fatalf("reload statistics: %v", err)
			}
			assertLocked(t, ctx, conn)
		})
	})
	t.Run("refreshed", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
				t.Fatalf("clear stat rows: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("refresh statistics: %v", err)
			}
			assertLocked(t, ctx, conn)
		})
	})
}
