package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestGetNodesByQualNamesUsesPartialIndexAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qual-name.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.AddBatch([]*graph.Node{
		{ID: "node::zeta", Kind: graph.KindFunction, Name: "Zeta", QualName: "pkg.Zeta", FilePath: "z.go"},
		{ID: "node::alpha", Kind: graph.KindFunction, Name: "Alpha", QualName: "pkg.Alpha", FilePath: "a.go"},
		{ID: "node::middle", Kind: graph.KindFunction, Name: "Middle", QualName: "pkg.Middle", FilePath: "m.go"},
	}, nil)

	assertQualNameLookupPlan(t, store)
	assertQualNameLookupParity(t, store)

	ordered := store.queryNodesSQL(
		nodesByQualNameLookupSQL,
		qualNameLookupPayload([]string{"pkg.Zeta", "pkg.Alpha", "pkg.Middle"}), store.viewGen,
	)
	if len(ordered) != 3 {
		t.Fatalf("ordered lookup returned %d nodes, want 3", len(ordered))
	}
	for i, want := range []string{"pkg.Alpha", "pkg.Middle", "pkg.Zeta"} {
		if ordered[i].QualName != want {
			t.Fatalf("ordered lookup[%d] qual_name = %q, want %q", i, ordered[i].QualName, want)
		}
	}

	// Qualified names are lookup labels, not identities. Preserve both rows and
	// keep the legacy singleton API deterministic by returning the smallest ID.
	if _, err := store.writerDB.Exec(
		`INSERT INTO nodes(id, kind, name, qual_name, file_path) VALUES (?, ?, ?, ?, ?)`,
		"node::duplicate", graph.KindFunction, "Duplicate", "pkg.Alpha", "duplicate.go",
	); err != nil {
		t.Fatalf("insert duplicate non-empty qual_name: %v", err)
	}
	if got := store.GetNodeByQualName("pkg.Alpha"); got == nil || got.ID != "node::alpha" {
		t.Fatalf("GetNodeByQualName(pkg.Alpha) = %#v, want deterministic smallest ID", got)
	}
	assertQualNameCandidateIDs(t, store, "pkg.Alpha", "node::alpha", "node::duplicate")

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertQualNameLookupPlan(t, store)
	assertQualNameLookupParity(t, store)
	assertQualNameCandidateIDs(t, store, "pkg.Alpha", "node::alpha", "node::duplicate")
}

func TestGetNodesByQualNamesSingleJSONBindHandles40001Names(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "large-qual-name.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.AddBatch([]*graph.Node{
		{ID: "node::first", Kind: graph.KindFunction, Name: "First", QualName: "hit.first", FilePath: "first.go"},
		{ID: "node::middle", Kind: graph.KindFunction, Name: "Middle", QualName: "hit.middle", FilePath: "middle.go"},
		{ID: "node::last", Kind: graph.KindFunction, Name: "Last", QualName: "hit.last", FilePath: "last.go"},
	}, nil)

	const count = 40001
	qualNames := make([]string, count)
	for i := range qualNames {
		qualNames[i] = fmt.Sprintf("missing.%05d", i)
	}
	qualNames[0] = "hit.first"
	qualNames[count/2] = "hit.middle"
	qualNames[count-1] = "hit.last"

	// One bind for the whole name page plus one for the generation: the page
	// size must never reach the SQL text, whatever its length.
	if binds := strings.Count(nodesByQualNameLookupSQL, "?"); binds != 2 {
		t.Fatalf("qualified-name lookup bind count = %d, want the JSON page plus the generation", binds)
	}
	got := store.GetNodesByQualNames(qualNames)
	if len(got) != 3 {
		t.Fatalf("large qualified-name lookup returned %d matches, want 3", len(got))
	}
	for qualName, wantID := range map[string]string{
		"hit.first":  "node::first",
		"hit.middle": "node::middle",
		"hit.last":   "node::last",
	} {
		nodes := got[qualName]
		if len(nodes) != 1 || nodes[0] == nil || nodes[0].ID != wantID {
			t.Fatalf("large lookup[%q] = %#v, want one node with id %q", qualName, nodes, wantID)
		}
	}
}

func TestGetNodesByQualNamesFailsClosedOnDecodeQueryAndClosedStoreErrors(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "qual-name-errors.sqlite"))
	if err != nil {
		t.Fatal(err)
	}

	store.AddBatch([]*graph.Node{
		{ID: "node::bad", Kind: graph.KindFunction, Name: "Bad", QualName: "bad.qual", FilePath: "bad.go"},
		{ID: "node::good", Kind: graph.KindFunction, Name: "Good", QualName: "good.qual", FilePath: "good.go"},
	}, nil)
	if _, err := store.writerDB.Exec(`UPDATE nodes SET start_line = 'not-an-integer' WHERE id = 'node::bad'`); err != nil {
		t.Fatal(err)
	}

	// A row that will not decode is a storage failure, not a row to step
	// over. Skipping it returns a map that is short by exactly the corrupt
	// entries, and a caller reading "no such qualified name" cannot tell that
	// from a genuine miss — the one outcome this lookup must never produce.
	assertQualNameLookupRaises(t, store, "bad.qual")

	// INDEXED BY is deliberate: losing the intended index must fail closed
	// instead of silently degrading into a full nodes-table scan. A missing
	// mandatory index is schema drift, so the lookup raises it rather than
	// returning an empty map a caller would read as "no such qualified name".
	if _, err := store.writerDB.Exec(`DROP INDEX nodes_by_qual`); err != nil {
		t.Fatal(err)
	}
	assertQualNameLookupRaises(t, store, "good.qual")

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := store.GetNodesByQualNames([]string{"good.qual"}); len(got) != 0 {
		t.Fatalf("lookup on closed store returned %#v, want empty", got)
	}
}

// assertQualNameLookupRaises fails unless the lookup surfaces its storage
// error instead of returning a result the caller cannot distinguish from a
// legitimate miss.
func assertQualNameLookupRaises(t *testing.T, store *Store, qualName string) {
	t.Helper()
	var raised any
	func() {
		defer func() { raised = recover() }()
		got := store.GetNodesByQualNames([]string{qualName})
		t.Fatalf("degraded lookup returned %#v, want a surfaced error", got)
	}()
	if raised == nil {
		t.Fatal("degraded lookup did not surface an error")
	}
}

func assertQualNameLookupPlan(t *testing.T, store *Store) {
	t.Helper()
	rows, err := store.writerDB.Query(
		`EXPLAIN QUERY PLAN `+nodesByQualNameLookupSQL,
		qualNameLookupPayload([]string{"pkg.Alpha", "pkg.Zeta"}), store.viewGen,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, " | ")
	if !strings.Contains(plan, "SEARCH nodes USING INDEX nodes_by_qual") {
		t.Fatalf("qualified-name query did not seek through nodes_by_qual: %s", plan)
	}
	if strings.Contains(plan, "SCAN nodes") {
		t.Fatalf("qualified-name query regressed to a full nodes scan: %s", plan)
	}
}

func assertQualNameLookupParity(t *testing.T, store *Store) {
	t.Helper()
	input := []string{"pkg.Zeta", "", "missing.qual", "pkg.Alpha", "pkg.Zeta", "pkg.Middle"}
	got := store.GetNodesByQualNames(input)
	if len(got) != 3 {
		t.Fatalf("batched qualified-name lookup returned %d matches, want 3", len(got))
	}
	for _, qualName := range []string{"pkg.Alpha", "pkg.Middle", "pkg.Zeta"} {
		want := store.GetNodeByQualName(qualName)
		if want == nil {
			// Explicit return so the dereference below is provably
			// nil-safe rather than safe only by Fatalf convention.
			t.Fatalf("individual lookup unexpectedly missed %q", qualName)
			return
		}
		nodes := got[qualName]
		if len(nodes) == 0 || nodes[0] == nil || nodes[0].ID != want.ID {
			t.Fatalf("batch lookup[%q] = %#v, individual lookup = %#v", qualName, nodes, want)
		}
	}
	if _, exists := got["missing.qual"]; exists {
		t.Fatal("batched lookup invented a missing qualified name")
	}
	if _, exists := got[""]; exists {
		t.Fatal("batched lookup retained an empty qualified name")
	}
}

func assertQualNameCandidateIDs(t *testing.T, store *Store, qualName string, wantIDs ...string) {
	t.Helper()
	nodes := store.GetNodesByQualNames([]string{qualName, qualName})[qualName]
	if len(nodes) != len(wantIDs) {
		t.Fatalf("qualified-name candidates for %q = %#v, want IDs %v", qualName, nodes, wantIDs)
	}
	for i, wantID := range wantIDs {
		if nodes[i] == nil || nodes[i].ID != wantID {
			t.Fatalf("qualified-name candidate %d for %q = %#v, want ID %q", i, qualName, nodes[i], wantID)
		}
	}
}
