package resolver

import (
	"reflect"
	"testing"
)

func TestImportRootOrder_DescendantsFirst(t *testing.T) {
	a, b, c := "repo/a/index.ts", "repo/b/index.ts", "repo/c/index.ts"
	targets := map[string]map[string]struct{}{a: {b: {}}, b: {c: {}}}
	for _, roots := range [][]string{{a, b, c}, {c, b, a}} {
		if got := importRootOrder(roots, targets); !reflect.DeepEqual(got, []string{c, b, a}) {
			t.Fatalf("roots %v produced order %v", roots, got)
		}
	}
	// A cycle's order is arbitrary, but imported roots must occur once each.
	targets[c] = map[string]struct{}{a: {}}
	got := importRootOrder([]string{a, b, a}, targets)
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("cyclic roots produced %v", got)
	}
	for _, root := range got {
		if root != a && root != b {
			t.Fatalf("scheduled non-imported root %q", root)
		}
	}
}

func TestImportReachableDirs_SingleTargetCacheImmutable(t *testing.T) {
	a, b := "repo/a/index.ts", "repo/b/index.ts"
	backing := []string{"repo/b", "sentinel", "sentinel"}
	cached := backing[:1]
	completed := map[string][]string{b: cached}
	targets := map[string]map[string]struct{}{a: {b: {}}}
	if got := importReachableDirs(a, targets, completed); !reflect.DeepEqual(got, []string{"repo/a", "repo/b"}) {
		t.Fatalf("unexpected closure %v", got)
	}
	if !reflect.DeepEqual(backing, []string{"repo/b", "sentinel", "sentinel"}) {
		t.Fatalf("extending a closure mutated its cached backing slice: %v", backing)
	}
	completed[b] = []string{"repo/b", "repo/a"}
	if got := importReachableDirs(a, targets, completed); !reflect.DeepEqual(got, completed[b]) {
		t.Fatalf("root directory already reachable, got %v", got)
	}
	// Same-directory files must reuse the directory without adding duplicates.
	same := "repo/b/other.ts"
	targets[same] = map[string]struct{}{b: {}}
	completed[b] = cached
	if got := importReachableDirs(same, targets, completed); !reflect.DeepEqual(got, cached) {
		t.Fatalf("same-directory closure has duplicates: %v", got)
	}
}
