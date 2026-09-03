package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func openReceiverRebindStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "receiver.sqlite"))
	if err != nil {
		t.Fatalf("open receiver store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close receiver store: %v", err)
		}
	})
	return s
}

func receiverEdge(t *testing.T, s *Store, from, to string) *graph.Edge {
	t.Helper()
	for _, edge := range s.GetOutEdges(from) {
		if edge.To == to && edge.Kind == graph.EdgeMemberOf {
			return edge
		}
	}
	return nil
}

func TestSQLiteRebindGoMethodReceiversCorrectnessAndIdempotence(t *testing.T) {
	s := openReceiverRebindStore(t)
	const (
		canonical = "repo::pkg/types.go::Server"
		method    = "repo::pkg/methods.go::Server.Run"
		phantom   = "repo::pkg/methods.go::Server"
	)
	s.AddBatch([]*graph.Node{
		{ID: canonical, Kind: graph.KindType, Name: "Server", FilePath: "repo::pkg/types.go", Language: "go"},
		{ID: method, Kind: graph.KindMethod, Name: "Run", FilePath: "repo::pkg/methods.go", Language: "go"},
		// Language gate: the same target shape on a non-Go method stays put.
		{ID: "repo::pkg/view.ts::Server.render", Kind: graph.KindMethod, Name: "render", FilePath: "repo::pkg/view.ts", Language: "typescript"},
		// Ambiguity gate: two Go types with the same package/name poison the
		// lookup, exactly as the in-memory resolver fallback does.
		{ID: "repo::amb/a.go::Dup", Kind: graph.KindType, Name: "Dup", FilePath: "repo::amb/a.go", Language: "go"},
		{ID: "repo::amb/b.go::Dup", Kind: graph.KindType, Name: "Dup", FilePath: "repo::amb/b.go", Language: "go"},
		{ID: "repo::amb/m.go::Dup.M", Kind: graph.KindMethod, Name: "M", FilePath: "repo::amb/m.go", Language: "go"},
	}, []*graph.Edge{
		{From: method, To: phantom, Kind: graph.EdgeMemberOf, FilePath: "repo::pkg/methods.go", Line: 10},
		{From: "repo::pkg/view.ts::Server.render", To: "repo::pkg/view.ts::Server", Kind: graph.EdgeMemberOf, FilePath: "repo::pkg/view.ts", Line: 2},
		{From: "repo::amb/m.go::Dup.M", To: "repo::amb/m.go::Dup", Kind: graph.EdgeMemberOf, FilePath: "repo::amb/m.go", Line: 3},
		// Kind gate: unrelated outgoing edges from an otherwise eligible Go
		// method must not enter the receiver candidate generation.
		{From: method, To: "unresolved::noise", Kind: graph.EdgeCalls, FilePath: "repo::pkg/methods.go", Line: 11},
	})

	// The topology rewrite invalidates persisted whole-graph analysis, but it
	// consumes no unresolved target and must leave mutation receipts complete.
	// Marking the receipt incomplete here would schedule an empty global
	// catch-up after indexing.
	generationID := buildMinimalAnalysisGeneration(t, s, "receiver", 0, true)
	beforeRevision := s.AnalysisMutationRevision()
	token := s.BeginMutationReceipt()
	changed, err := s.RebindGoMethodReceivers("")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	if receipt := s.EndMutationReceipt(token); !receipt.Complete || receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("receiver topology rewrite polluted resolution receipt: %+v", receipt)
	}
	if got := s.AnalysisMutationRevision(); got != beforeRevision+1 {
		t.Fatalf("analysis mutation revision = %d, want %d", got, beforeRevision+1)
	}
	var state int
	if err := s.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&state); err != nil {
		t.Fatalf("read analysis generation: %v", err)
	}
	if state != analysisGenerationStale || s.analysisGenerationPresent {
		t.Fatalf("analysis generation state=%d present=%v, want stale/false", state, s.analysisGenerationPresent)
	}

	if receiverEdge(t, s, method, canonical) == nil || receiverEdge(t, s, method, phantom) != nil {
		t.Fatalf("cross-file receiver was not rebound to %q", canonical)
	}
	if receiverEdge(t, s, "repo::pkg/view.ts::Server.render", "repo::pkg/view.ts::Server") == nil {
		t.Fatal("non-Go member edge was rewritten")
	}
	if receiverEdge(t, s, "repo::amb/m.go::Dup.M", "repo::amb/m.go::Dup") == nil {
		t.Fatal("ambiguous Go receiver was rewritten")
	}
	noisePreserved := false
	for _, edge := range s.GetOutEdges(method) {
		if edge.Kind == graph.EdgeCalls && edge.To == "unresolved::noise" {
			noisePreserved = true
			break
		}
	}
	if !noisePreserved {
		t.Fatal("kind-index receiver pass consumed an unrelated call edge")
	}

	// A second pass is a true no-op: no analysis bump and no receipt
	// invalidation. This is important for warm restarts.
	beforeRevision = s.AnalysisMutationRevision()
	token = s.BeginMutationReceipt()
	changed, err = s.RebindGoMethodReceivers("")
	if err != nil || changed != 0 {
		t.Fatalf("idempotent rebind changed=%d err=%v, want 0,nil", changed, err)
	}
	if receipt := s.EndMutationReceipt(token); !receipt.Complete {
		t.Fatalf("no-op receiver rebind invalidated receipt: %+v", receipt)
	}
	if got := s.AnalysisMutationRevision(); got != beforeRevision {
		t.Fatalf("no-op analysis revision = %d, want %d", got, beforeRevision)
	}
}

func TestSQLiteRebindGoMethodReceiversScopedToChangedFile(t *testing.T) {
	s := openReceiverRebindStore(t)
	s.AddBatch([]*graph.Node{
		{ID: "pkg/types.go::T", Kind: graph.KindType, Name: "T", FilePath: "pkg/types.go", Language: "go"},
		{ID: "pkg/a.go::T.A", Kind: graph.KindMethod, Name: "A", FilePath: "pkg/a.go", Language: "go"},
		{ID: "pkg/b.go::T.B", Kind: graph.KindMethod, Name: "B", FilePath: "pkg/b.go", Language: "go"},
	}, []*graph.Edge{
		{From: "pkg/a.go::T.A", To: "pkg/a.go::T", Kind: graph.EdgeMemberOf, FilePath: "pkg/a.go", Line: 1},
		{From: "pkg/b.go::T.B", To: "pkg/b.go::T", Kind: graph.EdgeMemberOf, FilePath: "pkg/b.go", Line: 1},
	})

	changed, err := s.RebindGoMethodReceivers("pkg/a.go")
	if err != nil || changed != 1 {
		t.Fatalf("scoped rebind changed=%d err=%v, want 1,nil", changed, err)
	}
	if receiverEdge(t, s, "pkg/a.go::T.A", "pkg/types.go::T") == nil {
		t.Fatal("changed file receiver was not rebound")
	}
	if receiverEdge(t, s, "pkg/b.go::T.B", "pkg/b.go::T") == nil {
		t.Fatal("unscoped file receiver was rewritten")
	}
}

func TestSQLiteRebindGoMethodReceiversIsolatedByRepo(t *testing.T) {
	s := openReceiverRebindStore(t)
	const (
		typeA   = "repo-a::types.go::T"
		typeB   = "repo-b::types.go::T"
		methodA = "repo-a::methods.go::T.M"
		phantom = "repo-a::methods.go::T"
	)
	s.AddBatch([]*graph.Node{
		{ID: typeA, Kind: graph.KindType, Name: "T", FilePath: "repo-a::types.go", Language: "go", RepoPrefix: "repo-a"},
		{ID: typeB, Kind: graph.KindType, Name: "T", FilePath: "repo-b::types.go", Language: "go", RepoPrefix: "repo-b"},
		{ID: methodA, Kind: graph.KindMethod, Name: "M", FilePath: "repo-a::methods.go", Language: "go", RepoPrefix: "repo-a"},
	}, []*graph.Edge{{From: methodA, To: phantom, Kind: graph.EdgeMemberOf, FilePath: "repo-a::methods.go", Line: 1}})

	changed, err := s.RebindGoMethodReceivers("")
	if err != nil || changed != 1 {
		t.Fatalf("cross-repo rebind changed=%d err=%v, want 1,nil", changed, err)
	}
	if receiverEdge(t, s, methodA, typeA) == nil {
		t.Fatalf("repo-a receiver did not bind to its canonical type %q", typeA)
	}
	if receiverEdge(t, s, methodA, typeB) != nil {
		t.Fatalf("repo-a receiver leaked across repositories to %q", typeB)
	}
}

func TestSQLiteRebindGoMethodReceiversProductionScopedPath(t *testing.T) {
	s := openReceiverRebindStore(t)
	const (
		canonical = "repo/pkg/types.go::T"
		method    = "repo/pkg/methods.go::T.M"
		phantom   = "repo/pkg/methods.go::T"
	)
	s.AddBatch([]*graph.Node{
		{ID: canonical, Kind: graph.KindType, Name: "T", FilePath: "repo/pkg/types.go", Language: "go", RepoPrefix: "repo"},
		{ID: method, Kind: graph.KindMethod, Name: "M", FilePath: "repo/pkg/methods.go", Language: "go", RepoPrefix: "repo"},
	}, []*graph.Edge{{From: method, To: phantom, Kind: graph.EdgeMemberOf, FilePath: "repo/pkg/methods.go", Line: 1}})

	changed, err := s.RebindGoMethodReceivers("repo/pkg/methods.go")
	if err != nil || changed != 1 {
		t.Fatalf("production-shape rebind changed=%d err=%v, want 1,nil", changed, err)
	}
	if receiverEdge(t, s, method, canonical) == nil {
		t.Fatalf("production-shape receiver did not bind to %q", canonical)
	}
}

func TestSQLiteRebindGoMethodReceiversMaxOpenConnsOne(t *testing.T) {
	s := openReceiverRebindStore(t)
	const (
		canonical = "repo::pkg/types.go::T"
		method    = "repo::pkg/methods.go::T.M"
	)
	s.AddBatch([]*graph.Node{
		{ID: canonical, Kind: graph.KindType, Name: "T", FilePath: "repo::pkg/types.go", Language: "go", RepoPrefix: "repo"},
		{ID: method, Kind: graph.KindMethod, Name: "M", FilePath: "repo::pkg/methods.go", Language: "go", RepoPrefix: "repo"},
	}, []*graph.Edge{{From: method, To: "repo::pkg/methods.go::T", Kind: graph.EdgeMemberOf, FilePath: "repo::pkg/methods.go", Line: 1}})

	generationID := buildMinimalAnalysisGeneration(t, s, "receiver-one-conn", 0, true)
	s.db.SetMaxOpenConns(1)
	// Hold the only reader slot. Receiver repair owns TEMP tables plus durable
	// DML and must therefore run entirely on the dedicated writer connection.
	heldReader, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer heldReader.Close() //nolint:errcheck // explicit close below; cleanup safety
	type result struct {
		changed int
		err     error
	}
	done := make(chan result, 1)
	go func() {
		changed, err := s.RebindGoMethodReceivers("")
		done <- result{changed: changed, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.changed != 1 {
			t.Fatalf("single-connection rebind changed=%d err=%v, want 1,nil", got.changed, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("single-connection receiver rebind deadlocked")
	}
	if err := heldReader.Close(); err != nil {
		t.Fatal(err)
	}
	if receiverEdge(t, s, method, canonical) == nil {
		t.Fatal("single-connection receiver rebind did not update topology")
	}
	var state int
	if err := s.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != analysisGenerationStale || s.analysisGenerationPresent {
		t.Fatalf("single-connection analysis state=%d present=%v, want stale/false", state, s.analysisGenerationPresent)
	}

	// An idempotent pass remains read-only and preserves a freshly activated
	// generation even with the same one-connection pool limit.
	generationID = buildMinimalAnalysisGeneration(t, s, "receiver-one-conn-idempotent", 0, true)
	beforeRevision := s.AnalysisMutationRevision()
	heldReader, err = s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan result, 1)
	go func() {
		changed, err := s.RebindGoMethodReceivers("")
		done <- result{changed: changed, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.changed != 0 {
			t.Fatalf("single-connection no-op changed=%d err=%v, want 0,nil", got.changed, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("single-connection no-op receiver rebind deadlocked")
	}
	if err := heldReader.Close(); err != nil {
		t.Fatal(err)
	}
	if !s.analysisGenerationPresent || s.AnalysisMutationRevision() != beforeRevision {
		t.Fatalf("single-connection no-op invalidated analysis: present=%v revision=%d want=%d", s.analysisGenerationPresent, s.AnalysisMutationRevision(), beforeRevision)
	}
	if err := s.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != analysisGenerationReady {
		t.Fatalf("single-connection no-op generation state=%d, want active", state)
	}
}

func TestSQLiteRebindGoMethodReceiversPreservesReindexDedupSemantics(t *testing.T) {
	s := openReceiverRebindStore(t)
	s.AddBatch([]*graph.Node{
		{ID: "pkg/type.go::T", Kind: graph.KindType, Name: "T", FilePath: "pkg/type.go", Language: "go"},
		{ID: "pkg/m.go::T.M", Kind: graph.KindMethod, Name: "M", FilePath: "pkg/m.go", Language: "go"},
		{ID: "pkg/n.go::T.N", Kind: graph.KindMethod, Name: "N", FilePath: "pkg/n.go", Language: "go"},
	}, []*graph.Edge{
		// Both rows collapse onto the same canonical logical key. The first
		// inserted (lowest id) payload must survive.
		{From: "pkg/m.go::T.M", To: "pkg/a.go::T", Kind: graph.EdgeMemberOf, FilePath: "pkg/m.go", Line: 7, Origin: "first", Confidence: 0.25},
		{From: "pkg/m.go::T.M", To: "pkg/b.go::T", Kind: graph.EdgeMemberOf, FilePath: "pkg/m.go", Line: 7, Origin: "second", Confidence: 0.75},
		// An existing canonical identity wins over the phantom row, matching
		// delete-old + INSERT OR IGNORE in ReindexEdges.
		{From: "pkg/n.go::T.N", To: "pkg/type.go::T", Kind: graph.EdgeMemberOf, FilePath: "pkg/n.go", Line: 9, Origin: "canonical", Confidence: 0.9},
		{From: "pkg/n.go::T.N", To: "pkg/n.go::T", Kind: graph.EdgeMemberOf, FilePath: "pkg/n.go", Line: 9, Origin: "phantom", Confidence: 0.1},
	})

	changed, err := s.RebindGoMethodReceivers("")
	if err != nil || changed != 3 {
		t.Fatalf("dedup rebind changed=%d err=%v, want 3,nil", changed, err)
	}
	m := s.GetOutEdges("pkg/m.go::T.M")
	if len(m) != 1 || m[0].To != "pkg/type.go::T" || m[0].Origin != "first" || m[0].Confidence != 0.25 {
		t.Fatalf("collapsed edge = %+v, want lowest-id payload on canonical target", m)
	}
	n := s.GetOutEdges("pkg/n.go::T.N")
	if len(n) != 1 || n[0].To != "pkg/type.go::T" || n[0].Origin != "canonical" || n[0].Confidence != 0.9 {
		t.Fatalf("existing canonical edge = %+v, want existing payload", n)
	}
}

func TestSQLiteReceiverRebindQueryPlansUseBoundedIndexes(t *testing.T) {
	s := openReceiverRebindStore(t)
	global := queryPlan(t, s, goMethodReceiverCandidatesGlobalSQL,
		baseViewGeneration, baseViewGeneration, baseViewGeneration, baseViewGeneration)
	for _, index := range []string{"edges_by_kind", "nodes_go_receiver_type"} {
		if !strings.Contains(global, index) {
			t.Fatalf("global receiver plan does not use %s:\n%s", index, global)
		}
	}
	if strings.Contains(strings.ToLower(global), "scan e") {
		t.Fatalf("global receiver plan scans the full edge table:\n%s", global)
	}
	if strings.Contains(global, "edges_go_member_receiver") {
		t.Fatalf("global receiver plan still depends on obsolete index:\n%s", global)
	}

	scoped := queryPlan(t, s, goMethodReceiverCandidatesForFileSQL,
		baseViewGeneration, baseViewGeneration, baseViewGeneration, "pkg/m.go", baseViewGeneration)
	for _, index := range []string{"nodes_by_file", "edges_by_from", "nodes_go_receiver_type"} {
		if !strings.Contains(scoped, index) {
			t.Fatalf("scoped receiver plan does not use %s:\n%s", index, scoped)
		}
	}
}

func TestSQLiteReceiverColumnsMigratePopulatedCurrentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-current.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	if _, err := raw.Exec(schemaSQL); err != nil {
		t.Fatalf("create pre-column schema: %v", err)
	}
	if _, err := raw.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion)); err != nil {
		t.Fatalf("stamp current schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO nodes (id, kind, name, file_path, language) VALUES
        ('pkg/type.go::T', 'type', 'T', 'pkg/type.go', 'go'),
        ('pkg/m.go::T.M', 'method', 'M', 'pkg/m.go', 'go')`); err != nil {
		t.Fatalf("seed legacy nodes: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO edges (from_id, to_id, kind, file_path, line)
        VALUES ('pkg/m.go::T.M', 'pkg/m.go::T', 'member_of', 'pkg/m.go', 1)`); err != nil {
		t.Fatalf("seed legacy edge: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open populated pre-column schema: %v", err)
	}
	for table, columns := range map[string][]string{
		"nodes": {"file_dir"},
		"edges": {"member_receiver", "member_receiver_dir"},
	} {
		for _, column := range columns {
			var count int
			q := fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_xinfo('%s') WHERE name = ?`, table)
			if err := s.db.QueryRow(q, column).Scan(&count); err != nil || count != 1 {
				t.Fatalf("%s.%s migration count=%d err=%v, want 1,nil", table, column, count, err)
			}
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'nodes_go_receiver_type'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("nodes_go_receiver_type count=%d err=%v, want 1,nil", count, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'edges_go_member_receiver'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("obsolete edges_go_member_receiver count=%d err=%v, want 0,nil", count, err)
	}
	if changed, err := s.RebindGoMethodReceivers(""); err != nil || changed != 1 {
		t.Fatalf("migrated store rebind changed=%d err=%v, want 1,nil", changed, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	// table_xinfo must prevent generated columns from being added twice.
	s, err = Open(path)
	if err != nil {
		t.Fatalf("second migrated open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second migrated close: %v", err)
	}
}
