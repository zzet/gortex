package store_sqlite

import (
	"math"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type explicitFTSRankRow struct {
	nodeID string
	score  float64
}

func seedFTSRankGeneration(t testing.TB, store *Store, generation int64, tag string) {
	t.Helper()
	scoped := store.AtGeneration(generation)
	type document struct {
		id         string
		repoPrefix string
		tokens     string
	}
	documents := []document{
		{id: tag + "-wanted-strong", repoPrefix: "wanted", tokens: "alpha alpha alpha alpha"},
		{id: tag + "-wanted-tie-a", repoPrefix: "wanted", tokens: "alpha beta"},
		{id: tag + "-wanted-tie-b", repoPrefix: "wanted", tokens: "alpha beta"},
		{id: tag + "-unowned-tie", repoPrefix: "", tokens: "alpha beta"},
		{id: tag + "-other-strong", repoPrefix: "other", tokens: "alpha alpha alpha alpha alpha"},
	}
	nodes := make([]*graph.Node, 0, len(documents))
	items := make([]graph.SymbolFTSItem, 0, len(documents))
	for _, document := range documents {
		nodes = append(nodes, &graph.Node{
			ID:         document.id,
			Kind:       graph.NodeKind("function"),
			Name:       "RankFixture" + document.id,
			FilePath:   tag + "/rank.go",
			Language:   "go",
			RepoPrefix: document.repoPrefix,
		})
		items = append(items, graph.SymbolFTSItem{NodeID: document.id, Tokens: document.tokens})
	}
	if err := scoped.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	if err := scoped.BatchUpsertSymbolFTS(items); err != nil {
		t.Fatal(err)
	}
}

func explicitBM25Ranks(t testing.TB, store *Store, query string, repoAllow []string) []explicitFTSRankRow {
	t.Helper()
	q := `SELECT symbol_fts.node_id, bm25(symbol_fts)
FROM symbol_fts
JOIN symbol_fts_rowid
  ON symbol_fts_rowid.fts_rowid = symbol_fts.rowid
 AND symbol_fts_rowid.view_gen = ?
WHERE symbol_fts MATCH ?`
	args := []any{store.viewGen, query}
	if len(repoAllow) > 0 {
		q += ` AND symbol_fts.repo_prefix IN (` + inPlaceholders(len(repoAllow)+1) + `)`
		args = append(args, "")
		for _, repoPrefix := range repoAllow {
			args = append(args, repoPrefix)
		}
	}
	q += ` ORDER BY bm25(symbol_fts)`
	rows, err := store.db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ranked []explicitFTSRankRow
	for rows.Next() {
		var row explicitFTSRankRow
		if err := rows.Scan(&row.nodeID, &row.score); err != nil {
			t.Fatal(err)
		}
		ranked = append(ranked, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ranked
}

func closeFTSRankScore(a, b float64) bool {
	return math.Abs(a-b) <= 1e-12*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func requireProductionFTSRanksMatchBM25(t testing.TB, store *Store, query string, repoAllow []string, limit int) []float64 {
	t.Helper()
	baseline := explicitBM25Ranks(t, store, query, repoAllow)
	if len(baseline) < limit {
		t.Fatalf("explicit BM25 returned %d rows, want at least %d", len(baseline), limit)
	}
	hits, err := store.SearchSymbolsRepoScoped(query, repoAllow, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != limit {
		t.Fatalf("production search returned %d hits, want %d", len(hits), limit)
	}
	baselineByID := make(map[string]float64, len(baseline))
	for _, row := range baseline {
		baselineByID[row.nodeID] = -row.score
	}
	seen := make(map[string]bool, len(hits))
	scores := make([]float64, len(hits))
	for i, hit := range hits {
		want, ok := baselineByID[hit.NodeID]
		if !ok {
			t.Fatalf("production returned ineligible node %q", hit.NodeID)
		}
		if !closeFTSRankScore(hit.Score, want) {
			t.Fatalf("score for %q = %.17g, explicit BM25 = %.17g", hit.NodeID, hit.Score, want)
		}
		if i > 0 && hit.Score > hits[i-1].Score && !closeFTSRankScore(hit.Score, hits[i-1].Score) {
			t.Fatalf("production scores are not descending: %v", hits)
		}
		seen[hit.NodeID] = true
		scores[i] = hit.Score
	}
	cutoff := -baseline[limit-1].score
	for _, row := range baseline {
		score := -row.score
		if score > cutoff && !closeFTSRankScore(score, cutoff) && !seen[row.nodeID] {
			t.Fatalf("production omitted above-cutoff node %q (score %.17g, cutoff %.17g)", row.nodeID, score, cutoff)
		}
	}
	return scores
}

func requireFTSRankScope(t testing.TB, store *Store, tag string) {
	t.Helper()
	hits, err := store.SearchSymbolsRepoScoped("alpha", []string{"wanted"}, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 4 {
		t.Fatalf("scoped search returned %d hits, want 4: %v", len(hits), hits)
	}
	seen := make(map[string]bool, len(hits))
	for _, hit := range hits {
		seen[hit.NodeID] = true
		if hit.NodeID == tag+"-other-strong" {
			t.Fatalf("scoped search included other repository: %v", hits)
		}
	}
	for _, suffix := range []string{"-wanted-strong", "-wanted-tie-a", "-wanted-tie-b", "-unowned-tie"} {
		if !seen[tag+suffix] {
			t.Errorf("scoped search omitted %q: %v", tag+suffix, hits)
		}
	}
}

func TestSearchSymbolsPinsDefaultBM25Rank(t *testing.T) {
	store, _ := openTempStore(t)
	seedFTSRankGeneration(t, store, 0, "g0")
	seedFTSRankGeneration(t, store, 3, "g3")

	stores := []struct {
		name  string
		store *Store
		tag   string
	}{
		{name: "generation_0", store: store.AtGeneration(0), tag: "g0"},
		{name: "generation_3", store: store.AtGeneration(3), tag: "g3"},
	}
	before := make(map[string][]float64, len(stores))
	for _, fixture := range stores {
		t.Run(fixture.name+"/default_rank", func(t *testing.T) {
			requireFTSRankScope(t, fixture.store, fixture.tag)
			before[fixture.name] = requireProductionFTSRanksMatchBM25(t, fixture.store, "alpha", []string{"wanted"}, 3)
		})
	}

	if _, err := store.writerDB.Exec(`INSERT INTO symbol_fts(symbol_fts, rank) VALUES('rank', 'bm25(0.0, 0.0, 0.0)')`); err != nil {
		t.Fatal(err)
	}
	var customRank, explicitBM25 float64
	if err := store.db.QueryRow(`SELECT symbol_fts.rank, bm25(symbol_fts)
FROM symbol_fts
JOIN symbol_fts_rowid
  ON symbol_fts_rowid.fts_rowid = symbol_fts.rowid
 AND symbol_fts_rowid.view_gen = ?
WHERE symbol_fts MATCH ?
LIMIT 1`, int64(0), "alpha").Scan(&customRank, &explicitBM25); err != nil {
		t.Fatal(err)
	}
	if closeFTSRankScore(customRank, explicitBM25) {
		t.Fatalf("custom rank fixture is inactive: rank=%.17g bm25=%.17g", customRank, explicitBM25)
	}

	for _, fixture := range stores {
		t.Run(fixture.name+"/custom_rank_configured", func(t *testing.T) {
			requireFTSRankScope(t, fixture.store, fixture.tag)
			after := requireProductionFTSRanksMatchBM25(t, fixture.store, "alpha", []string{"wanted"}, 3)
			want := before[fixture.name]
			for i := range want {
				if !closeFTSRankScore(after[i], want[i]) {
					t.Fatalf("score[%d] changed under persistent custom rank: before=%.17g after=%.17g", i, want[i], after[i])
				}
			}
		})
	}
}
