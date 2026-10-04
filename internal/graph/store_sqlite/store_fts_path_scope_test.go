package store_sqlite

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSymbolPathScopeFiltersBeforeLimitAndPreservesRank(t *testing.T) {
	s, _ := newSymbolSearchContextStore(t)
	var nodes []*graph.Node
	var items []graph.SymbolFTSItem
	for i := 0; i < 320; i++ {
		n := &graph.Node{ID: fmt.Sprintf("repo/noise/%03d.go::Flood", i), Name: "Flood", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: fmt.Sprintf("repo/noise/%03d.go", i)}
		nodes = append(nodes, n)
		items = append(items, graph.SymbolFTSItem{NodeID: n.ID, Tokens: "worker needle worker needle"})
	}
	for _, path := range []string{"repo/target/a.go", "repo/target/b.go", "repo/targetX/c.go"} {
		n := &graph.Node{ID: path + "::Match", Name: "Match", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: path}
		nodes = append(nodes, n)
		items = append(items, graph.SymbolFTSItem{NodeID: n.ID, Tokens: "worker needle"})
	}
	if err := s.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.BulkUpsertSymbolFTS("repo", items); err != nil {
		t.Fatal(err)
	}
	all, err := s.SearchSymbolsRepoScopedContext(context.Background(), "worker needle", []string{"repo"}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	head, err := s.SearchSymbolsRepoScopedContext(context.Background(), "worker needle", []string{"repo"}, 1)
	if err != nil || len(head) != 1 || symbolPathAllowed(s.GetNode(head[0].NodeID).FilePath, "repo", []string{"target"}) {
		t.Fatalf("fixture lacks excluded unscoped head: %v err%v", head, err)
	}
	var want []graph.SymbolHit
	for _, hit := range all {
		n := s.GetNode(hit.NodeID)
		if symbolPathAllowed(n.FilePath, n.RepoPrefix, []string{"target"}) {
			want = append(want, hit)
		}
	}
	if len(want) != 2 {
		t.Fatalf("reference matches=%v", want)
	}
	for _, limit := range []int{1, 2, 100} {
		got, err := s.searchSymbolsPathScopedContext(context.Background(), "worker needle", []string{"repo"}, []string{"target"}, limit)
		if err != nil {
			t.Fatal(err)
		}
		end := len(want)
		if end > limit {
			end = limit
		}
		if !reflect.DeepEqual(got, want[:end]) {
			t.Fatalf("limit%d got%v want%v", limit, got, want[:end])
		}
	}
	bundles, err := s.SearchSymbolBundlesPathScopedContext(context.Background(), "worker needle", []string{"repo"}, []string{"target"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 1 || bundles[0].Node.ID != want[0].NodeID {
		t.Fatalf("bundles=%v", bundles)
	}
	miss, err := s.SearchSymbolBundlesPathScopedContext(context.Background(), "worker needle", []string{"unknown"}, []string{"target"}, 100)
	if err != nil || len(miss) != 0 {
		t.Fatalf("unknown repo=%v err%v", miss, err)
	}
	// The exact tier also narrows before its limit and cannot let an excluded
	// exact hit suppress an in-path FTS hit.
	outside := &graph.Node{ID: "repo/noise/z.go::ExactPick", Name: "ExactPick", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/noise/z.go"}
	if err := s.AddBatchChecked([]*graph.Node{outside}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSymbolFTS(outside.ID, "exact pick"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSymbolFTS(want[0].NodeID, "exact pick"); err != nil {
		t.Fatal(err)
	}
	got, err := s.searchSymbolsPathScopedContext(context.Background(), "ExactPick", []string{"repo"}, []string{"target"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].NodeID != want[0].NodeID {
		t.Fatalf("excluded exact hit masked in-path FTS: %v", got)
	}
}

func TestSymbolPathSQLMatchesLiteralRepoRelativePredicate(t *testing.T) {
	s, _ := newSymbolSearchContextStore(t)
	paths := []string{"repo/a%_[/x.go", "repo/aXYZQ/x.go", "repo/A%_[/x.go", "repo/目录/x.go", "repo/目录X/x.go", `repo\目录\x.go`, "a%_[", "a%_[/child", "repo/repo/a%_[/x.go"}
	var nodes []*graph.Node
	var items []graph.SymbolFTSItem
	for i, path := range paths {
		repo := "repo"
		if i == 6 || i == 7 {
			repo = ""
		}
		n := &graph.Node{ID: fmt.Sprintf("literal/%d", i), Name: "LiteralHit", Kind: graph.KindFunction, FilePath: path, RepoPrefix: repo}
		nodes = append(nodes, n)
		items = append(items, graph.SymbolFTSItem{NodeID: n.ID, Tokens: "literal needle"})
	}
	if err := s.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	// FTS ownership follows actual node prefixes; unowned rows remain permitted.
	for _, n := range nodes {
		if err := s.UpsertSymbolFTS(n.ID, "literal needle"); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		paths []string
		ids   []int
	}{
		{[]string{"a%_["}, []int{0, 6, 7}},
		{[]string{"目录"}, []int{3, 5}},
		{[]string{"a%_[", "目录"}, []int{0, 3, 5, 6, 7}},
		{[]string{"missing"}, nil},
		{nil, []int{0, 1, 2, 3, 4, 5, 6, 7, 8}},
	}
	for _, tc := range cases {
		got, err := s.searchSymbolsPathScopedContext(context.Background(), "literal needle", []string{"repo"}, tc.paths, 100)
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, h := range got {
			ids[h.NodeID] = true
		}
		want := map[string]bool{}
		for _, i := range tc.ids {
			want[fmt.Sprintf("literal/%d", i)] = true
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("literal prefixes%v got%v want%v", tc.paths, ids, want)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SearchSymbolBundlesPathScopedContext(ctx, "literal needle", nil, []string{"目录"}, 1); err != context.Canceled {
		t.Fatalf("cancel error=%v", err)
	}
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	assertBlockedSymbolSearchCancels(t, s, func(ctx context.Context) error {
		_, err := s.SearchSymbolBundlesPathScopedContext(ctx, "literal needle", nil, []string{"目录"}, 1)
		return err
	})
}
