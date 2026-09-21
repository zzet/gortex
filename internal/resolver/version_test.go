package resolver

import (
	"strings"
	"testing"
)

// The resolver version is a cache key, so the tests here pin the two
// properties a cache key has to have and nothing else:
//
//  1. it MOVES when the resolution contract moves — a pass that joins, leaves,
//     is renamed or is reordered, and a raised semantics version;
//  2. it does NOT move otherwise — the same binary asked twice answers the
//     same string, and the value is pinned by a golden so a drift that nobody
//     intended fails here instead of silently invalidating every stored
//     generation in the field (or, worse, silently reusing one it should not).

// resolverVersionGolden is the fingerprint this source tree produces.
//
// Update it in the same change that moves the contract — a registered pass
// added, removed, renamed or reordered, or ResolutionSemanticsVersion raised —
// and say which in the commit message. A diff that changes this line and
// nothing else in internal/resolver is the signal that something re-keyed
// every cached generation by accident.
const resolverVersionGolden = "resolver-1:8732a339e696a0eb7f4f8d378b332a91"

func TestResolverVersionIsPinned(t *testing.T) {
	if got := Version(); got != resolverVersionGolden {
		t.Fatalf("the resolution contract's fingerprint moved:\n got  %q\n want %q\n"+
			"If the move was intended (a pass joined/left/was renamed, or "+
			"ResolutionSemanticsVersion was raised) update resolverVersionGolden in the "+
			"same commit; every stored generation re-keys once.", got, resolverVersionGolden)
	}
}

func TestResolverVersionIsStable(t *testing.T) {
	first, second := Version(), Version()
	if first != second {
		t.Fatalf("two reads of the resolution contract disagree: %q then %q", first, second)
	}
	if !strings.HasPrefix(first, "resolver-1:") {
		t.Fatalf("the version %q does not name the semantics version it was produced under", first)
	}
}

// TestResolverVersionNamesTheRegisteredPassSet is the revert-red half of the
// golden: a fingerprint that ignored the registry would still satisfy the
// golden above, and would then fail to invalidate when a synthesizer is added
// or removed. Every registered name must be load-bearing.
func TestResolverVersionNamesTheRegisteredPassSet(t *testing.T) {
	names := FrameworkSynthesizerNames()
	if len(names) == 0 {
		t.Fatal("the resolver registers no passes; the fingerprint would name nothing")
	}
	if got := resolverVersionFingerprint(names, ResolutionSemanticsVersion); got != Version() {
		t.Fatalf("Version() is not the fingerprint of the registered pass set:\n got  %q\n want %q",
			Version(), got)
	}
	baseline := Version()
	for i := range names {
		dropped := append(append([]string(nil), names[:i]...), names[i+1:]...)
		if got := resolverVersionFingerprint(dropped, ResolutionSemanticsVersion); got == baseline {
			t.Fatalf("dropping the registered pass %q left the resolver version at %q; "+
				"a generation built with that pass would be reused by a binary without it",
				names[i], got)
		}
	}
}

func TestResolverVersionMovesWithTheContract(t *testing.T) {
	names := FrameworkSynthesizerNames()
	baseline := resolverVersionFingerprint(names, ResolutionSemanticsVersion)

	t.Run("a raised semantics version", func(t *testing.T) {
		if got := resolverVersionFingerprint(names, ResolutionSemanticsVersion+1); got == baseline {
			t.Fatal("raising the semantics version left the resolver version where it was")
		}
	})

	t.Run("a pass joins the registry", func(t *testing.T) {
		joined := append(append([]string(nil), names...), "a-new-dispatch-pass")
		if got := resolverVersionFingerprint(joined, ResolutionSemanticsVersion); got == baseline {
			t.Fatal("a new registered pass left the resolver version where it was")
		}
	})

	t.Run("a pass is renamed", func(t *testing.T) {
		renamed := append([]string(nil), names...)
		renamed[0] += "-v2"
		if got := resolverVersionFingerprint(renamed, ResolutionSemanticsVersion); got == baseline {
			t.Fatal("a renamed pass left the resolver version where it was")
		}
	})

	// Execution order decides what the passes emit — several read the edges an
	// earlier pass landed — so two registries with the same members in a
	// different order are two contracts.
	t.Run("the run order changes", func(t *testing.T) {
		if len(names) < 2 {
			t.Fatalf("the registry holds %d passes; ordering cannot be pinned", len(names))
		}
		swapped := append([]string(nil), names...)
		swapped[0], swapped[1] = swapped[1], swapped[0]
		if got := resolverVersionFingerprint(swapped, ResolutionSemanticsVersion); got == baseline {
			t.Fatal("reordering the registered passes left the resolver version where it was")
		}
	})

	// Length-delimited encoding: two registries that concatenate to the same
	// bytes must not collide.
	t.Run("the encoding is length delimited", func(t *testing.T) {
		a := resolverVersionFingerprint([]string{"ab", "c"}, ResolutionSemanticsVersion)
		b := resolverVersionFingerprint([]string{"a", "bc"}, ResolutionSemanticsVersion)
		if a == b {
			t.Fatal(`{"ab","c"} and {"a","bc"} produced one fingerprint; the encoding is ambiguous`)
		}
	})
}
