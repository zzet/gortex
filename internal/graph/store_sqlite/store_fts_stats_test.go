package store_sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func ftsDocs(prefix string, n int, tokens func(i int) string) ([]*graph.Node, []graph.SymbolFTSItem) {
	var nodes []*graph.Node
	var items []graph.SymbolFTSItem
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s/f%03d.go::S%03d", payloadRepo, i, i)
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("S%03d", i), FilePath: fmt.Sprintf("%s/f%03d.go", payloadRepo, i), RepoPrefix: payloadRepo})
		items = append(items, graph.SymbolFTSItem{NodeID: id, Tokens: prefix + " " + tokens(i)})
	}
	return nodes, items
}

// The statistics, prefix counts and generation rows agree with the documents
// written, and a published generation's scoped stamp survives the publication
// of a new top generation (which moves the whole table's stamp).
func TestSymbolFTSStatsPrefixHitsAndGenerationRows(t *testing.T) {
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)
	baseNodes, baseItems := ftsDocs("base", 30, func(i int) string { return "handler" })
	store.AddBatch(baseNodes, nil)
	require.NoError(t, store.BatchUpsertSymbolFTS(baseItems))
	genNodes, genItems := ftsDocs("layer", 20, func(i int) string {
		if i%2 == 0 {
			return "handle request handler"
		}
		return "store"
	})
	handle.AddBatch(genNodes, nil)
	require.NoError(t, handle.BatchUpsertSymbolFTS(genItems))
	require.NoError(t, store.PublishPayloadGeneration(ctx, generationID, 8000))

	all, err := store.SymbolFTSStats(ctx)
	require.NoError(t, err)
	// Seeded payload rows may carry documents of their own: compare against
	// the sidecar and the Go-side token counts.
	var docs int64
	require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM symbol_fts`).Scan(&docs))
	require.Equal(t, docs, all.Rows)
	require.GreaterOrEqual(t, all.Tokens, int64(30*2+10*4+10*2))

	gen, err := store.SymbolFTSGenerationStats(ctx, generationID)
	require.NoError(t, err)
	require.Equal(t, int64(20), gen.Rows)
	require.Equal(t, int64(10*4+10*2), gen.Tokens)
	require.Contains(t, gen.Stamp, "gen:")

	hits, err := store.SymbolFTSPrefixHits(ctx, generationID, []string{"hand", "store", "base", ""})
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"hand": 10, "store": 10, "base": 0, "": 0}, hits)
	whole, err := store.SymbolFTSPrefixHits(ctx, -1, []string{"handler"})
	require.NoError(t, err)
	require.GreaterOrEqual(t, whole["handler"], int64(40))

	var got []SymbolFTSRow
	var after int64
	for {
		page, next, err := store.SymbolFTSGenerationRows(ctx, generationID, "", after, 7)
		require.NoError(t, err)
		got = append(got, page...)
		if next == 0 {
			break
		}
		after = next
	}
	require.Len(t, got, 20)
	for _, r := range got {
		want := 2
		if len(r.Tokens) > len("layer store") {
			want = 4
		}
		require.Equal(t, want, r.TokenCount, "%+v", r)
	}
	matched, _, err := store.SymbolFTSGenerationRows(ctx, generationID, `tokens : request`, 0, 0)
	require.NoError(t, err)
	require.Len(t, matched, 10)

	// A new top generation: the table's stamp moves, the lower generation's
	// scoped stamp and statistics do not.
	stampBefore := all.Stamp
	req := payloadRequest()
	req.LayerID = "layer-top"
	req.TreeOID = "tree-top"
	req.CreatedAt = 9000
	nextID, nextHandle, err := store.BeginPayloadGeneration(ctx, req)
	require.NoError(t, err)
	topNodes, topItems := ftsDocs("top", 5, func(i int) string { return "handler" })
	nextHandle.AddBatch(topNodes, nil)
	require.NoError(t, nextHandle.BatchUpsertSymbolFTS(topItems))
	require.NoError(t, store.PublishPayloadGeneration(ctx, nextID, 9001))
	after2, err := store.SymbolFTSStats(ctx)
	require.NoError(t, err)
	require.NotEqual(t, stampBefore, after2.Stamp, "the table's stamp must move with a new generation")
	gen2, err := store.SymbolFTSGenerationStats(ctx, generationID)
	require.NoError(t, err)
	require.Equal(t, gen, gen2, "the lower generation's scoped stamp moved")
}

// SQLite varints: one byte below 128, big-endian 7-bit groups above, the
// ninth byte whole.
func TestSQLiteVarintsDecode(t *testing.T) {
	require.Equal(t, []uint64{5, 300, 0}, sqliteVarints([]byte{5, 0x82, 0x2c, 0}))
	require.Equal(t, []uint64{16384}, sqliteVarints([]byte{0x81, 0x80, 0x00}))
	require.Equal(t, []uint64{^uint64(0)}, sqliteVarints([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}))
}

// With the revision triggers installed, whole-table prefix counts stay equal to
// FTS5's own count through every kind of change, and a published generation
// costs no walk of the shared index: new generations (ASCII and not), a write
// to the base, a retired generation, and a document written outside the
// ownership sidecar.
func TestSymbolFTSPrefixHitsStayExactIncrementally(t *testing.T) {
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)
	baseNodes, baseItems := ftsDocs("base", 30, func(i int) string {
		if i%3 == 0 {
			return "handler storeKeeper"
		}
		return "handle"
	})
	store.AddBatch(baseNodes, nil)
	require.NoError(t, store.BatchUpsertSymbolFTS(baseItems))
	require.NoError(t, store.EnsureRowCounters(ctx))
	walks := 0
	symbolFTSPrefixWalkObserver = func() { walks++ }
	t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
	terms := []string{"hand", "handler", "store", "req", "base"}
	exact := func(label string) {
		t.Helper()
		got, err := store.SymbolFTSPrefixHits(ctx, -1, terms)
		require.NoError(t, err)
		for _, term := range terms {
			var want int64
			require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, ftsPrefixTerm(term)).Scan(&want))
			require.Equal(t, want, got[term], "%s: term %q", label, term)
		}
	}
	exact("baseline")
	require.Equal(t, len(terms), walks, "the baseline walks each term once")

	// A new generation, published: counted from its rows, no walk.
	genNodes, genItems := ftsDocs("layer", 12, func(i int) string { return "handle request" })
	handle.AddBatch(genNodes, nil)
	require.NoError(t, handle.BatchUpsertSymbolFTS(genItems))
	require.NoError(t, store.PublishPayloadGeneration(ctx, generationID, 8000))
	walks = 0
	exact("after a publication")
	require.Zero(t, walks, "a publication walked the shared index")

	// A second, with a non-ASCII document: scoped FTS5 counts, still exact.
	req := payloadRequest()
	req.LayerID, req.TreeOID, req.CreatedAt = "layer-2", "tree-2", 9000
	second, secondHandle, err := store.BeginPayloadGeneration(ctx, req)
	require.NoError(t, err)
	n2, i2 := ftsDocs("über", 3, func(i int) string { return "handler" })
	secondHandle.AddBatch(n2, nil)
	require.NoError(t, secondHandle.BatchUpsertSymbolFTS(i2))
	require.NoError(t, store.PublishPayloadGeneration(ctx, second, 9001))
	exact("after a non-ASCII generation")

	// A base write re-baselines.
	require.NoError(t, store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: baseNodes[1].ID, Tokens: "base storefront"}}))
	walks = 0
	exact("after a base write")
	require.Equal(t, len(terms), walks)

	// A retirement re-baselines.
	require.NoError(t, store.RetirePayloadGeneration(ctx, generationID, nil))
	exact("after a retirement")

	// A document outside the sidecar moves the table's document count.
	store.writeMu.Lock()
	_, err = store.writerDB.Exec(`INSERT INTO symbol_fts(rowid, node_id, repo_prefix, tokens) VALUES (999999, 'orphan', '', 'handler orphan')`)
	store.writeMu.Unlock()
	require.NoError(t, err)
	exact("after an orphan document")
}

func TestASCIITokensHavePrefixFollowsUnicode61(t *testing.T) {
	for _, c := range []struct {
		text, prefix string
		has, ascii   bool
	}{
		{"handle_request", "req", true, true},
		{"HandleRequest", "req", false, true},
		{"Handle Request", "req", true, true},
		{"x1y z", "1", false, true},
		{"x 1y", "1", true, true},
		{"straße", "str", false, false},
		{"", "a", false, true},
	} {
		has, ascii := asciiTokensHavePrefix(c.text, c.prefix)
		require.Equal(t, c.has, has, "%+v", c)
		require.Equal(t, c.ascii, ascii, "%+v", c)
	}
}

// One MATCH over several generations returns, per generation, exactly what
// that generation's own read returns, and reports a generation over its bound.
func TestSymbolFTSViewGenerationRowsSplitsOneMatchByGeneration(t *testing.T) {
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)
	baseNodes, baseItems := ftsDocs("base", 25, func(i int) string {
		if i%2 == 0 {
			return "handler request"
		}
		return "store"
	})
	store.AddBatch(baseNodes, nil)
	require.NoError(t, store.BatchUpsertSymbolFTS(baseItems))
	genNodes, genItems := ftsDocs("layer", 15, func(i int) string {
		if i%3 == 0 {
			return "handle"
		}
		return "request"
	})
	handle.AddBatch(genNodes, nil)
	require.NoError(t, handle.BatchUpsertSymbolFTS(genItems))
	require.NoError(t, store.PublishPayloadGeneration(ctx, generationID, 8000))
	// Another generation with matching documents that no call asks for.
	req := payloadRequest()
	req.LayerID, req.TreeOID, req.CreatedAt = "layer-other", "tree-other", 8500
	otherID, otherHandle, err := store.BeginPayloadGeneration(ctx, req)
	require.NoError(t, err)
	on, oi := ftsDocs("other", 4, func(i int) string { return "handler request store" })
	otherHandle.AddBatch(on, nil)
	require.NoError(t, otherHandle.BatchUpsertSymbolFTS(oi))
	require.NoError(t, store.PublishPayloadGeneration(ctx, otherID, 8600))

	own := func(g int64, match string) []SymbolFTSRow {
		var out []SymbolFTSRow
		var after int64
		for {
			page, next, err := store.SymbolFTSGenerationRows(ctx, g, match, after, 7)
			require.NoError(t, err)
			out = append(out, page...)
			if next == 0 {
				return out
			}
			after = next
		}
	}
	for _, match := range []string{`tokens : "hand" *`, `tokens : "request" *`, `tokens : "store" *`, `tokens : "zzz" *`} {
		for _, gens := range [][]int64{{0, generationID}, {generationID}, {generationID, 0, generationID}} {
			got, over, err := store.SymbolFTSViewGenerationRows(ctx, gens, match, 0)
			require.NoError(t, err)
			require.Empty(t, over)
			distinct := map[int64]bool{}
			for _, g := range gens {
				distinct[g] = true
			}
			require.Len(t, got, len(distinct), "rows keyed by a generation nobody asked for")
			for _, g := range gens {
				require.Equal(t, own(g, match), got[g], "match %q generation %d of %v", match, g, gens)
			}
		}
	}
	got, over, err := store.SymbolFTSViewGenerationRows(ctx, []int64{0, generationID}, `tokens : "request" *`, 8)
	require.NoError(t, err)
	require.True(t, over[generationID], "generation with 10 matches over a bound of 8")
	require.Nil(t, got[generationID])
	require.True(t, over[0], "the base has 13 matches, over a bound of 8")
	got, over, err = store.SymbolFTSViewGenerationRows(ctx, []int64{0, generationID}, `tokens : "request" *`, 13)
	require.NoError(t, err)
	require.Empty(t, over)
	require.Len(t, got[0], 13)
	require.Len(t, got[generationID], 10)
}

// The no-MATCH read seeks a generation's own documents: its plan drives the
// sidecar's (view_gen, fts_rowid) range and looks each document up by rowid
// (no scan of symbol_fts), and it returns exactly what the walk returned, for
// a dense generation and for one whose rowids are sparse (interleaved with
// another generation's).
func TestSymbolFTSGenerationRowsSeeksTheGenerationsOwnDocuments(t *testing.T) {
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)
	// Interleave: base and generation documents written alternately so the
	// generation's rowids are sparse across the table.
	for i := 0; i < 12; i++ {
		bn, bi := ftsDocs(fmt.Sprintf("base%d", i), 5, func(int) string { return "handler" })
		for j := range bn {
			bn[j].ID = fmt.Sprintf("%s-b%d", bn[j].ID, i)
			bi[j].NodeID = bn[j].ID
		}
		store.AddBatch(bn, nil)
		require.NoError(t, store.BatchUpsertSymbolFTS(bi))
		gn, gi := ftsDocs(fmt.Sprintf("layer%d", i), 1, func(int) string { return "request" })
		gn[0].ID = fmt.Sprintf("%s-g%d", gn[0].ID, i)
		gi[0].NodeID = gn[0].ID
		handle.AddBatch(gn, nil)
		require.NoError(t, handle.BatchUpsertSymbolFTS(gi))
	}
	require.NoError(t, store.PublishPayloadGeneration(ctx, generationID, 8000))
	st, err := store.SymbolFTSGenerationStats(ctx, generationID)
	require.NoError(t, err)
	require.Equal(t, int64(12), st.Rows)
	require.Greater(t, st.Hi-st.Lo+1, 4*st.Rows, "precondition: the generation's rowids are sparse")

	// The plan: the sidecar range drives, symbol_fts is looked up by rowid.
	rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+symbolFTSGenerationSeekSQL, generationID, 0, 100)
	require.NoError(t, err)
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	joined := strings.Join(plan, " | ")
	t.Logf("plan: %s", joined)
	require.Contains(t, joined, "symbol_fts_rowid_by_generation")
	require.NotContains(t, joined, "SCAN m", "the sidecar must be ranged, not scanned")
	require.Regexp(t, `VIRTUAL TABLE INDEX 0:=|VIRTUAL TABLE INDEX [0-9]+:.*=`, joined, "symbol_fts must be looked up by rowid")

	// Parity with the walk, in pages.
	walk := func(g int64) []SymbolFTSRow {
		cur, err := store.db.Query(`SELECT symbol_fts.rowid, symbol_fts.node_id, symbol_fts.repo_prefix, symbol_fts.tokens, d.sz
  FROM symbol_fts CROSS JOIN symbol_fts_rowid m ON m.fts_rowid = symbol_fts.rowid AND m.view_gen = ?
  LEFT JOIN symbol_fts_docsize d ON d.id = symbol_fts.rowid ORDER BY symbol_fts.rowid`, g)
		require.NoError(t, err)
		defer cur.Close()
		var out []SymbolFTSRow
		for cur.Next() {
			var r SymbolFTSRow
			var sz []byte
			require.NoError(t, cur.Scan(&r.RowID, &r.NodeID, &r.RepoPrefix, &r.Tokens, &sz))
			for _, v := range sqliteVarints(sz) {
				r.TokenCount += int(v)
			}
			out = append(out, r)
		}
		require.NoError(t, cur.Err())
		return out
	}
	for _, g := range []int64{generationID, 0} {
		var got []SymbolFTSRow
		var after int64
		for {
			page, next, err := store.SymbolFTSGenerationRows(ctx, g, "", after, 5)
			require.NoError(t, err)
			got = append(got, page...)
			if next == 0 {
				break
			}
			after = next
		}
		require.Equal(t, walk(g), got, "generation %d", g)
	}
}

// The no-MATCH read costs the generation's own documents, not the table: on
// one connection, with mmap off so every page fetch is counted, reading a
// sparse 16-document generation out of a 4,000-document table fetches a
// bounded number of pages per document, while the walk it replaces fetches
// pages for every row of the table.
func TestSymbolFTSGenerationRowsReadsInProportionToTheGeneration(t *testing.T) {
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)
	const baseDocs, genDocs = 4000, 16
	for i := 0; i < genDocs; i++ {
		bn, bi := ftsDocs(fmt.Sprintf("base%d", i), baseDocs/genDocs, func(int) string { return "handler" })
		for j := range bn {
			bn[j].ID = fmt.Sprintf("%s-b%d", bn[j].ID, i)
			bi[j].NodeID = bn[j].ID
		}
		store.AddBatch(bn, nil)
		require.NoError(t, store.BatchUpsertSymbolFTS(bi))
		gn, gi := ftsDocs(fmt.Sprintf("layer%d", i), 1, func(int) string { return "request" })
		gn[0].ID = fmt.Sprintf("%s-g%d", gn[0].ID, i)
		gi[0].NodeID = gn[0].ID
		handle.AddBatch(gn, nil)
		require.NoError(t, handle.BatchUpsertSymbolFTS(gi))
	}
	require.NoError(t, store.PublishPayloadGeneration(ctx, generationID, 8000))

	// One reader connection, so its page counters see every statement, and
	// no memory map, whose page fetches SQLite does not count.
	store.db.SetMaxOpenConns(1)
	_, err := store.db.ExecContext(ctx, `PRAGMA mmap_size=0`)
	require.NoError(t, err)
	// Page fetches: the read gate adds each reader connection's page-cache
	// counters (hits and misses) to its sums, and resets them, when the
	// connection returns to the pool; reading the sums after each statement
	// has finished counts every page the statement fetched.
	pageFetches := func() int {
		return int(store.readGate.cacheHits.Load() + store.readGate.cacheMisses.Load())
	}

	before := pageFetches()
	var got int
	var after int64
	for {
		page, next, err := store.SymbolFTSGenerationRows(ctx, generationID, "", after, 64)
		require.NoError(t, err)
		got += len(page)
		if next == 0 {
			break
		}
		after = next
	}
	seek := pageFetches() - before
	require.Equal(t, genDocs, got)

	before = pageFetches()
	cur, err := store.db.QueryContext(ctx, `SELECT symbol_fts.rowid, symbol_fts.tokens, d.sz
  FROM symbol_fts CROSS JOIN symbol_fts_rowid m ON m.fts_rowid = symbol_fts.rowid AND m.view_gen = ?
  LEFT JOIN symbol_fts_docsize d ON d.id = symbol_fts.rowid ORDER BY symbol_fts.rowid`, generationID)
	require.NoError(t, err)
	var walked int
	for cur.Next() {
		walked++
	}
	require.NoError(t, cur.Err())
	require.NoError(t, cur.Close())
	walk := pageFetches() - before
	require.Equal(t, genDocs, walked)

	t.Logf("page fetches: seek=%d (%.1f per document) walk=%d (%.1f per document) over %d table rows",
		seek, float64(seek)/genDocs, walk, float64(walk)/genDocs, baseDocs+genDocs)
	require.Positive(t, seek, "no page fetch was counted: the measurement is not reaching the reads")
	require.LessOrEqual(t, seek, 64+20*genDocs, "the read must cost the generation's own documents")
	require.GreaterOrEqual(t, walk, 4*seek, "precondition: the walk reads the whole table")
}
