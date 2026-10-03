package trigram

import "testing"

// TestPathUnderAnyPrefix_SegmentBoundary pins the segment-boundary
// semantics the in-search path restriction must share with the
// post-filter (#845 round-2): a prefix matches the exact path or
// anything under prefix/, never a raw string extension — `pkg/su`
// must not consume a limit slot on `pkg/sub/x.go` that the post-filter
// would drop.
func TestPathUnderAnyPrefix_SegmentBoundary(t *testing.T) {
	cases := []struct {
		path string
		pre  string
		want bool
	}{
		{"pkg/sub/x.go", "pkg/sub", true},
		{"pkg/sub/x.go", "pkg/su", false},
		{"pkg/sub", "pkg/sub", true},
		{"pkg/sub/x.go", "", true},
		{"pkg/sub/x.go", "other", false},
	}
	for _, tc := range cases {
		if got := PathUnderAnyPrefix(tc.path, []string{tc.pre}); got != tc.want {
			t.Errorf("PathUnderAnyPrefix(%q, %q) = %v, want %v", tc.path, tc.pre, got, tc.want)
		}
	}
	if !PathUnderAnyPrefix("anything", nil) {
		t.Error("an empty prefix set is the unscoped default")
	}
}

// TestSearcher_GrepPaths_PrefixAppliedBeforeLimit pins the issue-#827
// semantics: the path restriction runs before the limit cut, so a
// scoped query returns scoped matches even when out-of-scope files own
// the head of the path-ordered scan. The fixture orders docs
// (uppercase names) and other/ ahead of src/ lexicographically, and a
// limit of 3 is exactly enough for the out-of-scope head to consume
// the global cut — which is what made post-filtered scoped queries
// return zero.
func TestSearcher_GrepPaths_PrefixAppliedBeforeLimit(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "ADoc.md", "Needle in the readme\n")
	mk(t, root, "ZDoc.md", "Needle in the changelog\n")
	mk(t, root, "other/c.go", "Needle elsewhere\n")
	mk(t, root, "src/a.go", "Needle in code a\n")
	mk(t, root, "src/b.go", "Needle in code b\n")

	s := Build(root, []string{"ADoc.md", "ZDoc.md", "other/c.go", "src/a.go", "src/b.go"})

	// Fixture drift guard: the unscoped head at limit 3 must hold no
	// src/ hit, or this test no longer exercises the wipeout shape.
	unscoped := s.Grep("Needle", 3)
	if len(unscoped) == 0 {
		t.Fatal("unscoped grep returned nothing")
	}
	for _, hit := range unscoped {
		if hit.Path == "src/a.go" || hit.Path == "src/b.go" {
			t.Fatalf("fixture drifted: a src/ hit reached the global head at limit 3: %+v", unscoped)
		}
	}

	hits := s.GrepPaths("Needle", []string{"src"}, 3)
	if len(hits) != 2 {
		t.Fatalf("GrepPaths(src) = %d hits, want 2: %+v", len(hits), hits)
	}
	if hits[0].Path != "src/a.go" || hits[0].Line != 1 {
		t.Errorf("hit[0] = %+v, want src/a.go:1", hits[0])
	}
	if hits[1].Path != "src/b.go" || hits[1].Line != 1 {
		t.Errorf("hit[1] = %+v, want src/b.go:1", hits[1])
	}

	// Empty prefix set is the unscoped default.
	if all := s.GrepPaths("Needle", nil, 0); len(all) != 5 {
		t.Errorf("GrepPaths(nil prefixes) = %d hits, want 5", len(all))
	}

	// Multiple prefixes union.
	if both := s.GrepPaths("Needle", []string{"src", "other"}, 0); len(both) != 3 {
		t.Errorf("GrepPaths(src|other) = %d hits, want 3: %+v", len(both), both)
	}
}
