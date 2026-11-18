package storetest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func testScopedDeadCodeReader(t *testing.T, factory Factory) {
	s := factory(t)
	reader, ok := s.(graph.ScopedDeadCodeReader)
	if !ok {
		t.Skip("backend does not implement graph.ScopedDeadCodeReader")
	}
	for _, node := range []*graph.Node{
		{ID: "a-dead", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-live", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-weak", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-missing-caller", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-type", Kind: graph.KindType, RepoPrefix: "a"},
		{ID: "a-iface", Kind: graph.KindInterface, RepoPrefix: "a", Meta: map[string]any{"methods": []string{"Work"}}},
		{ID: "a-file", Kind: graph.KindFile, RepoPrefix: "a"},
		{ID: "b-caller", Kind: graph.KindFunction, RepoPrefix: "b"},
		{ID: "b-type", Kind: graph.KindType, RepoPrefix: "b"},
		{ID: "b-iface", Kind: graph.KindInterface, RepoPrefix: "b", Meta: map[string]any{"methods": []string{"Work"}}},
		{ID: "b-file", Kind: graph.KindFile, RepoPrefix: "b"},
		{ID: "unowned-dead", Kind: graph.KindFunction},
		{ID: "unowned-file", Kind: graph.KindFile},
	} {
		s.AddNode(node)
	}
	for _, edge := range []*graph.Edge{
		{From: "b-caller", To: "a-live", Kind: graph.EdgeCalls},
		{From: "b-caller", To: "a-weak", Kind: graph.EdgeReads},
		{From: "missing-caller", To: "a-missing-caller", Kind: graph.EdgeCalls},
		{From: "a-type", To: "b-iface", Kind: graph.EdgeImplements},
		{From: "b-type", To: "a-iface", Kind: graph.EdgeImplements},
		{From: "missing-type", To: "a-iface", Kind: graph.EdgeImplements},
	} {
		s.AddEdge(edge)
	}
	kinds := []graph.NodeKind{graph.KindFunction}
	incoming := map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls}}
	for _, tc := range []struct {
		name       string
		prefixes   []string
		candidates []string
		files      []string
		implements []string
	}{
		{"one repo", []string{"a"}, []string{"a-dead", "a-weak"}, []string{"a-file"}, []string{"a-type:b-iface"}},
		{"other repo", []string{"b"}, []string{"b-caller"}, []string{"b-file"}, []string{"b-type:a-iface"}},
		{"union deduplicates prefixes", []string{"b", "a", "a"}, []string{"a-dead", "a-weak", "b-caller"}, []string{"a-file", "b-file"}, []string{"a-type:b-iface", "b-type:a-iface"}},
		{"empty matches nothing", []string{}, nil, nil, nil},
		{"unowned is explicit", []string{""}, []string{"unowned-dead"}, []string{"unowned-file"}, nil},
		{"nil preserves unscoped", nil, []string{"a-dead", "a-weak", "b-caller", "unowned-dead"}, []string{"a-file", "b-file", "unowned-file"}, []string{"a-type:b-iface", "b-type:a-iface", "missing-type:a-iface"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sortNodeIDs(reader.DeadCodeCandidatesInRepos(kinds, incoming, tc.prefixes)); !slices.Equal(got, tc.candidates) {
				t.Fatalf("candidates = %v, want %v (cross-repo/dangling incoming usage must survive)", got, tc.candidates)
			}
			if got := sortNodeIDs(slices.Collect(reader.NodesByKindInRepos(graph.KindFile, tc.prefixes))); !slices.Equal(got, tc.files) {
				t.Fatalf("files = %v, want %v", got, tc.files)
			}
			var rows []string
			for _, row := range reader.IfaceImplementsRowsInRepos(tc.prefixes) {
				rows = append(rows, row.TypeID+":"+row.IfaceID)
				if fmt.Sprint(row.IfaceMeta["methods"]) != "[Work]" {
					t.Fatalf("interface metadata lost: %#v", row)
				}
			}
			slices.Sort(rows)
			if !slices.Equal(rows, tc.implements) {
				t.Fatalf("implements = %v, want %v (interface targets may be outside scope)", rows, tc.implements)
			}
		})
	}
}

// Filtered reads must equal walking the exact adjacency mask, including absent
// source endpoints and foreign interface metadata. This is also the contract
// used by the request-side contract-core wrapper.
func testFilteredScopedDeadCodeReader(t *testing.T, factory Factory) {
	s := factory(t)
	reader, ok := s.(graph.FilteredScopedDeadCodeReader)
	if !ok {
		t.Skip("backend does not implement filtered scoped dead-code reads")
	}
	for _, n := range []*graph.Node{
		{ID: "a-used", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-via", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-missing", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-type", Kind: graph.KindType, RepoPrefix: "a"},
		{ID: "b-caller", Kind: graph.KindFunction, RepoPrefix: "b"},
		{ID: "b-iface", Kind: graph.KindInterface, RepoPrefix: "b", Meta: map[string]any{"methods": []string{"Work"}}},
	} {
		s.AddNode(n)
	}
	for _, e := range []*graph.Edge{
		{From: "b-caller", To: "a-used", Kind: graph.EdgeCalls},
		{From: "b-caller", To: "a-via", Kind: graph.EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "missing", To: "a-missing", Kind: graph.EdgeCalls},
		{From: "a-type", To: "b-iface", Kind: graph.EdgeImplements},
		{From: "missing-type", To: "b-iface", Kind: graph.EdgeImplements},
	} {
		s.AddEdge(e)
	}
	for _, prefixes := range [][]string{nil, {"a"}, {"b"}, {}, {""}} {
		for _, filter := range []graph.OutlineEdgeFilter{
			{}, {ExcludeKinds: []graph.EdgeKind{graph.EdgeCalls}},
			{ExcludeNodeKinds: []graph.NodeKind{graph.KindFunction}},
			{ExcludeNodeKinds: []graph.NodeKind{graph.KindType}},
			{ExcludeNodeIDs: map[string]bool{"b-caller": true, "missing-type": true}},
			{ExcludeVia: "spring.Bean"},
		} {
			allowed := func(n *graph.Node) bool {
				if prefixes == nil {
					return true
				}
				return n != nil && slices.Contains(prefixes, n.RepoPrefix)
			}
			var expected []*graph.Node
			for n := range s.NodesByKind(graph.KindFunction) {
				if !allowed(n) {
					continue
				}
				live := false
				for _, e := range s.GetInEdges(n.ID) {
					if e.Kind == graph.EdgeCalls && filter.Allows(e, s.GetNode(e.From), n) {
						live = true
						break
					}
				}
				if !live {
					expected = append(expected, n)
				}
			}
			got, err := reader.DeadCodeCandidatesInReposFiltered([]graph.NodeKind{graph.KindFunction}, map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls}}, prefixes, filter)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(sortNodeIDs(got), sortNodeIDs(expected)) {
				t.Fatalf("filtered candidates scope=%v filter=%+v got=%v want=%v", prefixes, filter, sortNodeIDs(got), sortNodeIDs(expected))
			}
			var expectedRows []string
			for e := range s.EdgesByKind(graph.EdgeImplements) {
				from, to := s.GetNode(e.From), s.GetNode(e.To)
				if allowed(from) && filter.Allows(e, from, to) {
					expectedRows = append(expectedRows, e.From+":"+e.To)
				}
			}
			rows, err := reader.IfaceImplementsRowsInReposFiltered(prefixes, filter)
			if err != nil {
				t.Fatal(err)
			}
			var gotRows []string
			for _, row := range rows {
				gotRows = append(gotRows, row.TypeID+":"+row.IfaceID)
			}
			slices.Sort(gotRows)
			slices.Sort(expectedRows)
			if !slices.Equal(gotRows, expectedRows) {
				t.Fatalf("filtered implements scope=%v filter=%+v got=%v want=%v", prefixes, filter, gotRows, expectedRows)
			}
		}
	}
}
