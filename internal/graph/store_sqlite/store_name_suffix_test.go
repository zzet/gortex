package store_sqlite

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// nameIndexFixture builds an index the way loadGenerationNameIndex does, from
// names in id order: repeated fragments (so a name can hold the needle more
// than once), mixed case, and non-ASCII names whose Go and LIKE folding differ.
func nameIndexFixture(n int, seed int64) *generationNameIndex {
	return nameIndexFixtureFrom(n, seed, []string{"Get", "get", "Handler", "ab", "AB", "Store", "x", "Node", "Édit", "straße", "ÄÖ", "_", "1"})
}

func nameIndexFixtureFrom(n int, seed int64, parts []string) *generationNameIndex {
	r := rand.New(rand.NewSource(seed))
	idx := &generationNameIndex{}
	for i := 0; i < n; i++ {
		var b strings.Builder
		for k := 0; k < 1+r.Intn(5); k++ {
			b.WriteString(parts[r.Intn(len(parts))])
		}
		name := b.String()
		e := generationNameEntry{id: fmt.Sprintf("repo/f.go::%07d", i), fold: strings.ToLower(name)}
		if !isASCII(name) {
			e.raw = name
		}
		idx.entries = append(idx.entries, e)
	}
	return idx
}

// The suffix-array lookup answers exactly what the scan answers — same ids,
// same (id) order, same limit — for both foldings, over names that repeat the
// needle and names with non-ASCII bytes.
func TestNameSuffixArrayAnswersExactlyLikeTheScan(t *testing.T) {
	prev := generationNameSuffixMinEntries
	generationNameSuffixMinEntries = 1
	t.Cleanup(func() { generationNameSuffixMinEntries = prev })
	ctx := context.Background()
	idx := nameIndexFixture(6000, 7)
	scan := &generationNameIndex{entries: idx.entries}
	idx.buildSuffixArray()
	require.NotNil(t, idx.sa)
	for _, raw := range []string{"get", "Get", "ab", "abab", "handler", "gethandler", "x", "é", "É", "straß", "ß", "ä", "1_", "zzz", "_", "storenode"} {
		for _, likeFolding := range []bool{true, false} {
			needle := strings.ToLower(raw)
			match := func(e *generationNameEntry) bool { return strings.Contains(e.fold, needle) }
			if likeFolding {
				needle = asciiLower(raw)
				match = func(e *generationNameEntry) bool { return likeContains(e, needle) }
			}
			for _, limit := range []int{0, 1, 37} {
				want, err := scan.matchingIDs(ctx, limit, match)
				require.NoError(t, err)
				got, err := idx.containingIDs(ctx, limit, needle, likeFolding, match)
				require.NoError(t, err)
				require.Equal(t, want, got, "needle %q like=%t limit=%d", raw, likeFolding, limit)
			}
		}
	}
}

// The lookup is logarithmic in the index: a needle with few matches costs
// about the same on an index 16× larger, where the scan grows with it.
func TestNameSuffixArrayLookupDoesNotScan(t *testing.T) {
	prev := generationNameSuffixMinEntries
	generationNameSuffixMinEntries = 1
	t.Cleanup(func() { generationNameSuffixMinEntries = prev })
	ctx := context.Background()
	measure := func(n int) (sa, scan time.Duration) {
		// Real names are overwhelmingly ASCII; the few others are checked
		// one by one.
		idx := nameIndexFixtureFrom(n, 11, []string{"Get", "Handler", "ab", "Store", "x", "Node", "_", "1", "Resolve", "Edge"})
		idx.entries = append(idx.entries, generationNameEntry{id: "repo/z.go::needle", fold: "qqqneedleqqq"})
		sort.Slice(idx.entries, func(i, j int) bool { return idx.entries[i].id < idx.entries[j].id })
		idx.buildSuffixArray()
		plain := &generationNameIndex{entries: idx.entries}
		match := func(e *generationNameEntry) bool { return strings.Contains(e.fold, "needle") }
		const rounds = 200
		start := time.Now()
		for i := 0; i < rounds; i++ {
			ids, err := idx.containingIDs(ctx, 0, "needle", false, match)
			require.NoError(t, err)
			require.Len(t, ids, 1)
		}
		sa = time.Since(start) / rounds
		start = time.Now()
		for i := 0; i < rounds; i++ {
			ids, err := plain.containingIDs(ctx, 0, "needle", false, match)
			require.NoError(t, err)
			require.Len(t, ids, 1)
		}
		return sa, time.Since(start) / rounds
	}
	smallSA, smallScan := measure(10_000)
	bigSA, bigScan := measure(160_000)
	t.Logf("10k: suffix %s scan %s; 160k: suffix %s scan %s", smallSA, smallScan, bigSA, bigScan)
	require.Less(t, bigSA, bigScan/20, "the suffix lookup must not scale with the index")
}

// A sealed generation's index, loaded the way a lookup loads it, carries the
// suffix array once it is large enough.
func TestSealedNameIndexCarriesTheSuffixArray(t *testing.T) {
	prev := generationNameSuffixMinEntries
	generationNameSuffixMinEntries = 1
	t.Cleanup(func() { generationNameSuffixMinEntries = prev })
	ctx := context.Background()
	store, generationID := publishedStampGeneration(t)
	index, ok := store.AtGeneration(generationID).sealedNameIndex(ctx)
	require.True(t, ok)
	require.NotNil(t, index.sa, "the loaded index has no suffix array")
	ids, err := index.containingIDs(ctx, 0, "bump", true, nil)
	require.NoError(t, err)
	require.Equal(t, []string{stampSrcA}, ids)
}
