package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestFindNodesByNameBoundedCapsTenThousandHomonyms(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const homonyms = 10_000
	nodes := make([]*graph.Node, 0, homonyms+2)
	for index := 0; index < homonyms; index++ {
		nodes = append(nodes, &graph.Node{
			ID:          fmt.Sprintf("repo/src/handler-%05d.go::handle", index),
			Name:        "handle",
			Kind:        graph.KindFunction,
			FilePath:    fmt.Sprintf("repo/src/handler-%05d.go", index),
			RepoPrefix:  "repo",
			WorkspaceID: "workspace",
			ProjectID:   "project",
			Meta: map[string]any{
				"doc":             "payload that the localization projection must not hydrate",
				"search_doc":      "large search-only payload",
				"custom_metadata": stringsRepeatForProjectionTest("payload", 16),
			},
		})
	}
	// These rows prove every scope predicate is applied before COUNT and LIMIT.
	nodes = append(nodes,
		&graph.Node{ID: "foreign/src/f.go::handle", Name: "handle", Kind: graph.KindFunction, FilePath: "foreign/src/f.go", RepoPrefix: "foreign", WorkspaceID: "foreign"},
		&graph.Node{ID: "repo/src/value.go::handle", Name: "handle", Kind: graph.KindVariable, FilePath: "repo/src/value.go", RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project"},
	)
	store.BeginBulkLoad()
	store.AddBatch(nodes, nil)
	if err := store.FlushBulk(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	page, err := store.FindNodesByNameBounded(
		context.Background(),
		"handle",
		graph.LocalizationNodeScope{
			WorkspaceID: "workspace",
			ProjectID:   "project",
			RepoAllow:   map[string]bool{"repo": true},
			Kinds:       map[graph.NodeKind]bool{graph.KindFunction: true},
		},
		8,
	)
	if err != nil {
		t.Fatalf("bounded lookup: %v", err)
	}
	if page.Total != 9 {
		t.Fatalf("total = %d, want threshold total 9", page.Total)
	}
	if !page.Truncated {
		t.Fatal("truncated = false, want LIMIT+1 sentinel saturation")
	}
	if len(page.Nodes) != 8 {
		t.Fatalf("nodes = %d, want hard cap 8", len(page.Nodes))
	}
	for index, node := range page.Nodes {
		want := fmt.Sprintf("repo/src/handler-%05d.go::handle", index)
		if node.ID != want {
			t.Fatalf("node[%d] = %q, want deterministic %q", index, node.ID, want)
		}
		if _, hydrated := node.Meta["doc"]; hydrated {
			t.Fatalf("node[%d] hydrated promoted doc payload", index)
		}
		if _, hydrated := node.Meta["custom_metadata"]; hydrated {
			t.Fatalf("node[%d] retained unrelated metadata", index)
		}
	}
}

func TestFindNodesByNameBoundedFindsProductionRowsBehindTests(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	nodes := make([]*graph.Node, 0, 42)
	for index := 0; index < 32; index++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("a-tests/%02d.go::handle", index), Name: "handle",
			Kind: graph.KindFunction, FilePath: fmt.Sprintf("a-tests/%02d.go", index),
			Meta: map[string]any{"is_test": true},
		})
	}
	for index := 0; index < 10; index++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("z-prod/%02d.go::handle", index), Name: "handle",
			Kind: graph.KindFunction, FilePath: fmt.Sprintf("z-prod/%02d.go", index),
		})
	}
	store.BeginBulkLoad()
	store.AddBatch(nodes, nil)
	if err := store.FlushBulk(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	page, err := store.FindNodesByNameBounded(
		context.Background(), "handle", graph.LocalizationNodeScope{ExcludeTests: true}, 8,
	)
	if err != nil {
		t.Fatalf("bounded lookup: %v", err)
	}
	if len(page.Nodes) != 8 || page.Total != 9 || !page.Truncated {
		t.Fatalf("page = %#v, want eight production nodes and saturation sentinel", page)
	}
	for _, node := range page.Nodes {
		if len(node.FilePath) < len("z-prod/") || node.FilePath[:len("z-prod/")] != "z-prod/" {
			t.Fatalf("test declaration leaked into production page: %#v", node)
		}
	}
}

func TestFindNodesByNameBoundedPreservesTestClassification(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddNode(&graph.Node{
		ID: "repo/handler_test.go::handle", Name: "handle", Kind: graph.KindFunction,
		FilePath: "repo/handler_test.go", Meta: map[string]any{"is_test": true, "doc": "omit me"},
	})

	page, err := store.FindNodesByNameBounded(context.Background(), "handle", graph.LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatalf("bounded lookup: %v", err)
	}
	if len(page.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(page.Nodes))
	}
	if isTest, _ := page.Nodes[0].Meta["is_test"].(bool); !isTest {
		t.Fatalf("projected meta = %#v, want is_test classification", page.Nodes[0].Meta)
	}
	if _, hydrated := page.Nodes[0].Meta["doc"]; hydrated {
		t.Fatal("bounded projection hydrated doc")
	}
}

func TestFindNodesByNameBoundedHonorsCancellation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddNode(&graph.Node{ID: "repo/f.go::handle", Name: "handle", Kind: graph.KindFunction, FilePath: "repo/f.go"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err := store.FindNodesByNameBounded(ctx, "handle", graph.LocalizationNodeScope{}, 8)
	if err == nil {
		t.Fatalf("error = nil with cancelled context; page = %#v", page)
	}
	if len(page.Nodes) != 0 || page.Total != 0 {
		t.Fatalf("cancelled lookup returned partial page %#v", page)
	}
}

func TestOverlaidViewFindNodesByNameBoundedAppliesSQLiteScopeWithoutMutatingCaller(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	nodes := make([]*graph.Node, 0, 302)
	for index := 0; index < 300; index++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("repo/a-overlay-hidden.go::handle:%03d", index), Name: "handle",
			Kind: graph.KindFunction, FilePath: "repo/a-overlay-hidden.go",
		})
	}
	nodes = append(nodes,
		&graph.Node{
			ID: "repo/b-caller-hidden.go::handle", Name: "handle", Kind: graph.KindFunction,
			FilePath: "repo/b-caller-hidden.go",
		},
		&graph.Node{
			ID: "repo/z-visible.go::handle", Name: "handle", Kind: graph.KindFunction,
			FilePath: "repo/z-visible.go",
		},
	)
	store.BeginBulkLoad()
	store.AddBatch(nodes, nil)
	if err := store.FlushBulk(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/a-overlay-hidden.go", true)
	callerFiles := map[string]bool{"repo/b-caller-hidden.go": true}
	page, err := graph.NewOverlaidView(store, layer).FindNodesByNameBounded(
		context.Background(), "handle",
		graph.LocalizationNodeScope{ExcludeFiles: callerFiles}, 8,
	)
	if err != nil {
		t.Fatalf("bounded SQLite overlay lookup: %v", err)
	}
	if page.Total != 1 || page.Truncated || len(page.Nodes) != 1 ||
		page.Nodes[0].FilePath != "repo/z-visible.go" {
		t.Fatalf("page = %#v, want only the visible row behind excluded keyset pages", page)
	}
	if len(callerFiles) != 1 || !callerFiles["repo/b-caller-hidden.go"] {
		t.Fatalf("caller exclusion map was mutated: %#v", callerFiles)
	}
	if _, added := callerFiles["repo/a-overlay-hidden.go"]; added {
		t.Fatalf("overlay path leaked into caller exclusion map: %#v", callerFiles)
	}
}

func TestFindFileNodesBoundedCapsScopesKindsAndDropsPayloads(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const filePath = "shared/dense.go"
	nodes := make([]*graph.Node, 0, 192)
	for index := 0; index < 64; index++ {
		nodes = append(nodes,
			&graph.Node{
				ID: fmt.Sprintf("repo/dense.go::fn-%03d", index), Name: fmt.Sprintf("fn%d", index),
				Kind: graph.KindFunction, FilePath: filePath, RepoPrefix: "repo",
				WorkspaceID: "workspace", ProjectID: "project",
				Meta: map[string]any{
					"signature":       "func with promoted payload",
					"doc":             stringsRepeatForProjectionTest("documentation", 32),
					"custom_metadata": stringsRepeatForProjectionTest("metadata", 32),
				},
			},
			&graph.Node{
				ID: fmt.Sprintf("repo/dense.go::value-%03d", index), Name: fmt.Sprintf("value%d", index),
				Kind: graph.KindVariable, FilePath: filePath, RepoPrefix: "repo",
				WorkspaceID: "workspace", ProjectID: "project",
			},
			&graph.Node{
				ID: fmt.Sprintf("foreign/dense.go::fn-%03d", index), Name: fmt.Sprintf("foreign%d", index),
				Kind: graph.KindFunction, FilePath: filePath, RepoPrefix: "foreign",
				WorkspaceID: "foreign", ProjectID: "foreign",
			},
		)
	}
	store.BeginBulkLoad()
	store.AddBatch(nodes, nil)
	if err := store.FlushBulk(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	page, err := store.FindFileNodesBounded(
		context.Background(), filePath,
		graph.LocalizationNodeScope{
			WorkspaceID: "workspace", ProjectID: "project",
			RepoAllow: map[string]bool{"repo": true},
			Kinds:     map[graph.NodeKind]bool{graph.KindFunction: true},
		},
		8,
	)
	if err != nil {
		t.Fatalf("bounded file lookup: %v", err)
	}
	if page.Total != 9 || !page.Truncated || len(page.Nodes) != 8 {
		t.Fatalf("page = %#v, want threshold total 9, truncated, cap 8", page)
	}
	if cap(page.Nodes) != len(page.Nodes) {
		t.Fatalf("page capacity = %d, want exact returned length %d", cap(page.Nodes), len(page.Nodes))
	}
	for index, node := range page.Nodes {
		want := fmt.Sprintf("repo/dense.go::fn-%03d", index)
		if node.ID != want {
			t.Fatalf("node[%d] = %q, want deterministic %q", index, node.ID, want)
		}
		if node.Kind != graph.KindFunction || node.RepoPrefix != "repo" || node.WorkspaceID != "workspace" {
			t.Fatalf("scope or kind was applied after cap: %#v", node)
		}
		if node.Meta != nil {
			t.Fatalf("summary node[%d] hydrated metadata: %#v", index, node.Meta)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if cancelled, err := store.FindFileNodesBounded(ctx, filePath, graph.LocalizationNodeScope{}, 8); err == nil || len(cancelled.Nodes) != 0 {
		t.Fatalf("cancelled file lookup = %#v, %v; want empty error result", cancelled, err)
	}
}

func TestFindFileNodesBoundedFindsProductionRowsBehindTests(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const filePath = "repo/dense.go"
	nodes := make([]*graph.Node, 0, 310)
	for index := 0; index < 300; index++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("repo/dense.go::a-test-%03d", index), Name: "test",
			Kind: graph.KindFunction, FilePath: filePath, Meta: map[string]any{"is_test": true},
		})
	}
	for index := 0; index < 10; index++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("repo/dense.go::z-prod-%03d", index), Name: "prod",
			Kind: graph.KindFunction, FilePath: filePath,
		})
	}
	store.BeginBulkLoad()
	store.AddBatch(nodes, nil)
	if err := store.FlushBulk(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	page, err := store.FindFileNodesBounded(
		context.Background(), filePath, graph.LocalizationNodeScope{ExcludeTests: true}, 8,
	)
	if err != nil {
		t.Fatalf("bounded file lookup: %v", err)
	}
	if page.Total != 9 || !page.Truncated || len(page.Nodes) != 8 {
		t.Fatalf("page = %#v, want production sentinel after test rows", page)
	}
	for _, node := range page.Nodes {
		if node.Name != "prod" {
			t.Fatalf("test declaration consumed production cap: %#v", node)
		}
	}
}

func TestFindFileNodesBoundedFindsDefinitionsBehindExcludedKinds(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const filePath = "repo/generated.go"
	nodes := make([]*graph.Node, 0, 310)
	for index := 0; index < 300; index++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("repo/generated.go::a-param-%03d", index), Name: "arg",
			Kind: graph.KindParam, FilePath: filePath,
		})
	}
	for index := 0; index < 10; index++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("repo/generated.go::z-function-%03d", index), Name: "function",
			Kind: graph.KindFunction, FilePath: filePath,
		})
	}
	store.BeginBulkLoad()
	store.AddBatch(nodes, nil)
	if err := store.FlushBulk(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	page, err := store.FindFileNodesBounded(
		context.Background(), filePath,
		graph.LocalizationNodeScope{ExcludeKinds: map[graph.NodeKind]bool{graph.KindParam: true}}, 8,
	)
	if err != nil {
		t.Fatalf("bounded file lookup: %v", err)
	}
	if page.Total != 9 || !page.Truncated || len(page.Nodes) != 8 {
		t.Fatalf("page = %#v, want definition sentinel behind excluded params", page)
	}
	for _, node := range page.Nodes {
		if node.Kind != graph.KindFunction {
			t.Fatalf("excluded kind consumed the cap: %#v", node)
		}
	}
}

func TestFindFileNodesBoundedDoesNotTransferMetadataWhenTestsIncluded(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const nodeID = "repo/file.go::handler"
	store.AddNode(&graph.Node{
		ID: nodeID, Name: "handler", Kind: graph.KindFunction, FilePath: "repo/file.go",
		Meta: map[string]any{"signature": "func handler()", "doc": "promoted payload"},
	})
	if _, err := store.writerDB.Exec(`UPDATE nodes SET meta = ? WHERE id = ?`, []byte("not-valid-metadata"), nodeID); err != nil {
		t.Fatalf("corrupt metadata fixture: %v", err)
	}

	page, err := store.FindFileNodesBounded(
		context.Background(), "repo/file.go", graph.LocalizationNodeScope{}, 8,
	)
	if err != nil {
		t.Fatalf("summary lookup decoded metadata it must not select: %v", err)
	}
	if len(page.Nodes) != 1 || page.Nodes[0].ID != nodeID {
		t.Fatalf("page = %#v, want one summary node", page)
	}
	if page.Nodes[0].Meta != nil {
		t.Fatalf("summary hydrated metadata: %#v", page.Nodes[0].Meta)
	}
}

func TestFindFileNodesBoundedPlanUsesFileIndexWithoutSorter(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddNode(&graph.Node{
		ID: "repo/file.go::handler", Name: "handler", Kind: graph.KindFunction,
		FilePath: "repo/file.go", RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project",
	})

	predicate, args := localizationFileNodePredicate("repo/file.go", graph.LocalizationNodeScope{
		WorkspaceID: "workspace", ProjectID: "project",
		RepoAllow:    map[string]bool{"repo": true},
		Kinds:        map[graph.NodeKind]bool{graph.KindFunction: true, graph.KindMethod: true},
		ExcludeKinds: map[graph.NodeKind]bool{graph.KindParam: true, graph.KindLocal: true},
	})
	args = append(args, store.viewGen, "", 257)
	rows, err := store.db.Query(
		`EXPLAIN QUERY PLAN SELECT `+lookupNodeSummaryCols+` FROM nodes INDEXED BY nodes_by_file WHERE `+predicate+` AND view_gen = ? AND id > ? ORDER BY id LIMIT ?`,
		args...,
	)
	if err != nil {
		t.Fatalf("explain bounded file query: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query plan rows: %v", err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "nodes_by_file") {
		t.Fatalf("query plan does not use nodes_by_file:\n%s", plan)
	}
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Fatalf("query plan sorts bounded file rows:\n%s", plan)
	}
}

func stringsRepeatForProjectionTest(value string, count int) string {
	out := ""
	for index := 0; index < count; index++ {
		out += value
	}
	return out
}
