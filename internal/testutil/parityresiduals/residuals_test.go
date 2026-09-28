package parityresiduals

import (
	"os"
	"path/filepath"
	"testing"
)

const testList = "| id | class | kind | side | pattern | partner | why |\n" +
	"|---|---|---|---|---|---|---|\n" +
	"| R1 | edge | arg_of | view | `^(?P<src>[^>]+)>unresolved::(?P<callee>\\w+)[\\|](?P<at>.+)$` | `^(?P<src>[^>]+)>[^>]*::(?P<callee>\\w+)#param:\\w+[\\|](?P<at>.+)$` | paired |\n" +
	"| R2 | node | function | view | `^ext::go:` | - | unpaired |\n" +
	"| R3 | meta | contract | - | `handler_(ident\\|trail)` | - | meta |\n"

func TestLoadAndPartnerRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "list.md")
	if err := os.WriteFile(path, []byte(testList), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[2].Pattern.String() != "handler_(ident|trail)" {
		t.Fatalf("parsed %d entries, meta pattern %q", len(entries), entries[2].Pattern)
	}
	paired := Rows{
		Edges: map[string][]string{"arg_of": {"a.go::Twice>unresolved::Apply|a.go:5"}},
		Nodes: map[string][]string{"function": {"ext::go:os::Getenv|external::go:os|Getenv|0-0"}},
		Meta:  map[string]string{},
	}
	clean := Rows{
		Edges: map[string][]string{"arg_of": {"a.go::Twice>a.go::Apply#param:f|a.go:5"}},
		Nodes: map[string][]string{},
		Meta:  map[string]string{},
	}
	res := Compare(paired, clean, entries)
	if bad := res.Unexcused(); len(bad) != 1 || bad[0].Side != "clean" {
		// The clean half has no entry here, so it alone is unexcused.
		t.Fatalf("unexcused = %+v, want only the clean half", bad)
	}
	if res.Met["R1"] != 1 || res.Met["R2"] != 1 {
		t.Fatalf("met = %v", res.Met)
	}
	// The same view row without its partner (a different site) is not excused.
	clean.Edges["arg_of"] = []string{"a.go::Twice>a.go::Apply#param:f|a.go:9"}
	res = Compare(paired, clean, entries)
	for _, d := range res.Unexcused() {
		if d.Side == "view" {
			return
		}
	}
	t.Fatalf("an unpaired view row was excused: %s", Report(res))
}
