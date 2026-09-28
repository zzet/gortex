package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// The in-memory symbol-FTS ranker (search/fts_rank.go) reads the store through
// search.ViewFTSRankSource. The store package imports the search package, so
// the store cannot name that interface and the search package cannot name the
// store; this adapter, in a package that imports both, maps one onto the
// other. It is installed once for the process (init below).
//
// Mapping of the store's calls (store_sqlite/store_fts_stats.go):
//
//   - SymbolFTSStats: the same values; only the type differs.
//   - SymbolFTSPrefixHits(ctx, -1, prefixes): the whole table's document
//     frequency of `tokens : "p" *`, which for the one indexed column is the
//     nHit of the phrase `"p"*`. The store returns no stamp with the counts, so
//     the adapter brackets them between two statistics reads and reports the
//     stamp only when both agree; otherwise the ranker retries once, then
//     declines.
//   - SymbolFTSGenerationRows(ctx, gen, match, after, limit): paged; the
//     adapter reads every page (up to one row past the ranker's cap) and leaves
//     the repository filter to the ranker, which applies the store's rule.
//   - SymbolExactHitsViewGenerations and SymbolSearchCoreKey are the store's
//     own. A handle without a core (SymbolSearchCoreKey nil) reports "no
//     source" and keeps the store's FTS5 query.

func init() {
	search.SetFTSRankSourceAdapter(adaptStoreFTSRankSource)
}

// storeExactTier is the store's exact-name tier, split from its full-text
// tier.
type storeExactTier interface {
	SymbolExactHitsViewGenerations(ctx context.Context, query string, repoPrefixes []string, viewGens []int64, limit int) (map[int64][]graph.SymbolHit, []int64, error)
}

// storeSearchCoreKeyer names the store core a ranker's kept match lists
// belong to.
type storeSearchCoreKeyer interface {
	SymbolSearchCoreKey() any
}

type storeFTSRankSource struct {
	s     *store_sqlite.Store
	exact storeExactTier
	key   any
}

func adaptStoreFTSRankSource(searcher any) (search.ViewFTSRankSource, bool) {
	s, ok := searcher.(*store_sqlite.Store)
	if !ok || s == nil {
		return nil, false
	}
	exact, ok := searcher.(storeExactTier)
	if !ok {
		return nil, false
	}
	var key any
	if k, ok := searcher.(storeSearchCoreKeyer); ok {
		key = k.SymbolSearchCoreKey()
	}
	if key == nil {
		return nil, false
	}
	base := &storeFTSRankSource{s: s, exact: exact, key: key}
	if view, ok := searcher.(storeViewMatch); ok {
		return &storeFTSViewMatchSource{storeFTSRankSource: base, view: view}, true
	}
	return base, true
}

// storeViewMatch is the store's one-MATCH read over several generations
// (the store's side of the contract; the ranker's side is
// search.FTSViewMatchSource). Until the store serves it, every generation that
// needs a MATCH is read with its own, as before.
type storeViewMatch interface {
	SymbolFTSViewGenerationRows(ctx context.Context, generations []int64, match string, perGeneration int) (map[int64][]store_sqlite.SymbolFTSRow, map[int64]bool, error)
}

// storeFTSViewMatchSource is the adapter over a store that serves the
// one-MATCH read.
type storeFTSViewMatchSource struct {
	*storeFTSRankSource
	view storeViewMatch
}

func (a *storeFTSViewMatchSource) SymbolFTSViewGenerationRows(ctx context.Context, generations []int64, match string, perGeneration int) (map[int64][]search.FTSRankRow, map[int64]bool, error) {
	rows, over, err := a.view.SymbolFTSViewGenerationRows(ctx, generations, match, perGeneration)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[int64][]search.FTSRankRow, len(rows))
	for generation, generationRows := range rows {
		converted := make([]search.FTSRankRow, len(generationRows))
		for i, r := range generationRows {
			converted[i] = search.FTSRankRow{Rowid: r.RowID, NodeID: r.NodeID, RepoPrefix: r.RepoPrefix, Tokens: r.Tokens, TokenCount: r.TokenCount}
		}
		out[generation] = converted
	}
	return out, over, nil
}

func (a *storeFTSRankSource) SymbolFTSStats(ctx context.Context) (search.FTSRankStats, error) {
	st, err := a.s.SymbolFTSStats(ctx)
	if err != nil {
		return search.FTSRankStats{}, err
	}
	return search.FTSRankStats{Rows: st.Rows, Tokens: st.Tokens, Stamp: st.Stamp}, nil
}

func (a *storeFTSRankSource) SymbolFTSPrefixHits(ctx context.Context, terms []string) (map[string]int64, string, error) {
	before, err := a.s.SymbolFTSStats(ctx)
	if err != nil {
		return nil, "", err
	}
	hits, err := a.s.SymbolFTSPrefixHits(ctx, -1, terms)
	if err != nil {
		return nil, "", err
	}
	after, err := a.s.SymbolFTSStats(ctx)
	if err != nil {
		return nil, "", err
	}
	if before.Stamp != after.Stamp {
		return hits, "", nil
	}
	return hits, after.Stamp, nil
}

// storeFTSRowsCap stops a generation read one row past what the ranker keeps.
const storeFTSRowsCap = 50_001

// storeFTSRowsPage is the page size of a generation read: the whole capped
// read in one statement, because every page of a MATCH read re-runs the FTS5
// query and so re-walks its prefix doclists over the whole table. A variable
// so a test can make every read span pages.
var storeFTSRowsPage = storeFTSRowsCap

func (a *storeFTSRankSource) SymbolFTSGenerationRows(ctx context.Context, generation int64, match string, _ []string) ([]search.FTSRankRow, error) {
	var out []search.FTSRankRow
	after := int64(0)
	for {
		rows, next, err := a.s.SymbolFTSGenerationRows(ctx, generation, match, after, storeFTSRowsPage)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, search.FTSRankRow{Rowid: r.RowID, NodeID: r.NodeID, RepoPrefix: r.RepoPrefix, Tokens: r.Tokens, TokenCount: r.TokenCount})
		}
		if next == 0 || len(out) >= storeFTSRowsCap {
			return out, nil
		}
		after = next
	}
}

// SymbolFTSGenerationRun is a generation's document count and first and last
// rowid, from the store's generation statistics (memoized for a sealed
// generation).
func (a *storeFTSRankSource) SymbolFTSGenerationRun(ctx context.Context, generation int64) (search.FTSGenerationRun, error) {
	st, err := a.s.SymbolFTSGenerationStats(ctx, generation)
	if err != nil {
		return search.FTSGenerationRun{}, err
	}
	run := search.FTSGenerationRun{Docs: st.Rows, DocsKnown: true}
	if st.Rows > 0 && st.Hi >= st.Lo {
		run.Lo, run.Hi, run.Known = st.Lo, st.Hi, true
	}
	return run, nil
}

// SymbolFTSTokenize is the store's FTS5 tokenizer, for rows the ranker cannot
// tokenize itself.
func (a *storeFTSRankSource) SymbolFTSTokenize(ctx context.Context, texts []string) ([][]string, error) {
	return store_sqlite.SymbolFTSTokenize(ctx, texts)
}

func (a *storeFTSRankSource) SymbolExactHitsViewGenerations(ctx context.Context, query string, repoPrefixes []string, viewGens []int64, limit int) (map[int64][]graph.SymbolHit, []int64, error) {
	return a.exact.SymbolExactHitsViewGenerations(ctx, query, repoPrefixes, viewGens, limit)
}

func (a *storeFTSRankSource) SymbolSearchCoreKey() any { return a.key }
