package store_sqlite

import (
	"context"
	"errors"
	"github.com/zzet/gortex/internal/graph"
	"reflect"
	"runtime"
	"sort"
	"testing"
	"time"
)

func declarationFact(n *graph.Node) graph.Node {
	return graph.Node{ID: n.ID, Name: n.Name, Kind: n.Kind, FilePath: n.FilePath, RepoPrefix: n.RepoPrefix}
}
func TestExactNameDeclarationsPreserveAllKindsAndGeneration(t *testing.T) {
	s, _ := openTempStore(t)
	raw := string([]byte{'a', 0xff})
	for _, gen := range []int64{0, 41} {
		if err := s.AtGeneration(gen).AddBatchChecked([]*graph.Node{
			{ID: "a", Name: "Shared", Kind: graph.KindConstant, FilePath: "repo/a.go", RepoPrefix: "repo", Meta: map[string]any{"doc": "unconsumed", "value": "/one"}},
			{ID: "b", Name: "Shared", Kind: graph.KindParam, FilePath: "other/b.go", RepoPrefix: "other"},
			{ID: "c", Name: "Other", Kind: graph.KindFunction, FilePath: "repo/c.go", RepoPrefix: "repo"},
			{ID: "raw", Name: raw, Kind: graph.KindConstant},
			{ID: "replacement", Name: "a�", Kind: graph.KindConstant},
			{ID: "empty", Name: "", Kind: graph.KindFile},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	h := s.AtGeneration(41)
	for _, names := range [][]string{{"Shared", "Other", "Missing", "", "Shared"}, {raw, "Shared", raw}, {"Shared"}} {
		var want, got []graph.Node
		if err := h.VisitNodesByNamesContext(t.Context(), names, func(n *graph.Node) bool { want = append(want, declarationFact(n)); return true }); err != nil {
			t.Fatal(err)
		}
		if err := h.VisitNodeDeclarationsByNamesContext(t.Context(), names, func(n *graph.Node) bool {
			if n.Meta != nil || n.StartLine != 0 {
				t.Fatal("declaration materialized unconsumed enrichment")
			}
			got = append(got, *n)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		sort.Slice(want, func(i, j int) bool { return want[i].ID < want[j].ID })
		sort.Slice(got, func(i, j int) bool { return got[i].ID < got[j].ID })
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("declarations=%v full=%v", got, want)
		}
	}
	var owned []string
	if err := h.VisitNodeDeclarationsByNamesInRepoContext(t.Context(), []string{"Shared", "Other"}, "repo", func(n *graph.Node) bool { owned = append(owned, n.ID); return true }); err != nil {
		t.Fatal(err)
	}
	sort.Strings(owned)
	if !reflect.DeepEqual(owned, []string{"a", "c"}) {
		t.Fatalf("repo declarations=%v", owned)
	}
	// A location column consumed only by full-node clients is deliberately bad.
	// The narrow contract remains valid; the generic full API must still fail.
	if _, err := s.writerDB.Exec(`UPDATE nodes SET start_line='not-an-integer' WHERE view_gen=41 AND id='a'`); err != nil {
		t.Fatal(err)
	}
	if err := h.VisitNodesByNamesContext(t.Context(), []string{"Shared", "Other"}, func(*graph.Node) bool { return true }); err == nil {
		t.Fatal("full reader lost authoritative decode error")
	}
	n := 0
	if err := h.VisitNodeDeclarationsByNamesContext(t.Context(), []string{"Shared", "Other"}, func(*graph.Node) bool { n++; return true }); err != nil || n != 3 {
		t.Fatalf("declaration facts=%d err=%v", n, err)
	}
}
func TestExactNameDeclarationsStopCancelAndErrors(t *testing.T) {
	s, _ := openTempStore(t)
	if err := s.AddBatchChecked([]*graph.Node{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}, nil); err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := s.VisitNodeDeclarationsByNamesContext(t.Context(), []string{"A", "B"}, func(*graph.Node) bool { n++; return false }); err != nil || n != 1 {
		t.Fatalf("stop=%d err=%v", n, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	if err := s.VisitNodeDeclarationsByNamesContext(ctx, []string{"A", "B"}, func(*graph.Node) bool { cancel(); return true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("callback cancel=%v", err)
	}
	n = 0
	if err := s.VisitNodeDeclarationsByNamesContext(ctx, []string{"A"}, func(*graph.Node) bool { n++; return true }); !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("precancel=%v rows=%d", err, n)
	}
	if _, err := s.visitNameDeclarationQuery(t.Context(), `SELECT NULL,'A','constant','',''`, nil, func(*graph.Node) bool { t.Fatal("invalid identity delivered"); return true }); err == nil {
		t.Fatal("required fact scan error lost")
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitNodeDeclarationsByNamesContext(t.Context(), []string{"A", "B"}, func(*graph.Node) bool { return true }); err == nil {
		t.Fatal("closed query error lost")
	}
}
func TestExactNameDeclarationsCancelReaderWait(t *testing.T) {
	s, _ := openTempStore(t)
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	baseline := s.db.Stats().WaitCount
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.VisitNodeDeclarationsByNamesContext(ctx, []string{"A", "B"}, func(*graph.Node) bool { return true })
	}()
	deadline := time.Now().Add(time.Second)
	for s.db.Stats().WaitCount == baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if s.db.Stats().WaitCount == baseline {
		t.Fatal("reader never waited")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled reader remained blocked")
	}
}
