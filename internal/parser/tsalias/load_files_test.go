package tsalias

import (
	"os"
	"path/filepath"
	"testing"
)

// LoadFiles over the scope configs a walk visited reproduces the walk's
// Collection, including the order of scopes with equally long prefixes.
func TestLoadFilesMatchesWalk(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tsconfig.json", `{"extends":"./.nuxt/tsconfig.json","compilerOptions":{"baseUrl":"."}}`)
	write(".nuxt/tsconfig.json", `{"compilerOptions":{"paths":{"~/*":["../*"]}}}`)
	write("jsconfig.json", `{"compilerOptions":{"paths":{"#/*":["lib/*"]}}}`)
	write("ab/tsconfig.json", `{"compilerOptions":{"paths":{"@a/*":["x/*"]}}}`)
	write("a-b/tsconfig.json", `{"compilerOptions":{"paths":{"@b/*":["y/*"]}}}`)
	write("a/b/tsconfig.json", `{"compilerOptions":{"paths":{"@c/*":["z/*"]}}}`)
	write("node_modules/pkg/tsconfig.json", `{"compilerOptions":{"paths":{"@n/*":["n/*"]}}}`)

	var scopes []string
	walked := LoadObserved(root, func(rel string, _ []byte, found bool) {
		if found && IsScopeConfig(rel) {
			scopes = append(scopes, rel)
		}
	})
	// Reverse to prove LoadFiles does not depend on the caller's order.
	for i, j := 0, len(scopes)-1; i < j; i, j = i+1, j-1 {
		scopes[i], scopes[j] = scopes[j], scopes[i]
	}
	listed := LoadFiles(root, scopes, nil)
	if walked.Digest() != listed.Digest() {
		t.Fatalf("digest mismatch: walk %s, listed %s (scopes %v)", walked.Digest(), listed.Digest(), scopes)
	}
	if IsScopeConfig(".nuxt/tsconfig.json") || IsScopeConfig("node_modules/pkg/tsconfig.json") {
		t.Fatal("configs under skipped directories are parents only, never scopes")
	}
}

func TestWalkOrderLess(t *testing.T) {
	cases := []struct {
		a, b string
		less bool
	}{
		{"a-b/tsconfig.json", "a/b/tsconfig.json", false}, // WalkDir visits "a" before "a-b"
		{"a/b/tsconfig.json", "a/tsconfig.json", true},
		{"jsconfig.json", "tsconfig.json", true},
		{"tsconfig.json", "ab/tsconfig.json", false},
	}
	for _, c := range cases {
		if got := WalkOrderLess(c.a, c.b); got != c.less {
			t.Errorf("WalkOrderLess(%q, %q) = %v, want %v", c.a, c.b, got, c.less)
		}
	}
}
