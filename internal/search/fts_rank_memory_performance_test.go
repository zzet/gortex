//go:build performance

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

// fts_kept_bytes against the heap actually held: a generation of 20,000
// documents shaped like the live store's (a qualified name, a path and a
// signature per document: about 25 tokens of 6-9 characters), kept by a
// ranker, measured with the runtime's heap statistics.
func TestPerformanceKeptBytesEstimateAgainstTheHeap(t *testing.T) {
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
