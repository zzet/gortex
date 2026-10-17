package resolver

import (
	"fmt"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// A deadline stops the warm between reads of one file's references: with a
// clock that passes the deadline after the first read, only the first batch
// of distinct targets is read and the warm reports itself incomplete; with no
// deadline every batch is read.
func TestWarmPendingNamesStopsBetweenReadsAtItsDeadline(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo/b.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"})
	var pending []*graph.Edge
	for i := 0; i < 3*warmPendingNamesBatch; i++ {
		name := fmt.Sprintf("N%d", i)
		g.AddNode(&graph.Node{ID: "repo/a.go::" + name, Kind: graph.KindFunction, Name: name, FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"})
		pending = append(pending, &graph.Edge{From: "repo/b.go::F", To: graph.UnresolvedMarker + name, Kind: graph.EdgeCalls, FilePath: "repo/b.go"})
	}
	full := &scopeFinderCountingStore{Graph: g}
	complete, err := WarmPendingNamesUntil(full, pending, time.Time{})
	if err != nil || !complete || full.scoped == 0 {
		t.Fatalf("no deadline: complete=%t err=%v reads=%d", complete, err, full.scoped)
	}

	now := time.Unix(1000, 0)
	deadline := now.Add(time.Second)
	bounded := &scopeFinderCountingStore{Graph: g}
	clock := &steppingClock{at: now}
	warmPendingNamesNow = clock.now
	t.Cleanup(func() { warmPendingNamesNow = time.Now })
	clock.afterReads, clock.store, clock.passed = 1, bounded, deadline.Add(time.Second)
	complete, err = WarmPendingNamesUntil(bounded, pending, deadline)
	if err != nil || complete {
		t.Fatalf("a passed deadline still completed: complete=%t err=%v", complete, err)
	}
	if bounded.scoped == 0 || bounded.scoped >= full.scoped {
		t.Fatalf("bounded warm read %d times, unbounded %d; want at least one read and fewer", bounded.scoped, full.scoped)
	}
}

// steppingClock reads as at until store has made afterReads scoped reads,
// then as passed.
type steppingClock struct {
	at, passed time.Time
	afterReads int
	store      *scopeFinderCountingStore
}

func (c *steppingClock) now() time.Time {
	if c.store != nil && c.store.scoped >= c.afterReads {
		return c.passed
	}
	return c.at
}
