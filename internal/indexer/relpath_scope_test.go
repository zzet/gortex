package indexer

import (
	"fmt"
	"strings"
	"testing"
)

func legacyRelPathScope(path string, scope map[string]bool) bool {
	if scope[path] {
		return true
	}
	for key := range scope {
		if key == "." || strings.HasPrefix(path, key+"/") {
			return true
		}
	}
	return false
}

func TestRelPathScopeExactSemantics(t *testing.T) {
	paths := []string{"", ".", "a", "a/b", "ab/c", "a/", "a//b", "/", "/a", "/a/b", "./a", "a/b/c"}
	keys := []string{"", ".", "a", "a/", "/", "/a", "a/b", "missing"}
	for _, path := range paths {
		for _, key := range keys {
			for _, enabled := range []bool{false, true} {
				scope := map[string]bool{key: enabled}
				if got, want := relPathInScope(path, scope), legacyRelPathScope(path, scope); got != want {
					t.Fatalf("path=%q scope=%v got=%v want=%v", path, scope, got, want)
				}
			}
		}
	}
	if relPathInScope("a/b", nil) {
		t.Fatal("nil scope matches")
	}
}

var relPathScopeResult bool

func BenchmarkRelPathScopeCardinality(b *testing.B) {
	for _, size := range []int{1, 100, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			scope := make(map[string]bool, size)
			for i := range size {
				scope[fmt.Sprintf("unrelated/%06d", i)] = true
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				relPathScopeResult = relPathInScope("src/nested/file.go", scope)
			}
		})
	}
}

func TestRelPathScopeZeroAllocations(t *testing.T) {
	scope := map[string]bool{strings.Repeat("long", 32): false, "src": false}
	if n := testing.AllocsPerRun(100, func() { relPathScopeResult = relPathInScope("other/nested/file.go", scope) }); n != 0 {
		t.Fatalf("allocations per lookup=%v, want zero", n)
	}
}
