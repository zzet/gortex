package search_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/search"
)

// memoryFixture: three dense generations of 60 documents each (41, 42, 43)
// and one of 400 (44).
func memoryFixture(t *testing.T) *ownedFTSTable {
	o := newOwnedFTSTable(t)
	rows := fixtureRows("m", 1000, 11)
	next := 0
	for _, g := range []struct {
		gen  int64
		docs int
	}{{41, 60}, {42, 60}, {43, 60}, {44, 400}} {
		for i := 0; i < g.docs; i++ {
			r := rows[next%len(rows)]
			next++
			r[0] = fmt.Sprintf("m%d", next)
			o.insert(t, g.gen, r)
		}
	}
	return o
}

func keptBytesOf(t *testing.T, ranker *search.FTSRanker, generation int64) int64 {
	t.Helper()
	docs, bytes := search.FTSKeptDocs()
	if _, err := ranker.WarmGenerations(context.Background(), []int64{generation}); err != nil {
		t.Fatal(err)
	}
	afterDocs, afterBytes := search.FTSKeptDocs()
	if afterDocs <= docs {
		t.Fatalf("generation %d was not kept", generation)
	}
	return afterBytes - bytes
}

// The kept documents are bounded in bytes: past the cap the least recently
// used generation is dropped (and read whole again when next needed); a
// generation larger than the cap on its own is not kept and keeps the MATCH
// read; Forget drops a generation at once. Every answer still equals FTS5's.
func TestKeptDocumentsAreBoundedAndDropped(t *testing.T) {
	ctx := context.Background()
	o := memoryFixture(t)
	probe := search.NewFTSRanker(memoryFixture(t))
	one := keptBytesOf(t, probe, 41) // the size of one 60-document generation
	probe.Forget(41)

	t.Cleanup(search.SetFTSKeptMaxBytesForTest(2*one + one/2)) // two fit, three do not
	ranker := search.NewFTSRanker(o)
	rankQ := func(generation int64, q string) {
		t.Helper()
		got, ok, err := ranker.Rank(ctx, generation, true, q, nil, 20)
		if err != nil || !ok {
			t.Fatalf("gen %d: ok=%v err=%v", generation, ok, err)
		}
		if want := o.storeRankOwned(t, generation, q, 20); renderHits(got) != renderHits(want) {
			t.Fatalf("gen %d:\n memory %s\n fts5   %s", generation, renderHits(got), renderHits(want))
		}
	}
	rank := func(generation int64) { t.Helper(); rankQ(generation, "checkout route") }
	wholeOf := func(generation int64) int { return wholeReads(o)[generation] }
	rank(41)
	rank(42)
	if _, err := ranker.WarmGenerations(ctx, []int64{41}); err != nil { // 41 used again: 42 is now the least recent
		t.Fatal(err)
	}
	if _, err := ranker.WarmGenerations(ctx, []int64{43}); err != nil { // over the cap: 42 goes
		t.Fatal(err)
	}
	if _, err := ranker.WarmGenerations(ctx, []int64{41, 43}); err != nil {
		t.Fatal(err)
	}
	if wholeOf(41) != 1 || wholeOf(43) != 1 {
		t.Fatalf("the recently used generations were read again: 41 x%d, 43 x%d", wholeOf(41), wholeOf(43))
	}
	// 42 was dropped at the cap: a query takes the MATCH read for it, and does
	// not read it whole again; the warm does.
	matchBefore := matchReads(o, 42)
	rankQ(42, "dirty sampler refresh")
	if wholeOf(42) != 1 || matchReads(o, 42) == matchBefore {
		t.Fatalf("a query after the drop: %d whole read(s) of 42, %d new MATCH read(s); want 1 and some", wholeOf(42), matchReads(o, 42)-matchBefore)
	}
	evictions := search.FTSDocsEvictions()
	if _, err := ranker.WarmGenerations(ctx, []int64{42}); err != nil {
		t.Fatal(err)
	}
	if wholeOf(42) != 2 {
		t.Fatalf("the warm did not read the dropped generation again: read whole %d time(s), want 2", wholeOf(42))
	}
	if search.FTSDocsEvictions() <= evictions-1 {
		t.Fatal("no eviction recorded")
	}

	// 400 documents do not fit the cap on their own: not kept, MATCH read.
	rank(44)
	rank(44)
	if wholeOf(44) != 1 || matchReads(o, 44) == 0 {
		t.Fatalf("the oversized generation: %d whole read(s), %d MATCH read(s); want 1 and some", wholeOf(44), matchReads(o, 44))
	}

	// Forget drops at once.
	docs, _ := search.FTSKeptDocs()
	ranker.Forget(42)
	if after, _ := search.FTSKeptDocs(); after >= docs {
		t.Fatalf("Forget dropped nothing: %d -> %d kept documents", docs, after)
	}
	rankQ(42, "flip coordinator")
	if wholeOf(42) != 3 {
		t.Fatalf("a forgotten generation was not read again on its next query (%d whole reads)", wholeOf(42))
	}
}

// fts_kept_bytes against the heap actually held: a generation of 20,000
// documents shaped like the live store's (a qualified name, a path and a
// signature per document: about 25 tokens of 6-9 characters), kept by a
// ranker, measured with the runtime's heap statistics.
func TestKeptBytesEstimateAgainstTheHeap(t *testing.T) {
	ctx := context.Background()
	o := newOwnedFTSTable(t)
	docs := 20_000
	if raw := os.Getenv("GORTEX_FTS_KEPT_DOCS"); raw != "" { // a larger run, e.g. 240000
		n, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatal(err)
		}
		docs = n
	}
	// One generation up to 20,000 documents; a larger run is split into
	// generations of the live base's sizes (82,584, 79,812, 16,282 and 61,324
	// documents: 240,002 in all), each under the whole-read bound.
	sizes := []int{docs}
	if docs > 20_000 {
		sizes = []int{82_584, 79_812, 16_282, 61_324}
		docs = 0
		for _, n := range sizes {
			docs += n
		}
	}
	var generations []int64
	gen, left := int64(50), sizes[0]
	generations = append(generations, gen)
	for i := 0; i < docs; i++ {
		if left == 0 {
			sizes = sizes[1:]
			gen++
			left = sizes[0]
			generations = append(generations, gen)
		}
		left--
		tokens := []string{
			"internal", "indexer", fmt.Sprintf("checkout%d", i%97), "coordinator", "go",
			fmt.Sprintf("flip%d", i), "checkout", "coordinator", "flip", "route",
			"generations", "context", "error", "func", "ctx", "store", "sqlite", "view",
			"generation", "publication", "applied", "ownership", "masks", "commit", "dirty",
		}
		o.insert(t, gen, [3]string{fmt.Sprintf("gortex/internal/indexer/checkout_coordinator%d.go::CheckoutCoordinator.flip%d", i%97, i), "gortex", strings.Join(tokens, " ")})
	}
	ranker := search.NewFTSRanker(o)
	// The table's own rows are read by the double into fresh strings too;
	// read once and drop, so the measured round keeps only what the ranker
	// holds.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, estimateBefore := search.FTSKeptDocs()
	started := time.Now()
	if _, err := ranker.WarmGenerations(ctx, generations); err != nil {
		t.Fatal(err)
	}
	warm := time.Since(started)
	runtime.GC()
	runtime.ReadMemStats(&after)
	_, estimateAfter := search.FTSKeptDocs()
	estimate := estimateAfter - estimateBefore
	measured := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	runtime.KeepAlive(ranker)
	t.Logf("%d documents: fts_kept_bytes %d (%.0f B/doc), heap held %d (%.0f B/doc), ratio %.2f; read, tokenize and keep %s",
		docs, estimate, float64(estimate)/float64(docs), measured, float64(measured)/float64(docs), float64(estimate)/float64(measured), warm)
	if estimate <= 0 || measured <= 0 || float64(estimate) < 0.5*float64(measured) || float64(estimate) > 2*float64(measured) {
		t.Fatalf("fts_kept_bytes %d is not within a factor of two of the heap held, %d", estimate, measured)
	}
}
