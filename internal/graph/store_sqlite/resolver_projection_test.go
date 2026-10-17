package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func openResolverProjectionTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close SQLite store: %v", err)
		}
	})
	return store
}

func collectResolverProjection[T any](seq func(func(T) bool)) []T {
	var rows []T
	for row := range seq {
		rows = append(rows, row)
	}
	return rows
}

func TestResolverProjectionRowsIgnoreMetadata(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	store.AddBatch([]*graph.Node{
		{
			ID: "a::src/main.go", Kind: graph.KindFile, Name: "main.go",
			FilePath: "a::src/main.go", RepoPrefix: "a", WorkspaceID: "workspace-a",
			Meta: map[string]any{"large": make([]byte, 1<<20)},
		},
		{
			ID: "a::src/main.go::local", Kind: graph.KindLocal, Name: "local",
			FilePath: "a::src/main.go", RepoPrefix: "a", StartLine: 12,
			Meta: map[string]any{"large": make([]byte, 1<<20)},
		},
		{
			ID: "b::lib.go", Kind: graph.KindFile, Name: "lib.go",
			FilePath: "b::lib.go", RepoPrefix: "b", WorkspaceID: "workspace-b",
			Meta: map[string]any{"large": make([]byte, 1<<20)},
		},
	}, nil)

	// A full node scan would have to decode this invalid payload. Resolver
	// projections select only fixed columns, so malformed residual metadata is
	// deliberately irrelevant to all three reads below.
	if _, err := store.writerDB.Exec(`UPDATE nodes SET meta = ?`, []byte{0xff, 0x00, 0x7f}); err != nil {
		t.Fatalf("corrupt residual metadata fixture: %v", err)
	}

	entries := collectResolverProjection(store.ScopeBindingNodesSeq(graph.KindLocal, graph.KindLocal))
	wantEntries := []graph.ScopeBindingNode{{
		ID: "a::src/main.go::local", Kind: graph.KindLocal, Name: "local", StartLine: 12,
	}}
	if !reflect.DeepEqual(entries, wantEntries) {
		t.Fatalf("index entries = %#v, want %#v", entries, wantEntries)
	}

	globalFiles := collectResolverProjection(store.FileNodeIdentitiesSeq(nil))
	wantGlobalFiles := []graph.FileNodeIdentity{
		{ID: "a::src/main.go", FilePath: "a::src/main.go", RepoPrefix: "a", WorkspaceID: "workspace-a"},
		{ID: "b::lib.go", FilePath: "b::lib.go", RepoPrefix: "b", WorkspaceID: "workspace-b"},
	}
	if !reflect.DeepEqual(globalFiles, wantGlobalFiles) {
		t.Fatalf("global file identities = %#v, want %#v", globalFiles, wantGlobalFiles)
	}

	scopedFiles := collectResolverProjection(store.FileNodeIdentitiesSeq([]string{"b", "b"}))
	if want := wantGlobalFiles[1:]; !reflect.DeepEqual(scopedFiles, want) {
		t.Fatalf("scoped file identities = %#v, want %#v", scopedFiles, want)
	}
	if empty := collectResolverProjection(store.FileNodeIdentitiesSeq([]string{})); len(empty) != 0 {
		t.Fatalf("non-nil empty repository scope returned %d rows", len(empty))
	}

	placements := store.NodePlacementsByIDs([]string{
		"", "a::src/main.go", "a::src/main.go", "a::src/main.go::local", "missing",
	})
	wantPlacements := map[string]graph.NodePlacement{
		"a::src/main.go":        {Kind: graph.KindFile, FilePath: "a::src/main.go", RepoPrefix: "a"},
		"a::src/main.go::local": {Kind: graph.KindLocal, FilePath: "a::src/main.go", RepoPrefix: "a"},
	}
	if !reflect.DeepEqual(placements, wantPlacements) {
		t.Fatalf("placements = %#v, want %#v", placements, wantPlacements)
	}
}

func TestNodePlacementsByIDsChunksAndDeduplicates(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	firstID := "repo::first.go"
	secondID := "repo::second.go"
	store.AddBatch([]*graph.Node{
		{ID: firstID, Kind: graph.KindFile, Name: "first.go", FilePath: firstID, RepoPrefix: "repo"},
		{ID: secondID, Kind: graph.KindFile, Name: "second.go", FilePath: secondID, RepoPrefix: "repo"},
	}, nil)

	ids := make([]string, lookupChunkSize+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("missing-%04d", i)
	}
	ids[0] = firstID
	ids[lookupChunkSize] = secondID
	ids = append(ids, firstID, "", "missing")

	placements := store.NodePlacementsByIDs(ids)
	if len(placements) != 2 {
		t.Fatalf("placements returned %d rows, want 2", len(placements))
	}
	for _, id := range []string{firstID, secondID} {
		if got, ok := placements[id]; !ok || got.FilePath != id || got.Kind != graph.KindFile || got.RepoPrefix != "repo" {
			t.Fatalf("placement[%q] = %#v, %v", id, got, ok)
		}
	}
	if _, ok := placements["missing"]; ok {
		t.Fatal("missing ID unexpectedly returned a placement")
	}
}

func TestInEdgeIdentitiesByNodeIDsChunksAndIgnoresMetadata(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	const (
		firstSource  = "repo::first.go::source"
		firstTarget  = "unresolved::first"
		secondSource = "repo::second.go::source"
		secondTarget = "unresolved::second"
	)
	store.AddBatch([]*graph.Node{
		{ID: firstSource, Kind: graph.KindFunction, Name: "first", FilePath: "repo::first.go", RepoPrefix: "repo"},
		{ID: secondSource, Kind: graph.KindFunction, Name: "second", FilePath: "repo::second.go", RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: firstSource, To: firstTarget, Kind: graph.EdgeCalls, FilePath: "repo::first.go", Line: 7, Meta: map[string]any{"large": make([]byte, 1<<20)}},
		{From: firstSource, To: firstTarget, Kind: graph.EdgeReferences, FilePath: "repo::first.go", Line: 8, Meta: map[string]any{"large": make([]byte, 1<<20)}},
		{From: secondSource, To: secondTarget, Kind: graph.EdgeCalls, FilePath: "repo::second.go", Line: 9, Meta: map[string]any{"large": make([]byte, 1<<20)}},
	})
	if _, err := store.writerDB.Exec(`UPDATE edges SET meta = ?`, []byte{0xff, 0x00, 0x7f}); err != nil {
		t.Fatalf("corrupt residual edge metadata fixture: %v", err)
	}

	ids := make([]string, lookupChunkSize+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("missing-%04d", i)
	}
	ids[0] = firstTarget
	ids[lookupChunkSize] = secondTarget
	ids = append(ids, firstTarget, "", "missing")
	got := store.GetInEdgeIdentitiesByNodeIDs(ids)
	want := map[string]map[graph.EdgeIdentity]struct{}{
		firstTarget: {
			{From: firstSource, To: firstTarget, Kind: graph.EdgeCalls, FilePath: "repo::first.go", Line: 7}:      {},
			{From: firstSource, To: firstTarget, Kind: graph.EdgeReferences, FilePath: "repo::first.go", Line: 8}: {},
		},
		secondTarget: {
			{From: secondSource, To: secondTarget, Kind: graph.EdgeCalls, FilePath: "repo::second.go", Line: 9}: {},
		},
	}
	if len(got) != len(want) {
		t.Fatalf("incoming identity buckets = %d, want %d", len(got), len(want))
	}
	for target, wanted := range want {
		if len(got[target]) != len(wanted) {
			t.Fatalf("incoming identities[%q] = %#v, want %d rows", target, got[target], len(wanted))
		}
		for _, identity := range got[target] {
			if _, ok := wanted[identity]; !ok {
				t.Fatalf("unexpected incoming identity for %q: %#v", target, identity)
			}
		}
	}
}

func TestResolverProjectionYieldCanReenterStore(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	store.AddBatch([]*graph.Node{
		{
			ID: "repo::file.go", Kind: graph.KindFile, Name: "file.go",
			FilePath: "repo::file.go", RepoPrefix: "repo",
		},
		{
			ID: "repo::file.go::local", Kind: graph.KindLocal, Name: "local",
			FilePath: "repo::file.go", RepoPrefix: "repo", StartLine: 3,
		},
	}, nil)
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)

	assertReentry := func(label string) {
		t.Helper()
		// Background startup probes can own the pool independently of this
		// iterator. Bounded re-entry on a one-connection pool proves that
		// the iterator released its rows before invoking the callback.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var one int
		if err := store.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("%s re-entry query: %v", label, err)
		}
		if one != 1 {
			t.Fatalf("%s SELECT 1 = %d", label, one)
		}
	}

	for range store.ScopeBindingNodesSeq(graph.KindLocal) {
		assertReentry("node index")
		break
	}
	for range store.FileNodeIdentitiesSeq(nil) {
		assertReentry("file identity")
		break
	}
}

func TestResolverProjectionKeysetFreezesHighWaterAcrossPages(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	rowCount := resolverProjectionPageSize + 1
	nodes := make([]*graph.Node, 0, rowCount*2)
	wantBindingIDs := make([]string, 0, rowCount)
	wantFileIDs := make([]string, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		fileID := fmt.Sprintf("repo::file-%04d.go", i)
		localID := fileID + "::local"
		nodes = append(nodes,
			&graph.Node{
				ID: fileID, Kind: graph.KindFile, Name: fmt.Sprintf("file-%04d.go", i),
				FilePath: fileID, RepoPrefix: "repo",
			},
			&graph.Node{
				ID: localID, Kind: graph.KindLocal, Name: fmt.Sprintf("local-%04d", i),
				FilePath: fileID, RepoPrefix: "repo",
			},
		)
		wantFileIDs = append(wantFileIDs, fileID)
		wantBindingIDs = append(wantBindingIDs, localID)
	}
	store.AddBatch(nodes, nil)

	var bindingIDs []string
	insertedBinding := false
	for row := range store.ScopeBindingNodesSeq(graph.KindLocal) {
		bindingIDs = append(bindingIDs, row.ID)
		if !insertedBinding {
			insertedBinding = true
			store.AddBatch([]*graph.Node{{
				ID: "z::z.go::local", Kind: graph.KindLocal, Name: "z", FilePath: "z::z.go",
			}}, nil)
		}
	}
	if !reflect.DeepEqual(bindingIDs, wantBindingIDs) {
		t.Fatalf("binding page IDs = %v, want %v", bindingIDs, wantBindingIDs)
	}

	var fileIDs []string
	insertedFile := false
	for row := range store.FileNodeIdentitiesSeq(nil) {
		fileIDs = append(fileIDs, row.ID)
		if !insertedFile {
			insertedFile = true
			store.AddBatch([]*graph.Node{{
				ID: "z::z.go", Kind: graph.KindFile, Name: "z.go", FilePath: "z::z.go", RepoPrefix: "z",
			}}, nil)
		}
	}
	if !reflect.DeepEqual(fileIDs, wantFileIDs) {
		t.Fatalf("file page IDs = %v, want %v", fileIDs, wantFileIDs)
	}
}
