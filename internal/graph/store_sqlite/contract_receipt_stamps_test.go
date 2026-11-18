package store_sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// TestContractReceiptStampsAndWriteSequence pins the stamp scan's store
// half: payload-free rows by range and by point, and a write sequence that
// lives on the shared core, so a write through one generation handle is
// visible through every other handle's sample and seqs.
func TestContractReceiptStampsAndWriteSequence(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	writer, reader := s.AtGeneration(0), s.AtGeneration(0)
	a, b := boundaryStorageReceipt("a.go", "a1"), boundaryStorageReceipt("b.go", "b1")
	if err := writer.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a, b}); err != nil {
		t.Fatal(err)
	}
	if begun, done := reader.ContractReceiptWriteSample("repo"); begun != 1 || done != 1 {
		t.Fatalf("one settled write: begun=%d done=%d", begun, done)
	}
	if seqs := reader.ContractReceiptWriteSeqs("repo", "", []string{a.FilePath, b.FilePath, "repo/none.go"}); seqs[a.FilePath] != 1 || seqs[b.FilePath] != 1 || seqs["repo/none.go"] != 0 {
		t.Fatalf("seqs=%v", seqs)
	}
	all, err := reader.ContractBoundaryReceiptStampsContext(ctx, "repo", "", nil)
	if err != nil || len(all) != 2 || all[a.FilePath].Fingerprint != "a1" || all[a.FilePath].Accepted {
		t.Fatalf("range stamps=%v err=%v", all, err)
	}
	if err := writer.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	point, err := reader.ContractBoundaryReceiptStampsContext(ctx, "repo", "", []string{a.FilePath, a.FilePath, "repo/none.go"})
	if err != nil || len(point) != 1 || !point[a.FilePath].Accepted {
		t.Fatalf("point stamps=%v err=%v", point, err)
	}
	// Accepting is a write too: a same-identity transition bumps the seq.
	if seqs := reader.ContractReceiptWriteSeqs("repo", "", []string{a.FilePath, b.FilePath}); seqs[a.FilePath] != 2 || seqs[b.FilePath] != 1 {
		t.Fatalf("seqs after accept=%v", seqs)
	}
	if begun, done := reader.ContractReceiptWriteSample("repo"); begun != 2 || done != 2 {
		t.Fatalf("bracket after accept: begun=%d done=%d", begun, done)
	}
	if begun, done := reader.ContractReceiptWriteSample("other"); begun != 0 || done != 0 {
		t.Fatalf("an untouched repository has an empty bracket: %d %d", begun, done)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reader.ContractBoundaryReceiptStampsContext(canceled, "repo", "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan=%v", err)
	}
}

// TestContractReceiptSettleAndEvents pins the settle half of the write
// sequence: a receipt is settled only at the write seq its mutation wrote, a
// later write of the path unsettles it, and every ended write and settle
// advances the event counter and closes the channel a waiter holds.
func TestContractReceiptSettleAndEvents(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	writer, reader := s.AtGeneration(0), s.AtGeneration(0)
	events, changed := reader.ContractReceiptEvents()
	a := boundaryStorageReceipt("a.go", "a1")
	if err := writer.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("an ended receipt write closes the waiter's channel")
	}
	afterWrite, _ := reader.ContractReceiptEvents()
	if afterWrite != events+1 {
		t.Fatalf("events after a write: %d, want %d", afterWrite, events+1)
	}
	seqs := writer.ContractReceiptWriteSeqs("repo", "", []string{a.FilePath})
	// A stale seq settles nothing and announces nothing.
	writer.SettleContractReceiptWrites("repo", "", map[string]uint64{a.FilePath: seqs[a.FilePath] + 1})
	if state := reader.ContractReceiptWriteStates("repo", "", []string{a.FilePath})[a.FilePath]; state.Settled {
		t.Fatalf("settled at a seq the path was never written at: %+v", state)
	}
	if now, _ := reader.ContractReceiptEvents(); now != afterWrite {
		t.Fatalf("a settle that matched nothing is not an event: %d", now)
	}
	_, changed = reader.ContractReceiptEvents()
	writer.SettleContractReceiptWrites("repo", "", seqs)
	state := reader.ContractReceiptWriteStates("repo", "", []string{a.FilePath, "repo/none.go"})
	if !state[a.FilePath].Settled || state[a.FilePath].Seq != seqs[a.FilePath] || state["repo/none.go"].Settled {
		t.Fatalf("states after settle: %+v", state)
	}
	select {
	case <-changed:
	default:
		t.Fatal("a settle closes the waiter's channel")
	}
	// The next write of the path is in flight again until it settles or is
	// accepted.
	if err := writer.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	if state := reader.ContractReceiptWriteStates("repo", "", []string{a.FilePath})[a.FilePath]; state.Settled || state.Seq != seqs[a.FilePath]+1 {
		t.Fatalf("a later write unsettles the path: %+v", state)
	}
}

// TestContractReceiptCoreWriteWindows pins the core-write window: a staging
// write of unaccepted rows opens it as it begins; it closes when its
// mutation's body ends at the seq it staged, when the staged row is accepted,
// or when its mutation settles it. Ending at a stale seq or a refused accept
// closes nothing, and an ending steps the repository's bracket and wakes
// waiters, as a receipt write does.
func TestContractReceiptCoreWriteWindows(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	writer, reader := s.AtGeneration(0), s.AtGeneration(0)
	state := func(path string) ContractReceiptWriteState {
		return reader.ContractReceiptWriteStates("repo", "", []string{path})[path]
	}
	a, b := boundaryStorageReceipt("a.go", "a1"), boundaryStorageReceipt("b.go", "b1")
	if st := state(a.FilePath); st.Writing || st.Seq != 0 {
		t.Fatalf("a path never written has no window: %+v", st)
	}
	if err := writer.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a, b}); err != nil {
		t.Fatal(err)
	}
	if !state(a.FilePath).Writing || !state(b.FilePath).Writing {
		t.Fatal("staging unaccepted rows opens their windows")
	}
	seqs := writer.ContractReceiptWriteSeqs("repo", "", []string{a.FilePath})
	events, changed := reader.ContractReceiptEvents()
	begun, done := reader.ContractReceiptWriteSample("repo")
	writer.EndContractCoreWrites("repo", "", map[string]uint64{a.FilePath: seqs[a.FilePath] + 1})
	if !state(a.FilePath).Writing {
		t.Fatal("an ending at a seq the path was not staged at closes nothing")
	}
	if now, _ := reader.ContractReceiptEvents(); now != events {
		t.Fatalf("an ending that closed nothing is not an event: %d", now)
	}
	writer.EndContractCoreWrites("repo", "", seqs)
	if st := state(a.FilePath); st.Writing || st.Settled || st.Seq != seqs[a.FilePath] {
		t.Fatalf("the body's ending closes the window and moves nothing else: %+v", st)
	}
	select {
	case <-changed:
	default:
		t.Fatal("an ending wakes the waiters")
	}
	if nb, nd := reader.ContractReceiptWriteSample("repo"); nb != begun+1 || nd != done+1 {
		t.Fatalf("an ending steps the bracket: %d/%d -> %d/%d", begun, done, nb, nd)
	}
	// A refused accept leaves the window open; the accept closes it.
	if err := writer.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	if !state(a.FilePath).Writing {
		t.Fatal("staging the path again opens a new window")
	}
	stale := boundaryStorageReceipt("a.go", "a2")
	if err := writer.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{stale}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("accept of a row other than the staged one: %v", err)
	}
	if !state(a.FilePath).Writing {
		t.Fatal("a refused accept closes nothing")
	}
	if err := writer.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	if state(a.FilePath).Writing {
		t.Fatal("the accept closes the window")
	}
	// A settle closes the window at the seq it settles, in one event, and
	// steps the bracket.
	if !state(b.FilePath).Writing {
		t.Fatal("b's window is still open")
	}
	events, _ = reader.ContractReceiptEvents()
	begun, done = reader.ContractReceiptWriteSample("repo")
	writer.SettleContractReceiptWrites("repo", "", writer.ContractReceiptWriteSeqs("repo", "", []string{b.FilePath}))
	if st := state(b.FilePath); st.Writing || !st.Settled {
		t.Fatalf("a settle closes the window: %+v", st)
	}
	if now, _ := reader.ContractReceiptEvents(); now != events+1 {
		t.Fatalf("a settle and its close are one event: %d -> %d", events, now)
	}
	if nb, nd := reader.ContractReceiptWriteSample("repo"); nb != begun+1 || nd != done+1 {
		t.Fatalf("a settle steps the bracket: %d/%d -> %d/%d", begun, done, nb, nd)
	}
	// The body's ending after the settle closes nothing and announces
	// nothing.
	writer.EndContractCoreWrites("repo", "", writer.ContractReceiptWriteSeqs("repo", "", []string{b.FilePath}))
	if now, _ := reader.ContractReceiptEvents(); now != events+1 {
		t.Fatalf("an ending after the settle is not an event: %d", now)
	}
}

// TestContractCoreWindowGapRing pins the quiet-gap ring: a window that opens
// after the path's previous window closed records the gap from that close,
// whether the body's end, the accept's commit (not its begin) or a settle
// closed it, with how long the previous window was open. A window opened
// again while it is open records nothing, the ring keeps the latest gaps
// only, and a query returns the gaps opened at or after its since.
func TestContractCoreWindowGapRing(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	now := base
	at := func(d time.Duration) {
		mu.Lock()
		now = base.Add(d)
		mu.Unlock()
	}
	s.contractReceiptWrites.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	a, b := boundaryStorageReceipt("a.go", "a1"), boundaryStorageReceipt("b.go", "b1")
	stage := func(row graph.ContractBoundaryReceipt) {
		t.Helper()
		if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row}); err != nil {
			t.Fatal(err)
		}
	}
	gaps := func(path string, since time.Time) []ContractCoreWindowGap {
		return s.ContractCoreWindowGaps("repo", []string{path}, since)
	}
	end := func(path string) {
		s.EndContractCoreWrites("repo", "", s.ContractReceiptWriteSeqs("repo", "", []string{path}))
	}

	at(0)
	stage(a)
	if got := gaps(a.FilePath, time.Time{}); len(got) != 0 {
		t.Fatalf("a first window has no previous close: %+v", got)
	}
	// The body ends 5ms after the open; the next apply opens 120ms later.
	at(5 * time.Millisecond)
	end(a.FilePath)
	at(125 * time.Millisecond)
	stage(a)
	got := gaps(a.FilePath, time.Time{})
	if len(got) != 1 || got[0].Quiet != 120*time.Millisecond || got[0].OpenPrev != 5*time.Millisecond ||
		got[0].Seq != s.ContractReceiptWriteSeqs("repo", "", []string{a.FilePath})[a.FilePath] || !got[0].OpenedAt.Equal(base.Add(125*time.Millisecond)) {
		t.Fatalf("the gap runs from the close to the next open: %+v", got)
	}
	// Opened again while open: no gap.
	at(130 * time.Millisecond)
	stage(a)
	if got := gaps(a.FilePath, time.Time{}); len(got) != 1 {
		t.Fatalf("a window opened again while open records nothing: %+v", got)
	}

	// The accept begins at 200ms and commits at 240ms; the next open is at
	// 310ms: the gap is the 70ms after the commit.
	accepting := false
	restore := SetContractReceiptWriteHookForTest(func(stage string, paths []string) {
		if !accepting {
			return
		}
		switch stage {
		case "begun":
			at(200 * time.Millisecond)
		case "ending":
			at(240 * time.Millisecond)
		}
	})
	accepting = true
	if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	accepting = false
	restore()
	if s.ContractReceiptWriteStates("repo", "", []string{a.FilePath})[a.FilePath].Writing {
		t.Fatal("the accept closed the window")
	}
	at(310 * time.Millisecond)
	stage(a)
	got = gaps(a.FilePath, base.Add(300*time.Millisecond))
	if len(got) != 1 || got[0].Quiet != 70*time.Millisecond || got[0].OpenPrev != 110*time.Millisecond {
		t.Fatalf("the gap runs from the accept's commit, the previous window from its open: %+v", got)
	}

	// A settle closes the window too.
	at(400 * time.Millisecond)
	stage(b)
	at(410 * time.Millisecond)
	s.SettleContractReceiptWrites("repo", "", s.ContractReceiptWriteSeqs("repo", "", []string{b.FilePath}))
	at(440 * time.Millisecond)
	stage(b)
	if got := gaps(b.FilePath, time.Time{}); len(got) != 1 || got[0].Quiet != 30*time.Millisecond || got[0].OpenPrev != 10*time.Millisecond {
		t.Fatalf("the gap after a settle: %+v", got)
	}

	// The ring keeps the latest gaps of a path.
	for i := range contractCoreWindowGapRing + 4 {
		at(time.Second + time.Duration(2*i)*time.Millisecond)
		end(b.FilePath)
		at(time.Second + time.Duration(2*i+1)*time.Millisecond)
		stage(b)
	}
	ring := gaps(b.FilePath, time.Time{})
	if len(ring) != contractCoreWindowGapRing || !ring[len(ring)-1].OpenedAt.Equal(base.Add(time.Second+time.Duration(2*(contractCoreWindowGapRing+3)+1)*time.Millisecond)) {
		t.Fatalf("the ring keeps the latest %d gaps: %d, last %+v", contractCoreWindowGapRing, len(ring), ring[len(ring)-1])
	}
	if both := s.ContractCoreWindowGaps("repo", []string{a.FilePath, b.FilePath}, time.Time{}); len(both) != 2+contractCoreWindowGapRing {
		t.Fatalf("a query over two paths: %d", len(both))
	}
	if other := s.ContractCoreWindowGaps("other", []string{a.FilePath}, time.Time{}); len(other) != 0 {
		t.Fatalf("another repository's paths: %+v", other)
	}
}

// TestContractOpenCoreWindowsListsEveryOpenWindow pins the open-window
// listing a repair re-checks under the lane: every open window of the
// repository with the seq that opened it and when, by path, whatever its seq;
// a closed window is gone, another repository's windows are not listed.
func TestContractOpenCoreWindowsListsEveryOpenWindow(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	s.contractReceiptWrites.now = func() time.Time { return base }
	a, b, c := boundaryStorageReceipt("a.go", "a1"), boundaryStorageReceipt("b.go", "b1"), boundaryStorageReceipt("c.go", "c1")
	other := boundaryStorageReceipt("o.go", "o1")
	other.RepoPrefix, other.FilePath = "other", "other/o.go"
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{c, a, b, other}); err != nil {
		t.Fatal(err)
	}
	// b is staged again: its window is the newer one.
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{b}); err != nil {
		t.Fatal(err)
	}
	s.EndContractCoreWrites("repo", "", s.ContractReceiptWriteSeqs("repo", "", []string{c.FilePath}))
	open := s.OpenContractCoreWindows("repo")
	if len(open) != 2 || open[0].Path != a.FilePath || open[1].Path != b.FilePath ||
		open[0].OpenerSeq != 1 || open[1].OpenerSeq != 2 || !open[0].OpenedAt.Equal(base) || !open[0].MarkedAt.IsZero() {
		t.Fatalf("open windows: %+v", open)
	}
	if others := s.OpenContractCoreWindows("other"); len(others) != 1 || others[0].Path != other.FilePath {
		t.Fatalf("another repository's windows: %+v", others)
	}
	if none := s.OpenContractCoreWindows("none"); len(none) != 0 {
		t.Fatalf("a repository without windows: %+v", none)
	}
}

// TestContractOrphanMarkClearsAtEveryClose pins the orphan registry: a failed
// mutation marks the windows it opened at the seqs that opened them, a mark
// at a seq the window was not opened at marks nothing, and only an open
// window carrying its own mark is orphaned. The mark is dropped at every close
// of the path: the body's end, the accept's commit and a settle. A refused
// accept closes nothing and keeps it; a window opened again over a marked one
// is a newer window and is not orphaned.
func TestContractOrphanMarkClearsAtEveryClose(t *testing.T) {
	ctx := context.Background()
	stage := func(t *testing.T, s *Store, rows ...graph.ContractBoundaryReceipt) {
		t.Helper()
		if err := s.SetContractBoundaryReceiptsContext(ctx, rows); err != nil {
			t.Fatal(err)
		}
	}
	a := boundaryStorageReceipt("a.go", "a1")
	mark := func(s *Store) map[string]uint64 {
		seqs := s.ContractReceiptWriteSeqs("repo", "", []string{a.FilePath})
		s.OrphanContractCoreWrites("repo", "", seqs)
		return seqs
	}
	orphaned := func(s *Store) []ContractCoreWindow { return s.OrphanedContractCoreWindows("repo") }
	marks := func(s *Store) int {
		s.contractReceiptWrites.mu.Lock()
		defer s.contractReceiptWrites.mu.Unlock()
		return len(s.contractReceiptWrites.orphaned)
	}
	for _, close := range []struct {
		name  string
		close func(*Store, map[string]uint64) error
	}{
		{"body_end", func(s *Store, seqs map[string]uint64) error { s.EndContractCoreWrites("repo", "", seqs); return nil }},
		{"accept", func(s *Store, _ map[string]uint64) error {
			return s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a})
		}},
		{"settle", func(s *Store, seqs map[string]uint64) error {
			s.SettleContractReceiptWrites("repo", "", seqs)
			return nil
		}},
	} {
		t.Run(close.name, func(t *testing.T) {
			s := openCatalogStore(t)
			stage(t, s, a)
			events, changed := s.ContractCoreOrphanEvents()
			s.OrphanContractCoreWrites("repo", "", map[string]uint64{a.FilePath: 99})
			if len(orphaned(s)) != 0 || marks(s) != 0 {
				t.Fatal("a mark at a seq the window was not opened at marks nothing")
			}
			seqs := mark(s)
			got := orphaned(s)
			if len(got) != 1 || got[0].Path != a.FilePath || got[0].OpenerSeq != seqs[a.FilePath] || got[0].MarkedAt.IsZero() {
				t.Fatalf("the failed mutation's window is orphaned: %+v", got)
			}
			if now, _ := s.ContractCoreOrphanEvents(); now != events+1 {
				t.Fatalf("a mark is an orphan event: %d -> %d", events, now)
			}
			select {
			case <-changed:
			default:
				t.Fatal("a mark wakes the orphan waiters")
			}
			if err := close.close(s, seqs); err != nil {
				t.Fatal(err)
			}
			if state := s.ContractReceiptWriteStates("repo", "", []string{a.FilePath})[a.FilePath]; state.Writing {
				t.Fatalf("the %s closed the window: %+v", close.name, state)
			}
			if len(orphaned(s)) != 0 || marks(s) != 0 {
				t.Fatalf("the %s dropped the mark: %d", close.name, marks(s))
			}
		})
	}
	t.Run("refused_accept", func(t *testing.T) {
		s := openCatalogStore(t)
		stage(t, s, a)
		mark(s)
		stale := a
		stale.Fingerprint = "other"
		if err := s.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{stale}); !errors.Is(err, ErrCatalogStaleGuard) {
			t.Fatalf("a stale accept: %v", err)
		}
		if len(orphaned(s)) != 1 {
			t.Fatal("a refused accept closes nothing and keeps the mark")
		}
	})
	t.Run("opened_again", func(t *testing.T) {
		s := openCatalogStore(t)
		stage(t, s, a)
		mark(s)
		stage(t, s, a)
		if got := orphaned(s); len(got) != 0 {
			t.Fatalf("a window opened again over a marked one is not orphaned: %+v", got)
		}
		if open := s.OpenContractCoreWindows("repo"); len(open) != 1 || !open[0].MarkedAt.IsZero() {
			t.Fatalf("the newer window is open and unmarked: %+v", open)
		}
	})
}

// TestContractSeedQueriesSelectOneRepositoryErrorfully pins the enrichment
// round's seed reads: the repository's provides/consumes owner edges (by
// source node repository) and its Spring reader and Java method nodes, with
// errors returned rather than swallowed.
func TestContractSeedQueriesSelectOneRepositoryErrorfully(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	nodes := []*graph.Node{
		{ID: "repo/Clocks.java::Clocks", Kind: graph.KindType, Name: "Clocks", FilePath: "repo/Clocks.java", RepoPrefix: "repo", Language: "java"},
		{ID: "repo/Clocks.java::Clocks.systemClock", Kind: graph.KindMethod, Name: "systemClock", FilePath: "repo/Clocks.java", RepoPrefix: "repo", Language: "java"},
		{ID: "repo/Settings.java::Settings.url", Kind: graph.KindField, Name: "url", FilePath: "repo/Settings.java", RepoPrefix: "repo", Language: "java", Meta: map[string]any{"spring_config_keys": []string{"app.url"}}},
		{ID: "repo/plain.go::Plain", Kind: graph.KindFunction, Name: "Plain", FilePath: "repo/plain.go", RepoPrefix: "repo", Language: "go"},
		{ID: "other/Other.java::Other.run", Kind: graph.KindMethod, Name: "run", FilePath: "other/Other.java", RepoPrefix: "other", Language: "java"},
	}
	edges := []*graph.Edge{
		{From: "repo/Clocks.java::Clocks", To: "repo/Clocks.java::Clocks.systemClock", Kind: graph.EdgeProvides, FilePath: "repo/Clocks.java", Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingBean, "provides_for": "Clock"}},
		{From: "repo/plain.go::Plain", To: "repo/Clocks.java::Clocks", Kind: graph.EdgeCalls, FilePath: "repo/plain.go"},
		{From: "other/Other.java::Other.run", To: "repo/Clocks.java::Clocks", Kind: graph.EdgeConsumes, FilePath: "other/Other.java"},
	}
	if err := s.AddBatchChecked(nodes, edges); err != nil {
		t.Fatal(err)
	}
	seedEdges, err := s.ContractSeedEdgesContext(ctx, "repo")
	if err != nil || len(seedEdges) != 1 || seedEdges[0].Kind != graph.EdgeProvides || seedEdges[0].Meta[graph.MetaDIBinding] != graph.DIBindingBean {
		t.Fatalf("seed edges=%v err=%v", seedEdges, err)
	}
	seedNodes, err := s.ContractSeedNodesContext(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, node := range seedNodes {
		ids = append(ids, node.ID)
	}
	want := []string{"repo/Clocks.java::Clocks.systemClock", "repo/Settings.java::Settings.url"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("seed nodes=%v want %v", ids, want)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ContractSeedEdgesContext(canceled, "repo"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled seed edges=%v", err)
	}
	if _, err := s.ContractSeedNodesContext(canceled, "repo"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled seed nodes=%v", err)
	}
}

// TestContractReceiptWriteBracketHoldsAWriteInFlight pins the bracket order a
// scan relies on: begun and the path's seq move before the write executes,
// and done only after its transaction ended, committed or rolled back.
func TestContractReceiptWriteBracketHoldsAWriteInFlight(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	row := boundaryStorageReceipt("held.go", "h1")
	began, committed, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	restore := SetContractReceiptWriteHookForTest(func(stage string, paths []string) {
		if len(paths) != 1 || paths[0] != row.FilePath {
			return
		}
		switch stage {
		case "begun":
			close(began)
		case "ending":
			close(committed)
			<-release
		}
	})
	defer restore()
	var wg sync.WaitGroup
	wg.Add(1)
	var writeErr error
	go func() {
		defer wg.Done()
		writeErr = s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row})
	}()
	<-began
	<-committed
	if begun, done := s.ContractReceiptWriteSample("repo"); begun != 1 || done != 0 {
		t.Fatalf("a committed write before its done is in flight: begun=%d done=%d", begun, done)
	}
	if seqs := s.ContractReceiptWriteSeqs("repo", "", []string{row.FilePath}); seqs[row.FilePath] != 1 {
		t.Fatalf("the seq moved at begin: %v", seqs)
	}
	stamps, err := s.ContractBoundaryReceiptStampsContext(ctx, "repo", "", []string{row.FilePath})
	if err != nil || stamps[row.FilePath].Fingerprint != "h1" {
		t.Fatalf("the committed row is readable while the write is in flight: %v %v", stamps, err)
	}
	close(release)
	wg.Wait()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if begun, done := s.ContractReceiptWriteSample("repo"); begun != 1 || done != 1 {
		t.Fatalf("settled: begun=%d done=%d", begun, done)
	}
	restore()
	// A refused write still ends: its done is bumped after the rollback.
	bad := row
	bad.Fingerprint = ""
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{bad}); err == nil {
		t.Fatal("an incomplete receipt was accepted")
	}
	if begun, done := s.ContractReceiptWriteSample("repo"); begun != done {
		t.Fatalf("a refused write left the bracket open: begun=%d done=%d", begun, done)
	}
}

func TestContractCoreWindowCloseIdentifiesTheOpenedSequence(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0)
	now := base
	s.contractReceiptWrites.now = func() time.Time { return now }
	a, b := boundaryStorageReceipt("a.go", "a1"), boundaryStorageReceipt("b.go", "b1")
	stage := func(row graph.ContractBoundaryReceipt) uint64 {
		require.NoError(t, s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row}))
		return s.ContractReceiptWriteSeqs("repo", "", []string{row.FilePath})[row.FilePath]
	}
	first := stage(a)
	now = base.Add(time.Second)
	s.EndContractCoreWrites("repo", "", map[string]uint64{a.FilePath: first})
	now = base.Add(2 * time.Second)
	second := stage(a)
	now = base.Add(3 * time.Second)
	s.EndContractCoreWrites("repo", "", map[string]uint64{a.FilePath: second})
	other := stage(b)
	now = base.Add(4 * time.Second)
	s.EndContractCoreWrites("repo", "", map[string]uint64{b.FilePath: other})
	at, ok := s.ContractCoreWindowClosedAt("repo", "", a.FilePath, first)
	require.True(t, ok)
	require.Equal(t, base.Add(time.Second), at, "later and unrelated windows never substitute for this close")
	at, ok = s.ContractCoreWindowClosedAt("repo", "", a.FilePath, second)
	require.True(t, ok)
	require.Equal(t, base.Add(3*time.Second), at)
	close, found := s.ContractCoreWindowLastClose("repo", "", a.FilePath, base.Add(2*time.Second))
	require.True(t, found)
	require.Equal(t, first, close.OpenerSeq)
	require.Equal(t, base.Add(time.Second).UnixNano(), close.ClosedAtNS)
	require.Equal(t, a.FilePath, close.Path)
	_, found = s.ContractCoreWindowLastClose("repo", "", a.FilePath, base)
	require.False(t, found)
	_, ok = s.ContractCoreWindowClosedAt("repo", "", a.FilePath, second+100)
	require.False(t, ok)
	for i := range contractCoreWindowGapRing + 1 {
		now = base.Add(time.Duration(5+i) * time.Second)
		seq := stage(a)
		s.EndContractCoreWrites("repo", "", map[string]uint64{a.FilePath: seq})
	}
	_, ok = s.ContractCoreWindowClosedAt("repo", "", a.FilePath, first)
	require.False(t, ok, "an evicted close is unknown, never the latest close")
	_, found = s.ContractCoreWindowLastClose("repo", "", a.FilePath, base.Add(2*time.Second))
	require.False(t, found, "history cannot invent a censored interval boundary")
	s.ForgetContractReceiptWritesForTest("repo", "", []string{a.FilePath})
	_, ok = s.ContractCoreWindowClosedAt("repo", "", a.FilePath, first)
	require.False(t, ok, "a restarted sequence cannot match a previous process's close")
}
