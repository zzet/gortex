package search

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// Reference the original byte-wise unicode61 ASCII subset, including refusing
// the entire row when a non-ASCII byte follows otherwise valid tokens.
func referenceFTSTokens(text string) ([]string, bool) {
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

func TestFTSTokensByteEquivalence(t *testing.T) {
	check := func(text string) {
		t.Helper()
		want, wantOK := referenceFTSTokens(text)
		got, ok := ftsTokensOf(text)
		if ok != wantOK || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: got %q, %v; want %q, %v", text, got, ok, want, wantOK)
		}
	}
	for i := 0; i < 256; i++ {
		check(string([]byte{byte(i)}))
		check("AB12_" + string([]byte{byte(i)}) + "_Cd34")
	}
	for _, text := range []string{"", " ", "Aa0Zz9", "A\x00B\tC\r\nD", "one one ONE", "café", "İ", "日本語", strings.Repeat("longTOKEN123", 1000)} {
		check(text)
	}
	rng := rand.New(rand.NewSource(17))
	for i := 0; i < 2000; i++ {
		text := make([]byte, rng.Intn(512))
		for j := range text {
			text[j] = byte(rng.Intn(128))
		}
		check(string(text))
	}
}

func TestFTSTokensNormalizedAllocationBudget(t *testing.T) {
	text := strings.Repeat("normalized token123 ", 32)
	var tokens []string
	allocs := testing.AllocsPerRun(100, func() {
		tokens, _ = ftsTokensOf(text)
	})
	if len(tokens) != 64 {
		t.Fatalf("got %d tokens, want 64", len(tokens))
	}
	if allocs > 1 {
		t.Fatalf("normalized text allocated %.0f times, want only the token slice", allocs)
	}
}

func FuzzFTSTokens(f *testing.F) {
	for _, text := range []string{"", "Func(ctx Context) Error", "a_b::c.d/123", "café", "ascii then \xff", "日本語"} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		want, wantOK := referenceFTSTokens(text)
		got, ok := ftsTokensOf(text)
		if ok != wantOK || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: got %q, %v; want %q, %v", text, got, ok, want, wantOK)
		}
	})
}
