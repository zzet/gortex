package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
)

// TestRelationFreshnessCLIKinds runs `gortex call` in a child process while
// the owned daemon's watcher publication of one file, changed outside any
// tool, is held at a real step. It covers the edit kinds the CLI cells of the
// watched matrix did not hold yet: an intra-file call, a cross-file call, a
// caller-body change, a rename and a signature change. While held, the CLI's
// machine JSON must disclose pending relations for the changed file and claim
// nothing fresh; an old answer without that disclosure is silent stale. After
// publication the CLI reply holds the exact answer and, once no generation is
// pending for the file, no relation state.
func TestRelationFreshnessCLIKinds(t *testing.T) {
	const (
		mainFile  = "package main\nfunc Target() {}\nfunc Control() { Target() }\n"
		withCall  = "func Changed() int { Target(); return 1 }"
		withoutIt = "func Changed() int { return 2 }"
		renamed   = "func Renamed() int { Target(); return 1 }"
	)
	crossFile := map[string]string{
		"main.go":   "package main\nfunc Target() {}\nfunc Other() {}\nfunc Control() { Target() }\n",
		"caller.go": "package main\nfunc Changed() { Other() }\n",
	}
	for _, row := range []relationFreshnessCLIKind{
		{
			name: "intra_file_call/relations_callers_json", files: map[string]string{"main.go": mainFile + withoutIt + "\n"},
			file: "main.go", old: withoutIt, new: withCall,
			callers: [2]string{"main.go::Changed"}, before: [2]int{0, 0}, published: [2]int{1, 0},
		},
		{
			name: "caller_body_change/relations_callers_json", files: map[string]string{"main.go": mainFile + withCall + "\n"},
			file: "main.go", old: withCall, new: withoutIt,
			callers: [2]string{"main.go::Changed"}, before: [2]int{1, 0}, published: [2]int{0, 0},
		},
		{
			// Held after the new rows are written, before the resolver binds
			// the new call: the cross-file edge is still unresolved.
			name: "cross_file_call/relations_callers_json", files: crossFile, afterApply: true,
			file: "caller.go", old: "func Changed() { Other() }", new: "func Changed() { Target() }",
			callers: [2]string{"caller.go::Changed"}, before: [2]int{0, 0}, published: [2]int{1, 0},
		},
		{
			name: "rename/relations_callers_json", files: map[string]string{"main.go": mainFile + withCall + "\n"},
			file: "main.go", old: withCall, new: renamed,
			callers: [2]string{"main.go::Changed", "main.go::Renamed"}, gone: true,
			before: [2]int{1, 0}, published: [2]int{0, 1},
		},
	} {
		t.Run(row.name, func(t *testing.T) { testRelationFreshnessCLICallers(t, row) })
	}
	t.Run("signature_change/trace_call_chain_json", testRelationFreshnessCLISignature)
}

type relationFreshnessCLIKind struct {
	name       string
	files      map[string]string
	file       string // the changed file
	old, new   string
	afterApply bool // hold after the new rows are written instead of before eviction
	// callers[0] is the changed caller before the change; callers[1], when
	// set, is the same caller under its new name.
	callers           [2]string
	gone              bool // callers[0] no longer exists after publication
	before, published [2]int
}

// relationFreshnessCLIHoldStore pauses the watcher's publication of one file
// at a real step: before its old rows are evicted, or after its new rows and
// mtime are written but before the resolver binds their calls.
type relationFreshnessCLIHoldStore struct {
	*store_sqlite.Store
	path       string
	afterApply bool
	armed      atomic.Bool
	entered    chan struct{}
	release    chan struct{}
}

func (s *relationFreshnessCLIHoldStore) pause() {
	close(s.entered)
	<-s.release
}

func (s *relationFreshnessCLIHoldStore) EvictFiles(paths []string) (int, int) {
	if !s.afterApply && slices.Contains(paths, s.path) && s.armed.CompareAndSwap(true, false) {
		s.pause()
	}
	return s.Store.EvictFiles(paths)
}

func (s *relationFreshnessCLIHoldStore) BulkSetFileMtimes(repo string, mtimes map[string]int64) error {
	if err := s.Store.BulkSetFileMtimes(repo, mtimes); err != nil {
		return err
	}
	if _, ok := mtimes[s.path]; ok && s.afterApply && s.armed.CompareAndSwap(true, false) {
		s.pause()
	}
	return nil
}

// relationFreshnessCLIWitnessWatcher records the newest generation with
// unpublished relations the server read from the watcher for one file. Every
// relation label is built from these snapshots, so a reply after publication
// may carry a relation state only when a generation admitted during its read
// explains it (the read's own catch-up for the same bytes, or a late native
// event for them).
type relationFreshnessCLIWitnessWatcher struct {
	*indexer.Watcher
	key    string
	mu     sync.Mutex
	newest uint64
}

func (w *relationFreshnessCLIWitnessWatcher) TierPaths() map[string]indexer.MutationTierState {
	paths := w.Watcher.TierPaths()
	for path, tiers := range paths {
		if tiers.Generation == 0 || tiers.Failed || !tiers.RelationsAt.IsZero() || pathkey.MutationKey(path) != w.key {
			continue
		}
		w.mu.Lock()
		w.newest = max(w.newest, tiers.Generation)
		w.mu.Unlock()
	}
	return paths
}

func (w *relationFreshnessCLIWitnessWatcher) generation() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.newest
}

type relationFreshnessCLIKindFixture struct {
	root    string
	path    string
	store   *relationFreshnessCLIHoldStore
	watcher *relationFreshnessCLIWitnessWatcher
	unblock func()
	cli     func(context.Context, ...string) map[string]any
	ctx     context.Context
}

func newRelationFreshnessCLIKindFixture(t *testing.T, files map[string]string, file string, afterApply bool) *relationFreshnessCLIKindFixture {
	t.Helper()
	t.Setenv("GORTEX_TOOLS", "full")
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	root, err := filepath.EvalSymlinks(startupPublicationRepo(t, startupPublicationEnv(t), "repo"))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "main.go")))
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
	}
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cli-kinds.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	f := &relationFreshnessCLIKindFixture{root: root, path: filepath.Join(root, file)}
	f.store = &relationFreshnessCLIHoldStore{Store: db, path: file, afterApply: afterApply, entered: make(chan struct{}), release: make(chan struct{})}
	f.unblock = sync.OnceFunc(func() { close(f.store.release) })
	logger := zap.NewNop()
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	idx := indexer.New(f.store, registry, config.Default().Index, logger)
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := gortexmcp.NewServer(query.NewEngine(f.store), f.store, idx, nil, logger, nil)
	t.Cleanup(s.DrainBackground)
	watcher, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, logger)
	require.NoError(t, err)
	require.NoError(t, watcher.Start([]string{root}))
	t.Cleanup(func() { f.unblock(); require.NoError(t, watcher.Stop()) })
	f.watcher = &relationFreshnessCLIWitnessWatcher{Watcher: watcher, key: pathkey.MutationKey(f.path)}
	s.SetWatcher(f.watcher)
	f.cli = relationFreshnessCLIDaemon(t, root, s)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	f.ctx = ctx
	return f
}

// change writes the file's new bytes outside any tool and waits until the
// watcher's publication of them reaches its real pause.
func (f *relationFreshnessCLIKindFixture) change(t *testing.T, content string) {
	t.Helper()
	f.store.armed.Store(true)
	require.NoError(t, os.WriteFile(f.path, []byte(content), 0o600))
	select {
	case <-f.store.entered:
	case <-f.ctx.Done():
		t.Fatal("native watcher did not reach its real publication pause")
	}
	held := f.watcher.TierState(f.path)
	require.NotZero(t, held.Generation)
	require.True(t, held.RelationsAt.IsZero())
	require.False(t, held.Failed)
}

// release lets the held publication finish and waits until the watcher has
// no admitted generation left for the file.
func (f *relationFreshnessCLIKindFixture) release(t *testing.T) {
	t.Helper()
	f.unblock()
	f.awaitNoGeneration(t)
}

func (f *relationFreshnessCLIKindFixture) awaitNoGeneration(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return f.watcher.TierState(f.path).Generation == 0 },
		10*time.Second, 5*time.Millisecond, "the watcher kept an admitted generation for the file")
}

// recovered reads until a reply after publication carries no relation state.
// read scores the reply's answer itself and returns its relation state; a
// state is allowed only when a generation admitted during that read explains
// it, and the next reply is then scored again.
func (f *relationFreshnessCLIKindFixture) recovered(t *testing.T, read func() string) {
	t.Helper()
	for range 3 {
		f.awaitNoGeneration(t)
		admitted := f.watcher.generation()
		state := read()
		if state == "" {
			return
		}
		require.Greater(t, f.watcher.generation(), admitted,
			"a CLI reply after publication kept a relation state no generation admitted during its read explains: %s", state)
	}
	t.Fatal("CLI replies after publication kept admitting catch-up generations")
}

// relationFreshnessCLIState returns the reply's relation state and the
// fields that carry it, and checks that a reply disclosing a state claims
// nothing fresh.
func relationFreshnessCLIState(t *testing.T, payload map[string]any) (string, map[string]any) {
	t.Helper()
	rider, _ := payload["freshness"].(map[string]any)
	fields := rider
	if fields["graph_freshness"] == nil {
		fields = payload
	}
	state, _ := fields["graph_freshness"].(string)
	if state != "" {
		for _, claims := range []map[string]any{payload, rider} {
			require.NotEqual(t, "fresh", claims["graph_freshness"])
			require.NotEqual(t, true, claims["fresh"])
		}
	}
	return state, fields
}

func relationFreshnessCLIRequirePending(t *testing.T, state string, fields map[string]any, file string) {
	t.Helper()
	require.Equal(t, "pending", state)
	require.Equal(t, "relations", fields["freshness_scope"])
	require.Equal(t, []any{file}, fields["pending_files"])
}

func testRelationFreshnessCLICallers(t *testing.T, row relationFreshnessCLIKind) {
	const (
		target  = "main.go::Target"
		control = "main.go::Control"
	)
	f := newRelationFreshnessCLIKindFixture(t, row.files, row.file, row.afterApply)
	stored := func() [2]int {
		var set [2]int
		for _, edge := range f.store.GetInEdges(target) {
			for i, caller := range row.callers {
				if caller != "" && edge.From == caller && edge.Kind == graph.EdgeCalls {
					set[i]++
				}
			}
		}
		return set
	}
	probe := func(phase string) string {
		t.Helper()
		payload := f.cli(f.ctx, "call", "relations", "--index", f.root, "--format", "json", "--json",
			`{"operation":"callers","target":{"symbol":"`+target+`"},"output":{"format":"json","max_bytes":0}}`)
		require.Equal(t, 1, relationFreshnessCLIEdges(payload, control, target), "%s: control edge", phase)
		var got [2]int
		for i, caller := range row.callers {
			if caller != "" {
				got[i] = relationFreshnessCLIEdges(payload, caller, target)
			}
		}
		state, fields := relationFreshnessCLIState(t, payload)
		switch phase {
		case "before":
			require.Equal(t, row.before, got)
			require.Empty(t, state)
		case "held":
			relationFreshnessCLIRequirePending(t, state, fields, row.file)
			// The old edge set, the new one, or only the edges both agree
			// on: a held rename can drop the old name before the new one
			// resolves.
			agreed := [2]int{min(row.before[0], row.published[0]), min(row.before[1], row.published[1])}
			require.Contains(t, [][2]int{row.before, row.published, agreed}, got, "held reply invented an edge set")
		case "recovered":
			require.Equal(t, row.published, got, "a CLI reply after publication returned a stale edge set")
			if state != "" {
				relationFreshnessCLIRequirePending(t, state, fields, row.file)
			}
		}
		return state
	}

	require.Equal(t, row.before, stored())
	probe("before")
	changed := strings.Replace(row.files[row.file], row.old, row.new, 1)
	require.NotEqual(t, row.files[row.file], changed)
	f.change(t, changed)
	require.Equal(t, row.before, stored())
	probe("held")
	require.Equal(t, row.before, stored(), "publication must stay held through the CLI reply")
	require.True(t, f.watcher.TierState(f.path).RelationsAt.IsZero())

	f.release(t)
	if row.gone {
		require.Nil(t, f.store.GetNode(row.callers[0]))
	}
	require.Equal(t, row.published, stored())
	f.recovered(t, func() string { return probe("recovered") })
}

// A signature change moves the following function down two lines. The CLI
// reads the call chain through `gortex call trace` while the watcher's
// eviction of the old rows is held: a reply serving the old line layout
// without a pending disclosure would be silent stale.
func testRelationFreshnessCLISignature(t *testing.T) {
	const (
		target      = "main.go::Target"
		following   = "main.go::Following"
		example     = "main.go::Example"
		head        = "package main\n\nfunc Example() int { return 1 }\n\n"
		old         = "func Target() int { return Example() }"
		replacement = "func Target(optional ...int) int {\n return Example()\n}"
		tail        = "\n\nfunc Following() int { return Target() }\n"
	)
	f := newRelationFreshnessCLIKindFixture(t, map[string]string{"main.go": head + old + tail}, "main.go", false)
	storedGap := func() int {
		t.Helper()
		first, second := f.store.GetNode(target), f.store.GetNode(following)
		require.NotNil(t, first)
		require.NotNil(t, second)
		return second.StartLine - first.StartLine
	}
	probe := func(phase string) string {
		t.Helper()
		payload := f.cli(f.ctx, "call", "trace", "--index", f.root, "--format", "json", "--json",
			`{"operation":"call_chain","target":{"symbol":"`+following+`"},"options":{"depth":2,"limit":1000},"output":{"format":"json","max_bytes":0}}`)
		nodes, ok := payload["nodes"].([]any)
		require.True(t, ok, "%s: call chain nodes: %v", phase, payload)
		lines := make(map[string]int, 2)
		for _, value := range nodes {
			node, ok := value.(map[string]any)
			require.True(t, ok)
			id, _ := node["id"].(string)
			if id != target && id != following {
				continue
			}
			line, ok := node["start_line"].(float64)
			require.True(t, ok)
			require.NotContains(t, lines, id)
			lines[id] = int(line)
		}
		require.Len(t, lines, 2)
		gap := lines[following] - lines[target]
		require.Equal(t, 1, relationFreshnessCLIEdges(payload, following, target), "%s: %s -> %s", phase, following, target)
		require.Equal(t, 1, relationFreshnessCLIEdges(payload, target, example), "%s: %s -> %s", phase, target, example)
		state, fields := relationFreshnessCLIState(t, payload)
		switch phase {
		case "before":
			require.Equal(t, 2, gap)
			require.Empty(t, state)
		case "held":
			relationFreshnessCLIRequirePending(t, state, fields, "main.go")
			require.Contains(t, []int{2, 4}, gap, "held reply invented a line layout")
		case "recovered":
			require.Equal(t, 4, gap, "a CLI reply after publication returned the old line layout")
			if state != "" {
				relationFreshnessCLIRequirePending(t, state, fields, "main.go")
			}
		}
		return state
	}

	require.Equal(t, 2, storedGap())
	probe("before")
	f.change(t, head+replacement+tail)
	require.Equal(t, 2, storedGap())
	probe("held")
	require.Equal(t, 2, storedGap(), "publication must stay held through the CLI reply")

	f.release(t)
	require.Equal(t, 4, storedGap())
	f.recovered(t, func() string { return probe("recovered") })
}
