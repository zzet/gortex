// The in-memory FTS ranker ranks one generation's symbol full-text matches in memory,
// with exactly the scores and order the store's FTS5 query gives them.
//
// The store ranks a generation with
//
//	SELECT node_id, bm25(symbol_fts) FROM symbol_fts
//	WHERE symbol_fts MATCH '"t1"* OR "t2"* …' AND rowid BETWEEN lo AND hi …
//	ORDER BY rank
//
// and FTS5 pays two whole-table walks for it, whatever the rowid bound: every
// prefix term expands over every indexed term with that prefix, and bm25 counts
// each phrase's matching rows over the whole table for its IDF. The per-
// generation page memo cannot keep that off the hot path after a publication:
// bm25's statistics are the shared table's, so every FTS write — every
// publication — moves the corpus stamp the memo is keyed by, and every
// generation of the stack is queried again.
//
// The ranker splits bm25 into what does not change and what does:
//
//   - a generation's matching rows, each with its per-phrase instance counts
//     and its token count, are a function of the immutable generation and the
//     query alone; they are read once (FTSRankSource.SymbolFTSGenerationRows) and kept
//     for as long as the generation lives, across publications;
//   - the table's statistics — row count, total tokens (FTS5's averages
//     record) and each phrase's matching-row count — are read per search
//     (FTSRankSource.SymbolFTSStats, FTSRankSource.SymbolFTSPrefixHits), which the store
//     keeps current without a walk.
//
// The score is fts5Bm25Function's own arithmetic, operation for operation, with
// its explicit roundings and the driver's own log (libc.Xlog), so the doubles
// are the store's bit for bit; rows are ordered by score, ties by rowid, which
// is the order FTS5's sorter returns them in. TestFTSRankerRanksLikeFTS5 pins the
// hits and score bits against FTS5 itself.
//
// In-memory ranking is exact only where this file reproduces FTS5's
// unicode61 tokenizer: ASCII text. A query term or a row with any other
// character makes Rank report ok=false and the caller keeps the SQL path.

package search

import (
	"container/list"
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/libc"

	"github.com/zzet/gortex/internal/graph"
)

// FTSRankStats is the FTS table's bm25 corpus statistics: the row count and the
// total token count (FTS5's averages record), and the stamp that names this
// state of the table.
type FTSRankStats struct {
	Rows   int64
	Tokens int64
	Stamp  string
}

// FTSRankRow is one full-text row of a generation: its FTS rowid, the node it
// indexes, its repository, its indexed tokens text and its token count (the
// row's docsize).
type FTSRankRow struct {
	Rowid      int64
	NodeID     string
	RepoPrefix string
	Tokens     string
	TokenCount int
}

// FTSRankSource is the store reads the ranker needs.
type FTSRankSource interface {
	// SymbolFTSStats returns the table's statistics and stamp.
	SymbolFTSStats(ctx context.Context) (FTSRankStats, error)
	// SymbolFTSPrefixHits returns, per term, the number of table rows
	// matching the phrase "term"* — the nHit of bm25's IDF — and the stamp
	// of the table state the counts describe.
	SymbolFTSPrefixHits(ctx context.Context, terms []string) (map[string]int64, string, error)
	// SymbolFTSGenerationRows returns the generation's rows that match
	// match (or every row of a small generation; the ranker filters),
	// restricted to repoAllow ('' always allowed) when it is non-empty.
	SymbolFTSGenerationRows(ctx context.Context, generation int64, match string, repoAllow []string) ([]FTSRankRow, error)
}

// FTSScoringSnapshotSource supplies all global BM25 weights from one read
// snapshot. It is optional: legacy sources keep their stamp-checked reads.
// Only immutable positive-generation searches use it; mutable generation zero
// keeps its existing reads and freshness witnesses.
type FTSScoringSnapshotSource interface {
	SymbolFTSScoringSnapshot(ctx context.Context, terms []string) (FTSRankStats, map[string]int64, error)
}

// FTSRankTerms returns the prefix terms of the MATCH the store builds for query
// (buildFTSMatch with normalization), and whether each is one FTS5 token this
// package can match exactly: ASCII letters and digits only.
func FTSRankTerms(query string) ([]string, bool) {
	tokens := Tokenize(query)
	if len(tokens) == 0 {
		tokens = TokenizeQuery(query)
	}
	tokens = NormalizeFTSTokens(tokens)
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t == "" {
			continue
		}
		folded, ok := ftsFoldTerm(t)
		if !ok {
			return nil, false
		}
		out = append(out, folded)
	}
	return out, len(out) > 0
}

// FTSRankMatch is the FTS5 MATCH expression for terms, as the store builds it.
func FTSRankMatch(terms []string) string {
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		parts = append(parts, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
	}
	return strings.Join(parts, " OR ")
}

// foldTerm is unicode61's view of a quoted term: one token when it is made of
// ASCII letters and digits only, lower-cased.
func ftsFoldTerm(t string) (string, bool) {
	b := []byte(t)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z':
			b[i] = c + 'a' - 'A'
		default:
			return "", false
		}
	}
	return string(b), len(b) > 0
}

// tokensOf splits text the way unicode61 does for ASCII: runs of letters and
// digits, lower-cased. ok is false when text holds a non-ASCII byte.
func ftsTokensOf(text string) ([]string, bool) {
	var out []string
	start := -1
	b := []byte(text)
	for i := 0; i <= len(b); i++ {
		alnum := false
		if i < len(b) {
			c := b[i]
			if c >= 0x80 {
				return nil, false
			}
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
				alnum = true
			case c >= 'A' && c <= 'Z':
				b[i] = c + 'a' - 'A'
				alnum = true
			}
		}
		if alnum && start < 0 {
			start = i
		}
		if !alnum && start >= 0 {
			out = append(out, string(b[start:i]))
			start = -1
		}
	}
	return out, true
}

// ftsMatchRow is one matching row with what bm25 reads from it.
type ftsMatchRow struct {
	rowid  int64
	nodeID string
	freq   []float64 // instances per phrase, in the query's phrase order
	size   int       // the row's token count
}

// ftsGenerationMatches is one generation's matching rows for one query, in
// rowid order. exact is false when a row could not be matched exactly.
type ftsGenerationMatches struct {
	rows  []ftsMatchRow
	exact bool
}

type ftsCacheKey struct {
	generation int64
	terms      string
	repos      string
}

type ftsCacheEntry struct {
	key   ftsCacheKey
	m     *ftsGenerationMatches
	elem  *list.Element
	bytes int64
}

// ftsMatchesBytes is a match list's approximate heap.
func ftsMatchesBytes(m *ftsGenerationMatches) int64 {
	if m == nil {
		return 0
	}
	n := int64(48)
	for _, row := range m.rows {
		n += 64 + int64(len(row.nodeID)) + 8*int64(len(row.freq))
	}
	return n
}

// ftsCacheMaxEntries bounds the retained (generation, query) match lists;
// ftsCacheMaxRows bounds one list: a larger generation match is not ranked in
// memory.
const (
	ftsCacheMaxEntries = 512
	ftsCacheMaxRows    = 50_000
)

// FTSGenerationDocsSource is a FTSRankSource that reports a generation's
// rowid run. A small immutable generation whose run is dense is then read
// whole, once, with no FTS5 query (SymbolFTSGenerationRows with an empty
// match), and every later query is matched against its documents in memory: a
// MATCH on the shared table walks each query prefix's doclists over the whole
// table, even for a generation of a few hundred rows, and a query naming a new
// identifier (the first search after an edit) never finds a kept match list.
//
// The store reads a generation with no MATCH from its ownership sidecar, one
// rowid seek per document, so the whole read costs the generation's own
// documents.
type FTSGenerationDocsSource interface {
	SymbolFTSGenerationRun(ctx context.Context, generation int64) (FTSGenerationRun, error)
}

// FTSTokenizer is a FTSRankSource that returns FTS5's own terms for texts of
// the indexed column (the store's SymbolFTSTokenize), for the rows the ranker
// cannot tokenize itself.
type FTSTokenizer interface {
	SymbolFTSTokenize(ctx context.Context, texts []string) ([][]string, error)
}

// tokenizeNonASCII fills tokens[i] for every i in pending with FTS5's own terms
// for rows[i], and reports why it could not ("" when it did).
func (r *FTSRanker) tokenizeNonASCII(ctx context.Context, rows []FTSRankRow, tokens [][]string, pending []int) string {
	tk, ok := r.src.(FTSTokenizer)
	if !ok {
		return "non_ascii"
	}
	texts := make([]string, len(pending))
	for j, i := range pending {
		texts[j] = rows[i].Tokens
	}
	terms, err := tk.SymbolFTSTokenize(ctx, texts)
	if err != nil || len(terms) != len(texts) {
		return "non_ascii_tokenize_failed"
	}
	for j, i := range pending {
		tokens[i] = terms[j]
	}
	ftsNonASCIIRows.Add(int64(len(pending)))
	return ""
}

// ftsNonASCIIRows counts the rows tokenized by FTS5 itself; ftsWholeReadRefused
// the whole reads refused (diagnostics).
var (
	ftsNonASCIIRows     atomic.Int64
	ftsWholeReadRefused atomic.Int64
)

// FTSNonASCIIRows reports how many kept rows were tokenized by FTS5 itself,
// and how many whole reads were refused.
func FTSNonASCIIRows() (tokenized, refused int64) {
	return ftsNonASCIIRows.Load(), ftsWholeReadRefused.Load()
}

// FTSFreshRowsSource is a FTSRankSource that hands over, once, the rows a
// generation's build wrote (the store's fresh-rows hand-off), so the first
// search over a new generation reads none of its documents.
type FTSFreshRowsSource interface {
	TakeFreshGenerationRows(generation int64) (rows []FTSRankRow, complete bool)
}

// ftsFreshPendingMax bounds the handed-over generations waiting for their
// first query.
const ftsFreshPendingMax = 64

// ftsFreshAdopted / ftsFreshRejected count handed-over generations used as the
// generation's documents, and ones that did not match the generation's
// statistics (diagnostics and tests).
var (
	ftsFreshAdopted  atomic.Int64
	ftsFreshRejected atomic.Int64
)

// FTSFreshCounts reports how many handed-over generations were used, and how
// many were refused for not matching the generation's statistics.
func FTSFreshCounts() (adopted, rejected int64) {
	return ftsFreshAdopted.Load(), ftsFreshRejected.Load()
}

// AdoptFresh takes generation's handed-over rows from the source, if it offers
// complete ones, and keeps them for the generation's first query. It reads
// nothing from the store (the hand-off is in memory), so the route pre-warm
// calls it inside the publication. It reports whether rows were taken.
func (r *FTSRanker) AdoptFresh(generation int64) bool {
	src, ok := r.src.(FTSFreshRowsSource)
	if !ok || generation <= 0 {
		return false
	}
	rows, complete := src.TakeFreshGenerationRows(generation)
	if !complete || len(rows) == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fresh == nil {
		r.fresh = make(map[int64][]FTSRankRow)
	}
	r.fresh[generation] = rows
	r.freshOrder = append(r.freshOrder, generation)
	for len(r.freshOrder) > ftsFreshPendingMax {
		delete(r.fresh, r.freshOrder[0])
		r.freshOrder = r.freshOrder[1:]
	}
	return true
}

// takeFresh returns and forgets generation's pending handed-over rows.
func (r *FTSRanker) takeFresh(generation int64) []FTSRankRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, ok := r.fresh[generation]
	if !ok {
		return nil
	}
	delete(r.fresh, generation)
	for i, g := range r.freshOrder {
		if g == generation {
			r.freshOrder = append(r.freshOrder[:i], r.freshOrder[i+1:]...)
			break
		}
	}
	return rows
}

// AdoptFreshFTSRows hands generation's fresh rows to the ranker of
// searcher's store core (FTSRanker.AdoptFresh).
func AdoptFreshFTSRows(searcher any, generation int64) bool {
	src, ok := ftsRankSourceOf(searcher)
	if !ok {
		return false
	}
	r := ftsRankerFor(src)
	if r == nil {
		return false
	}
	return r.AdoptFresh(generation)
}

// FTSGenerationRun is a generation's document count and first and last rowid.
// Known is false when the run is not reported (the base, one being
// built).
type FTSGenerationRun struct {
	Docs, Lo, Hi int64
	// Known: Lo and Hi are the generation's run (a sealed generation).
	Known bool
	// DocsKnown: Docs is the generation's document count (read from the
	// ownership sidecar whether or not the generation is sealed).
	DocsKnown bool
}

// ftsWholeReads / ftsWholeReadsSparse count the generations read whole and the
// ones kept on the MATCH read because their run was not dense (diagnostics).
var (
	ftsWholeReads       atomic.Int64
	ftsWholeReadsSparse atomic.Int64
)

// FTSWholeReadDecision is one generation's whole-read decision
// (diagnostics).
type FTSWholeReadDecision struct {
	Generation int64 `json:"generation"`
	Docs       int64 `json:"docs"`
	Width      int64 `json:"width"`
	TableRows  int64 `json:"table_rows"`
	Dense      bool  `json:"dense"`
	At         int64 `json:"at_unix_ms"`
	// Refused, on a record made after the read, is why the documents read
	// were not kept: non_ascii (no tokenizer for a non-ASCII row),
	// non_ascii_tokenize_failed, too_many_rows, over_cap; NonASCII is how
	// many rows needed FTS5's own terms.
	Refused  string `json:"refused,omitempty"`
	NonASCII int    `json:"non_ascii,omitempty"`
}

var wholeReadDecisions struct {
	mu     sync.Mutex
	recent []FTSWholeReadDecision
}

func recordWholeReadDecision(generation int64, run FTSGenerationRun, tableRows int64, dense bool) {
	d := FTSWholeReadDecision{Generation: generation, Docs: run.Docs, Width: run.Hi - run.Lo + 1, TableRows: tableRows, Dense: dense, At: time.Now().UnixMilli()}
	wholeReadDecisions.mu.Lock()
	defer wholeReadDecisions.mu.Unlock()
	wholeReadDecisions.recent = append(wholeReadDecisions.recent, d)
	if n := len(wholeReadDecisions.recent); n > viewMatchDecisionsKept {
		wholeReadDecisions.recent = append([]FTSWholeReadDecision(nil), wholeReadDecisions.recent[n-viewMatchDecisionsKept:]...)
	}
}

// recordWholeReadRefusal records why a generation's documents, read whole,
// were not kept.
func recordWholeReadRefusal(generation int64, run FTSGenerationRun, reason string, nonASCII int) {
	ftsWholeReadRefused.Add(1)
	d := FTSWholeReadDecision{Generation: generation, Docs: run.Docs, Width: run.Hi - run.Lo + 1, Refused: reason, NonASCII: nonASCII, At: time.Now().UnixMilli()}
	wholeReadDecisions.mu.Lock()
	defer wholeReadDecisions.mu.Unlock()
	wholeReadDecisions.recent = append(wholeReadDecisions.recent, d)
	if n := len(wholeReadDecisions.recent); n > viewMatchDecisionsKept {
		wholeReadDecisions.recent = append([]FTSWholeReadDecision(nil), wholeReadDecisions.recent[n-viewMatchDecisionsKept:]...)
	}
}

// FTSWholeReadDecisions returns the most recent whole-read decisions.
func FTSWholeReadDecisions() []FTSWholeReadDecision {
	wholeReadDecisions.mu.Lock()
	defer wholeReadDecisions.mu.Unlock()
	return append([]FTSWholeReadDecision(nil), wholeReadDecisions.recent...)
}

// FTSWholeReadCounts reports the whole reads made and the ones declined for a
// sparse rowid run.
func FTSWholeReadCounts() (read, sparse int64) {
	return ftsWholeReads.Load(), ftsWholeReadsSparse.Load()
}

// ftsWholeReadAllowed decides whether a generation's documents are read
// whole (once, then kept). The store reads a generation with no MATCH from its
// ownership sidecar, one rowid seek per document, so a whole read costs the
// generation's own documents wherever its rowids lie; every known generation of
// at most ftsDocsMaxGenerationRows documents is read whole, once, and every
// later query — new names included — is matched against it in memory. A
// generation with no documents needs no read at all (the caller answers it
// empty); a larger one, or one whose document count is not known, keeps the
// per-query MATCH read.
func ftsWholeReadAllowed(run FTSGenerationRun) bool {
	return run.DocsKnown && run.Docs > 0 && run.Docs <= ftsDocsMaxGenerationRows
}

// ftsDocsMaxGenerationRows bounds the generations read whole.
var ftsDocsMaxGenerationRows int64 = 100_000

// The ranker's memory is bounded in bytes (approximate heap: strings, slice
// headers and row structs; ftsDocsBytes, ftsMatchesBytes):
//
//   - kept documents: at most ftsKeptMaxBytes across generations (256 MiB;
//     GORTEX_FTS_KEPT_MAX_MB). Past it the least recently used generation's
//     documents are dropped — used: a query ranked it or a route warm named it
//     — and a query then takes the MATCH read for it: only the background warm
//     reads it whole again, so the cap never moves a whole read to the first
//     search after an edit. A generation larger than the cap on its own is not
//     kept at all and keeps the MATCH read;
//   - kept match lists: at most ftsMatchMaxBytes and ftsCacheMaxEntries lists,
//     least recently used first out.
//
// A generation is also dropped whole — documents and match lists — when it
// stops being servable (retirement): Materializer.ForgetGeneration reaches
// ForgetFTSGeneration.
var (
	ftsKeptMaxBytes  int64 = 256 << 20
	ftsMatchMaxBytes int64 = 64 << 20
)

// SetFTSKeptMaxBytesForTest sets the kept-documents cap until restore runs.
// Tests only.
func SetFTSKeptMaxBytesForTest(n int64) (restore func()) {
	previous := ftsKeptMaxBytes
	ftsKeptMaxBytes = n
	return func() { ftsKeptMaxBytes = previous }
}

// SetFTSWholeReadMaxRowsForTest sets the largest generation read whole until
// restore runs (0: none is). Tests only.
func SetFTSWholeReadMaxRowsForTest(n int64) (restore func()) {
	previous := ftsDocsMaxGenerationRows
	ftsDocsMaxGenerationRows = n
	return func() { ftsDocsMaxGenerationRows = previous }
}

// ftsGenerationDocs is an immutable generation's documents, or (rows nil,
// usable false) the record that it is not read whole.
type ftsGenerationDocs struct {
	rows   []FTSRankRow
	tokens [][]string // rows[i]'s tokens, tokenized once
	bytes  int64      // approximate heap the kept documents hold
	usable bool
	elem   *list.Element
	// evicted marks documents dropped at the cap: a query takes the MATCH
	// read for the generation, and only a warm reads it whole again.
	evicted bool
}

// ftsKeptDocs / ftsKeptBytes track the documents all rankers keep
// (diagnostics: FTSKeptDocs).
var (
	ftsKeptDocs  atomic.Int64
	ftsKeptBytes atomic.Int64
)

// ftsStoreReads counts every store read the rankers make (statistics, runs,
// generation rows, MATCH reads, prefix counts); FTSStoreReads reports it.
var ftsStoreReads atomic.Int64

// FTSStoreReads reports how many store reads the rankers have made
// (diagnostics and tests).
func FTSStoreReads() int64 { return ftsStoreReads.Load() }

// FTSKeptDocs reports how many generation documents the rankers keep and
// their approximate heap in bytes (strings and slice headers).
func FTSKeptDocs() (docs, bytes int64) { return ftsKeptDocs.Load(), ftsKeptBytes.Load() }

func ftsDocsBytes(rows []FTSRankRow, tokens [][]string) int64 {
	// Allocation sizes, not lengths: a string's bytes are allocated in
	// 8-byte (and larger) size classes, and a slice holds its capacity.
	alloc := func(n int) int64 {
		if n == 0 {
			return 0
		}
		return int64((n + 7) &^ 7)
	}
	n := int64(cap(rows))*64 + int64(cap(tokens))*24
	for i, r := range rows {
		n += alloc(len(r.NodeID)) + alloc(len(r.RepoPrefix)) + alloc(len(r.Tokens))
		if i < len(tokens) {
			n += int64(cap(tokens[i])) * 16
			for _, t := range tokens[i] {
				n += alloc(len(t))
			}
		}
	}
	return n
}

// FTSRanker ranks generations in memory over a FTSRankSource, keeping each immutable
// generation's matches.
type FTSRanker struct {
	src FTSRankSource

	mu      sync.Mutex
	entries map[ftsCacheKey]*ftsCacheEntry
	lru     *list.List

	docs         map[int64]*ftsGenerationDocs
	docsLRU      *list.List // generation (int64), most recently used first
	fresh        map[int64][]FTSRankRow
	freshOrder   []int64
	docsBytes    int64
	entriesBytes int64

	reads     int // generation reads of any kind, for tests
	docsReads int // of which whole-generation reads, for tests
	viewReads int // of which one-MATCH reads over several generations, for tests
}

// NewFTSRanker returns a ranker over src.
func NewFTSRanker(src FTSRankSource) *FTSRanker {
	return &FTSRanker{src: src, entries: make(map[ftsCacheKey]*ftsCacheEntry), lru: list.New(), docs: make(map[int64]*ftsGenerationDocs), docsLRU: list.New()}
}

// Forget drops every kept match list and document set of generation
// (retirement).
func (r *FTSRanker) Forget(generation int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, e := range r.entries {
		if key.generation == generation {
			r.lru.Remove(e.elem)
			r.entriesBytes -= e.bytes
			delete(r.entries, key)
		}
	}
	r.dropDocsLocked(generation)
}

// dropDocsLocked forgets generation's kept documents. r.mu is held.
func (r *FTSRanker) dropDocsLocked(generation int64) {
	d, ok := r.docs[generation]
	if !ok {
		return
	}
	if d.elem != nil {
		r.docsLRU.Remove(d.elem)
	}
	r.docsBytes -= d.bytes
	ftsKeptDocs.Add(-int64(len(d.rows)))
	ftsKeptBytes.Add(-d.bytes)
	delete(r.docs, generation)
}

// evictDocsLocked drops generation's kept documents at the cap and leaves a
// tombstone, so a query does not read the generation whole again (it takes the
// MATCH read) until a warm does. r.mu is held.
func (r *FTSRanker) evictDocsLocked(generation int64) {
	r.dropDocsLocked(generation)
	r.docs[generation] = &ftsGenerationDocs{evicted: true}
	ftsDocsEvictions.Add(1)
}

// ftsDocsEvictions counts the generations dropped at the cap (diagnostics).
var ftsDocsEvictions atomic.Int64

// FTSDocsEvictions reports how many generations' documents were dropped at
// the cap.
func FTSDocsEvictions() int64 { return ftsDocsEvictions.Load() }

// ForgetFTSGeneration drops generation from the ranker of searcher's store
// core (retirement: the generation stopped being servable).
func ForgetFTSGeneration(searcher any, generation int64) {
	src, ok := ftsRankSourceOf(searcher)
	if !ok {
		return
	}
	if r := ftsRankerFor(src); r != nil {
		r.Forget(generation)
	}
}

// generationDocs returns an immutable generation's documents when the source
// can size it and it is small; nil otherwise. Both outcomes are kept.
func (r *FTSRanker) generationDocs(ctx context.Context, generation int64) (*ftsGenerationDocs, error) {
	return r.generationDocsFor(ctx, generation, false)
}

// generationDocsFor is generationDocs; warm reads a generation evicted at the
// cap whole again, where a query leaves it to the MATCH read.
func (r *FTSRanker) generationDocsFor(ctx context.Context, generation int64, warm bool) (*ftsGenerationDocs, error) {
	sizer, ok := r.src.(FTSGenerationDocsSource)
	if !ok {
		return nil, nil
	}
	r.mu.Lock()
	d, ok := r.docs[generation]
	if ok && d.elem != nil {
		r.docsLRU.MoveToFront(d.elem)
	}
	if ok && d.evicted && warm {
		delete(r.docs, generation)
		ok = false
	}
	r.mu.Unlock()
	if ok {
		return d, nil
	}
	d = &ftsGenerationDocs{}
	ftsStoreReads.Add(1)
	run, err := sizer.SymbolFTSGenerationRun(ctx, generation)
	if err != nil {
		return nil, err
	}
	whole := false
	switch {
	case run.DocsKnown && run.Docs == 0:
		// No documents: every query matches nothing here, with no read.
		d = &ftsGenerationDocs{usable: true}
	case run.DocsKnown:
		ftsStoreReads.Add(1)
		stats, err := r.src.SymbolFTSStats(ctx)
		if err != nil {
			return nil, err
		}
		whole = ftsWholeReadAllowed(run)
		if !whole {
			ftsWholeReadsSparse.Add(1)
		}
		recordWholeReadDecision(generation, run, stats.Rows, whole)
	}
	if whole {
		var rows []FTSRankRow
		// The rows the generation's build handed over serve instead of a read
		// when they are exactly the generation's: its document count and first
		// and last rowid, from the statistics just read.
		if fresh := r.takeFresh(generation); fresh != nil {
			if int64(len(fresh)) == run.Docs && (!run.Known || (fresh[0].Rowid == run.Lo && fresh[len(fresh)-1].Rowid == run.Hi)) {
				rows = fresh
				ftsFreshAdopted.Add(1)
			} else {
				ftsFreshRejected.Add(1)
			}
		}
		if rows == nil {
			ftsWholeReads.Add(1)
			ftsStoreReads.Add(1)
			var err error
			rows, err = r.src.SymbolFTSGenerationRows(ctx, generation, "", nil)
			if err != nil {
				return nil, err
			}
			r.mu.Lock()
			r.reads++
			r.docsReads++
			r.mu.Unlock()
		}
		// Every document must tokenize exactly as FTS5 does. ASCII rows are
		// tokenized here; a row with any other character gets SQLite's own
		// terms for it (FTSTokenizer: FTS5's tokenizer on a private in-memory
		// table), read once, with the generation. Without them the generation
		// is refused and keeps the MATCH read.
		refused := ""
		if int64(len(rows)) > ftsDocsMaxGenerationRows {
			refused = "too_many_rows"
		}
		tokens := make([][]string, len(rows))
		var pending []int
		for i := 0; refused == "" && i < len(rows); i++ {
			t, ok := ftsTokensOf(rows[i].Tokens)
			if !ok {
				pending = append(pending, i)
				continue
			}
			tokens[i] = t
		}
		if refused == "" && len(pending) > 0 {
			refused = r.tokenizeNonASCII(ctx, rows, tokens, pending)
		}
		if refused == "" {
			d = &ftsGenerationDocs{rows: rows, tokens: tokens, bytes: ftsDocsBytes(rows, tokens), usable: true}
			if d.bytes > ftsKeptMaxBytes {
				// Larger than the cap on its own: not kept; the generation
				// keeps the MATCH read.
				refused = "over_cap"
				d = &ftsGenerationDocs{}
			}
		}
		if refused != "" {
			recordWholeReadRefusal(generation, run, refused, len(pending))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if kept, ok := r.docs[generation]; ok {
		return kept, nil
	}
	r.docs[generation] = d
	d.elem = r.docsLRU.PushFront(generation)
	r.docsBytes += d.bytes
	ftsKeptDocs.Add(int64(len(d.rows)))
	ftsKeptBytes.Add(d.bytes)
	for r.docsBytes > ftsKeptMaxBytes && r.docsLRU.Len() > 1 {
		oldest := r.docsLRU.Back().Value.(int64)
		if oldest == generation {
			break
		}
		r.evictDocsLocked(oldest)
	}
	return d, nil
}

// WarmGenerations reads whole, ahead of any query, every generation of
// generations the whole-read rule admits and the ranker does not keep yet (a
// generation above zero; the base corpus is never kept). It reports how many
// it read.
func (r *FTSRanker) WarmGenerations(ctx context.Context, generations []int64) (int, error) {
	if r == nil || r.src == nil {
		return 0, nil
	}
	read := 0
	for _, generation := range generations {
		if generation <= 0 {
			continue
		}
		before := r.docsReadCount()
		if _, err := r.generationDocsFor(ctx, generation, true); err != nil {
			return read, err
		}
		if r.docsReadCount() > before {
			read++
		}
	}
	return read, nil
}

func (r *FTSRanker) docsReadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.docsReads
}

// WarmFTSGenerations warms the ranker of searcher's store core with
// generations (FTSRanker.WarmGenerations): the route pre-warm calls it with a
// route's base generations, so their one whole read is paid there and not by
// the first search after an edit. It reports how many generations it read; a
// store that does not serve the ranker reads nothing.
func WarmFTSGenerations(ctx context.Context, searcher any, generations []int64) (int, error) {
	src, ok := ftsRankSourceOf(searcher)
	if !ok {
		return 0, nil
	}
	ranker := ftsRankerFor(src)
	if ranker == nil {
		return 0, nil
	}
	return ranker.WarmGenerations(ctx, generations)
}

// Rank returns generation's first limit hits for query, scored and ordered
// exactly as the store's FTS5 query does, with Score = -bm25 as the store
// reports it. ok is false when the ranking cannot be exact in memory (a
// non-ASCII term or row, a match list over the cap, statistics that moved
// under the read); the caller then asks the store. immutable says the
// generation's rows never change, so its match list may be kept.
func (r *FTSRanker) Rank(ctx context.Context, generation int64, immutable bool, query string, repoAllow []string, limit int) ([]graph.SymbolHit, bool, error) {
	if r == nil || r.src == nil {
		return nil, false, nil
	}
	if limit <= 0 {
		limit = 20
	}
	terms, ok := FTSRankTerms(query)
	if !ok {
		return nil, false, nil
	}
	m, err := r.matches(ctx, generation, immutable, terms, repoAllow)
	if err != nil || m == nil || !m.exact {
		return nil, false, err
	}
	stats, idf, ok, err := r.idf(ctx, terms)
	if err != nil || !ok {
		return nil, false, err
	}
	return ftsRank(m.rows, idf, float64(stats.Tokens)/float64(stats.Rows), limit), true, nil
}

// idf reads the table statistics and each phrase's IDF, retrying once when
// the two reads describe different table states.
func (r *FTSRanker) idf(ctx context.Context, terms []string) (FTSRankStats, []float64, bool, error) {
	stats, idf, _, ok, err := r.idfHits(ctx, terms, false)
	return stats, idf, ok, err
}

// idfHits is idf with each phrase's matching-document count over the whole
// table. Allowed immutable scopes use a coherent snapshot when supplied;
// other sources keep the existing bounded stamp comparison.
func (r *FTSRanker) idfHits(ctx context.Context, terms []string, allowSnapshot bool) (FTSRankStats, []float64, map[string]int64, bool, error) {
	unique := make([]string, 0, len(terms))
	seen := make(map[string]struct{}, len(terms))
	for _, t := range terms {
		if _, dup := seen[t]; !dup {
			seen[t] = struct{}{}
			unique = append(unique, t)
		}
	}
	snapshot, coherent := r.src.(FTSScoringSnapshotSource)
	coherent = coherent && allowSnapshot
	for attempt := 0; attempt < 2; attempt++ {
		var stats FTSRankStats
		var hits map[string]int64
		var err error
		if coherent {
			ftsStoreReads.Add(1)
			stats, hits, err = snapshot.SymbolFTSScoringSnapshot(ctx, unique)
			if err != nil {
				return FTSRankStats{}, nil, nil, false, err
			}
			if stats.Rows <= 0 || stats.Stamp == "" {
				return FTSRankStats{}, nil, nil, false, nil
			}
		} else {
			ftsStoreReads.Add(1)
			stats, err = r.src.SymbolFTSStats(ctx)
			if err != nil {
				return FTSRankStats{}, nil, nil, false, err
			}
			ftsStoreReads.Add(1)
			var stamp string
			hits, stamp, err = r.src.SymbolFTSPrefixHits(ctx, unique)
			if err != nil {
				return FTSRankStats{}, nil, nil, false, err
			}
			if stamp != stats.Stamp || stats.Rows <= 0 {
				continue
			}
		}
		idf := make([]float64, len(terms))
		for i, t := range terms {
			nHit, found := hits[t]
			if !found {
				return FTSRankStats{}, nil, nil, false, nil
			}
			idf[i] = ftsBM25IDF(stats.Rows, nHit)
		}
		return stats, idf, hits, true, nil
	}
	return FTSRankStats{}, nil, nil, false, nil
}

// bm25IDF is fts5Bm25GetData's IDF: log((N - nHit + 0.5)/(nHit + 0.5)),
// floored at 1e-6, through the driver's own log.
func ftsBM25IDF(nRow, nHit int64) float64 {
	idf := libc.Xlog(nil, (float64(nRow-nHit)+float64(0.5))/(float64(nHit)+float64(0.5)))
	if idf <= float64(0) {
		idf = float64(1e-06)
	}
	return idf
}

// bm25Score is fts5Bm25Function's arithmetic for one row, with its explicit
// float64 conversions (they forbid a fused multiply-add the C code does not
// do): the returned value is the bm25() SQL value, -1*score.
func ftsBM25Score(freq []float64, size int, idf []float64, avgdl float64) float64 {
	k1 := float64(1.2)
	b := float64(0.75)
	score := float64(0)
	D := float64(int32(size))
	for i := range freq {
		score = score + float64(idf[i]*(float64(freq[i]*(k1+float64(1)))/(freq[i]+float64(k1*(float64(1)-b+float64(b*D)/avgdl)))))
	}
	return float64(-float64(1) * score)
}

// rank scores rows and returns the first limit, ordered by bm25 then rowid.
func ftsRank(rows []ftsMatchRow, idf []float64, avgdl float64, limit int) []graph.SymbolHit {
	type scored struct {
		row   *ftsMatchRow
		value float64
	}
	all := make([]scored, 0, len(rows))
	for i := range rows {
		all = append(all, scored{row: &rows[i], value: ftsBM25Score(rows[i].freq, rows[i].size, idf, avgdl)})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].value != all[j].value {
			return all[i].value < all[j].value
		}
		return all[i].row.rowid < all[j].row.rowid
	})
	hits := make([]graph.SymbolHit, 0, min(limit, len(all)))
	for _, s := range all {
		if len(hits) == limit {
			break
		}
		if s.row.nodeID == "" {
			continue
		}
		hits = append(hits, graph.SymbolHit{NodeID: s.row.nodeID, Score: -s.value})
	}
	return hits
}

// matches returns the generation's match list for terms, from the cache when
// the generation is immutable and it was read before.
func (r *FTSRanker) matches(ctx context.Context, generation int64, immutable bool, terms, repoAllow []string) (*ftsGenerationMatches, error) {
	m, err := r.matchesWithoutMatchRead(ctx, generation, immutable, terms, repoAllow)
	if err != nil || m != nil {
		return m, err
	}
	ftsStoreReads.Add(1)
	rows, err := r.src.SymbolFTSGenerationRows(ctx, generation, FTSRankMatch(terms), repoAllow)
	r.mu.Lock()
	r.reads++
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	m = ftsBuildMatches(rows, terms, repoAllow)
	r.keep(generation, immutable, terms, repoAllow, m)
	return m, nil
}

// matchesWithoutMatchRead answers the generation's match list from the cache
// or from its documents read whole, and nil when a MATCH read is needed.
func (r *FTSRanker) matchesWithoutMatchRead(ctx context.Context, generation int64, immutable bool, terms, repoAllow []string) (*ftsGenerationMatches, error) {
	if !immutable {
		return nil, nil
	}
	key := ftsCacheKey{generation: generation, terms: strings.Join(terms, "\x00"), repos: ftsRepoKey(repoAllow)}
	r.mu.Lock()
	if e, ok := r.entries[key]; ok {
		r.lru.MoveToFront(e.elem)
		r.mu.Unlock()
		return e.m, nil
	}
	r.mu.Unlock()
	docs, err := r.generationDocs(ctx, generation)
	if err != nil || docs == nil || !docs.usable {
		return nil, err
	}
	// The documents that match no phrase are dropped here, as the MATCH would
	// have left them out.
	m := ftsBuildMatchesTokenized(docs.rows, docs.tokens, terms, repoAllow)
	r.keep(generation, immutable, terms, repoAllow, m)
	return m, nil
}

// keep retains an immutable generation's answer. A decline (a row it cannot
// tokenize, a match list over the cap) is kept too: it is as permanent as a
// match list, and repeating the read would only repeat the decline.
func (r *FTSRanker) keep(generation int64, immutable bool, terms, repoAllow []string, m *ftsGenerationMatches) {
	if !immutable || m == nil {
		return
	}
	key := ftsCacheKey{generation: generation, terms: strings.Join(terms, "\x00"), repos: ftsRepoKey(repoAllow)}
	r.mu.Lock()
	defer r.mu.Unlock()
	size := ftsMatchesBytes(m)
	if e, ok := r.entries[key]; ok {
		r.entriesBytes += size - e.bytes
		e.m, e.bytes = m, size
		r.lru.MoveToFront(e.elem)
		return
	}
	e := &ftsCacheEntry{key: key, m: m, bytes: size}
	e.elem = r.lru.PushFront(e)
	r.entries[key] = e
	r.entriesBytes += size
	for r.lru.Len() > 1 && (r.lru.Len() > ftsCacheMaxEntries || r.entriesBytes > ftsMatchMaxBytes) {
		back := r.lru.Back()
		old := back.Value.(*ftsCacheEntry)
		r.lru.Remove(back)
		r.entriesBytes -= old.bytes
		delete(r.entries, old.key)
	}
}

// FTSViewMatchSource is a FTSRankSource that runs one FTS5 MATCH over several
// generations at once. A MATCH walks each query prefix's doclists over the
// whole shared table whatever generation bound it carries, so reading a view's
// generations one MATCH each pays that walk once per generation; one MATCH
// split by generation after the read pays it once. The split is exact because
// bm25 scores a row from the row and the whole table's statistics alone.
type FTSViewMatchSource interface {
	// SymbolFTSViewGenerationRows runs match once and returns, per requested
	// generation, its matching documents in rowid order. A generation with
	// more than perGeneration matching documents is reported in over instead
	// (its rows may be absent or partial).
	SymbolFTSViewGenerationRows(ctx context.Context, generations []int64, match string, perGeneration int) (rows map[int64][]FTSRankRow, over map[int64]bool, err error)
}

// RankGenerations ranks each generation as Rank does (a generation above zero
// is immutable, generation zero is the mutable base) and returns the rankings
// made in memory by generation, and the generations it declined, in request
// order. The generations that need a MATCH read — no kept match list, not read
// whole — are read together in one MATCH when the source can
// (FTSViewMatchSource) and there are two or more of them.
func (r *FTSRanker) RankGenerations(ctx context.Context, generations []int64, query string, repoAllow []string, limit int) (map[int64][]graph.SymbolHit, []int64, error) {
	if r == nil || r.src == nil {
		return nil, generations, nil
	}
	if limit <= 0 {
		limit = 20
	}
	terms, ok := FTSRankTerms(query)
	if !ok {
		return nil, generations, nil
	}
	matches := make(map[int64]*ftsGenerationMatches, len(generations))
	var need []int64
	for _, generation := range generations {
		if _, done := matches[generation]; done || containsGeneration(need, generation) {
			continue
		}
		m, err := r.matchesWithoutMatchRead(ctx, generation, generation > 0, terms, repoAllow)
		if err != nil {
			return nil, nil, err
		}
		if m != nil {
			matches[generation] = m
			continue
		}
		need = append(need, generation)
	}
	// The table statistics and each phrase's whole-table count: read once per
	// query for the ranking below, and read here, before the MATCH reads, so
	// the one-MATCH decision uses them without a read of its own.
	allowSnapshot := len(generations) > 0
	for _, generation := range generations {
		if generation <= 0 {
			allowSnapshot = false
			break
		}
	}
	stats, idf, hits, ok, err := r.idfHits(ctx, terms, allowSnapshot)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, generations, nil
	}
	batch, canBatch := r.src.(FTSViewMatchSource)
	engage := canBatch && len(need) > 1 && ftsViewMatchAllowed(terms, hits)
	if canBatch {
		recordViewMatchDecision(terms, hits, len(need), engage)
	}
	if engage {
		ftsStoreReads.Add(1)
		rows, over, err := batch.SymbolFTSViewGenerationRows(ctx, need, FTSRankMatch(terms), ftsCacheMaxRows)
		r.mu.Lock()
		r.reads++
		r.viewReads++
		r.mu.Unlock()
		ftsViewMatchReads.Add(1)
		if err != nil {
			return nil, nil, err
		}
		for _, generation := range need {
			m := &ftsGenerationMatches{exact: false}
			if !over[generation] {
				m = ftsBuildMatches(rows[generation], terms, repoAllow)
			}
			r.keep(generation, generation > 0, terms, repoAllow, m)
			matches[generation] = m
		}
		need = nil
	}
	for _, generation := range need {
		m, err := r.matches(ctx, generation, generation > 0, terms, repoAllow)
		if err != nil {
			return nil, nil, err
		}
		matches[generation] = m
	}
	ranked := make(map[int64][]graph.SymbolHit, len(matches))
	var declined []int64
	for _, generation := range generations {
		if _, done := ranked[generation]; done || containsGeneration(declined, generation) {
			continue
		}
		m := matches[generation]
		if m == nil || !m.exact {
			declined = append(declined, generation)
			continue
		}
		ranked[generation] = ftsRank(m.rows, idf, float64(stats.Tokens)/float64(stats.Rows), limit)
	}
	return ranked, declined, nil
}

// FTSViewMatchMode selects when the ranker reads a view's generations with one
// MATCH (FTSViewMatchSource): never, only for selective queries, or always.
type FTSViewMatchMode int32

const (
	FTSViewMatchOff FTSViewMatchMode = iota
	FTSViewMatchSelective
	FTSViewMatchOn
)

// ftsViewMatchSelectiveHits is the selective mode's bound: a query whose
// phrases match at most this many documents over the whole table in total.
// One MATCH iterates every matching document of the table when the view holds
// the base generation (its rowids have no run to bound the walk by), so a
// common prefix would cost more than the per-generation reads it replaces.
var ftsViewMatchSelectiveHits int64 = 20_000

var ftsViewMatchMode atomic.Int32

var ftsViewMatchReads atomic.Int64

func init() {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_FTS_VIEW_MATCH"))) {
	case "on":
		ftsViewMatchMode.Store(int32(FTSViewMatchOn))
	case "selective":
		ftsViewMatchMode.Store(int32(FTSViewMatchSelective))
	default:
		// Off until measured: every generation keeps its own MATCH read.
		ftsViewMatchMode.Store(int32(FTSViewMatchOff))
	}
	if raw := os.Getenv("GORTEX_FTS_KEPT_MAX_MB"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			ftsKeptMaxBytes = n << 20
		}
	}
	if raw := os.Getenv("GORTEX_FTS_VIEW_MATCH_MAX_HITS"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			ftsViewMatchSelectiveHits = n
		}
	}
}

// SetFTSViewMatchMode sets the one-MATCH mode for the process (the daemon's
// loopback diagnostics listener, for a measurement), and CurrentFTSViewMatchMode
// reads it.
func SetFTSViewMatchMode(mode FTSViewMatchMode) { ftsViewMatchMode.Store(int32(mode)) }

// CurrentFTSViewMatchMode is the one-MATCH mode in force.
func CurrentFTSViewMatchMode() FTSViewMatchMode { return FTSViewMatchMode(ftsViewMatchMode.Load()) }

// SetFTSViewMatchModeForTest sets the one-MATCH mode until restore runs.
// Tests only.
func SetFTSViewMatchModeForTest(mode FTSViewMatchMode) (restore func()) {
	previous := ftsViewMatchMode.Swap(int32(mode))
	return func() { ftsViewMatchMode.Store(previous) }
}

// FTSViewMatchReads reports how many one-MATCH reads the rankers made
// (diagnostics).
func FTSViewMatchReads() int64 { return ftsViewMatchReads.Load() }

// ftsViewMatchAllowed applies the one-MATCH mode to a query's phrases, given
// each phrase's whole-table document count.
func ftsViewMatchAllowed(terms []string, hits map[string]int64) bool {
	switch FTSViewMatchMode(ftsViewMatchMode.Load()) {
	case FTSViewMatchOn:
		return true
	case FTSViewMatchSelective:
		return ftsTermHits(terms, hits) <= ftsViewMatchSelectiveHits
	}
	return false
}

// ftsTermHits sums the phrases' whole-table document counts.
func ftsTermHits(terms []string, hits map[string]int64) int64 {
	var total int64
	for _, term := range terms {
		total += hits[term]
	}
	return total
}

// FTSViewMatchDecision is one query's one-MATCH decision (diagnostics): its
// phrases, their whole-table document count in total, how many generations
// needed a MATCH, the mode, and whether one MATCH was used.
type FTSViewMatchDecision struct {
	Terms   []string `json:"terms"`
	Hits    int64    `json:"hits"`
	Need    int      `json:"need"`
	Mode    int      `json:"mode"`
	Engaged bool     `json:"engaged"`
	At      int64    `json:"at_unix_ms"`
}

var viewMatchDecisions struct {
	mu     sync.Mutex
	recent []FTSViewMatchDecision
}

const viewMatchDecisionsKept = 128

func recordViewMatchDecision(terms []string, hits map[string]int64, need int, engaged bool) {
	d := FTSViewMatchDecision{
		Terms: append([]string(nil), terms...), Hits: ftsTermHits(terms, hits), Need: need,
		Mode: int(ftsViewMatchMode.Load()), Engaged: engaged, At: time.Now().UnixMilli(),
	}
	viewMatchDecisions.mu.Lock()
	defer viewMatchDecisions.mu.Unlock()
	viewMatchDecisions.recent = append(viewMatchDecisions.recent, d)
	if n := len(viewMatchDecisions.recent); n > viewMatchDecisionsKept {
		viewMatchDecisions.recent = append([]FTSViewMatchDecision(nil), viewMatchDecisions.recent[n-viewMatchDecisionsKept:]...)
	}
}

// FTSViewMatchDecisions returns the most recent one-MATCH decisions.
func FTSViewMatchDecisions() []FTSViewMatchDecision {
	viewMatchDecisions.mu.Lock()
	defer viewMatchDecisions.mu.Unlock()
	return append([]FTSViewMatchDecision(nil), viewMatchDecisions.recent...)
}

func containsGeneration(generations []int64, generation int64) bool {
	for _, g := range generations {
		if g == generation {
			return true
		}
	}
	return false
}

func ftsRepoKey(repoAllow []string) string {
	if len(repoAllow) == 0 {
		return ""
	}
	repos := append([]string(nil), repoAllow...)
	sort.Strings(repos)
	return strconv.Itoa(len(repos)) + "\x00" + strings.Join(repos, "\x00")
}

// buildMatches keeps the rows matching at least one phrase, with their
// per-phrase instance counts, in rowid order.
func ftsBuildMatches(rows []FTSRankRow, terms, repoAllow []string) *ftsGenerationMatches {
	return ftsBuildMatchesTokenized(rows, nil, terms, repoAllow)
}

// ftsBuildMatchesTokenized is ftsBuildMatches over rows whose tokens are
// given (tokens[i] for rows[i]); a nil tokens tokenizes each row.
func ftsBuildMatchesTokenized(rows []FTSRankRow, tokens [][]string, terms, repoAllow []string) *ftsGenerationMatches {
	var allowed map[string]struct{}
	if len(repoAllow) > 0 {
		allowed = make(map[string]struct{}, len(repoAllow)+1)
		allowed[""] = struct{}{}
		for _, repo := range repoAllow {
			allowed[repo] = struct{}{}
		}
	}
	out := &ftsGenerationMatches{exact: true}
	scratch := make([]float64, len(terms))
	for i, row := range rows {
		if allowed != nil {
			if _, ok := allowed[row.RepoPrefix]; !ok {
				continue
			}
		}
		var rowTokens []string
		if tokens != nil {
			rowTokens = tokens[i]
		} else {
			var ok bool
			rowTokens, ok = ftsTokensOf(row.Tokens)
			if !ok {
				return &ftsGenerationMatches{exact: false}
			}
		}
		for k := range scratch {
			scratch[k] = 0
		}
		matched := false
		for _, tok := range rowTokens {
			for k, term := range terms {
				if strings.HasPrefix(tok, term) {
					scratch[k] += float64(1)
					matched = true
				}
			}
		}
		if !matched {
			continue
		}
		freq := append([]float64(nil), scratch...)
		size := row.TokenCount
		if size <= 0 {
			size = len(rowTokens)
		}
		out.rows = append(out.rows, ftsMatchRow{rowid: row.Rowid, nodeID: row.NodeID, freq: freq, size: size})
		if len(out.rows) > ftsCacheMaxRows {
			return &ftsGenerationMatches{exact: false}
		}
	}
	sort.Slice(out.rows, func(i, j int) bool { return out.rows[i].rowid < out.rows[j].rowid })
	return out
}
