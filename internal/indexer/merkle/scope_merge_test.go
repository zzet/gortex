package merkle

import (
	"reflect"
	"strings"
	"testing"
)

func TestMergeScopePreservesUnprocessedLeaves(t *testing.T) {
	prior := &Tree{
		Root: "old-root",
		Files: map[string]FileNode{
			"outside/kept.go":   {Hash: strings.Repeat("a", 64), Mtime: 11, Salt: "old-extractor"},
			"outside/failed.go": {Hash: "", Mtime: 12, Salt: "failed-extractor"},
			"scope/changed.go":  {Hash: strings.Repeat("b", 64), Mtime: 13, Salt: "before"},
			"scope/deleted.go":  {Hash: strings.Repeat("c", 64), Mtime: 14, Salt: "before"},
		},
		Dirs: map[string]string{"old-only-directory": "obsolete"},
	}
	scoped := &Tree{
		Root: "partial-root",
		Files: map[string]FileNode{
			"scope/changed.go": {Hash: strings.Repeat("d", 64), Mtime: 21, Salt: "new-extractor"},
			"scope/added.go":   {Hash: strings.Repeat("e", 64), Mtime: 22, Salt: "new-extractor"},
			"outside/kept.go":  {Hash: strings.Repeat("f", 64), Mtime: 99, Salt: "must-not-certify"},
			"outside/stray.go": {Hash: strings.Repeat("f", 64), Mtime: 99, Salt: "must-not-add"},
		},
		Dirs: map[string]string{"partial-only-directory": "obsolete"},
	}
	wantFiles := map[string]FileNode{
		"outside/kept.go":   prior.Files["outside/kept.go"],
		"outside/failed.go": prior.Files["outside/failed.go"],
		"scope/changed.go":  scoped.Files["scope/changed.go"],
		"scope/added.go":    scoped.Files["scope/added.go"],
	}
	got := MergeScope(prior, scoped, func(rel string) bool { return strings.HasPrefix(rel, "scope/") })
	if !reflect.DeepEqual(wantFiles, got.Files) {
		t.Fatalf("merged leaves = %#v, want %#v", got.Files, wantFiles)
	}
	want := &Tree{Files: wantFiles, Dirs: make(map[string]string)}
	want.aggregate()
	if got.Root != want.Root || !reflect.DeepEqual(got.Dirs, want.Dirs) {
		t.Fatalf("merged directories/root were not recomputed: %#v", got)
	}
	if prior.Root != "old-root" || scoped.Root != "partial-root" ||
		prior.Files["scope/changed.go"].Mtime != 13 ||
		scoped.Files["outside/kept.go"].Mtime != 99 {
		t.Fatal("merge mutated an input tree")
	}
	delete(got.Files, "outside/kept.go")
	if _, ok := prior.Files["outside/kept.go"]; !ok {
		t.Fatal("merged map aliases prior input")
	}
	delete(got.Files, "scope/changed.go")
	if _, ok := scoped.Files["scope/changed.go"]; !ok {
		t.Fatal("merged map aliases scoped input")
	}
}

func TestMergeScopeEmptyAndFullReplacement(t *testing.T) {
	prior := &Tree{Files: map[string]FileNode{
		"outside.go": {Hash: strings.Repeat("a", 64), Mtime: 1, Salt: "old"},
		"scope.go":   {Hash: strings.Repeat("b", 64), Mtime: 2, Salt: "old"},
	}}
	contains := func(rel string) bool { return rel == "scope.go" }
	got := MergeScope(prior, nil, contains)
	if len(got.Files) != 1 || got.Files["outside.go"] != prior.Files["outside.go"] {
		t.Fatalf("empty scope must remove only in-scope leaves: %#v", got.Files)
	}
	replacement := &Tree{Files: map[string]FileNode{"new.go": {Hash: strings.Repeat("c", 64), Mtime: 3}}}
	got = MergeScope(prior, replacement, nil)
	if !reflect.DeepEqual(got.Files, replacement.Files) {
		t.Fatalf("nil predicate must replace the full baseline: %#v", got.Files)
	}
	got = MergeScope(nil, replacement, func(rel string) bool { return rel == "new.go" })
	if !reflect.DeepEqual(got.Files, replacement.Files) {
		t.Fatalf("missing prior must retain rebuilt scope: %#v", got.Files)
	}
	got = MergeScope(nil, nil, nil)
	if len(got.Files) != 0 {
		t.Fatalf("empty inputs produced leaves: %#v", got.Files)
	}
}
