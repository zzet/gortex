package search_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/search"
)

// snapshotTable makes the old detached observations disagree deterministically,
// while the optional capability reads real SQLite weights in one transaction.
type snapshotTable struct {
	*ftsTable
	drift                              bool
	snapshots, legacyStats, legacyHits int
	snapshotError                      error
	forcedRows                         []search.FTSRankRow
}

func (s *snapshotTable) SymbolFTSStats(ctx context.Context) (search.FTSRankStats, error) {
	s.legacyStats++
	stats, err := s.ftsTable.SymbolFTSStats(ctx)
	if s.drift {
		stats.Stamp = "detached-before"
	}
	return stats, err
}
func (s *snapshotTable) SymbolFTSPrefixHits(ctx context.Context, p []string) (map[string]int64, string, error) {
	s.legacyHits++
	hits, stamp, err := s.ftsTable.SymbolFTSPrefixHits(ctx, p)
	if s.drift {
		stamp = "detached-after"
	}
	return hits, stamp, err
}
func (s *snapshotTable) SymbolFTSScoringSnapshot(ctx context.Context, p []string) (search.FTSRankStats, map[string]int64, error) {
	s.snapshots++
	if s.snapshotError != nil {
		return search.FTSRankStats{}, nil, s.snapshotError
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return search.FTSRankStats{}, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only fixture snapshot
	var block []byte
	if err := tx.QueryRowContext(ctx, `SELECT block FROM symbol_fts_data WHERE id=1`).Scan(&block); err != nil {
		return search.FTSRankStats{}, nil, err
	}
	rows, n := varint(block)
	stats := search.FTSRankStats{Rows: int64(rows), Stamp: fmt.Sprintf("%x", block)}
	for off := n; off < len(block); {
		v, m := varint(block[off:])
		stats.Tokens += int64(v)
		off += m
	}
	hits := make(map[string]int64, len(p))
	for _, term := range p {
		var count int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, search.FTSRankMatch([]string{term})).Scan(&count); err != nil {
			return search.FTSRankStats{}, nil, err
		}
		hits[term] = count
	}
	return stats, hits, ctx.Err()
}
func (s *snapshotTable) SymbolFTSGenerationRows(ctx context.Context, g int64, match string, repos []string) ([]search.FTSRankRow, error) {
	if s.forcedRows != nil {
		return s.forcedRows, nil
	}
	return s.ftsTable.SymbolFTSGenerationRows(ctx, g, match, repos)
}
func TestFTSScoringSnapshotAvoidsDetachedStampMismatch(t *testing.T) {
	src := &snapshotTable{ftsTable: newFTSTable(t), drift: true}
	src.addGeneration(t, 1, fixtureRows("root", 60, 1))
	src.addGeneration(t, 2, fixtureRows("dirty", 12, 3))
	ranked, declined, err := search.NewFTSRanker(src).RankGenerations(context.Background(), []int64{1, 2}, "config", nil, 7)
	if err != nil || len(declined) != 0 || len(ranked) != 2 {
		t.Fatalf("ranked %v declined %v error %v", ranked, declined, err)
	}
	if src.snapshots != 1 || src.legacyStats != 0 || src.legacyHits != 0 {
		t.Fatalf("snapshot=%d detached stats/hits=%d/%d", src.snapshots, src.legacyStats, src.legacyHits)
	}
	for _, g := range []int64{1, 2} {
		if got, want := renderHits(ranked[g]), renderHits(src.storeRank(t, g, "config", nil, 7)); got != want {
			t.Fatalf("gen%d got %s want %s", g, got, want)
		}
	}
}
func TestFTSScoringSnapshotKeepsMutableAndLegacyPaths(t *testing.T) {
	src := &snapshotTable{ftsTable: newFTSTable(t), drift: true}
	src.addGeneration(t, 0, fixtureRows("base", 40, 1))
	src.addGeneration(t, 1, fixtureRows("root", 20, 2))
	_, declined, err := search.NewFTSRanker(src).RankGenerations(context.Background(), []int64{0, 1}, "config", nil, 7)
	if err != nil || len(declined) != 2 || src.snapshots != 0 || src.legacyStats != 2 || src.legacyHits != 2 {
		t.Fatalf("declines %v err %v reads snapshot/stats/hits=%d/%d/%d", declined, err, src.snapshots, src.legacyStats, src.legacyHits)
	}
	// A provider without the optional capability keeps exact existing scoring.
	legacy := src.ftsTable
	ranked, declined, err := search.NewFTSRanker(legacy).RankGenerations(context.Background(), []int64{0, 1}, "config", nil, 7)
	if err != nil || len(declined) != 0 {
		t.Fatalf("legacy declines %v error %v", declined, err)
	}
	for _, g := range []int64{0, 1} {
		if got, want := renderHits(ranked[g]), renderHits(legacy.storeRank(t, g, "config", nil, 7)); got != want {
			t.Fatalf("gen%d got %s want %s", g, got, want)
		}
	}
}
func TestFTSScoringSnapshotRanksLikeSQLiteThroughGlobalChanges(t *testing.T) {
	src := &snapshotTable{ftsTable: newFTSTable(t)}
	src.addGeneration(t, 0, fixtureRows("base", 120, 7))
	src.addGeneration(t, 10, fixtureRows("root", 300, 1))
	src.addGeneration(t, 11, fixtureRows("dirty", 40, 4))
	ranker := search.NewFTSRanker(src)
	check := func() {
		t.Helper()
		for _, query := range []string{"config", "checkout route", "load config", "co", "rule"} {
			for _, repos := range [][]string{nil, {"alpha"}, {"beta"}} {
				for _, limit := range []int{1, 7, 200} {
					ranked, declined, err := ranker.RankGenerations(context.Background(), []int64{10, 11}, query, repos, limit)
					if err != nil || len(declined) != 0 {
						t.Fatalf("%q decline %v err %v", query, declined, err)
					}
					for _, g := range []int64{10, 11} {
						if got, want := renderHits(ranked[g]), renderHits(src.storeRank(t, g, query, repos, limit)); got != want {
							t.Fatalf("%q gen%d repos%v limit%d got %s want %s", query, g, repos, limit, got, want)
						}
					}
				}
			}
		}
	}
	check()
	src.addGeneration(t, 12, fixtureRows("new", 30, 8))
	check()
	src.deleteRow(t, fixtureRows("new", 30, 8)[0][0])
	check()
	// Primary writes must change CURRENT global weights, even with immutable cached matches.
	src.addGeneration(t, 0, fixtureRows("primary-update", 15, 3))
	check()
	if src.legacyStats != 0 || src.legacyHits != 0 {
		t.Fatal("coherent source used detached reads")
	}
}
func TestFTSScoringSnapshotPreservesRefusalsAndErrors(t *testing.T) {
	t.Run("non_ascii_match", func(t *testing.T) {
		src := &snapshotTable{ftsTable: newFTSTable(t)}
		src.addGeneration(t, 1, [][3]string{{"unicode", "", "config café"}})
		_, declined, err := search.NewFTSRanker(src).RankGenerations(context.Background(), []int64{1}, "config", nil, 7)
		if err != nil || len(declined) != 1 {
			t.Fatalf("declined %v error %v", declined, err)
		}
	})
	t.Run("unsupported_query", func(t *testing.T) {
		src := &snapshotTable{ftsTable: newFTSTable(t)}
		src.addGeneration(t, 1, [][3]string{{"unicode", "", "config café"}})
		_, declined, err := search.NewFTSRanker(src).RankGenerations(context.Background(), []int64{1}, "café", nil, 7)
		if err != nil || len(declined) != 1 || src.snapshots != 0 {
			t.Fatalf("declined %v error %v snapshots%d", declined, err, src.snapshots)
		}
	})
	t.Run("matching_row_cap", func(t *testing.T) {
		src := &snapshotTable{ftsTable: newFTSTable(t)}
		src.addGeneration(t, 1, [][3]string{{"one", "", "config"}})
		src.forcedRows = make([]search.FTSRankRow, 50001)
		for i := range src.forcedRows {
			src.forcedRows[i] = search.FTSRankRow{Rowid: int64(i + 1), NodeID: fmt.Sprintf("row%d", i), Tokens: "config", TokenCount: 1}
		}
		ranked, declined, err := search.NewFTSRanker(src).RankGenerations(context.Background(), []int64{1}, "config", nil, 7)
		if err != nil || len(declined) != 1 || len(ranked) != 0 {
			t.Fatalf("ranked %v declined %v error %v", ranked, declined, err)
		}
	})
	t.Run("error", func(t *testing.T) {
		sentinel := errors.New("snapshot failed")
		src := &snapshotTable{ftsTable: newFTSTable(t), snapshotError: sentinel}
		src.addGeneration(t, 1, fixtureRows("root", 2, 1))
		ranked, _, err := search.NewFTSRanker(src).RankGenerations(context.Background(), []int64{1}, "config", nil, 7)
		if !errors.Is(err, sentinel) || ranked != nil {
			t.Fatalf("ranked %v error %v", ranked, err)
		}
	})
}
