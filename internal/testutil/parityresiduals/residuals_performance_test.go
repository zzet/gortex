//go:build performance

package parityresiduals

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// scaleRows makes n view rows into unresolved callees and n clean rows into
// resolved parameters, paired for every even i, plus shared rows both hold.
func scaleRows(n int) (Rows, Rows) {
	view := Rows{Nodes: map[string][]string{}, Edges: map[string][]string{}}
	clean := Rows{Nodes: map[string][]string{}, Edges: map[string][]string{}}
	for i := 0; i < n; i++ {
		at := fmt.Sprintf("repo/pkg/f%d.go:%d", i%97, i)
		view.Edges["arg_of"] = append(view.Edges["arg_of"], fmt.Sprintf("repo/pkg/a.go::Src%d>unresolved::Callee%d|%s", i, i, at))
		site := at
		if i%2 == 1 {
			site = at + "0" // unpaired
		}
		clean.Edges["arg_of"] = append(clean.Edges["arg_of"], fmt.Sprintf("repo/unresolved::x>repo/pkg/b.go::Callee%d#param:v|%s", i, site))
		shared := fmt.Sprintf("repo/pkg/a.go::Shared%d>repo/pkg/b.go::Fn%d|%s", i, i, at)
		view.Edges["arg_of"] = append(view.Edges["arg_of"], shared)
		clean.Edges["arg_of"] = append(clean.Edges["arg_of"], shared)
	}
	return view, clean
}

// A comparison of 200,000 differing rows (100,000 on each side) over 300,000
// rows a side finishes within 60 s: partners are found by key, once per
// entry and kind, not by scanning the other side for every difference (the
// old scan was hours at this size). The counts come before any excusing.
func TestPerformanceCompareScalesByKey(t *testing.T) {
	entries := loadScaleList(t)
	view, clean := scaleRows(100000)
	var lines []string
	started := time.Now()
	res := CompareWithProgress(view, clean, entries, func(s string) { lines = append(lines, s) })
	took := time.Since(started)
	t.Logf("compared in %s; %d progress lines; first: %s", took, len(lines), strings.SplitN(lines[0], "\n", 2)[0])
	if took > 60*time.Second {
		t.Fatalf("the comparison took %s, over the 60 s bound", took)
	}
	if len(res.Differences) != 200000 {
		t.Fatalf("%d differences, want 200000", len(res.Differences))
	}
	if res.Met["R901"] != 50000 || res.Met["R902"] != 50000 {
		t.Fatalf("met %v, want R901=50000 R902=50000 (the paired half of each side)", res.Met)
	}
	if !strings.HasPrefix(lines[0], "differences before excusing: 200000") || !strings.Contains(lines[0], "edge arg_of only-view: 100000") {
		t.Fatalf("the first progress line is not the counts: %q", lines[0])
	}
	if len(lines) < 21 {
		t.Fatalf("%d progress lines, want the counts, one per 10,000 and the last", len(lines))
	}
}
