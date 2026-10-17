package indexer

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/progress"
)

// cloneSigMetaKey is the Node.Meta key under which a function/method's
// base64-encoded MinHash signature is stored. The graph-wide LSH pass
// reads it back out — keeping the signature on the node makes the pass
// a pure graph walk (no file IO), correct under incremental reindex,
// and safe across multi-repo graphs.
const cloneSigMetaKey = "clone_sig"

// cloneTokensMetaKey is the Node.Meta key under which the normalised-
// token count of a function/method body is stored alongside the clone
// signature. Used by the length-stratified LSH pass to bucket items
// into overlapping size classes so a pair with size ratio > ~1.6
// (Jaccard ≤ 0.625, well below the 0.82 clone threshold) is never
// considered as a candidate.
const cloneTokensMetaKey = "clone_tokens"

// cloneShinglesMetaKey is the Node.Meta key under which a function /
// method's raw shingle hash set is stashed during the per-file parse,
// so the global CMS-filter pass (finaliseCloneSignatures) can decide
// which shingles to exclude before computing the final MinHash
// signature. The entry is deleted from Meta as soon as the signature
// lands — it is intentionally short-lived because the shingle set is
// large (≈ tokens − 2 entries per body) and persisting it across the
// clone-detection pass would waste tens of MB on a monorepo.
const cloneShinglesMetaKey = "clone_shingles"

// cloneBodyMetaKey is the Node.Meta key under which a function/method's
// body identity is kept: a 64-bit FNV-1a hash of its sorted shingle set,
// hex-encoded. It is what lets a per-file delta tell a body it re-derived
// unchanged (its clone rows still hold) from a changed one, without the
// prior bytes. Additive: excluded from every fingerprint
// (fingerprintMetaKeys), and ignored by a binary that does not know it.
const cloneBodyMetaKey = "clone_body"

// cloneBodyTextIdentity is the body identity of a body too short to shingle:
// the same hash over its whitespace-normalised text, marked so it never equals
// a shingle-set identity.
func cloneBodyTextIdentity(body string) string {
	var h uint64 = 14695981039346656037
	for _, b := range []byte("t:" + strings.Join(strings.Fields(body), " ")) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return "t" + strconv.FormatUint(h, 16)
}

// cloneBodyIdentity is the body identity of a shingle set.
func cloneBodyIdentity(shingles []uint64) string {
	sorted := append([]uint64(nil), shingles...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var h uint64 = 14695981039346656037
	for _, s := range sorted {
		for i := 0; i < 8; i++ {
			h ^= (s >> (8 * i)) & 0xff
			h *= 1099511628211
		}
	}
	return strconv.FormatUint(h, 16)
}

// CMS-filter tuning.
//
// cmsBoilerplateRatio: a shingle appearing in more than this fraction
// of bodies is treated as boilerplate and excluded from signature
// computation. 1% is the textbook value used by near-duplicate web
// indexing systems and balances precision (false-clone suppression)
// against recall (genuine clones whose shared content happens to use
// a moderately common idiom).
//
// cmsMinCorpus: below this many bodies the global frequency
// distribution is too thin for the threshold to be meaningful — a
// 200-body repo has no shingle that legitimately appears in 2 bodies
// without already being noise — so we fall back to unfiltered MinHash.
// Around this size the LSH pass is also fast enough that filtering
// gains nothing.
//
// minSurvivingShingles: after filtering, a body with fewer
// discriminative shingles than this is dropped from clone detection
// entirely. MinHash over a handful of shingles produces random slot
// values that collide unpredictably in LSH bands; the body is then a
// false-clone factory, not a real clone source. Boilerplate-dominated
// bodies (e.g. trivial controller / DTO wrappers) land here.
const (
	cmsBoilerplateRatio  = 0.01
	minSurvivingShingles = 8
	// clonePairBatchSize bounds both sides of clone-edge materialisation.
	// Each relation contributes at most two endpoint IDs and two directed
	// edges, so one batch stays below SQLite's 5,000-ID lookup chunk and
	// caps the AddBatch payload at 4,096 edges. This avoids both the old
	// per-pair query/write N+1 and an unbounded all-pairs edge allocation.
	clonePairBatchSize = 2048
	// cloneCorpusFinalizeBatch bounds durable signature projection writes and
	// the transient raw-shingle payload retained outside the store pager.
	cloneCorpusFinalizeBatch = 1024
)

// cmsMinCorpus is the body-count floor below which the CMS boilerplate
// filter is disabled (useFilter=false) and the pass falls back to
// unfiltered MinHash — see the doc comment above for the rationale and
// default. It is a package-level var (not a const) purely so the clone
// equivalence tests can temporarily lower it to force useFilter=true on a
// small fixture and exercise the filtered batch/incremental paths; restore
// it via t.Cleanup. Production never mutates it — the default semantics are
// unchanged.
var cmsMinCorpus = 2000

// applyCloneSignatures is the per-file half of clone detection. It runs
// inside applyCoverageDomains (gated on the "clones" coverage domain),
// slices each function/method body out of the file source, computes a
// MinHash signature, and stamps it on the node's Meta. Bodies below
// clones.MinTokens normalised tokens produce no signature and are
// silently skipped — they are dominated by boilerplate and would only
// add noise to the LSH buckets.
//
// Allocation note: the body slicing path computes one []int of line
// offsets per file and one string per emitted body. The previous
// implementation went through splitLines (which materialises the
// whole source as N per-line Go strings) and a quadratic concat in
// bodyText (each iteration grew the output via "out += ..."). Profile
// showed bodyText + splitLinesUpTo at 3+ GiB per 30 s window — both
// are now O(file_bytes) one-shot allocations.
func applyCloneSignatures(src []byte, result *parser.ExtractionResult) {
	if result == nil || len(result.Nodes) == 0 {
		return
	}
	// Most data/config files contain no callable bodies. Check that before
	// building an O(lines) offset table, which can otherwise dominate
	// allocation for large generated JSON and other non-code sources.
	hasCallable := false
	for _, n := range result.Nodes {
		if n != nil && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
			hasCallable = true
			break
		}
	}
	if !hasCallable {
		return
	}

	// Compute newline offsets once per file rather than splitting the
	// source into N Go strings. offsets[i] is the byte index where
	// line i+1 (1-indexed) starts; the sentinel offsets[len(offsets)-1]
	// is len(src) so the slice math doesn't need a special case for
	// the last line.
	offsets := lineOffsets(src)
	for _, n := range result.Nodes {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		body := bodyTextFromOffsets(src, offsets, n.StartLine, n.EndLine)
		if body == "" {
			continue
		}
		// Stash the deduplicated shingle set rather than the final
		// MinHash signature: signature computation is deferred to the
		// global CMS-filter pass (finaliseCloneSignatures), which
		// derives a per-corpus boilerplate-shingle set and excludes it
		// from each body's signature. The shingle slice is short-lived
		// on Meta — finaliseCloneSignatures clears it after stamping
		// the real signature.
		shingles, tokens, ok := clones.Shingles(body)
		if !ok {
			// Too short to shingle: no clone rows, but the body identity is
			// still what tells a delta the body is unchanged (clone_carry.go),
			// taken over the whitespace-normalised text.
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			n.Meta[cloneBodyMetaKey] = cloneBodyTextIdentity(body)
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta[cloneShinglesMetaKey] = shingles
		n.Meta[cloneTokensMetaKey] = tokens
		n.Meta[cloneBodyMetaKey] = cloneBodyIdentity(shingles)
	}
}

// lineOffsets returns the byte offsets of each line in src. For a file
// with N lines the result has length N+1: the first entry is 0, each
// subsequent entry is the byte index immediately after a '\n', and the
// final sentinel is len(src) so callers can slice the last line as
// src[offsets[N-1]:offsets[N]] without special-casing EOF.
//
// One allocation (the []int) instead of N (one string per line via
// strings.Split). Lifetime is per-file: the caller drops the slice
// when the file's worker batch finishes.
func lineOffsets(src []byte) []int {
	// Reserve a generous initial capacity to avoid repeated slice
	// growth on typical source files (~ 200 lines). The slice grows
	// from here for larger files; small files waste a bit of headroom
	// that goes back to the GC immediately.
	offsets := make([]int, 1, 256)
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			offsets = append(offsets, i+1)
		}
	}
	offsets = append(offsets, len(src))
	return offsets
}

// bodyTextFromOffsets returns src[startLine..endLine] (both 1-indexed,
// inclusive) as one Go string. The trailing newline of the last
// included line is stripped so output matches the old line-join
// semantics ("a\nb" not "a\nb\n"). Returns "" for degenerate or
// out-of-bounds ranges, matching bodyText.
func bodyTextFromOffsets(src []byte, offsets []int, startLine, endLine int) string {
	if startLine <= 0 || endLine < startLine {
		return ""
	}
	lo := startLine - 1
	hi := endLine
	// len(offsets) = lineCount + 1 (sentinel). lineCount = len(offsets) - 1.
	lineCount := len(offsets) - 1
	if lo >= lineCount {
		return ""
	}
	if hi > lineCount {
		hi = lineCount
	}
	startOff := offsets[lo]
	endOff := offsets[hi]
	// Strip the trailing '\n' that bounds the last included line so the
	// output matches the line-join semantics callers and tests expect.
	if endOff > startOff && endOff <= len(src) && endOff-1 >= 0 && src[endOff-1] == '\n' {
		endOff--
	}
	return string(src[startOff:endOff])
}

// bodyText returns the source spanning [startLine, endLine] (both
// 1-indexed, inclusive) joined by newlines. Kept as a legacy helper
// for the unit-test surface; production callers go through
// applyCloneSignatures → bodyTextFromOffsets, which avoids both the
// whole-source string copy in splitLines and the O(N²) concat below.
func bodyText(lines []string, startLine, endLine int) string {
	if startLine <= 0 || endLine < startLine {
		return ""
	}
	lo := startLine - 1
	hi := endLine
	if lo >= len(lines) {
		return ""
	}
	if hi > len(lines) {
		hi = len(lines)
	}
	// Precompute the joined size so the strings.Builder grows once,
	// turning the previous O(N²) "out += ..." into O(total_bytes).
	total := 0
	for i := lo; i < hi; i++ {
		total += len(lines[i])
		if i > lo {
			total++ // separating '\n'
		}
	}
	var b strings.Builder
	b.Grow(total)
	for i := lo; i < hi; i++ {
		if i > lo {
			b.WriteByte('\n')
		}
		b.WriteString(lines[i])
	}
	return b.String()
}

// computeCloneSigFromShingles is the per-body signature kernel shared by
// the whole-graph finalise pass (finaliseCloneSignatures) and the
// incremental maintainer (incrementalCloneIndex.UpdateFuncs). Both paths
// MUST route through this function so a body's signature is byte-identical
// regardless of which path stamped it — that is what lets the equivalence
// test assert exact set equality between the batch and incremental clone
// edges.
//
// cms is the corpus Count-Min Sketch; threshold is the boilerplate cutoff
// (a shingle whose CMS count exceeds it is dropped). useFilter selects the
// branch:
//
//   - useFilter true: exclude high-frequency shingles, then require the
//     surviving set to clear minSurvivingShingles before computing MinHash.
//   - useFilter false: keep every shingle and apply no floor (legacy
//     small-corpus behaviour) — cms may be nil in this branch.
//
// Returns the signature and ok=false when the body is dropped from clone
// detection (empty / below the surviving floor) — the caller then leaves
// the node without a clone_sig, exactly as the batch pass does.
func computeCloneSigFromShingles(cms *clones.CMS, threshold uint32, useFilter bool, shingles []uint64) (clones.Signature, bool) {
	var filtered []uint64
	if useFilter {
		filtered = make([]uint64, 0, len(shingles))
		for _, sh := range shingles {
			if cms.Count(sh) > threshold {
				continue
			}
			filtered = append(filtered, sh)
		}
	} else {
		filtered = shingles
	}
	floor := minSurvivingShingles
	if !useFilter {
		// Without filtering, every shingle survives — fall back to the
		// legacy gate so we don't silently drop bodies the old code
		// would have kept.
		floor = 0
	}
	return clones.SignatureFromShingles(filtered, floor)
}

// finaliseCloneSignatures runs after every file's shingles have been
// stamped on its function / method nodes (by applyCloneSignatures
// during the per-file parse). It builds a Count-Min Sketch of shingle
// frequencies across every body in the graph, then walks the bodies
// again and computes a MinHash signature excluding shingles that
// exceed the boilerplate threshold (present in > cmsBoilerplateRatio
// of bodies). The stashed shingle set is cleared from Meta as soon as
// the signature lands so the LSH pass downstream sees the same
// node-shape the legacy path produced — just with cleaner signatures.
//
// Bodies whose surviving shingle count falls below minSurvivingShingles
// are dropped from clone detection entirely (no clone_sig stamp): a
// body whose token stream is dominated by boilerplate is, by
// definition, a controller / DTO / dispatch shape rather than
// distinguishable code, and including it in MinHash would just produce
// random LSH collisions.
//
// Below cmsMinCorpus bodies the corpus is too small for the
// frequency distribution to be meaningful; the pass falls back to
// unfiltered MinHash so small repos preserve the legacy behaviour.
//
// Caller must hold g.ResolveMutex() — the function mutates Node.Meta
// (deletes clone_shingles, sets clone_sig) across nodes that other
// graph-wide passes (markTestSymbolsAndEmitEdges, ResolveTemporalCalls,
// reach.BuildIndex) also touch under the same mutex.
//
// Repo-scoped: only bodies whose n.RepoPrefix == repoPrefix enter the
// CMS / signature passes, so a multi-repo graph computes each repo's
// boilerplate sketch and per-body signatures from that repo's bodies
// alone — clone detection is per-repository. A standalone single-repo
// Indexer uses repoPrefix == "" and its nodes carry RepoPrefix == "",
// so the equality matches every node and behaviour is unchanged.
// cloneRepoNodes returns the nodes the per-repo clone passes must walk. In
// daemon multi-repo mode repoPrefix is non-empty, so GetRepoNodes selects just
// that repo's nodes (one backend query, and one meta decode per repo node)
// instead of decoding every node in a many-repo graph only to discard the other
// repos. Empty prefix is an exact single-repository predicate: the in-memory
// graph reads those nodes directly from its shards without creating a duplicate
// byRepo index or a graph-wide snapshot. The clone passes read blob-only Meta
// (clone_sig / clone_tokens / clone_shingles), so the full GetRepoNodes — not
// the meta-less light reader — is required here.
func cloneRepoNodes(g graph.Store, repoPrefix string) []*graph.Node {
	return g.GetRepoNodes(repoPrefix)
}

// cloneCorpusBaseline is the complete in-memory seed produced while the
// compact clone corpus is finalized. Successful paged finalization banks each
// signed row directly into the persistent stratified LSH; the global detector
// scans that retained index and the incremental maintainer then adopts it with
// the CMS, raw-shingle cache and corpus count. No item slice, duplicate LSH, or
// second corpus scan is needed on that path. A legacy compatibility scan may
// also prove the corpus structurally empty; that complete empty baseline avoids
// repeating the same full-node read. Cancelled, error, and non-empty legacy
// paths remain incomplete so the maintainer keeps its bounded Rebuild fallback.
type cloneCorpusBaseline struct {
	repoPrefix string
	cms        *clones.CMS
	lsh        *clones.StratifiedIndex
	shingles   map[string][]uint64
	items      []clones.Item // legacy fallback only
	itemCount  int
	corpus     int
	complete   bool
}

// finaliseCloneCorpusCtx takes the SQLite-first compact projection path. It
// keyset-pages the corpus twice only when at least one row is pending; an
// unchanged warm corpus is read once and performs no writes. A successful
// paged pass returns a complete baseline suitable for single-consumer adoption.
func finaliseCloneCorpusCtx(ctx context.Context, g graph.Store, repoPrefix string) *cloneCorpusBaseline {
	baseline := &cloneCorpusBaseline{repoPrefix: repoPrefix}
	pager, paged := g.(graph.CloneCorpusPager)
	writer, writable := g.(graph.CloneCorpusWriter)
	signatureWriter, signatureOnly := g.(graph.CloneCorpusSignatureWriter)
	initialization, stateful := g.(graph.CloneCorpusInitialization)
	if !paged || !writable {
		baseline.items, baseline.corpus = finaliseCloneSignaturesFromNodes(g, repoPrefix)
		if baseline.corpus == 0 && len(baseline.items) == 0 {
			completeEmptyCloneBaseline(baseline)
		}
		return baseline
	}

	baseline.cms = clones.NewCMS(65536, 4)
	baseline.lsh = clones.NewStratifiedIndex()
	baseline.shingles = make(map[string][]uint64, cloneCorpusFinalizeBatch)
	addItem := func(item clones.Item) {
		baseline.lsh.Add(item)
		baseline.itemCount++
	}
	abort := func() *cloneCorpusBaseline {
		baseline.cms = nil
		baseline.lsh = nil
		baseline.shingles = nil
		baseline.items = nil
		baseline.itemCount = 0
		baseline.corpus = 0
		baseline.complete = false
		return baseline
	}
	corpus, pending := 0, false
	after := ""
	for {
		if ctx.Err() != nil {
			return abort()
		}
		page, err := pager.CloneCorpusPage(repoPrefix, after, cloneCorpusFinalizeBatch)
		if err != nil {
			return abort()
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			corpus++
			if len(row.Shingles) > 0 {
				baseline.shingles[row.NodeID] = row.Shingles
			}
			for _, shingle := range row.Shingles {
				baseline.cms.Add(shingle)
			}
			if !row.Finalized {
				pending = true
				continue
			}
			if row.Signature == "" {
				continue
			}
			sig, ok := clones.DecodeSignature(row.Signature)
			if !ok {
				pending = true
				continue
			}
			addItem(clones.Item{ID: row.NodeID, Sig: sig, TokenCount: row.TokenCount})
		}
		after = page[len(page)-1].NodeID
		if len(page) < cloneCorpusFinalizeBatch {
			break
		}
	}
	baseline.corpus = corpus
	if corpus == 0 {
		// An explicit initialization row makes an empty page authoritative. This
		// is the common genuinely-empty repository path and avoids decoding every
		// node merely to prove the absence of clone bodies again.
		if stateful {
			initialized, err := initialization.CloneCorpusInitialized(repoPrefix)
			if err == nil && initialized {
				completeEmptyCloneBaseline(baseline)
				return baseline
			}
		}

		// Compatibility for a pre-projection store: one legacy scoped read
		// populates the sidecar, after which every restart takes the paged path.
		baseline.itemCount = 0
		baseline.items, baseline.corpus = finaliseCloneSignaturesFromNodes(g, repoPrefix)
		if baseline.corpus == 0 && len(baseline.items) == 0 {
			// The compatibility scan proves this repository has neither pending
			// bodies nor finalized signatures. Persist that fact when supported;
			// adapters without the optional capability preserve the old fallback.
			completeEmptyCloneBaseline(baseline)
			if stateful {
				_ = initialization.MarkCloneCorpusInitialized(repoPrefix)
			}
		} else {
			// Non-empty compatibility data still needs the exact Rebuild path.
			// Release the speculative compact seed before returning it.
			baseline.cms = nil
			baseline.lsh = nil
			baseline.shingles = nil
		}
		return baseline
	}
	if !pending {
		baseline.complete = true
		return baseline
	}

	baseline.lsh = clones.NewStratifiedIndex()
	baseline.itemCount = 0
	useFilter := corpus >= cmsMinCorpus
	threshold := uint32(0)
	if useFilter {
		threshold = uint32(float64(corpus) * cmsBoilerplateRatio)
		if threshold < 1 {
			threshold = 1
		}
	}
	after = ""
	for {
		if ctx.Err() != nil {
			return abort()
		}
		page, err := pager.CloneCorpusPage(repoPrefix, after, cloneCorpusFinalizeBatch)
		if err != nil {
			return abort()
		}
		if len(page) == 0 {
			break
		}
		var signatureUpdates []graph.CloneCorpusSignatureUpdate
		if signatureOnly {
			signatureUpdates = make([]graph.CloneCorpusSignatureUpdate, 0, len(page))
		}
		for i := range page {
			row := &page[i]
			sig, ok := computeCloneSigFromShingles(baseline.cms, threshold, useFilter, row.Shingles)
			row.Finalized = true
			row.Signature = ""
			if ok {
				row.Signature = clones.EncodeSignature(sig)
				addItem(clones.Item{ID: row.NodeID, Sig: sig, TokenCount: row.TokenCount})
			}
			if signatureOnly {
				signatureUpdates = append(signatureUpdates, graph.CloneCorpusSignatureUpdate{
					NodeID: row.NodeID, Signature: row.Signature,
				})
			}
		}
		if signatureOnly {
			err = signatureWriter.BulkSetCloneSignatures(repoPrefix, signatureUpdates)
		} else {
			err = writer.BulkSetCloneCorpus(repoPrefix, page)
		}
		if err != nil {
			return abort()
		}
		after = page[len(page)-1].NodeID
		if len(page) < cloneCorpusFinalizeBatch {
			break
		}
	}
	baseline.complete = true
	return baseline
}

func completeEmptyCloneBaseline(baseline *cloneCorpusBaseline) {
	if baseline.cms == nil {
		baseline.cms = clones.NewCMS(65536, 4)
	}
	if baseline.lsh == nil {
		baseline.lsh = clones.NewStratifiedIndex()
	}
	if baseline.shingles == nil {
		baseline.shingles = make(map[string][]uint64)
	}
	baseline.items = nil
	baseline.itemCount = 0
	baseline.corpus = 0
	baseline.complete = true
}

func finaliseCloneSignaturesFromNodes(g graph.Store, repoPrefix string) ([]clones.Item, int) {
	// First pass: collect every body that has stashed shingles. We
	// capture the *graph.Node pointers up front so the CMS-build pass
	// and the signature-compute pass don't both re-read the repo projection.
	repoNodes := cloneRepoNodes(g, repoPrefix)
	capHint := min(len(repoNodes), 8192)
	bodies := make([]*graph.Node, 0, capHint)
	// A legacy/in-memory graph may already carry a finalized clone_sig in
	// Node.Meta without a raw-shingle row. Keep those items in the detection
	// corpus. The SQLite-first projection normally supplies the same rows via
	// CloneCorpusPage, but the node fallback must remain compatible with graphs
	// built before that projection existed and with focused in-memory callers.
	items := make([]clones.Item, 0, capHint)
	for _, n := range repoNodes {
		if n == nil || n.Meta == nil {
			continue
		}
		if n.RepoPrefix != repoPrefix {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if _, ok := n.Meta[cloneShinglesMetaKey].([]uint64); ok {
			bodies = append(bodies, n)
			continue
		}
		encoded, _ := n.Meta[cloneSigMetaKey].(string)
		sig, ok := clones.DecodeSignature(encoded)
		if !ok {
			continue
		}
		items = append(items, clones.Item{
			ID: n.ID, Sig: sig, TokenCount: tokensFromMeta(n),
		})
	}
	if len(bodies) == 0 {
		return items, 0
	}

	useFilter := len(bodies) >= cmsMinCorpus
	var cms *clones.CMS
	var threshold uint32
	if useFilter {
		// Default sketch sizing — see the CMS doc comment for the
		// width/depth → ε/δ derivation. 1 MB peak for a transient,
		// per-build pass is comfortably below any constraint.
		cms = clones.NewCMS(65536, 4)
		for _, n := range bodies {
			shingles, _ := n.Meta[cloneShinglesMetaKey].([]uint64)
			for _, sh := range shingles {
				cms.Add(sh)
			}
		}
		threshold = uint32(float64(len(bodies)) * cmsBoilerplateRatio)
		if threshold < 1 {
			threshold = 1
		}
	}

	// Persist each body's raw shingle set to the clone_shingles sidecar
	// BEFORE deleting it from Meta. This loop walks EVERY body in the
	// corpus — both the survivors (which get a clone_sig below) and the
	// boilerplate-dropped bodies (which do not) — persisting any with a
	// non-empty shingle set. That is deliberate: incrementalCloneIndex.
	// Rebuild reseeds its CMS + corpus from these rows and must mirror
	// the bodies set this pass used to build its own CMS / threshold,
	// which is ALL eligible bodies, not just survivors. Persisting only
	// survivors here would under-seed Rebuild's sketch and skew the
	// incremental threshold away from the batch one. Meta stays lean
	// (the shingle set is large and only the CMS pass needs it), but the
	// durable sidecar copy lets a warm restart rebuild the incremental
	// CMS without re-parsing every body. Accumulate per node.RepoPrefix
	// so a multi-repo graph reseeds each repo's CMS in isolation.
	// Backends that don't implement CloneShingleWriter (no on-disk store)
	// simply skip this — the in-session incremental index caches shingles
	// in memory regardless.
	// Second pass: signature computation. Each body either lands a
	// fresh clone_sig (signature over surviving shingles) or is
	// dropped entirely (no clone_sig, never enters detection items
	// list). In both cases clone_shingles is removed from Meta. The
	// per-body kernel is computeCloneSigFromShingles — the incremental
	// maintainer calls the same kernel so signatures match exactly.
	projection := make([]graph.CloneCorpusRow, 0, min(len(bodies), cloneCorpusFinalizeBatch))
	writer, hasWriter := g.(graph.CloneCorpusWriter)
	flushProjection := func() {
		if hasWriter && len(projection) > 0 {
			_ = writer.BulkSetCloneCorpus(repoPrefix, projection)
		}
		projection = projection[:0]
	}
	for _, n := range bodies {
		shingles, _ := n.Meta[cloneShinglesMetaKey].([]uint64)
		sig, ok := computeCloneSigFromShingles(cms, threshold, useFilter, shingles)
		delete(n.Meta, cloneShinglesMetaKey)
		row := graph.CloneCorpusRow{
			NodeID: n.ID, RepoPrefix: n.RepoPrefix, Shingles: shingles,
			TokenCount: tokensFromMeta(n), Finalized: true,
		}
		if !ok {
			// Boilerplate-dominated or empty after filter — drop
			// from clone detection. detectClonesAndEmitEdges skips
			// nodes without a clone_sig.
			delete(n.Meta, cloneSigMetaKey)
			projection = append(projection, row)
			if len(projection) == cloneCorpusFinalizeBatch {
				flushProjection()
			}
			continue
		}
		row.Signature = clones.EncodeSignature(sig)
		n.Meta[cloneSigMetaKey] = row.Signature
		projection = append(projection, row)
		items = append(items, clones.Item{ID: n.ID, Sig: sig, TokenCount: row.TokenCount})
		if len(projection) == cloneCorpusFinalizeBatch {
			flushProjection()
		}
	}
	flushProjection()
	return items, len(bodies)
}

// CloneDetectionStats summarises one clone-detection run for the caller's
// logger. Exposed so the orchestrator can surface what the
// per-bucket cap dropped — a high skippedBucketItems means the
// workspace has a lot of templated boilerplate that LSH would have
// over-fanned-out on.
type CloneDetectionStats struct {
	Items              int // function/method nodes with a signature
	Pairs              int // detected clone pairs (after Jaccard filter)
	Edges              int // EdgeSimilarTo emitted (≈ 2·Pairs, modulo dedup)
	SkippedBuckets     int // LSH buckets dropped for exceeding maxBucketSize
	SkippedBucketItems int // total items inside the dropped buckets
	DiffusedPairs      int // semantically-related pairs surviving threshold+cap
	DiffusedEdges      int // EdgeSemanticallyRelated emitted (= 2·DiffusedPairs)
}

// detectClonesAndEmitEdgesWithBaselineCtx performs the context-aware clone
// pass and also returns the finalized compact-corpus baseline. The baseline is
// complete only when the paged projection was read successfully; callers hand
// it to AdoptBaselineOrRebuild after this function releases ResolveMutex.
func detectClonesAndEmitEdgesWithBaselineCtx(ctx context.Context, g graph.Store, repoPrefix string, threshold float64) (CloneDetectionStats, *cloneCorpusBaseline) {
	var stats CloneDetectionStats
	if g == nil {
		return stats, nil
	}
	reporter := progress.FromContext(ctx)
	// Serialise against other graph-wide passes that mutate Node.Meta
	// (markTestSymbolsAndEmitEdges, ResolveTemporalCalls, reach.BuildIndex,
	// releases enrichment). Without this lock, the AllNodes walk below
	// reads n.Meta while one of those writers mutates the same map and
	// the runtime aborts with "concurrent map read and map write" — the
	// observed daemon crash. Shares g.ResolveMutex() so all such passes
	// rendezvous on the same lock the resolver already uses.
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()

	// Finalise pending signatures: applyCloneSignatures stamped the
	// raw shingle set on each function/method node during the per-file
	// parse. This pass builds a Count-Min Sketch of corpus-wide shingle
	// frequencies, then computes the MinHash signature for each body
	// after excluding shingles whose frequency exceeds the boilerplate
	// threshold. The expensive LSH candidate enumeration that comes
	// next then runs over signatures that reflect discriminative
	// content only — k8s-style controller-pattern bodies stop colliding
	// on shared "if v err return v" / "( v . v )" shingles, which is
	// what drives the LSH bucket explosion at monorepo scale.
	//
	// Runs under the existing g.ResolveMutex() so the Meta mutations
	// (delete clone_shingles, set clone_sig) don't race the AllNodes
	// walk below.
	reporter.Report("clones: CMS-finalise signatures", 0, 0)
	baseline := finaliseCloneCorpusCtx(ctx, g, repoPrefix)
	stats.Items = baseline.itemCount
	if !baseline.complete || baseline.lsh == nil {
		stats.Items = len(baseline.items)
	}
	if stats.Items < 2 {
		return stats, baseline
	}

	reporter.Report("clones: LSH + Jaccard filter", stats.Items, 0)
	var detected []clones.Pair
	var sb, sbi int
	if baseline.complete && baseline.lsh != nil {
		detected, sb, sbi = baseline.lsh.DetectPairsWithStats(threshold)
	} else {
		detected, sb, sbi = clones.DetectPairsStratifiedWithStats(baseline.items, threshold)
	}
	stats.SkippedBuckets = sb
	stats.SkippedBucketItems = sbi
	stats.Pairs = len(detected)
	reporter.Report("clones: emit similarity edges", len(detected), 0)
	directPairs := make(map[[2]string]struct{}, len(detected))
	_, stats.Edges = materializeClonePairs(g, detected, graph.EdgeSimilarTo, directPairs)

	// Graph-diffusion smoothing. Runs here, after the direct clone
	// edges are materialised, while detectClonesAndEmitEdges still
	// holds g.ResolveMutex — the diffusion pass mutates Node-adjacent
	// edge state and must rendezvous on the same lock as the clone
	// pass it extends.
	reporter.Report("clones: diffuse similarity edges", 0, 0)
	dp, de := diffuseSimilarityEdges(g, detected, directPairs)
	stats.DiffusedPairs = dp
	stats.DiffusedEdges = de
	return stats, baseline
}

// Diffusion-pass tuning constants. The graph-diffusion smoothing pass
// blends direct clone similarities across one shared neighbour, then
// threshold-gates and caps the result so the semantically-related edge
// set stays bounded — it must never explode the graph's edge count.
const (
	// diffusionDamping discounts a two-hop blended score relative to
	// the direct clone similarities it is derived from. The diffused
	// score for a pair (A,C) bridged by B is
	//   damping · similarity(A,B) · similarity(B,C)
	// — a product (already ≤ each factor) further damped, so a
	// transitive relation is always weaker evidence than either
	// direct clone link it rests on. 0.9 keeps a strong A~B~C chain
	// comfortably above the emit threshold while still ranking it
	// below a genuine clone.
	diffusionDamping = 0.9
	// diffusionThreshold is the minimum diffused score for a pair to
	// be materialised as an EdgeSemanticallyRelated edge. Set below
	// the clone DefaultThreshold (0.82): the whole point of the pass
	// is to surface relatedness the clone filter rejected, so the
	// gate must admit sub-clone scores — but high enough that a chain
	// through two weak (~0.5) clone links is dropped as noise.
	diffusionThreshold = 0.55
	// diffusionMaxNeighbors caps the clone-graph fan-out considered
	// per node. A node in a large clone cluster (templated
	// boilerplate) would otherwise contribute a quadratic burst of
	// diffused pairs; bounding the per-node neighbour set keeps the
	// pass near-linear. Neighbours are taken in descending direct
	// similarity so the strongest links survive the cap.
	diffusionMaxNeighbors = 16
	// diffusionMaxPairs is the hard ceiling on emitted
	// semantically-related pairs across the whole graph. Pairs are
	// ranked by diffused score (descending) before the cut, so the
	// strongest relations survive when the ceiling binds. Two
	// directed edges are emitted per surviving pair.
	diffusionMaxPairs = 50000
)

// canonicalPair returns the (smaller, larger) ordering of two IDs so a
// pair has a single key regardless of argument order.
func canonicalPair(a, b string) [2]string {
	if a <= b {
		return [2]string{a, b}
	}
	return [2]string{b, a}
}

// diffusionEdge is one weighted link in the in-memory similarity graph
// the diffusion pass walks — a neighbour ID and the direct clone score.
type diffusionEdge struct {
	id    string
	score float64
}

// diffuseSimilarityEdges is the graph-diffusion smoothing pass. It
// takes the direct clone pairs produced by the LSH filter, builds the
// undirected similarity graph they describe, and for every pair (A,C)
// joined through a shared neighbour B derives a damped two-hop score.
// Surviving pairs (above diffusionThreshold, not already a direct
// clone, capped at diffusionMaxPairs) are materialised as a symmetric
// pair of EdgeSemanticallyRelated edges.
//
// The blend is a bounded 1-to-2-hop transitive product — not a dense
// O(n²) diffusion. It is deterministic: neighbour lists are sorted, the
// score for a pair is the max over its bridging neighbours (an
// associative reduction independent of visitation order), and the
// final cap cuts a score-sorted slice with ID tie-breaks.
//
// directPairs carries the canonicalised clone pairs already emitted as
// EdgeSimilarTo; any pair in that set is skipped so semantically_related
// and similar_to partition cleanly.
func diffuseSimilarityEdges(g graph.Store, pairs []clones.Pair, directPairs map[[2]string]struct{}) (diffusedPairs, diffusedEdges int) {
	if g == nil || len(pairs) < 2 {
		return 0, 0
	}

	// Adjacency: id → its similar neighbours with direct scores. Each
	// undirected clone pair contributes an entry on both endpoints.
	adj := make(map[string][]diffusionEdge)
	for _, p := range pairs {
		adj[p.A] = append(adj[p.A], diffusionEdge{id: p.B, score: p.Similarity})
		adj[p.B] = append(adj[p.B], diffusionEdge{id: p.A, score: p.Similarity})
	}

	// Sort each neighbour list by descending score (ID tie-break) and
	// apply the per-node fan-out cap. Sorting also makes the pair
	// enumeration below deterministic.
	for id, nbrs := range adj {
		sort.Slice(nbrs, func(i, j int) bool {
			if nbrs[i].score != nbrs[j].score {
				return nbrs[i].score > nbrs[j].score
			}
			return nbrs[i].id < nbrs[j].id
		})
		if len(nbrs) > diffusionMaxNeighbors {
			adj[id] = nbrs[:diffusionMaxNeighbors]
		}
	}

	// For each bridge node B, every unordered pair of its neighbours
	// (A,C) is a candidate two-hop relation. The diffused score is the
	// damped product of the two clone links; when multiple bridges
	// connect the same (A,C) the strongest (max) bridge wins.
	best := make(map[[2]string]float64)
	bridges := make([]string, 0, len(adj))
	for id := range adj {
		bridges = append(bridges, id)
	}
	sort.Strings(bridges)
	for _, b := range bridges {
		nbrs := adj[b]
		for i := range nbrs {
			for j := i + 1; j < len(nbrs); j++ {
				a, c := nbrs[i].id, nbrs[j].id
				if a == c {
					continue
				}
				key := canonicalPair(a, c)
				if _, isClone := directPairs[key]; isClone {
					continue // a direct clone — stays similar_to only
				}
				score := diffusionDamping * nbrs[i].score * nbrs[j].score
				if score < diffusionThreshold {
					continue
				}
				if score > best[key] {
					best[key] = score
				}
			}
		}
	}
	if len(best) == 0 {
		return 0, 0
	}

	// Rank surviving pairs by diffused score so the global cap keeps
	// the strongest relations; ID tie-breaks keep the cut deterministic.
	ranked := make([]clones.Pair, 0, len(best))
	for key, score := range best {
		ranked = append(ranked, clones.Pair{A: key[0], B: key[1], Similarity: score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Similarity != ranked[j].Similarity {
			return ranked[i].Similarity > ranked[j].Similarity
		}
		if ranked[i].A != ranked[j].A {
			return ranked[i].A < ranked[j].A
		}
		return ranked[i].B < ranked[j].B
	})
	if len(ranked) > diffusionMaxPairs {
		ranked = ranked[:diffusionMaxPairs]
	}

	return materializeClonePairs(g, ranked, graph.EdgeSemanticallyRelated, nil)
}

// materializeClonePairs persists symmetric clone-derived relations without a
// point-read or write per pair. Input is consumed in bounded chunks: endpoint
// nodes are prefetched once with GetNodesByIDs, directed edges are staged and
// deduplicated, then one AddBatch persists the chunk. seenPairs may be supplied
// by the caller to both suppress duplicate writes and retain the valid direct
// pair set for diffusion. Only pairs whose two endpoints exist are added to it.
//
// The returned counts intentionally preserve the old logical counters: every
// valid input occurrence contributes one pair and two edges even when a
// duplicate occurrence is suppressed from the physical write. Clone detection
// normally returns unique pairs, while incremental queries can surface a pair
// once from each newly-added endpoint.
func materializeClonePairs(g graph.Store, pairs []clones.Pair, kind graph.EdgeKind, seenPairs map[[2]string]struct{}) (materializedPairs, logicalEdges int) {
	if g == nil || len(pairs) == 0 {
		return 0, 0
	}
	if seenPairs == nil {
		seenPairs = make(map[[2]string]struct{}, min(len(pairs), clonePairBatchSize))
	}

	type pendingPair struct {
		pair        clones.Pair
		key         [2]string
		occurrences int
	}
	type edgeIdentity struct {
		from, to, file string
		kind           graph.EdgeKind
		line           int
	}

	for start := 0; start < len(pairs); start += clonePairBatchSize {
		end := min(start+clonePairBatchSize, len(pairs))
		pending := make([]pendingPair, 0, end-start)
		pendingByKey := make(map[[2]string]int, end-start)

		// Dedupe before the endpoint query. A duplicate already materialised
		// by an earlier chunk is known to have valid endpoints, so it still
		// contributes to the logical counters without another read or write.
		for _, pair := range pairs[start:end] {
			key := canonicalPair(pair.A, pair.B)
			if _, seen := seenPairs[key]; seen {
				materializedPairs++
				logicalEdges += 2
				continue
			}
			if pos, exists := pendingByKey[key]; exists {
				pending[pos].occurrences++
				continue
			}
			pendingByKey[key] = len(pending)
			pending = append(pending, pendingPair{pair: pair, key: key, occurrences: 1})
		}
		if len(pending) == 0 {
			continue
		}

		idSet := make(map[string]struct{}, 2*len(pending))
		for _, item := range pending {
			idSet[item.pair.A] = struct{}{}
			idSet[item.pair.B] = struct{}{}
		}
		ids := make([]string, 0, len(idSet))
		for id := range idSet {
			if id != "" {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		nodes := g.GetNodesByIDs(ids)

		edges := make([]*graph.Edge, 0, 2*len(pending))
		edgeSeen := make(map[edgeIdentity]struct{}, 2*len(pending))
		stage := func(from, to *graph.Node, similarity float64) {
			edge := cloneRelationEdge(kind, from, to, similarity)
			key := edgeIdentity{from: edge.From, to: edge.To, kind: edge.Kind, file: edge.FilePath, line: edge.Line}
			if _, duplicate := edgeSeen[key]; duplicate {
				return
			}
			edgeSeen[key] = struct{}{}
			edges = append(edges, edge)
		}
		for _, item := range pending {
			from := nodes[item.pair.A]
			to := nodes[item.pair.B]
			if from == nil || to == nil {
				continue
			}
			seenPairs[item.key] = struct{}{}
			materializedPairs += item.occurrences
			logicalEdges += 2 * item.occurrences
			stage(from, to, item.pair.Similarity)
			stage(to, from, item.pair.Similarity)
		}
		if len(edges) > 0 {
			g.AddBatch(nil, edges)
		}
	}
	return materializedPairs, logicalEdges
}

// cloneRelationEdge builds one directed clone-derived edge. Both direct
// similarity and diffused relatedness share the same locality, provenance and
// score metadata; only their edge kind differs.
func cloneRelationEdge(kind graph.EdgeKind, from, to *graph.Node, similarity float64) *graph.Edge {
	return &graph.Edge{
		From:       from.ID,
		To:         to.ID,
		Kind:       kind,
		FilePath:   from.FilePath,
		Line:       from.StartLine,
		Confidence: similarity,
		Origin:     graph.OriginASTInferred,
		Meta:       map[string]any{"similarity": similarity},
	}
}
