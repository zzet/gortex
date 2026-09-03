package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The generation-scoped unresolved enumeration.
//
// Every unresolved-work enumeration is generation-FILTERED — it returns only
// the handle's own rows — but that is not the same as being generation-BOUND.
// The indexes those scans drive off (edges_by_unresolved, or the rowid order
// itself) key on nothing the generation appears in, so a sparse generation's
// pass reached its own handful of rows by fetching and discarding every
// unresolved row the base corpus holds. The cases below pin the bound rather
// than the filter: what the scan is allowed to look at, not what it returns.
//
// The base corpus keeps the scan it has always had, and the last case is the
// pin for that: bounding a derived generation must not narrow generation 0.

const scopeSeedRepo = "repo"

// seedUnresolvedEdges writes n edges parked on distinct unresolved targets.
func seedUnresolvedEdges(t *testing.T, s *Store, n int, tag string) {
	t.Helper()
	const chunk = 2000
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		edges := make([]*graph.Edge, 0, end-start)
		for i := start; i < end; i++ {
			edges = append(edges, &graph.Edge{
				From:     fmt.Sprintf("%s/%s/f%06d.go::Fn", scopeSeedRepo, tag, i),
				To:       fmt.Sprintf("unresolved::Missing_%s_%06d", tag, i),
				Kind:     graph.EdgeCalls,
				FilePath: fmt.Sprintf("%s/%s/f%06d.go", scopeSeedRepo, tag, i),
				Line:     i%128 + 1,
			})
		}
		s.AddBatch(nil, edges)
	}
}

// maxUnresolvedRowID is the highest rowid any unresolved edge in the store
// occupies at the given generation.
func maxUnresolvedRowID(t *testing.T, s *Store, generation int64) int64 {
	t.Helper()
	var id int64
	err := s.db.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM edges WHERE view_gen = ? AND is_unresolved = 1`, generation,
	).Scan(&id)
	if err != nil {
		t.Fatalf("read max unresolved rowid at generation %d: %v", generation, err)
	}
	return id
}

func openScopeStore(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name+".sqlite"))
	if err != nil {
		t.Fatalf("open %s store: %v", name, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

const (
	scopeBaseUnresolved      = 4000
	scopeGeneration          = int64(7)
	scopeGenerationUnresolve = 25
)

// seedScopedGenerationStore lays a small derived generation on top of a base
// corpus whose unresolved backlog dwarfs it — the production shape, where a
// sparse generation carries one change's closure and the corpus beneath it
// carries every repository the workspace tracks.
func seedScopedGenerationStore(t *testing.T, name string) (*Store, *Store) {
	t.Helper()
	store := openScopeStore(t, name)
	seedUnresolvedEdges(t, store, scopeBaseUnresolved, "base")
	derived := store.AtGeneration(scopeGeneration)
	seedUnresolvedEdges(t, derived, scopeGenerationUnresolve, "gen")
	return store, derived
}

// TestGenerationUnresolvedScanIsBoundedToItsOwnRows is the work bound: the
// window a derived generation's pass declares must not reach a single row of
// the corpus beneath it, and the statement that walks it must seek into the
// generation rather than sweep the whole unresolved frontier.
func TestGenerationUnresolvedScanIsBoundedToItsOwnRows(t *testing.T) {
	store, derived := seedScopedGenerationStore(t, "generation-scan")

	scan, err := derived.BeginUnresolvedEdgeScan(t.Context())
	if err != nil {
		t.Fatalf("BeginUnresolvedEdgeScan: %v", err)
	}
	baseTop := maxUnresolvedRowID(t, store, baseViewGeneration)
	if baseTop == 0 {
		t.Fatal("the fixture's base corpus carries no unresolved edges")
	}
	if scan.LowWaterID <= baseTop {
		t.Fatalf("the generation's scan starts at rowid %d, at or below the base corpus's last "+
			"unresolved row %d — the pass walks the corpus's backlog to reach its own %d rows",
			scan.LowWaterID, baseTop, scopeGenerationUnresolve)
	}
	if scan.HighWaterID < scan.LowWaterID {
		t.Fatalf("scan window [%d, %d] is empty", scan.LowWaterID, scan.HighWaterID)
	}

	from, generation := derived.unresolvedScanSource(unresolvedHighWaterBaseSource)
	plan := strings.ToLower(queryPlan(t, derived,
		unresolvedEdgePageSQL(from, generation, ""), scopeGeneration, 0, scan.HighWaterID, 8))
	if !strings.Contains(plan, edgesByGenerationIndexName) || !strings.Contains(plan, "view_gen=?") {
		t.Fatalf("the generation's page statement does not seek by generation:\n%s", plan)
	}

	page, err := derived.ReadUnresolvedEdgePage(t.Context(), scan, 0, 16<<10, 16<<20)
	if err != nil {
		t.Fatalf("ReadUnresolvedEdgePage: %v", err)
	}
	if len(page.Edges) != scopeGenerationUnresolve || !page.Exhausted {
		t.Fatalf("generation page = %d edges (exhausted=%v), want %d and exhausted",
			len(page.Edges), page.Exhausted, scopeGenerationUnresolve)
	}
	for _, edge := range page.Edges {
		if !strings.Contains(edge.FilePath, "/gen/") {
			t.Fatalf("the generation's page returned a corpus edge: %s", edge.FilePath)
		}
	}
}

// TestGenerationUnresolvedIdentityScanIsBoundedToItsOwnRows is the same bound
// for the post-resolve attribution pass's enumeration, which walked the edges
// table in raw rowid order from the very first row.
func TestGenerationUnresolvedIdentityScanIsBoundedToItsOwnRows(t *testing.T) {
	_, derived := seedScopedGenerationStore(t, "generation-identity")

	kinds := []graph.EdgeKind{graph.EdgeCalls}
	from, generation := derived.unresolvedScanSource(unresolvedIdentityBaseSource)
	plan := strings.ToLower(queryPlan(t, derived,
		unresolvedIdentityPageSQL(from, generation, 1),
		scopeGeneration, 0, int64(1<<40), string(graph.EdgeCalls), 8))
	if !strings.Contains(plan, edgesByGenerationIndexName) || !strings.Contains(plan, "view_gen=?") {
		t.Fatalf("the generation's identity statement does not seek by generation:\n%s", plan)
	}

	seen := 0
	derived.ScanUnresolvedEdgeIdentitiesBatched(kinds, 8, func(batch []graph.EdgeIdentity) bool {
		for _, identity := range batch {
			if !strings.Contains(identity.FilePath, "/gen/") {
				t.Fatalf("the generation's identity scan returned a corpus edge: %s", identity.FilePath)
			}
			seen++
		}
		return true
	})
	if seen != scopeGenerationUnresolve {
		t.Fatalf("identity scan yielded %d identities, want %d", seen, scopeGenerationUnresolve)
	}
}

// TestGenerationUnresolvedFrontierIsBoundedToItsOwnRows covers the frontier
// census the resolver logs at both ends of every pass.
func TestGenerationUnresolvedFrontierIsBoundedToItsOwnRows(t *testing.T) {
	_, derived := seedScopedGenerationStore(t, "generation-frontier")

	from, generation := derived.unresolvedScanSource(unresolvedFrontierBaseSource)
	plan := strings.ToLower(queryPlan(t, derived, unresolvedFrontierSQL(from, generation), scopeGeneration))
	if !strings.Contains(plan, edgesByGenerationIndexName) || !strings.Contains(plan, "view_gen=?") {
		t.Fatalf("the generation's frontier statement does not seek by generation:\n%s", plan)
	}

	stats, err := derived.CountUnresolvedFrontier()
	if err != nil {
		t.Fatalf("CountUnresolvedFrontier: %v", err)
	}
	if stats.Pending != int64(scopeGenerationUnresolve) {
		t.Fatalf("generation frontier pending = %d, want %d", stats.Pending, scopeGenerationUnresolve)
	}
}

// TestBaseUnresolvedScanKeepsItsWholeScope is the inverse-regression pin.
// Bounding a derived generation must not narrow the base corpus's own pass: it
// still starts below its first row, still enumerates every one of them, and
// still drives off the frontier index it was tuned for.
func TestBaseUnresolvedScanKeepsItsWholeScope(t *testing.T) {
	store, _ := seedScopedGenerationStore(t, "base-scope")

	scan, err := store.BeginUnresolvedEdgeScan(t.Context())
	if err != nil {
		t.Fatalf("BeginUnresolvedEdgeScan: %v", err)
	}
	if scan.LowWaterID != 0 {
		t.Fatalf("the base corpus's scan starts at rowid %d, want 0 — it owns its whole frontier",
			scan.LowWaterID)
	}
	if want := maxUnresolvedRowID(t, store, baseViewGeneration); scan.HighWaterID != want {
		t.Fatalf("base high water = %d, want %d", scan.HighWaterID, want)
	}

	from, generation := store.unresolvedScanSource(unresolvedHighWaterBaseSource)
	plan := strings.ToLower(queryPlan(t, store,
		unresolvedEdgePageSQL(from, generation, ""), baseViewGeneration, 0, scan.HighWaterID, 8))
	if strings.Contains(plan, edgesByGenerationIndexName) {
		t.Fatalf("the base corpus's page statement was retargeted at the generation index:\n%s", plan)
	}

	total := 0
	after := int64(0)
	for {
		page, err := store.ReadUnresolvedEdgePage(t.Context(), scan, after, 512, 16<<20)
		if err != nil {
			t.Fatalf("ReadUnresolvedEdgePage: %v", err)
		}
		for _, edge := range page.Edges {
			if !strings.Contains(edge.FilePath, "/base/") {
				t.Fatalf("the base pass returned a generation edge: %s", edge.FilePath)
			}
		}
		total += len(page.Edges)
		after = page.NextID
		if page.Exhausted {
			break
		}
	}
	if total != scopeBaseUnresolved {
		t.Fatalf("base pass enumerated %d edges, want its whole backlog of %d", total, scopeBaseUnresolved)
	}

	seen := 0
	store.ScanUnresolvedEdgeIdentitiesBatched([]graph.EdgeKind{graph.EdgeCalls}, 512,
		func(batch []graph.EdgeIdentity) bool {
			seen += len(batch)
			return true
		})
	if seen != scopeBaseUnresolved {
		t.Fatalf("base identity scan yielded %d identities, want %d", seen, scopeBaseUnresolved)
	}

	stats, err := store.CountUnresolvedFrontier()
	if err != nil {
		t.Fatalf("CountUnresolvedFrontier: %v", err)
	}
	if stats.Pending != int64(scopeBaseUnresolved) {
		t.Fatalf("base frontier pending = %d, want %d", stats.Pending, scopeBaseUnresolved)
	}
}
