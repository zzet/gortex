package store_sqlite

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestVisitNodesByNameContextStopsEarlyAndReusesStatement(t *testing.T) {
	store, _ := openTempStore(t)
	var nodes []*graph.Node
	for i := 0; i < 128; i++ {
		nodes = append(nodes, &graph.Node{ID: string(rune('a'+i%26)) + string(rune(i+1000)), Name: "Shared", FilePath: "shared.go", Kind: graph.KindFunction})
	}
	if err := store.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		visits := 0
		err := store.VisitNodesByNameContext(context.Background(), "Shared", func(*graph.Node) bool {
			visits++
			return false
		})
		if err != nil || visits != 1 {
			t.Fatalf("run %d = visits %d, err %v", run, visits, err)
		}
	}
	visits := 0
	if err := store.VisitNodesByNameContext(context.Background(), "Missing", func(*graph.Node) bool { visits++; return true }); err != nil || visits != 0 {
		t.Fatalf("missing = visits %d, err %v", visits, err)
	}
	visits = 0
	if err := store.VisitNodesByNameContext(context.Background(), "Shared", func(*graph.Node) bool { visits++; return true }); err != nil || visits != len(nodes) {
		t.Fatalf("all rows = visits %d/%d, err %v", visits, len(nodes), err)
	}
}

func TestVisitNodesByNameContextCancelsReaderWait(t *testing.T) {
	store, _ := openTempStore(t)
	if err := store.AddBatchChecked([]*graph.Node{{ID: "one", Name: "Shared", FilePath: "one.go", Kind: graph.KindFunction}}, nil); err != nil {
		t.Fatal(err)
	}
	store.db.SetMaxOpenConns(1)
	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	baseline := store.db.Stats().WaitCount
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- store.VisitNodesByNameContext(ctx, "Shared", func(*graph.Node) bool { return true }) }()
	deadline := time.Now().Add(time.Second)
	for store.db.Stats().WaitCount == baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if store.db.Stats().WaitCount == baseline {
		t.Fatal("visitor never waited for the reader pool")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("visitor error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("visitor remained blocked after cancellation")
	}
}
