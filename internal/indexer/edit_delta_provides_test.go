package indexer

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// The provides rows a delta's resolver indexes are the stack's rows, kept per
// stack, with the delta's change set as its view holds it: every delta's
// answer equals a scan of its own view, a later delta over the same stack does
// not scan, and the earlier delta's changed file is served as the stack holds
// it once a later delta leaves it unchanged.
func TestEditDeltaProvidesRowsAreKeptPerStack(t *testing.T) {
	resetEditDeltaProvides()
	t.Cleanup(resetEditDeltaProvides)
	provides := func(file, from, forName, useClass string) ([]*graph.Node, *graph.Edge) {
		p := "repo/" + file
		nodes := []*graph.Node{
			{ID: p, Kind: graph.KindFile, Name: file, FilePath: p, RepoPrefix: "repo", Language: "typescript"},
			{ID: p + "::" + from, Kind: graph.KindType, Name: from, FilePath: p, RepoPrefix: "repo", Language: "typescript"},
		}
		edge := &graph.Edge{From: p + "::" + from, To: graph.UnresolvedMarker + useClass, Kind: graph.EdgeProvides, FilePath: p, Line: 3,
			Meta: map[string]any{graph.MetaDIProvidesFor: forName, graph.MetaDIBinding: graph.DIBindingUseClass}}
		return nodes, edge
	}
	build := func(files map[string][3]string) *graph.Graph {
		g := graph.New()
		keys := make([]string, 0, len(files))
		for k := range files {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, file := range keys {
			v := files[file]
			nodes, edge := provides(file, v[0], v[1], v[2])
			g.AddBatch(nodes, []*graph.Edge{edge})
		}
		return g
	}
	render := func(byRepo map[string][]*graph.Edge) string {
		var rows []string
		for repo, edges := range byRepo {
			for _, e := range edges {
				rows = append(rows, fmt.Sprintf("%s %s %s %v", repo, e.From, e.To, e.Meta[graph.MetaDIProvidesFor]))
			}
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	scan := func(g *graph.Graph) string {
		out := make(map[string][]*graph.Edge)
		for _, row := range scanProvidesRows(g) {
			out[row.repo] = append(out[row.repo], row.edge)
		}
		return render(out)
	}
	stackFiles := map[string][3]string{
		"x.ts": {"XModule", "Logger", "ConsoleLogger"},
		"y.ts": {"YModule", "Store", "MemoryStore"},
	}
	stack := build(stackFiles)
	deltaOver := func(changes map[string][3]string, changed ...string) (*editDeltaProvides, *graph.Graph) {
		files := make(map[string][3]string)
		for k, v := range stackFiles {
			files[k] = v
		}
		for k, v := range changes {
			files[k] = v
		}
		view := build(files)
		idx := &Indexer{repoPrefix: "repo", graph: view, resolver: resolver.New(view)}
		return installEditDeltaProvides(idx, stack, "stack", changed), view
	}

	// The first delta changes x.ts's binding: a miss, answered as its view.
	first, view1 := deltaOver(map[string][3]string{"x.ts": {"XModule", "Logger", "FileLogger"}}, "x.ts")
	if got, want := render(first.rows()), scan(view1); got != want || first.cached {
		t.Fatalf("first delta (cached=%t):\n%s\nwant:\n%s", first.cached, got, want)
	}
	// The second delta changes y.ts only: x.ts is back as the stack holds it.
	second, view2 := deltaOver(map[string][3]string{"y.ts": {"YModule", "Store", "DiskStore"}}, "y.ts")
	got, want := render(second.rows()), scan(view2)
	if got != want || !second.cached {
		t.Fatalf("second delta (cached=%t):\n%s\nwant:\n%s", second.cached, got, want)
	}
	if !strings.Contains(got, "ConsoleLogger") || strings.Contains(got, "FileLogger") {
		t.Fatalf("the first delta's change leaked into the stack's rows:\n%s", got)
	}
	// A delta that adds a file with a binding serves it.
	third, view3 := deltaOver(map[string][3]string{"z.ts": {"ZModule", "Clock", "SystemClock"}}, "z.ts")
	if got, want := render(third.rows()), scan(view3); got != want || !strings.Contains(got, "SystemClock") {
		t.Fatalf("third delta:\n%s\nwant:\n%s", got, want)
	}
}
