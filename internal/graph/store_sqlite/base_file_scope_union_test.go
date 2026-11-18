package store_sqlite

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestBaseFileScopeUnionPreservesPagedRowsAndPayload(t *testing.T) {
	store := openScopedProjectionTestStore(t)
	const perRepo = scopedProjectionPage + 19
	var nodes, positive []*graph.Node
	for i := 0; i < perRepo; i++ {
		for ordinal, repo := range []string{"", "a", "b", "foreign"} {
			id := fmt.Sprintf("row-%04d-%d", i, ordinal)
			node := &graph.Node{ID: id, Kind: graph.KindFile, Name: fmt.Sprintf("file-%04d.go", i), FilePath: fmt.Sprintf("%s/file-%04d.go", repo, i), RepoPrefix: repo, Language: "go", WorkspaceID: "workspace", ProjectID: "project", Meta: map[string]any{"ordinal": i, "nested": map[string]any{"repo": repo}}}
			nodes = append(nodes, node)
			copy := *node
			copy.Name = "positive-only"
			copy.Meta = map[string]any{"generation": 41}
			positive = append(positive, &copy)
		}
	}
	if err := store.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AtGeneration(41).AddBatchChecked(positive, nil); err != nil {
		t.Fatal(err)
	}
	removed, _ := store.EvictFile("a/file-0000.go")
	if removed < 1 {
		t.Fatal("fixture file was not evicted")
	}
	reference := func(generation int64) []*graph.Node {
		t.Helper()
		rows, err := store.db.Query(`SELECT `+lookupNodeCols+` FROM nodes WHERE kind=? AND view_gen=? AND repo_prefix IN ('','a','b') ORDER BY id`, graph.KindFile, generation)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []*graph.Node
		for rows.Next() {
			node, err := scanNodeCursor(rows)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, node)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, generation := range []int64{0, 41} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			want := reference(generation)
			wantCount := perRepo * 3
			if generation == 0 {
				wantCount--
			}
			if len(want) != wantCount {
				t.Fatalf("generation %d reference rows=%d, want %d; eviction crossed its generation", generation, len(want), wantCount)
			}
			var got []*graph.Node
			for node := range store.AtGeneration(generation).NodesInScopeSeq([]string{"b", "", "a", "b", ""}, nil, graph.KindFile) {
				got = append(got, node)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("generation %d: selected full payload/order differs (got %d, want %d)", generation, len(got), len(want))
			}
			if len(got) <= scopedProjectionPage*2 {
				t.Fatal("fixture does not cross multiple global pages")
			}
			if generation == 0 {
				query, args, ok := scopedNodeProjectionQuery([]string{"b", "", "a", "b", ""}, nil, string(graph.KindFile), lookupNodeCols, generation)
				if !ok {
					t.Fatal("missing bounded query")
				}
				last := ""
				var paged []*graph.Node
				for {
					rows, err := store.db.Query(query, append(append([]any(nil), args...), last, scopedProjectionPage)...)
					if err != nil {
						t.Fatal(err)
					}
					count := 0
					for rows.Next() {
						node, err := scanNodeCursor(rows)
						if err != nil {
							_ = rows.Close()
							t.Fatal(err)
						}
						if node.ID <= last {
							_ = rows.Close()
							t.Fatal("global cursor repeated/skipped order")
						}
						last = node.ID
						paged = append(paged, node)
						count++
					}
					err = rows.Err()
					_ = rows.Close()
					if err != nil {
						t.Fatal(err)
					}
					if count > scopedProjectionPage {
						t.Fatal("outer page exceeded its bound")
					}
					if count < scopedProjectionPage {
						break
					}
				}
				if !reflect.DeepEqual(paged, want) {
					t.Fatal("numbered shared cursor/limit lost full-row parity")
				}
			}
		})
	}
}

func TestBaseFileScopeUnionUsesBoundedRepoSeeks(t *testing.T) {
	store := openScopedProjectionTestStore(t)
	query, args, ok := scopedNodeProjectionQuery([]string{"repo", "", "repo"}, nil, string(graph.KindFile), lookupNodeCols, 0)
	if !ok || !strings.Contains(query, "UNION ALL") {
		t.Fatal("two normalized scopes did not use bounded union")
	}
	if len(args) != 4 || args[0] != "" || args[1] != "repo" {
		t.Fatalf("repo normalization/empty sentinel changed: %#v", args)
	}
	plan := scopedProjectionPlan(t, store, query, append(append([]any(nil), args...), "", scopedProjectionPage)...)
	if strings.Count(plan, "nodes_by_repo_kind (repo_prefix=? AND kind=? AND id>?)") != 2 {
		t.Fatalf("expected one repo/kind seek per branch:\n%s", plan)
	}
	if strings.Contains(plan, "USING PRIMARY KEY") || strings.Contains(plan, "nodes_by_generation") || strings.Contains(plan, "nodes_by_kind (") {
		t.Fatalf("base union uses a broad nodes walk:\n%s", plan)
	}
	// Each branch and global merge use the same numbered limit; the existing
	// pager binds it once. Any temp order is bounded by branch row caps.
	if strings.Count(query, "LIMIT ?6") != 3 || strings.Count(query, "n.id > ?5") != 2 {
		t.Fatalf("cursor/limit are not shared by every arm:\n%s", query)
	}
}

func TestBaseFileScopeUnionRetainsUnsupportedFallbacks(t *testing.T) {
	var many []string
	for i := 0; i < scopedBaseFileRepoUnionArms+1; i++ {
		many = append(many, fmt.Sprintf("repo-%02d", i))
	}
	cases := []struct {
		name         string
		repos, files []string
		kind         string
		generation   int64
	}{
		{"positive generation", []string{"", "repo"}, nil, string(graph.KindFile), 41},
		{"exact file frontier", []string{"", "repo"}, []string{"repo/file.go"}, string(graph.KindFile), 0},
		{"contract kind", []string{"", "repo"}, nil, string(graph.KindContract), 0},
		{"no kind", []string{"", "repo"}, nil, "", 0},
		{"too many normalized repos", many, nil, string(graph.KindFile), 0},
		{"only unowned", []string{"", ""}, nil, string(graph.KindFile), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query, args, ok := scopedNodeProjectionQuery(tc.repos, tc.files, tc.kind, lookupNodeCols, tc.generation)
			if !ok {
				t.Fatal("previously supported scope was refused")
			}
			if strings.Contains(query, "UNION ALL") || !strings.Contains(query, "json_each") || !strings.Contains(query, "n.view_gen = ?") {
				t.Fatalf("unsupported scope no longer uses original query:\n%s", query)
			}
			if tc.generation == 41 {
				want := `WITH requested_repos(repo_prefix) AS (SELECT CAST(value AS TEXT) FROM json_each(?)) SELECT ` + qualifiedNodeColumns("n", lookupNodeCols) + ` FROM nodes AS n JOIN requested_repos AS r ON r.repo_prefix = +n.repo_prefix WHERE n.kind = ? AND n.view_gen = ? AND n.id > ? ORDER BY n.id LIMIT ?`
				if query != want || !reflect.DeepEqual(args, []any{`["","repo"]`, string(graph.KindFile), int64(41)}) {
					t.Fatal("positive generation SQL or args changed")
				}
			}
		})
	}
}
