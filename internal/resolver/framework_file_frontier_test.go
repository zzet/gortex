package resolver

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// frameworkFileFrontierNodes reads a changed-file frontier through the file
// index; it must yield the rows, in the order, the SQLite scoped projections
// yield for the same repositories, files and kinds.
func TestFrameworkFileFrontierNodesMatchTheScopedProjections(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "frontier.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var nodes []*graph.Node
	kinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindField}
	for i := 0; i < 90; i++ {
		repo := []string{"a", "b", ""}[i%3]
		file := fmt.Sprintf("%s/f%d.go", repo, i%7)
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("%s::n%02d", file, i), Kind: kinds[i%len(kinds)], Name: fmt.Sprintf("n%d", i%11),
			FilePath: file, RepoPrefix: repo, Language: "go", Meta: map[string]any{"i": i},
		})
	}
	store.AddBatch(nodes, nil)
	ids := func(seq func(func(*graph.Node) bool)) []string {
		var out []string
		for node := range seq {
			out = append(out, fmt.Sprintf("%s|%s|%s|%v", node.ID, node.Kind, node.RepoPrefix, node.Meta["i"]))
		}
		return out
	}
	cases := []struct {
		repos, files []string
		kinds        []graph.NodeKind
	}{
		{[]string{"a"}, []string{"a/f0.go", "a/f3.go"}, []graph.NodeKind{graph.KindMethod}},
		{[]string{"a", "b"}, []string{"a/f0.go", "b/f1.go", "b/f4.go", "missing.go"}, []graph.NodeKind{graph.KindField, graph.KindFunction}},
		{nil, []string{"b/f1.go", "/f2.go"}, []graph.NodeKind{graph.KindType}},
		{[]string{""}, []string{"/f2.go", "/f5.go"}, []graph.NodeKind{graph.KindFunction}},
		{[]string{"b"}, []string{"a/f0.go"}, []graph.NodeKind{graph.KindMethod}},
	}
	for i, tc := range cases {
		want := ids(graph.NodesInScopeSeq(store, tc.repos, tc.files, tc.kinds...))
		got := ids(frameworkFileFrontierNodes(store, tc.repos, tc.files, tc.kinds...))
		if fmt.Sprint(want) != fmt.Sprint(got) {
			t.Fatalf("case %d kinds: projection %v, file read %v", i, want, got)
		}
		wantLight := ids(graph.NodesLightInScopeSeq(store, tc.repos, tc.files))
		gotLight := ids(frameworkFileFrontierNodes(store, tc.repos, tc.files))
		// The light projection carries no Meta; compare identities only.
		if len(wantLight) != len(gotLight) {
			t.Fatalf("case %d census: projection %d rows, file read %d rows", i, len(wantLight), len(gotLight))
		}
		for j := range wantLight {
			if lightID(wantLight[j]) != lightID(gotLight[j]) {
				t.Fatalf("case %d census row %d: projection %s, file read %s", i, j, wantLight[j], gotLight[j])
			}
		}
		if i == 0 && len(want) == 0 {
			t.Fatal("the first case selected nothing; the comparison is vacuous")
		}
	}
}

func lightID(row string) string {
	for i := 0; i < len(row); i++ {
		if row[i] == '|' {
			return row[:i]
		}
	}
	return row
}
