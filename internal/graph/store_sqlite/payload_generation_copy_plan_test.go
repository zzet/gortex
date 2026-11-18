package store_sqlite

import (
	"strings"
	"testing"
)

func TestGenerationCopyEdgesStartsFromSelectedRepository(t *testing.T) {
	s := openCatalogStore(t)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN SELECT e.from_id, e.to_id"+generationCopyEdgesFromSQL, 0, copyFixtureRepo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 || !strings.Contains(plan[0], "SEARCH n") || !strings.Contains(plan[0], "repo_prefix=?") || !strings.Contains(plan[1], "SEARCH e USING INDEX edges_by_from") || !strings.Contains(plan[1], "view_gen=? AND from_id=?") {
		t.Fatalf("copy must seek repository nodes then their generation-scoped adjacency, got %v", plan)
	}
}
func TestGenerationCopyEdgesKeepsOwnerAndGenerationAcrossPositiveSource(t *testing.T) {
	s := openCatalogStore(t)
	seedGenerationZero(t, s, copyFixtureRepo, 32, 64)
	seedGenerationZero(t, s, "foreign", 32, 64)
	source := reservedGeneration(t, s, "positive-source")
	first, err := s.CopyPayloadGeneration(t.Context(), 0, source, copyFixtureRepo)
	if err != nil {
		t.Fatal(err)
	}
	// Identical logical node IDs exist at zero and the positive source. Only
	// source-generation adjacency may be copied; foreign rows must stay absent.
	destination := reservedGeneration(t, s, "positive-destination")
	got, err := s.CopyPayloadGeneration(t.Context(), source, destination, copyFixtureRepo)
	if err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatalf("copy counts differ: source=%+v destination=%+v", first, got)
	}
	wantRows := generationRows(t, s, "edges", source, copyFixtureRepo)
	gotRows := generationRows(t, s, "edges", destination, copyFixtureRepo)
	if len(wantRows) == 0 || strings.Join(gotRows, "\n") != strings.Join(wantRows, "\n") {
		t.Fatalf("physical edge payload differs: got %v want %v", gotRows, wantRows)
	}
	if n := rawGenerationRowCount(t, s, "edges", destination); n != got.Edges {
		t.Fatalf("foreign or cross-generation edges leaked: physical=%d owned=%d", n, got.Edges)
	}
	integrityOK(t, s.db)
}
