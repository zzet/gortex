package trigram

import (
	"regexp"
	"slices"
	"testing"
)

// matchKeys renders matches as path:line, in the order returned.
func matchKeys(ms []Match) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Path+":"+string(rune('0'+m.Line)))
	}
	return out
}

// TestSearcher_UpdateKeepsPathOrderOfASortedBuild pins that a searcher built
// from sorted paths and patched with Update answers in the order, and with
// the limited prefix, a fresh Build of the same corpus answers with — even
// when the patch appended a path that sorts before the ones it already held.
func TestSearcher_UpdateKeepsPathOrderOfASortedBuild(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "b.go", "needle b\n")
	mk(t, root, "c.go", "needle c\n")
	s := Build(root, []string{"b.go", "c.go"})

	mk(t, root, "a.go", "needle a\n")
	s.Update("a.go")

	fresh := Build(root, []string{"a.go", "b.go", "c.go"})
	for _, limit := range []int{0, 1, 2} {
		got, want := matchKeys(s.Grep("needle", limit)), matchKeys(fresh.Grep("needle", limit))
		if !slices.Equal(got, want) {
			t.Errorf("Grep limit %d = %v, want the fresh build's %v", limit, got, want)
		}
		re := regexp.MustCompile("needle [a-c]")
		got = matchKeys(s.GrepRegexp(re, []string{"needle"}, "", limit))
		want = matchKeys(fresh.GrepRegexp(re, []string{"needle"}, "", limit))
		if !slices.Equal(got, want) {
			t.Errorf("GrepRegexp limit %d = %v, want the fresh build's %v", limit, got, want)
		}
		got = matchKeys(s.GrepRegexp(re, nil, "", limit))
		want = matchKeys(fresh.GrepRegexp(re, nil, "", limit))
		if !slices.Equal(got, want) {
			t.Errorf("GrepRegexp without literals limit %d = %v, want the fresh build's %v", limit, got, want)
		}
	}
}

// TestSearcher_UpdateKeepsDocIDOrderOfAnUnsortedBuild pins that a searcher
// whose Build input was not sorted keeps answering in docID order, as it
// always did.
func TestSearcher_UpdateKeepsDocIDOrderOfAnUnsortedBuild(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "c.go", "needle c\n")
	mk(t, root, "b.go", "needle b\n")
	s := Build(root, []string{"c.go", "b.go"})
	mk(t, root, "a.go", "needle a\n")
	s.Update("a.go")
	if got, want := matchKeys(s.Grep("needle", 0)), []string{"c.go:1", "b.go:1", "a.go:1"}; !slices.Equal(got, want) {
		t.Errorf("Grep = %v, want docID order %v", got, want)
	}
}

// TestSearcher_RemoveDropsAPathStillOnDisk pins Remove: the path stops
// matching although its file is still readable, and a later Update brings
// it back under the same docID.
func TestSearcher_RemoveDropsAPathStillOnDisk(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a.go", "needle a\n")
	mk(t, root, "b.go", "needle b\n")
	s := Build(root, []string{"a.go", "b.go"})

	s.Remove("a.go")
	s.Remove("never-seen.go")
	if got := matchKeys(s.Grep("needle", 0)); !slices.Equal(got, []string{"b.go:1"}) {
		t.Errorf("Grep after Remove = %v, want only b.go", got)
	}
	if got := s.DocCount(); got != 1 {
		t.Errorf("DocCount after Remove = %d, want 1", got)
	}
	s.Update("a.go")
	if got := matchKeys(s.Grep("needle", 0)); !slices.Equal(got, []string{"a.go:1", "b.go:1"}) {
		t.Errorf("Grep after re-Update = %v, want both files", got)
	}
	if got := len(s.snapshotPaths()); got != 2 {
		t.Errorf("%d docID slots after a Remove and re-Update, want 2 (the slot is reused)", got)
	}
}
