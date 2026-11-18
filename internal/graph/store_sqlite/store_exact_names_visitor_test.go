package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestVisitNodesByNamesContextParityGenerationAndStop(t *testing.T) {
	s, _ := openTempStore(t)
	for _, gen := range []int64{0, 41} {
		if err := s.AtGeneration(gen).AddBatchChecked([]*graph.Node{
			{ID: "a", Name: "Shared", Kind: graph.KindFunction, FilePath: "a.go"},
			{ID: "b", Name: "Shared", Kind: graph.KindFunction, FilePath: "b.go"},
			{ID: "c", Name: "Other", Kind: graph.KindFunction, FilePath: "c.go"},
			{ID: "empty", Name: "", Kind: graph.KindFile, FilePath: "empty.go"},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	h := s.AtGeneration(41)
	names := []string{"Shared", "Other", "Missing", ""}
	var want, got []string
	for _, name := range names {
		for _, n := range h.FindNodesByName(name) {
			want = append(want, n.ID)
		}
	}
	err := h.VisitNodesByNamesContext(context.Background(), append(names, "Shared"), func(n *graph.Node) bool { got = append(got, n.ID); return true })
	sort.Strings(want)
	sort.Strings(got)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("batch=%v want=%v err=%v", got, want, err)
	}
	for run := 0; run < 2; run++ {
		seen := 0
		if err := h.VisitNodesByNamesContext(context.Background(), names, func(*graph.Node) bool { seen++; return false }); err != nil || seen != 1 {
			t.Fatalf("early stop seen=%d err=%v", seen, err)
		}
	}
}

func TestVisitNodesByNamesContextCancelsReaderWait(t *testing.T) {
	s, _ := openTempStore(t)
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	baseline := s.db.Stats().WaitCount
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.VisitNodesByNamesContext(ctx, []string{"One", "Two"}, func(*graph.Node) bool { return true })
	}()
	deadline := time.Now().Add(time.Second)
	for s.db.Stats().WaitCount == baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if s.db.Stats().WaitCount == baseline {
		t.Fatal("batch did not wait for reader pool")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("batch cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("batch remained blocked")
	}
}

func TestVisitNodesByNamesContextScanAndQueryErrors(t *testing.T) {
	s, _ := openTempStore(t)
	if err := s.AddBatchChecked([]*graph.Node{{ID: "broken", Name: "One", Kind: graph.KindFunction}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writerDB.Exec(`UPDATE nodes SET start_line = 'not-an-integer' WHERE id = 'broken'`); err != nil {
		t.Fatal(err)
	}
	seen := 0
	if err := s.VisitNodesByNamesContext(context.Background(), []string{"One", "Two"}, func(*graph.Node) bool { seen++; return true }); err == nil || seen != 0 {
		t.Fatalf("scan error = seen %d err %v", seen, err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitNodesByNamesContext(context.Background(), []string{"One", "Two"}, func(*graph.Node) bool { return true }); err == nil {
		t.Fatal("closed reader error lost")
	}
}

func TestVisitNodesByNamesContextMalformedUTF8PreservesExactSpelling(t *testing.T) {
	s, _ := openTempStore(t)
	if err := s.AddBatchChecked([]*graph.Node{
		{ID: "replacement-neighbor", Name: "bad\ufffd", Kind: graph.KindFunction},
		{ID: "other", Name: "Other", Kind: graph.KindFunction},
		{ID: "third", Name: "Third", Kind: graph.KindFunction},
	}, nil); err != nil {
		t.Fatal(err)
	}
	names := []string{"bad\xff", "Other", "Third"}
	var want, got []string
	for _, name := range names {
		if err := s.VisitNodesByNameContext(context.Background(), name, func(n *graph.Node) bool { want = append(want, n.ID); return true }); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.VisitNodesByNamesContext(context.Background(), append(names, "Other"), func(n *graph.Node) bool { got = append(got, n.ID); return true }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(got, []string{"other", "third"}) {
		t.Fatalf("malformed spelling batch=%v reference=%v", got, want)
	}
	seen := 0
	if err := s.VisitNodesByNamesContext(context.Background(), names, func(*graph.Node) bool { seen++; return false }); err != nil || seen != 1 {
		t.Fatalf("raw-name stop seen=%d err=%v", seen, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen = 0
	if err := s.VisitNodesByNamesContext(ctx, names, func(*graph.Node) bool { seen++; cancel(); return true }); !errors.Is(err, context.Canceled) || seen != 1 {
		t.Fatalf("raw-name cancellation seen=%d err=%v", seen, err)
	}
}

type exactNamesStoreLayer struct {
	*graph.OverlayLayer
	handle *Store
}

func (l *exactNamesStoreLayer) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	return l.handle.VisitNodesByNamesContext(ctx, names, yield)
}
func (l *exactNamesStoreLayer) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	return l.handle.VisitNodesByNameContext(ctx, name, yield)
}

func TestVisitNodesByNamesContext7168NamesNineLayersQueryCount(t *testing.T) {
	s, _ := openTempStore(t)
	// Keep the existing SQL observer isolated from background WAL-pressure
	// reads so the counter measures only the exact-name workload.
	s.stopCheckpointLoop()
	names := make([]string, 7168)
	for i := range names {
		names[i] = fmt.Sprintf("introduced%d", i)
	}
	// Nine SQLite generations composed as layers, with all names absent: the
	// exact workload that previously performed one miss per name per layer.
	var view graph.Reader = s
	for gen := int64(1); gen < 9; gen++ {
		view = graph.NewOverlaidViewWithLayer(view, &exactNamesStoreLayer{OverlayLayer: graph.NewOverlayLayer(), handle: s.AtGeneration(gen)})
	}
	queries := 0
	readStatementObserver = func(q string) {
		if strings.Contains(q, "SELECT") {
			queries++
		}
	}
	t.Cleanup(func() { readStatementObserver = nil })
	start := time.Now()
	if err := graph.VisitNodesByNamesContext(context.Background(), view, names, func(*graph.Node) bool { t.Fatal("unexpected match"); return true }); err != nil {
		t.Fatal(err)
	}
	batchElapsed, batchQueries := time.Since(start), queries
	if batchQueries != 18 {
		t.Fatalf("batch queries=%d want18", batchQueries)
	}
	queries = 0
	start = time.Now()
	for _, name := range names {
		if err := graph.VisitNodesByNameContext(context.Background(), view, name, func(*graph.Node) bool { t.Fatal("unexpected match"); return true }); err != nil {
			t.Fatal(err)
		}
	}
	if queries != 64512 {
		t.Fatalf("reference queries=%d want64512", queries)
	}
	t.Logf("7168 names x9 layers: batch %d statements in %s; single-name reference %d in %s", batchQueries, batchElapsed, queries, time.Since(start))
	plan := planOf(t, s, exactNamesSeekSQL, `["One","Two"]`, int64(0))
	if !strings.Contains(plan, "nodes_by_name (name=? AND view_gen=?)") {
		t.Fatalf("batch plan = %s", plan)
	}
}
