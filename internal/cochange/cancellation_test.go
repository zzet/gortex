package cochange

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestMineContextMatchesRealGitAndPreservesLegacyFailurePolicy(t *testing.T) {
	root := gitRepo(t)
	commit(t, root, "one", map[string]string{"a.go": "one", "b.go": "one"})
	commit(t, root, "two", map[string]string{"a.go": "two", "b.go": "two"})
	got, err := MineContext(context.Background(), root, Options{})
	require.NoError(t, err)
	require.Equal(t, Mine(context.Background(), root, Options{}), got)
	require.Len(t, got.Pairs, 1)
	bad := t.TempDir()
	_, err = MineContext(context.Background(), bad, Options{})
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, "context-aware API must retain actual Git failure")
	require.Empty(t, Mine(context.Background(), bad, Options{}).Pairs, "legacy best-effort policy changed")
	legacy := graph.New()
	n, err := EnrichGraph(legacy, bad, "")
	require.NoError(t, err)
	require.Zero(t, n)
	_, err = EnrichGraphContext(context.Background(), legacy, bad, "")
	require.ErrorAs(t, err, &exit)
}

func TestMineContextCanceledBeforeSpawnReturnsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Setenv("PATH", t.TempDir()) // No executable can accidentally satisfy the request.
	res, err := MineContext(ctx, t.TempDir(), Options{})
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, res.Pairs)
	require.Zero(t, res.CommitsScanned)
	require.Empty(t, res.FileChanges)
}

type cancelFileScan struct {
	graph.Store
	cancel  context.CancelFunc
	scanned int
}

func (g *cancelFileScan) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		for n := range g.Store.NodesByKind(kind) {
			g.scanned++
			g.cancel()
			if !yield(n) {
				return
			}
		}
	}
}
func TestAddEdgesContextStopsCanceledIteratorBeforeAnyWrite(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{{ID: "a", Kind: graph.KindFile, FilePath: "a.go"}, {ID: "b", Kind: graph.KindFile, FilePath: "b.go"}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &cancelFileScan{Store: base, cancel: cancel}
	n, err := AddEdgesContext(ctx, g, []Pair{{FileA: "a.go", FileB: "b.go", Score: 1, Count: 2}}, "")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, n)
	require.Equal(t, 1, g.scanned, "iterator was not actually stopped")
	require.Zero(t, base.EdgeCount())
}

type cancelBatchStore struct {
	graph.Store
	cancel  context.CancelFunc
	failAt  int
	failure error
	calls   int
	sizes   []int
}

func (g *cancelBatchStore) AddBatchChecked(nodes []*graph.Node, edges []*graph.Edge) error {
	g.calls++
	g.sizes = append(g.sizes, len(edges))
	if g.calls == g.failAt {
		return g.failure
	}
	g.AddBatch(nodes, edges)
	if g.cancel != nil {
		g.cancel()
	}
	return nil
}
func cochangeBatchFixture() (graph.Store, []Pair) {
	g := graph.New()
	pairs := make([]Pair, coChangeEdgeBatchSize+1)
	for i := range pairs {
		a, b := fmt.Sprintf("a-%d.go", i), fmt.Sprintf("b-%d.go", i)
		pairs[i] = Pair{FileA: a, FileB: b, Score: 0.5, Count: 2}
		g.AddBatch([]*graph.Node{{ID: a, Kind: graph.KindFile, FilePath: a}, {ID: b, Kind: graph.KindFile, FilePath: b}}, nil)
	}
	return g, pairs
}
func TestAddEdgesContextCountsOnlyCompletedBatchesAndRetainsErrors(t *testing.T) {
	for _, kind := range []string{"cancel", "write error"} {
		t.Run(kind, func(t *testing.T) {
			base, pairs := cochangeBatchFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("second batch storage failure")
			g := &cancelBatchStore{Store: base}
			if kind == "cancel" {
				g.cancel = cancel
			} else {
				g.failAt = 2
				g.failure = sentinel
			}
			n, err := AddEdgesContext(ctx, g, pairs, "")
			if kind == "cancel" {
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, 1, g.calls)
			} else {
				require.ErrorIs(t, err, sentinel)
				require.Equal(t, 2, g.calls)
			}
			require.Equal(t, coChangeEdgeBatchSize, n, "pending/rejected batch was counted as written")
			require.Equal(t, coChangeEdgeBatchSize, base.EdgeCount())
			for _, size := range g.sizes {
				require.LessOrEqual(t, size, coChangeEdgeBatchSize)
			}
		})
	}
}
func TestAddEdgesContextAndEnrichmentMatchSuccessfulLegacyResults(t *testing.T) {
	root := gitRepo(t)
	commit(t, root, "one", map[string]string{"a.go": "1", "b.go": "1"})
	commit(t, root, "two", map[string]string{"a.go": "2", "b.go": "2"})
	legacy, contextual := graph.New(), graph.New()
	for _, g := range []graph.Store{legacy, contextual} {
		g.AddBatch([]*graph.Node{{ID: "repo/a.go", Kind: graph.KindFile, FilePath: "repo/a.go", RepoPrefix: "repo"}, {ID: "repo/b.go", Kind: graph.KindFile, FilePath: "repo/b.go", RepoPrefix: "repo"}}, nil)
	}
	n, err := EnrichGraph(legacy, root, "repo")
	require.NoError(t, err)
	got, err := EnrichGraphContext(context.Background(), contextual, root, "repo")
	require.NoError(t, err)
	require.Equal(t, n, got)
	require.Equal(t, coChangeEdgeSet(legacy), coChangeEdgeSet(contextual))
	pairs := []Pair{{FileA: "a.go", FileB: "b.go", Count: 2, Score: 1}, {FileA: "a.go", FileB: "missing.go", Count: 2, Score: 1}}
	got, err = AddEdgesContext(context.Background(), contextual, pairs, "repo")
	require.NoError(t, err)
	require.Equal(t, 2, got, "successful duplicate count must retain legacy semantics")
	require.Equal(t, 2, contextual.EdgeCount())
}

const cochangeProcessMode = "GORTEX_COCHANGE_TEST_CHILD"

func TestCochangeProcessHelper(t *testing.T) {
	switch os.Getenv(cochangeProcessMode) {
	case "blocked":
		if err := os.WriteFile(os.Getenv("GORTEX_COCHANGE_TEST_READY"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "independent":
		fmt.Println("ready")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			fmt.Println("alive:" + scanner.Text())
		}
		os.Exit(0)
	}
}
func TestMineCommandCancellationReapsOwnedHelperAndLeavesUnrelatedChild(t *testing.T) {
	// These are actual OS helper processes, not Git; ordinary-Git parity is
	// exercised separately above. Only exec-owned Process handles are stopped.
	exe, err := os.Executable()
	require.NoError(t, err)
	otherCtx, stopOther := context.WithCancel(context.Background())
	other := exec.CommandContext(otherCtx, exe, "-test.run=^TestCochangeProcessHelper$")
	other.Env = append(os.Environ(), cochangeProcessMode+"=independent", "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
	in, err := other.StdinPipe()
	require.NoError(t, err)
	out, err := other.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, other.Start())
	t.Logf("unrelated cochange helper pid=%d", other.Process.Pid)
	lines, readerDone := make(chan string, 4), make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(lines)
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	t.Cleanup(func() {
		_ = in.Close()
		stopOther()
		waitErr := other.Wait()
		t.Logf("unrelated cochange helper cleanup result=%v", waitErr)
		require.NotNil(t, other.ProcessState)
		t.Logf("reaped unrelated cochange helper pid=%d", other.ProcessState.Pid())
		select {
		case <-readerDone:
		case <-time.After(3 * time.Second):
			t.Error("unrelated helper pipe reader did not join")
		}
	})
	select {
	case line := <-lines:
		require.Equal(t, "ready", line)
	case <-time.After(3 * time.Second):
		t.Fatal("unrelated helper failed startup")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestCochangeProcessHelper$")
	ready := filepath.Join(t.TempDir(), "ready")
	cmd.Env = append(os.Environ(), cochangeProcessMode+"=blocked", "GORTEX_COCHANGE_TEST_READY="+ready, "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
	joined := make(chan struct{})
	var result error
	go func() { defer close(joined); _, result = mineCommandOutput(ctx, cmd) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("owned helper did not actually join")
		}
	})
	var pid int
	require.Eventually(t, func() bool {
		b, e := os.ReadFile(ready)
		if e != nil {
			return false
		}
		pid, e = strconv.Atoi(strings.TrimSpace(string(b)))
		return e == nil
	}, 3*time.Second, time.Millisecond)
	t.Logf("owned cochange helper pid=%d", pid)
	cancel()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled command did not join its actual child")
	}
	require.ErrorIs(t, result, context.Canceled)
	var exit *exec.ExitError
	require.ErrorAs(t, result, &exit, "actual child exit evidence was lost")
	require.NotNil(t, cmd.ProcessState, "kill without actual Wait is not completion")
	require.Equal(t, pid, cmd.ProcessState.Pid())
	t.Logf("reaped owned cochange helper pid=%d", cmd.ProcessState.Pid())
	_, err = in.Write([]byte("still-running\n"))
	require.NoError(t, err)
	select {
	case line := <-lines:
		require.Equal(t, "alive:still-running", line)
	case <-time.After(3 * time.Second):
		t.Fatal("unrelated child did not respond after owned cancellation")
	}
}
