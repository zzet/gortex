package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// inertTestWatcher builds a steady-state indexed repo. The explicit IndexFile
// stamps the tiered fingerprints; separate coverage below proves an old DB's
// first patch remains conservative.
func inertTestWatcher(t *testing.T, fileName, content string) (string, *Indexer, *Watcher) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	writeTestFile(t, path, content)

	g := graph.New()
	idx := newTestIndexer(g)
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NoError(t, idx.IndexFile(path))

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	return dir, idx, w
}

func TestWatcher_CommentOnlyChangeSkipsReindex(t *testing.T) {
	dir, idx, w := inertTestWatcher(t, "main.go", `package main

func Stable() {}
`)
	path := filepath.Join(dir, "main.go")
	before := nodeByName(t, idx, "main.go", "Stable")

	writeTestFile(t, path, `package main

// Stable does nothing at all.
// This comment block is brand new and shifts the line numbers.
func Stable() {}
`)
	_ = w.patchGraph(path, ChangeModified)

	after := nodeByName(t, idx, "main.go", "Stable")
	assert.Equal(t, 3, before.StartLine)
	assert.Equal(t, 5, after.StartLine, "metadata refresh must update shifted source spans")
	assert.Equal(t, 5, after.EndLine)
	assert.Contains(t, after.Meta["doc"], "brand new")
}

func TestWatcher_WhitespaceChangeSkipsReindex(t *testing.T) {
	dir, idx, w := inertTestWatcher(t, "main.go", `package main

func KeepMe() {}
`)
	path := filepath.Join(dir, "main.go")
	before := nodeByName(t, idx, "main.go", "KeepMe")

	writeTestFile(t, path, `package main



func KeepMe() {}


`)
	_ = w.patchGraph(path, ChangeModified)

	after := nodeByName(t, idx, "main.go", "KeepMe")
	assert.Equal(t, 3, before.StartLine)
	assert.Equal(t, 5, after.StartLine)
}

func TestWatcher_InertChangeEmitsZeroDeltaEvent(t *testing.T) {
	dir, _, w := inertTestWatcher(t, "main.go", `package main

func Same() {}
`)
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, `package main

// freshly added doc line
func Same() {}
`)
	_ = w.patchGraph(path, ChangeModified)

	select {
	case ev := <-w.Events():
		assert.Equal(t, ChangeModified, ev.Kind)
		assert.Equal(t, "metadata_only", ev.Classification)
		assert.Zero(t, ev.NodesAdded)
		assert.Zero(t, ev.NodesRemoved)
		assert.Zero(t, ev.EdgesAdded)
		assert.Zero(t, ev.EdgesRemoved)
	default:
		t.Fatal("metadata-only modify must publish a change event")
	}
	history := w.History()
	require.Len(t, history, 1)
	assert.Equal(t, "metadata_only", history[0].Classification)
}

func TestWatcher_InertChangeFiresCallbackWithUnchangedSymbols(t *testing.T) {
	dir, _, w := inertTestWatcher(t, "main.go", `package main

func Untouched() {}
`)
	path := filepath.Join(dir, "main.go")

	var mu sync.Mutex
	var gotOld, gotNew []string
	calls := 0
	w.OnSymbolChange(func(_ string, oldSymbols, newSymbols []*graph.Node) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		for _, n := range oldSymbols {
			gotOld = append(gotOld, n.Name)
		}
		for _, n := range newSymbols {
			gotNew = append(gotNew, n.Name)
		}
	})

	writeTestFile(t, path, `package main

// a comment, nothing else
func Untouched() {}
`)
	_ = w.patchGraph(path, ChangeModified)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, calls)
	assert.Equal(t, gotOld, gotNew)
	assert.Contains(t, gotOld, "Untouched")
}

func TestWatcher_StructuralChangeStillReindexes(t *testing.T) {
	dir, idx, w := inertTestWatcher(t, "main.go", `package main

func OldName() {}
`)
	path := filepath.Join(dir, "main.go")
	require.NotEmpty(t, idx.graph.FindNodesByName("OldName"))

	writeTestFile(t, path, `package main

func NewName() {}
`)
	_ = w.patchGraph(path, ChangeModified)

	assert.Empty(t, idx.graph.FindNodesByName("OldName"))
	assert.NotEmpty(t, idx.graph.FindNodesByName("NewName"))
	select {
	case ev := <-w.Events():
		assert.Equal(t, "structural", ev.Classification)
		assert.NotZero(t, ev.NodesRemoved)
	default:
		t.Fatal("a structural modify must publish a change event")
	}
}

func TestWatcher_AddedSymbolStillReindexes(t *testing.T) {
	dir, idx, w := inertTestWatcher(t, "main.go", `package main

func First() {}
`)
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, `package main

func First() {}

func Second() {}
`)
	_ = w.patchGraph(path, ChangeModified)
	assert.NotEmpty(t, idx.graph.FindNodesByName("First"))
	assert.NotEmpty(t, idx.graph.FindNodesByName("Second"))
}

func TestWatcher_SignatureChangeStillReindexes(t *testing.T) {
	dir, idx, w := inertTestWatcher(t, "main.go", `package main

func Compute(a int) {}
`)
	path := filepath.Join(dir, "main.go")
	funcBefore := nodeByName(t, idx, "main.go", "Compute")
	writeTestFile(t, path, `package main

func Compute(a int, b string) {}
`)
	_ = w.patchGraph(path, ChangeModified)
	funcAfter := nodeByName(t, idx, "main.go", "Compute")
	assert.NotSame(t, funcBefore, funcAfter)
}

func TestWatcher_InertSkipRefreshesMtime(t *testing.T) {
	dir, idx, w := inertTestWatcher(t, "main.go", `package main

func Steady() {}
`)
	path := filepath.Join(dir, "main.go")
	mtimeBefore := idx.FileMtimes()["main.go"]
	require.NotZero(t, mtimeBefore)

	future := time.Now().Add(2 * time.Second)
	writeTestFile(t, path, `package main

// a new comment
func Steady() {}
`)
	require.NoError(t, os.Chtimes(path, future, future))
	_ = w.patchGraph(path, ChangeModified)
	assert.Greater(t, idx.FileMtimes()["main.go"], mtimeBefore)
}

func TestWatcher_MetadataOnlyRefreshUpdatesEdgeLineAndTarget(t *testing.T) {
	dir, idx, w := inertTestWatcher(t, "main.go", `package main

func Target() {}
func Caller() { Target() }
`)
	path := filepath.Join(dir, "main.go")
	caller := nodeByName(t, idx, "main.go", "Caller")
	before := callEdge(t, idx, caller.ID)
	beforeLine, beforeTarget := before.Line, before.To

	writeTestFile(t, path, `package main

func Target() {}

// Caller invokes Target.
func Caller() { Target() }
`)
	_ = w.patchGraph(path, ChangeModified)

	after := callEdge(t, idx, caller.ID)
	assert.Greater(t, after.Line, beforeLine)
	assert.Equal(t, beforeTarget, after.To, "metadata refresh must preserve resolver-selected target")
	select {
	case ev := <-w.Events():
		assert.Equal(t, "metadata_only", ev.Classification)
		assert.Zero(t, ev.NodesAdded)
		assert.Zero(t, ev.EdgesAdded)
	default:
		t.Fatal("metadata-only refresh must emit an event")
	}
}

func TestWatcher_OldDatabaseFirstPatchIsConservative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Stable() {}\n")
	g := graph.New()
	idx := newTestIndexer(g)
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	// An old database: a cold index written before whole indexes stamped the
	// edit-routing fingerprints (a current whole index stamps the ones a
	// per-save stamps — TestWholeIndexStampsTheFileFingerprintsAPerSaveStamps).
	// The first patch must therefore fail closed to a structural file refresh;
	// that patch stamps the exact extraction so subsequent edits can take
	// narrower routes.
	stripFileFingerprints(t, g)
	coldNodes := idx.graph.GetFileNodes("main.go")
	assert.Equal(t, fileDeltaFingerprints{}, storedExtractionGraphFingerprints(coldNodes))
	assert.False(t, storedDerivedFingerprints(coldNodes).complete())
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true}, zap.NewNop())
	require.NoError(t, err)

	writeTestFile(t, path, "package main\n\n// first patch\nfunc Stable() {}\n")
	_ = w.patchGraph(path, ChangeModified)
	first := <-w.Events()
	assert.Equal(t, "structural", first.Classification)
	assert.NotZero(t, first.NodesRemoved)

	writeTestFile(t, path, "package main\n\n// second patch\n// now fingerprinted\nfunc Stable() {}\n")
	_ = w.patchGraph(path, ChangeModified)
	second := <-w.Events()
	assert.Equal(t, "metadata_only", second.Classification)
	assert.Zero(t, second.NodesRemoved)
}

func nodeByName(t *testing.T, idx *Indexer, relPath, name string) *graph.Node {
	t.Helper()
	for _, n := range idx.graph.GetFileNodes(relPath) {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("node %q not found in %s", name, relPath)
	return nil
}

func callEdge(t *testing.T, idx *Indexer, from string) *graph.Edge {
	t.Helper()
	for _, edge := range idx.graph.GetOutEdges(from) {
		if edge.Kind == graph.EdgeCalls {
			return edge
		}
	}
	t.Fatalf("call edge not found for %s", from)
	return nil
}
