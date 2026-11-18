package tstypes

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// buildSQLiteFixture writes the fixture to disk, extracts each file with
// the real per-language extractors, and loads the nodes/edges into a
// fresh on-disk SQLite store at dbPath. Returns the store and the on-disk
// source root.
func buildSQLiteFixture(t testing.TB, dbPath string, files map[string]string) (*store_sqlite.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store_sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		lang, ok := reg.DetectLanguage(rel)
		if !ok {
			t.Fatalf("no language for %s", rel)
		}
		ext, ok := reg.GetByLanguage(lang)
		if !ok {
			t.Fatalf("no extractor for %s", lang)
		}
		res, err := ext.Extract(rel, []byte(content))
		if err != nil {
			t.Fatalf("extract %s: %v", rel, err)
		}
		if res.Tree != nil {
			res.Tree.Close()
		}
		s.AddBatch(res.Nodes, res.Edges)
	}
	return s, dir
}

// On a disk backend GetOutEdges returns a detached row copy; confirming an
// edge mutates Confidence / ConfidenceLabel / Meta on that copy, and
// SetEdgeProvenance only writes origin+tier. Those extra attributes must
// still survive a reload — the engine round-trips the full edge through
// the backend's edge-attribute write path.
func TestEnrich_SQLiteConfirmationPersistsFullProvenance(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.sqlite")

	files := map[string]string{
		"a/Svc.java": javaSvc,
		"b/App.java": `package b;

import a.Svc;

public class App {
    public void handle(Svc s) {
        s.run();
    }
}
`,
	}
	s, root := buildSQLiteFixture(t, dbPath, files)

	p := NewProvider(JavaSpec(), zap.NewNop())
	if _, err := p.EnrichRepo(s, "", root); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	callerID := "b/App.java::App.handle"
	targetID := "a/Svc.java::Svc.run"

	// Sanity: the edge is confirmed in the live store.
	if e := outEdgeTo(s, callerID, targetID); e == nil {
		t.Fatalf("call edge not present after enrich; edges: %v", s.GetOutEdges(callerID))
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen from disk: every confirmed attribute must have persisted, not
	// just origin/tier.
	s2, err := store_sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	e := outEdgeTo(s2, callerID, targetID)
	if e == nil {
		t.Fatalf("call edge lost across reopen; edges: %v", s2.GetOutEdges(callerID))
	}
	if e.Origin != graph.OriginASTResolved {
		t.Errorf("origin = %q after reload, want %q", e.Origin, graph.OriginASTResolved)
	}
	if e.Confidence < astConfidence {
		t.Errorf("confidence = %v after reload, want >= %v (lost on disk write-back)", e.Confidence, astConfidence)
	}
	if e.ConfidenceLabel == "" {
		t.Errorf("confidence_label empty after reload (lost on disk write-back)")
	}
	if e.Meta == nil || e.Meta["semantic_source"] != "java-types" {
		t.Errorf("semantic_source = %v after reload, want java-types (lost on disk write-back)", metaVal(e))
	}
}

func outEdgeTo(s *store_sqlite.Store, fromID, toID string) *graph.Edge {
	for _, e := range s.GetOutEdges(fromID) {
		if e.Kind == graph.EdgeCalls && e.To == toID {
			return e
		}
	}
	return nil
}

func metaVal(e *graph.Edge) any {
	if e == nil || e.Meta == nil {
		return nil
	}
	return e.Meta["semantic_source"]
}
