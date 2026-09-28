package parityresiduals

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scaleList is two partnered entries, the shape of R1/R2 and R18/R19: a view
// row into an unresolved callee paired on callee and site with a clean row
// into the resolved callee's parameter, and the clean half.
const scaleList = "| id | class | kind | side | pattern | partner | mechanism |\n|---|---|---|---|---|---|---|\n" +
	"| R901 | edge | arg_of | view | `^(?P<src>[^>]+)>unresolved::(?P<callee>[A-Za-z_][A-Za-z0-9_]*)[\\|](?P<at>.+)$` | `^[^>]+>[^>]*::(?P<callee>[A-Za-z_][A-Za-z0-9_]*)#param:[a-z]+[\\|](?P<at>.+)$` | view half |\n" +
	"| R902 | edge | arg_of | clean | `^[^>]+>[^>]*::(?P<callee>[A-Za-z_][A-Za-z0-9_]*)#param:[a-z]+[\\|](?P<at>.+)$` | `^(?P<src>[^>]+)>unresolved::(?P<callee>[A-Za-z_][A-Za-z0-9_]*)[\\|](?P<at>.+)$` | clean half |\n"

func loadScaleList(t *testing.T) []Entry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "list.md")
	if err := os.WriteFile(path, []byte(scaleList), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

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
func TestCompareScalesByKey(t *testing.T) {
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

// naivePartner is the definition: some row of other matches the partner
// with every named group it shares with the pattern captured equal.
func naivePartner(e Entry, m []string, other []string) bool {
	want := map[string]string{}
	for i, name := range e.Pattern.SubexpNames() {
		if name != "" {
			want[name] = m[i]
		}
	}
	for _, row := range other {
		pm := e.Partner.FindStringSubmatch(row)
		if pm == nil {
			continue
		}
		ok := true
		for i, name := range e.Partner.SubexpNames() {
			if v, shared := want[name]; name != "" && shared && pm[i] != v {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// The keyed lookup answers exactly what the definition does, on random rows
// that share callees and sites partly.
func TestKeyedPartnerMatchesTheDefinition(t *testing.T) {
	entries := loadScaleList(t)
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 50; trial++ {
		view := Rows{Nodes: map[string][]string{}, Edges: map[string][]string{}}
		clean := Rows{Nodes: map[string][]string{}, Edges: map[string][]string{}}
		for i := 0; i < 40; i++ {
			at := fmt.Sprintf("f.go:%d", rng.Intn(6))
			view.Edges["arg_of"] = append(view.Edges["arg_of"], fmt.Sprintf("a.go::S%d>unresolved::C%d|%s", rng.Intn(4), rng.Intn(5), at))
			clean.Edges["arg_of"] = append(clean.Edges["arg_of"], fmt.Sprintf("unresolved::x%d>b.go::C%d#param:v|f.go:%d", rng.Intn(3), rng.Intn(5), rng.Intn(6)))
		}
		res := Compare(view, clean, entries)
		for _, d := range res.Differences {
			other := clean.Edges[d.Kind]
			if d.Side == "clean" {
				other = view.Edges[d.Kind]
			}
			want := ""
			for _, e := range entries {
				if e.Class != d.Class || e.Kind != d.Kind || e.Side != d.Side {
					continue
				}
				m := e.Pattern.FindStringSubmatch(d.Row)
				if m == nil {
					continue
				}
				if e.Partner == nil || naivePartner(e, m, other) {
					want = e.ID
					break
				}
			}
			if d.Residual != want {
				t.Fatalf("trial %d: %s %s excused by %q, the definition says %q", trial, d.Side, d.Row, d.Residual, want)
			}
		}
	}
}
