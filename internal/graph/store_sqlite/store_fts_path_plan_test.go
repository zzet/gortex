package store_sqlite

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSymbolPathPointPlanPreservesCompleteRankAndOwnerSemantics(t *testing.T) {
	s, _ := newSymbolSearchContextStore(t)
	cases := []struct {
		id, owner, path, ftsOwner string
		want                      bool
	}{
		{"mismatch", "other", "other/target/a.go", "", true},
		{"qualified", "scope", "scope/target/b.go", "scope", true},
		{"unqualified-owned", "scope", "target/c.go", "scope", true},
		{"mixed", "deep/owner", "deep\\owner/target\\d.go", "scope", true},
		{"owner-backslash", "deep\\owner", "target/e.go", "scope", true},
		{"boundary", "scope", "scope/targetX/no.go", "scope", false},
		{"case", "scope", "scope/Target/no.go", "scope", false},
		{"fts-excluded", "other", "other/target/f.go", "other", false},
	}
	var nodes []*graph.Node
	for _, c := range cases {
		nodes = append(nodes, &graph.Node{ID: c.id, Name: "PathRank", Kind: graph.KindFunction, RepoPrefix: c.owner, FilePath: c.path})
	}
	if err := s.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	byOwner := map[string][]graph.SymbolFTSItem{}
	for i, c := range cases {
		byOwner[c.ftsOwner] = append(byOwner[c.ftsOwner], graph.SymbolFTSItem{NodeID: c.id, Tokens: strings.Repeat("rank path ", 1+i%3)})
	}
	// Map iteration intentionally varies insertion order; tie parity is measured
	// against this database's original global ranked stream, not lexical IDs.
	for owner, items := range byOwner {
		if err := s.BulkUpsertSymbolFTS(owner, items); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.SearchSymbolsRepoScopedContext(context.Background(), "rank path", []string{"scope"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]bool{}
	for _, c := range cases {
		wants[c.id] = c.want
	}
	var expected []graph.SymbolHit
	for _, h := range all {
		if wants[h.NodeID] {
			expected = append(expected, h)
		}
	}
	if len(expected) != 5 {
		t.Fatalf("expected five independent matches, got%v", expected)
	}
	for _, limit := range []int{1, 3, 100} {
		got, handled, err := s.searchSymbolPathPointPlan(context.Background(), s.buildFTSMatch("rank path", true), []string{"scope"}, []string{"target"}, limit)
		if err != nil || !handled {
			t.Fatalf("handled%v err%v", handled, err)
		}
		end := min(limit, len(expected))
		if !reflect.DeepEqual(got, expected[:end]) {
			t.Fatalf("limit%d got%v want%v", limit, got, expected[:end])
		}
	}
	got, handled, err := s.searchSymbolPathPointPlan(context.Background(), s.buildFTSMatch("rank path", true), []string{"scope"}, []string{"missing.go"}, 100)
	if err != nil || !handled || len(got) != 0 {
		t.Fatalf("empty got%v handled%v err%v", got, handled, err)
	}
}

func TestSymbolPathPointPlanFallsBackWithoutCandidateRecallLimit(t *testing.T) {
	s, _ := newSymbolSearchContextStore(t)
	var ns []*graph.Node
	var items []graph.SymbolFTSItem
	for i := 0; i < symbolPathPointNodes+22; i++ {
		id := fmt.Sprintf("repo/target/%03d.go::Rank", i)
		ns = append(ns, &graph.Node{ID: id, Name: "Rank", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: fmt.Sprintf("repo/target/%03d.go", i)})
		items = append(items, graph.SymbolFTSItem{NodeID: id, Tokens: "rank path"})
	}
	if err := s.AddBatchChecked(ns, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.BulkUpsertSymbolFTS("repo", items); err != nil {
		t.Fatal(err)
	}
	match := s.buildFTSMatch("rank path", true)
	_, handled, err := s.searchSymbolPathPointPlan(context.Background(), match, []string{"repo"}, []string{"target"}, 500)
	if err != nil || handled {
		t.Fatalf("wide corpus must fall back handled%v err%v", handled, err)
	}
	_, handled, err = s.searchSymbolPathPointPlan(context.Background(), match, []string{"foreign-empty"}, []string{"target"}, 500)
	if err != nil || handled {
		t.Fatalf("raw foreign corpus must fall back before eligible filtering handled%v err%v", handled, err)
	}
	want, err := s.SearchSymbolsRepoScopedContext(context.Background(), "rank path", []string{"repo"}, 500)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.searchSymbolsPathScopedContext(context.Background(), "rank path", []string{"repo"}, []string{"target"}, 500)
	if err != nil || len(got) != len(items) || !reflect.DeepEqual(got, want) {
		t.Fatalf("got%d want%d err%v", len(got), len(want), err)
	}
	if _, err = s.writerDB.Exec(`DROP INDEX nodes_by_file`); err != nil {
		t.Fatal(err)
	}
	_, handled, err = s.searchSymbolPathPointPlan(context.Background(), match, []string{"repo"}, []string{"target"}, 500)
	if err != nil || handled {
		t.Fatalf("missing index must fall back handled%v err%v", handled, err)
	}
	got, err = s.searchSymbolsPathScopedContext(context.Background(), "rank path", []string{"repo"}, []string{"target"}, 500)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("missing-index rank changed err%v", err)
	}
}

func TestSymbolPathRawRangesConservativelyCoverDeepLiteralSpellings(t *testing.T) {
	cases := []struct{ path, owner, raw string }{
		{"%_[/Étage", "scope", "scope\\%_[/Étage/a.go"},
		{"a/b/c/d/e/f.go", "one/two/three", "one\\two/three\\a/b\\c/d/e\\f.go"},
		{"目录/界", "other", "other\\目录/界/file.go"},
		{"target", "back\\slash", "target/file.go"},
		{"target", "scope", "target/file.go"},
	}
	for _, c := range cases {
		rs, ok := symbolPathRawRanges([]string{c.path}, []string{c.owner})
		if !ok {
			t.Fatal("unexpected fallback")
		}
		covered := false
		for _, r := range rs {
			if c.raw >= r[0] && c.raw < r[1] {
				covered = true
			}
		}
		if !covered {
			t.Fatalf("range omitted raw%q path%q owner%q", c.raw, c.path, c.owner)
		}
	}
	if _, ok := symbolPathRawRanges([]string{""}, []string{"repo"}); ok {
		t.Fatal("empty prefix should retain original query")
	}
}

func TestSymbolPathPointPlanOwnerOverflowAndCancellation(t *testing.T) {
	s, _ := newSymbolSearchContextStore(t)
	var ns []*graph.Node
	for i := 0; i < symbolPathPlanOwners+1; i++ {
		owner := fmt.Sprintf("owner%03d", i)
		ns = append(ns, &graph.Node{ID: owner + "/file.go::Owner", Kind: graph.KindFunction, Name: "Owner", RepoPrefix: owner, FilePath: owner + "/file.go"})
	}
	if err := s.AddBatchChecked(ns, nil); err != nil {
		t.Fatal(err)
	}
	_, handled, err := s.searchSymbolPathPointPlan(context.Background(), s.buildFTSMatch("owner file", true), nil, []string{"file.go"}, 100)
	if err != nil || handled {
		t.Fatalf("owner overflow must use unrestricted plan handled%v err%v", handled, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, handled, err = s.searchSymbolPathPointPlan(ctx, s.buildFTSMatch("owner file", true), nil, []string{"file.go"}, 100)
	if err == nil || !handled {
		t.Fatalf("canceled lookup handled%v err%v", handled, err)
	}
}
