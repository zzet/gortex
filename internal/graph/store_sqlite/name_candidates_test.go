package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestNameCandidateIndexMigrationAndBulkLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.AddNode(&graph.Node{ID: "repo/a.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"})
	if _, err := s.writerDB.Exec(`DROP INDEX nodes_name_candidates`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writerDB.Exec(`PRAGMA user_version = 29`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("migrated version=%d,%v", version, err)
	}
	s.BeginBulkLoad()
	got, err := s.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 1, graph.NameSearchFilter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("candidate index missing during bulk window=%v,%v", got, err)
	}
	if err := s.FlushBulk(); err != nil {
		t.Fatal(err)
	}
	got, err = s.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 1, graph.NameSearchFilter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("candidate index missing after bulk rebuild=%v,%v", got, err)
	}
}

func TestNameCandidateProjectionIsCoveringForEveryRepoScope(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	for _, allow := range []map[string]bool{nil, {"repo": true}, {"repo": true, "other": true}, {"repo": false}} {
		q, args := nameCandidateSQL(0, "needle", graph.NameSearchFilter{RepoAllow: allow})
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan += detail + "\n"
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, "USING COVERING INDEX nodes_name_candidates (view_gen=?") {
			t.Fatalf("scope %v does not use the covering generation range:\n%s", allow, plan)
		}
		if len(allow) > 0 && !strings.Contains(plan, "repo_prefix=?") {
			t.Fatalf("missing repo seek:\n%s", plan)
		}
	}
}

func TestNameCandidatesFilterBeforeLimitAndDoNotDecodeDiscardedPayloads(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	nodes := []*graph.Node{
		{ID: "a/other.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "a/other.go", RepoPrefix: "a"},
		{ID: "b/outside.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "b/outside.go", RepoPrefix: "b"},
		{ID: "b/target.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "b/target.go", RepoPrefix: "b", WorkspaceID: "ws", ProjectID: "proj", Meta: map[string]any{"payload": "chosen"}},
		{ID: "external::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "external::needle"},
	}
	s.AddBatch(nodes, nil)
	// A full-row fallback would attempt to decode these deliberately broken
	// discarded payloads; compact filtering must never touch their metadata.
	if _, err := s.writerDB.Exec(`UPDATE nodes SET meta = ? WHERE id IN (?,?)`, []byte("invalid metadata"), nodes[0].ID, nodes[1].ID); err != nil {
		t.Fatal(err)
	}
	filter := graph.NameSearchFilter{RepoAllow: map[string]bool{"b": true}, Accept: func(n *graph.Node) bool {
		if n.Meta != nil {
			t.Fatal("predicate received hydrated metadata")
		}
		return n.FilePath == "b/target.go" && n.WorkspaceID == "ws" && n.ProjectID == "proj"
	}}
	got, err := s.FindNodesByNameContainingFilteredContext(context.Background(), "NEEDLE", 1, filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != nodes[2].ID || got[0].Meta["payload"] != "chosen" {
		t.Fatalf("selected payload = %#v", got)
	}
	got, err = s.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 0, graph.NameSearchFilter{RepoAllow: map[string]bool{"b": false}})
	if err != nil || !reflect.DeepEqual(nodeIDs(got), []string{nodes[3].ID}) {
		t.Fatalf("false allow-set prefixless policy = %v, %v", nodeIDs(got), err)
	}
}

func TestNameCandidatesSnapshotAndBaseMutationFreshness(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	old := &graph.Node{ID: "repo/a.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo", Meta: map[string]any{"payload": "old"}}
	s.AddNode(old)
	changed := false
	filter := graph.NameSearchFilter{RepoAllow: map[string]bool{"repo": true}, Accept: func(*graph.Node) bool {
		// Deliberate test-only interposition between key selection and payload
		// hydration; production predicates are pure and never perform writes.
		if !changed {
			changed = true
			s.AddNode(&graph.Node{ID: old.ID, Name: "renamed", Kind: old.Kind, FilePath: "other/a.go", RepoPrefix: "other", Meta: map[string]any{"payload": "new"}})
		}
		return true
	}}
	got, err := s.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 1, filter)
	if err != nil || len(got) != 1 || got[0].Name != "needle" || got[0].RepoPrefix != "repo" || got[0].Meta["payload"] != "old" {
		t.Fatalf("mixed read snapshot: %#v, %v", got, err)
	}
	got, err = s.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 1, graph.NameSearchFilter{})
	if err != nil || len(got) != 0 {
		t.Fatalf("renamed base hit stayed cached: %v, %v", nodeIDs(got), err)
	}
	got, err = s.FindNodesByNameContainingFilteredContext(context.Background(), "renamed", 1, graph.NameSearchFilter{})
	if err != nil || len(got) != 1 || got[0].Meta["payload"] != "new" {
		t.Fatalf("updated base payload: %#v, %v", got, err)
	}
	s.EvictFile("other/a.go")
	got, err = s.FindNodesByNameContainingFilteredContext(context.Background(), "renamed", 1, graph.NameSearchFilter{})
	if err != nil || len(got) != 0 {
		t.Fatalf("deleted base hit stayed cached: %v, %v", nodeIDs(got), err)
	}
}

func TestNameCandidatesLiteralUnicodeGenerationAndCancellation(t *testing.T) {
	_, h := seedNameIndexGeneration(t, true)
	for _, handle := range []*Store{h, h.AtGeneration(0)} {
		if handle.viewGen == 0 {
			for i, name := range nameIndexNames {
				handle.AddNode(&graph.Node{ID: string(rune('a' + i)), Name: name, Kind: graph.KindFunction})
			}
		}
		for _, query := range []string{"_", "%", `\`, "ÉCOLE", "école", "Ω", "ω", "CONFIG", "no-match"} {
			for _, limit := range []int{0, 1, 3} {
				got, err := handle.FindNodesByNameContainingFilteredContext(context.Background(), query, limit, graph.NameSearchFilter{Accept: func(*graph.Node) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				want := sqlFindContaining(t, handle, query, limit)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("generation %d literal %q/%d: %v != %v", handle.viewGen, query, limit, nodeIDs(got), nodeIDs(want))
				}
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := h.FindNodesByNameContainingFilteredContext(ctx, "config", 1, graph.NameSearchFilter{}); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("canceled lookup = %v, %v", got, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	_, err := h.FindNodesByNameContainingFilteredContext(ctx, "config", 1, graph.NameSearchFilter{Accept: func(*graph.Node) bool { cancel(); return true }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation = %v", err)
	}
}
