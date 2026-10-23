package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// mirroredFTSStore seeds a store with two repos' node rows written through
// the lean mirror path, plus one FTS doc filed WITHOUT a mirror row (the
// orphan shape the scope predicate must keep excluding).
func mirroredFTSStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "mirror.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	mirror, ok := graph.Store(s).(graph.FTSNodeMirror)
	if !ok {
		t.Fatalf("store must implement graph.FTSNodeMirror")
	}
	nodes := []*graph.Node{
		{ID: "app/a.go::Alpha", Kind: graph.KindFunction, Name: "AlphaWidget", FilePath: "a.go", RepoPrefix: "app"},
		{ID: "beta/b.go::BetaWidget", Kind: graph.KindType, Name: "BetaWidget", FilePath: "b.go", RepoPrefix: "beta"},
	}
	if err := mirror.MirrorFTSNodes(nodes); err != nil {
		t.Fatalf("MirrorFTSNodes: %v", err)
	}

	docs := []graph.SymbolFTSItem{
		{NodeID: "app/a.go::Alpha", Tokens: "alpha widget"},
		{NodeID: "beta/b.go::BetaWidget", Tokens: "beta widget"},
		{NodeID: "orphan::ghost::Ghost", Tokens: "ghost widget"},
	}
	if err := s.BatchUpsertSymbolFTS(docs); err != nil {
		t.Fatalf("BatchUpsertSymbolFTS: %v", err)
	}
	return s
}

// TestMirrorFTSNodes_FilesDocsByMirrorPrefix: the lean mirror alone must be
// sufficient for FTS filing — docs whose node row was mirrored get filed
// under the mirrored repo_prefix and pass the scoped narrow, exactly as if
// the full AddBatch path had written them.
func TestMirrorFTSNodes_FilesDocsByMirrorPrefix(t *testing.T) {
	s := mirroredFTSStore(t)

	appHits, err := s.SearchSymbolsRepoScoped("widget", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("scoped(app): %v", err)
	}
	if len(appHits) != 1 || appHits[0].NodeID != "app/a.go::Alpha" {
		t.Fatalf("scoped(app) = %+v, want exactly Alpha", appHits)
	}

	betaHits, err := s.SearchSymbolsRepoScoped("widget", []string{"beta"}, 20)
	if err != nil {
		t.Fatalf("scoped(beta): %v", err)
	}
	if len(betaHits) != 1 || betaHits[0].NodeID != "beta/b.go::BetaWidget" {
		t.Fatalf("scoped(beta) = %+v, want exactly BetaWidget", betaHits)
	}
}

// TestMirrorFTSNodes_Tier0ExactNameShortCircuit: the engine's exact-name
// tier-0 reads the mirrored rows — a symbol-kind exact name short-circuits
// with the dominant score, mirroring the full-node-write behaviour.
func TestMirrorFTSNodes_Tier0ExactNameShortCircuit(t *testing.T) {
	s := mirroredFTSStore(t)

	hits, err := s.SearchSymbolsRepoScoped("AlphaWidget", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != "app/a.go::Alpha" || hits[0].Score != 100.0 {
		t.Fatalf("tier-0 exact-name = %+v, want the 100.0 short-circuit on Alpha", hits)
	}
}

// TestMirrorFTSNodes_UnmirroredDocsStayOrphans: a doc upserted without a
// mirror row files under the empty prefix and must stay excluded from
// scoped searches (the #25 orphan semantics, unchanged by the lean path).
func TestMirrorFTSNodes_UnmirroredDocsStayOrphans(t *testing.T) {
	s := mirroredFTSStore(t)

	hits, err := s.SearchSymbolsRepoScoped("ghost", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("scoped search leaked the unmirrored doc: %+v", hits)
	}

	unscoped, err := s.SearchSymbols("ghost", 20)
	if err != nil {
		t.Fatalf("unscoped: %v", err)
	}
	found := false
	for _, h := range unscoped {
		if h.NodeID == "orphan::ghost::Ghost" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unscoped search lost the unmirrored doc: %+v", unscoped)
	}
}

// TestMirrorFTSNodes_UpsertByID: re-mirroring the same ID updates the row in
// place (last write wins), matching AddBatch's upsert semantics.
func TestMirrorFTSNodes_UpsertByID(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "mirror2.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mirror, ok := graph.Store(s).(graph.FTSNodeMirror)
	if !ok {
		t.Fatalf("store must implement graph.FTSNodeMirror")
	}

	first := &graph.Node{ID: "app/a.go::Alpha", Kind: graph.KindFunction, Name: "OldName", FilePath: "a.go", RepoPrefix: "app"}
	if err := mirror.MirrorFTSNodes([]*graph.Node{first}); err != nil {
		t.Fatalf("MirrorFTSNodes(1st): %v", err)
	}
	second := &graph.Node{ID: "app/a.go::Alpha", Kind: graph.KindType, Name: "AlphaWidget", FilePath: "a.go", RepoPrefix: "app"}
	if err := mirror.MirrorFTSNodes([]*graph.Node{second}); err != nil {
		t.Fatalf("MirrorFTSNodes(2nd): %v", err)
	}

	hits, err := s.SearchSymbolsRepoScoped("AlphaWidget", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != first.ID || hits[0].Score != 100.0 {
		t.Fatalf("re-mirror must satisfy the new exact name: %+v", hits)
	}
}

// TestMirrorFTSNodes_PreservesNonMirroredColumns (issue #36,
// R3-replace-delete-side-effects): the lean write must be update-only on
// conflict. A row already carrying a full AddBatch-written node keeps its
// non-mirrored columns (doc, signature, lines, …) after a mirror upsert;
// INSERT OR REPLACE — a delete+insert of the whole row — would have reset
// them to their DDL defaults.
func TestMirrorFTSNodes_PreservesNonMirroredColumns(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "mirror3.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	full := &graph.Node{
		ID: "app/a.go::Alpha", Kind: graph.KindFunction, Name: "AlphaWidget",
		QualName: "app.AlphaWidget", FilePath: "a.go", RepoPrefix: "app",
		StartLine: 5, EndLine: 42, Language: "go",
		Meta: map[string]any{
			"doc":       "the doc comment must survive the mirror",
			"signature": "func AlphaWidget() error",
		},
	}
	s.AddBatch([]*graph.Node{full}, nil)

	mirror, ok := graph.Store(s).(graph.FTSNodeMirror)
	if !ok {
		t.Fatalf("store must implement graph.FTSNodeMirror")
	}
	renamed := &graph.Node{ID: full.ID, Kind: graph.KindFunction, Name: "AlphaWidgetV2", FilePath: "a.go", RepoPrefix: "app"}
	if err := mirror.MirrorFTSNodes([]*graph.Node{renamed}); err != nil {
		t.Fatalf("MirrorFTSNodes: %v", err)
	}

	after := s.GetNodesByIDs([]string{full.ID})[full.ID]
	if after == nil {
		t.Fatalf("node row vanished after the mirror upsert")
	}
	if after.Name != "AlphaWidgetV2" {
		t.Errorf("mirrored name = %q, want AlphaWidgetV2", after.Name)
	}
	if after.Meta["doc"] != full.Meta["doc"] {
		t.Errorf("non-mirrored doc column reset: got %q, want %q", after.Meta["doc"], full.Meta["doc"])
	}
	if after.Meta["signature"] != full.Meta["signature"] {
		t.Errorf("non-mirrored signature column reset: got %q, want %q", after.Meta["signature"], full.Meta["signature"])
	}
	if after.QualName != full.QualName {
		t.Errorf("non-mirrored qual_name column reset: got %q, want %q", after.QualName, full.QualName)
	}
	if after.StartLine != 5 || after.EndLine != 42 {
		t.Errorf("non-mirrored line columns reset: got [%d,%d], want [5,42]", after.StartLine, after.EndLine)
	}
}

// TestMirrorChunkSizes (issue #36, R3-sql-var-ceiling-mirror-read): the
// derived statement shapes must respect the probed variable limit on both
// sides of the mirror — write rows × 6 binds and read rows × 2 binds + 1 —
// and never exceed the ftsMirrorChunkRows shape cap.
func TestMirrorChunkSizes(t *testing.T) {
	for _, limit := range []int{999, 1024, 32766, 8192} {
		writeRows, readRows := mirrorChunkSizes(limit)
		if writeRows*mirrorRowParams > limit-sqliteBatchVariableHeadroom {
			t.Errorf("limit %d: write shape %d rows × %d binds exceeds the budget", limit, writeRows, mirrorRowParams)
		}
		if readRows*2+1 > limit-sqliteBatchVariableHeadroom {
			t.Errorf("limit %d: read shape %d rows × 2 binds + view_gen exceeds the budget", limit, readRows)
		}
		if writeRows > ftsMirrorChunkRows || readRows > ftsMirrorChunkRows || writeRows < 1 || readRows < 1 {
			t.Errorf("limit %d: shapes out of bounds: write=%d read=%d", limit, writeRows, readRows)
		}
	}

	// The tuned shape is preserved on this engine's runtime limit: at 999
	// (the conservative fallback) both sides shrink below the 512 cap, and
	// at high limits they pin exactly at it.
	writeRows, readRows := mirrorChunkSizes(999)
	if writeRows > 163 || readRows > 491 { // (999-16)/6=163 ; (998-16)/2=491
		t.Errorf("limit 999: shapes must shrink under the 999 ceiling: write=%d read=%d", writeRows, readRows)
	}
	writeRows, readRows = mirrorChunkSizes(1 << 20)
	if writeRows != ftsMirrorChunkRows || readRows != ftsMirrorChunkRows {
		t.Errorf("huge limit must cap at ftsMirrorChunkRows: write=%d read=%d", writeRows, readRows)
	}
}
