package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/testenv"
)

const (
	relationFreshnessCLISocketEnv = "GORTEX_RELATION_FRESHNESS_CLI_SOCKET"
	relationFreshnessCLIArgsEnv   = "GORTEX_RELATION_FRESHNESS_CLI_ARGS"
)

// TestRelationFreshnessCLI runs `gortex call` in a child process against an
// owned daemon whose watcher publication of an out-of-band file change is
// held at a real step. While held, the CLI's machine JSON must disclose the
// pending relations instead of presenting the old edge set as fresh; once the
// watcher publishes, the next CLI reply must hold the exact edge set.
func TestRelationFreshnessCLI(t *testing.T) {
	// The child enters this branch only after the package TestMain sandbox.
	if socket := os.Getenv(relationFreshnessCLISocketEnv); socket != "" {
		var args []string
		require.NoError(t, json.Unmarshal([]byte(os.Getenv(relationFreshnessCLIArgsEnv)), &args))
		require.NoError(t, os.Setenv("GORTEX_DAEMON_SOCKET", socket))
		require.NoError(t, os.Setenv("GORTEX_AUTOSTART", "0"))
		require.NoError(t, os.Setenv("GORTEX_TOOLS", "full"))
		os.Args = append([]string{os.Args[0]}, args...)
		main()
		os.Exit(0) // Keep the testing harness's PASS line out of CLI stdout.
	}
	t.Run("native_create/trace_call_chain_json", testRelationFreshnessCLINativeCreate)
	t.Run("out_of_band_delete/relations_callers_json", testRelationFreshnessCLINativeDelete)
}

type relationFreshnessCLIExtractor struct {
	parser.Extractor
	name    string
	armed   atomic.Bool
	entered chan [sha256.Size]byte
	release chan struct{}
}

func (e *relationFreshnessCLIExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	return e.ExtractWithOptions(path, raw, parser.ExtractionOptions{})
}

func (e *relationFreshnessCLIExtractor) ExtractWithOptions(path string, raw []byte, options parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	if filepath.Base(path) == e.name && e.armed.CompareAndSwap(true, false) {
		e.entered <- sha256.Sum256(raw)
		<-e.release
	}
	return parser.Extract(e.Extractor, path, raw, options)
}

// A newly created file resolves a caller's previously unresolved call. The
// watcher's first extraction of the created bytes is held, so the graph still
// has only the unresolved edge.
func testRelationFreshnessCLINativeCreate(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	const (
		caller  = "caller.go::Caller"
		control = "caller.go::Control"
		target  = "new_target.go::Target"
		name    = "new_target.go"
		created = "package main\nfunc Target() {}\n"
	)
	root, err := filepath.EvalSymlinks(startupPublicationRepo(t, startupPublicationEnv(t), "repo"))
	require.NoError(t, err)
	path := filepath.Join(root, name)
	require.NoError(t, os.Remove(filepath.Join(root, "main.go")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "caller.go"),
		[]byte("package main\nfunc Control() {}\nfunc Caller() { Control(); Target() }\nfunc Unrelated() { Control() }\n"), 0600))

	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native-create.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	extractor := &relationFreshnessCLIExtractor{
		Extractor: languages.NewGoExtractor(), name: name, entered: make(chan [sha256.Size]byte, 1), release: make(chan struct{}),
	}
	unblock := sync.OnceFunc(func() { close(extractor.release) })
	registry := parser.NewRegistry()
	registry.Register(extractor)
	logger := zap.NewNop()
	idx := indexer.New(db, registry, config.Default().Index, logger)
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := gortexmcp.NewServer(query.NewEngine(db), db, idx, nil, logger, nil)
	s.SetMaterializer(&graphview.Materializer{Store: db, Catalog: db.Catalog(), Leases: graphview.NewLeaseManager()})
	t.Cleanup(s.DrainBackground)
	watcher, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, logger)
	require.NoError(t, err)
	require.NoError(t, watcher.Start([]string{root}))
	t.Cleanup(func() { unblock(); require.NoError(t, watcher.Stop()) })
	s.SetWatcher(watcher)
	cli := relationFreshnessCLIDaemon(t, root, s)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	stored := func(wantResolved bool) {
		t.Helper()
		exact, unresolved := 0, 0
		for _, edge := range db.GetOutEdges(caller) {
			switch {
			case edge.Kind != graph.EdgeCalls:
			case edge.To == target:
				exact++
			case strings.HasPrefix(edge.To, "unresolved::") && strings.HasSuffix(edge.To, "Target"):
				unresolved++
			}
		}
		if wantResolved {
			require.Equal(t, []int{1, 0}, []int{exact, unresolved})
			require.NotNil(t, db.GetNode(target))
		} else {
			require.Equal(t, []int{0, 1}, []int{exact, unresolved})
			require.Nil(t, db.GetNode(target))
		}
	}
	probe := func(phase string) {
		t.Helper()
		payload := cli(ctx, "call", "trace", "--index", root, "--format", "json", "--json",
			`{"operation":"call_chain","target":{"symbol":"`+caller+`"},"output":{"format":"json","max_bytes":0}}`)
		require.Equal(t, 1, relationFreshnessCLIEdges(payload, caller, control), "%s: control edge", phase)
		relationFreshnessCLIAssert(t, phase, payload, name, relationFreshnessCLIEdges(payload, caller, target), 0, 1)
	}

	stored(false)
	probe("before")
	extractor.armed.Store(true)
	offered := time.Now()
	require.NoError(t, os.WriteFile(path, []byte(created), 0600))
	wantHash := sha256.Sum256([]byte(created))
	select {
	case received := <-extractor.entered:
		require.Equal(t, wantHash, received, "the first watcher extraction owns the exact created bytes")
	case <-ctx.Done():
		t.Fatal("native watcher did not reach the real extraction")
	}
	held := watcher.TierState(path)
	require.NotZero(t, held.Generation)
	require.True(t, held.RelationsAt.IsZero())
	require.False(t, held.Failed)
	stored(false)
	probe("held")
	require.True(t, watcher.TierState(path).RelationsAt.IsZero())
	stored(false)

	unblock()
	for {
		select {
		case event, ok := <-watcher.Events():
			require.True(t, ok)
			if event.FilePath != path {
				continue
			}
			// Native create and write notifications can coalesce as modified.
			require.Contains(t, []indexer.ChangeKind{indexer.ChangeCreated, indexer.ChangeModified}, event.Kind)
			require.True(t, event.Timestamp.After(offered))
			require.Positive(t, event.NodesAdded)
			stored(true)
			probe("recovered")
			return
		case <-ctx.Done():
			t.Fatal("native watcher did not publish the creation")
		}
	}
}

type relationFreshnessCLIStore struct {
	*store_sqlite.Store
	path    string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *relationFreshnessCLIStore) EvictFiles(paths []string) (int, int) {
	// Watcher publication owns the path already. Pause before SQLite takes
	// its write lock, so the real pre-delete rows remain queryable.
	if slices.Contains(paths, s.path) && s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	return s.Store.EvictFiles(paths)
}

// A file holding a caller is deleted outside any tool. The watcher's eviction
// of its rows is held, so the graph still returns the deleted caller's edge.
func testRelationFreshnessCLINativeDelete(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	const (
		file    = "gone.go"
		deleted = "gone.go::Gone"
		control = "main.go::Control"
		target  = "main.go::Target"
	)
	root, err := filepath.EvalSymlinks(startupPublicationRepo(t, startupPublicationEnv(t), "repo"))
	require.NoError(t, err)
	path := filepath.Join(root, file)
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc Target() {}\nfunc Control() { Target() }\n"), 0600))
	require.NoError(t, os.WriteFile(path, []byte("package main\nfunc Gone() { Target() }\n"), 0600))

	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native-delete.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := &relationFreshnessCLIStore{Store: db, path: file, entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(store.release) })
	logger := zap.NewNop()
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	idx := indexer.New(store, registry, config.Default().Index, logger)
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := gortexmcp.NewServer(query.NewEngine(store), store, idx, nil, logger, nil)
	t.Cleanup(s.DrainBackground)
	watcher, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, logger)
	require.NoError(t, err)
	require.NoError(t, watcher.Start([]string{root}))
	t.Cleanup(func() { unblock(); require.NoError(t, watcher.Stop()) })
	s.SetWatcher(watcher)
	cli := relationFreshnessCLIDaemon(t, root, s)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	stored := func() int {
		count := 0
		for _, edge := range store.GetInEdges(target) {
			if edge.From == deleted && edge.Kind == graph.EdgeCalls {
				count++
			}
		}
		return count
	}
	probe := func(phase string) {
		t.Helper()
		payload := cli(ctx, "call", "relations", "--index", root, "--format", "json", "--json",
			`{"operation":"callers","target":{"symbol":"`+target+`"},"output":{"format":"json","max_bytes":0}}`)
		require.Equal(t, 1, relationFreshnessCLIEdges(payload, control, target), "%s: control edge", phase)
		relationFreshnessCLIAssert(t, phase, payload, file, relationFreshnessCLIEdges(payload, deleted, target), 1, 0)
	}

	require.Equal(t, 1, stored())
	probe("before")
	store.armed.Store(true)
	require.NoError(t, os.Remove(path))
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("native watcher did not reach the real file eviction")
	}
	state := watcher.TierState(path)
	require.NotZero(t, state.Generation)
	require.True(t, state.RelationsAt.IsZero())
	require.Equal(t, 1, stored())
	probe("held")
	require.Equal(t, 1, stored(), "eviction must remain held through the CLI reply")

	unblock()
	for {
		select {
		case change, ok := <-watcher.Events():
			require.True(t, ok, "native watcher stopped before the deletion was published")
			if change.FilePath != path || change.Kind != indexer.ChangeDeleted {
				continue
			}
			require.Positive(t, change.NodesRemoved)
			require.Eventually(t, func() bool { return watcher.TierState(path).Generation == 0 }, time.Second, 5*time.Millisecond)
			require.Zero(t, stored())
			require.Nil(t, store.GetNode(deleted))
			probe("recovered")
			return
		case <-ctx.Done():
			t.Fatal("native watcher did not publish the deletion")
		}
	}
}

// relationFreshnessCLIDaemon serves s on an owned socket and returns a probe
// that runs one `gortex call` in a child process and decodes its JSON stdout.
func relationFreshnessCLIDaemon(t *testing.T, root string, s *gortexmcp.Server) func(context.Context, ...string) map[string]any {
	t.Helper()
	short := testenv.ShortTempDir(t)
	socket := filepath.Join(short, "s")
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(short, "p"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(short, "state.json"))
	logger := zap.NewNop()
	runtime := daemon.New(socket, version, logger)
	controller := probeController(t, fmt.Sprintf("repos:\n  - path: %q\n", root))
	controller.ready.Store(true)
	runtime.Controller = controller
	runtime.MCPDispatcher = newMCPDispatcher(s, nil, logger)
	require.NoError(t, runtime.Listen())
	served := make(chan error, 1)
	go func() { served <- runtime.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, runtime.Shutdown())
		select {
		case err := <-served:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("owned daemon did not stop")
		}
	})
	self, err := os.Executable()
	require.NoError(t, err)
	return func(ctx context.Context, args ...string) map[string]any {
		t.Helper()
		encoded, err := json.Marshal(args)
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, self, "-test.run=^TestRelationFreshnessCLI$")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), relationFreshnessCLISocketEnv+"="+socket, relationFreshnessCLIArgsEnv+"="+string(encoded), "TMPDIR="+t.TempDir())
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		wire, err := cmd.Output()
		require.NoError(t, err, "%s", stderr.String())
		var payload map[string]any
		require.NoError(t, json.Unmarshal(wire, &payload), "%s", wire)
		require.Equal(t, false, payload["truncated"])
		return payload
	}
}

func relationFreshnessCLIEdges(payload map[string]any, from, to string) int {
	rows, _ := payload["edges"].([]any)
	count := 0
	for _, row := range rows {
		if edge, ok := row.(map[string]any); ok && edge["from"] == from && edge["to"] == to && edge["kind"] == "calls" {
			count++
		}
	}
	return count
}

// relationFreshnessCLIAssert scores one CLI reply. Before the change and
// after publication the edge set is exact and carries no relation state.
// While publication is held, the edge set is one of the two real states and
// the reply discloses pending relations for the changed file, with no fresh
// claim anywhere.
func relationFreshnessCLIAssert(t *testing.T, phase string, payload map[string]any, file string, edges, before, published int) {
	t.Helper()
	rider, _ := payload["freshness"].(map[string]any)
	fields := rider
	if fields["graph_freshness"] == nil {
		fields = payload
	}
	state, _ := fields["graph_freshness"].(string)
	switch phase {
	case "before", "recovered":
		want := before
		if phase == "recovered" {
			want = published
		}
		require.Equal(t, want, edges, "%s edge count", phase)
		require.Empty(t, state, "%s relation state", phase)
	case "held":
		require.Contains(t, []int{before, published}, edges, "held reply invented an edge set")
		require.Equal(t, "pending", state)
		require.Equal(t, "relations", fields["freshness_scope"])
		require.Equal(t, []any{file}, fields["pending_files"])
		for _, claims := range []map[string]any{payload, rider} {
			require.NotEqual(t, "fresh", claims["graph_freshness"])
			require.NotEqual(t, true, claims["fresh"])
		}
	}
}
