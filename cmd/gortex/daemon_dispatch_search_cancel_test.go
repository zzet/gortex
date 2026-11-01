package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/testenv"
)

// The real SQLite store and native FTS adapter remain in use. Only entry into
// the FTS query is delayed, deliberately ignoring cancellation until released,
// so the test proves admission recovery independently of SQLite interruption.
// This is an owned six-prefix fixture, not reference-corpus load evidence.
type heldDispatchFTSStore struct {
	*store_sqlite.Store
	once     sync.Once
	started  chan context.Context
	release  <-chan struct{}
	finished chan struct{}
}

func (s *heldDispatchFTSStore) SearchSymbolBundlesContext(ctx context.Context, text string, limit int) ([]graph.SymbolBundle, error) {
	first := false
	s.once.Do(func() { first = true; s.started <- ctx })
	if first {
		defer close(s.finished)
	}
	<-s.release
	return s.Store.SearchSymbolBundlesContext(ctx, text, limit)
}

func TestDaemonCancelledSearchOnSixRepoFTSReturnsCapacityWithin100ms(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	t.Setenv("GORTEX_TOOLS", "full")
	t.Setenv("GORTEX_TOOLS_MODE", "hide")
	store, err := store_sqlite.Open(filepath.Join(home, "six.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var nodes []*graph.Node
	var items []graph.SymbolFTSItem
	for i := range 6 {
		repo := fmt.Sprintf("fixture-%d", i)
		root := filepath.Join(home, repo)
		require.NoError(t, os.MkdirAll(root, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "source.go"), []byte("package fixture\nfunc NeedleSentinel(){}\n"), 0600))
		id := repo + "/source.go::NeedleSentinel"
		nodes = append(nodes, &graph.Node{ID: id, Name: "NeedleSentinel", Kind: graph.KindFunction, Language: "go", RepoPrefix: repo, FilePath: "source.go", StartLine: 2, EndLine: 2})
		items = append(items, graph.SymbolFTSItem{NodeID: id, Tokens: strings.Join(search.Tokenize("NeedleSentinel"), " ")})
	}
	store.AddBatch(nodes, nil)
	require.NoError(t, store.BulkUpsertSymbolFTS("", items))
	count, err := store.SymbolFTSCount()
	require.NoError(t, err)
	require.Equal(t, 6, count)
	prefixes := map[string]bool{}
	for _, node := range store.AllNodesLight() {
		prefixes[node.RepoPrefix] = true
	}
	require.Len(t, prefixes, 6)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	held := &heldDispatchFTSStore{Store: store, started: make(chan context.Context, 1), release: release, finished: make(chan struct{})}
	t.Cleanup(unblock)
	engine := query.NewEngine(held)
	engine.SetSearch(search.NewSymbolSearcherBackend(held))
	policy := gortexmcp.ToolPolicyConfig{Preset: "full"}
	srv := gortexmcp.NewServer(engine, held, nil, nil, zap.NewNop(), nil, gortexmcp.MultiRepoOptions{ToolPolicy: &policy})
	dispatcher := newMCPDispatcher(srv, nil, zap.NewNop())
	dir := testenv.ShortTempDir(t)
	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "state.json"))
	server, serverDone := startProxyRestartDaemon(t, socket, dispatcher)
	t.Cleanup(func() { unblock(); stopProxyRestartDaemon(t, server, serverDone) })
	client, err := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeMCP, Tools: "full", ToolsMode: "hide"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Conn.SetDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"cancel-test","version":"1"}}}`)))
	initialized, err := client.ReadMCPFrame()
	require.NoError(t, err)
	require.Contains(t, string(initialized), `"result"`)
	require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)))
	require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"needle sentinel","assist":false}}}`)))
	type frameResult struct {
		frame []byte
		err   error
	}
	responseCh := make(chan frameResult, 1)
	go func() {
		frame, readErr := client.ReadMCPFrame()
		responseCh <- frameResult{frame: frame, err: readErr}
	}()
	var queryContext context.Context
	select {
	case queryContext = <-held.started:
	case early := <-responseCh:
		t.Fatalf("search_symbols returned before entering injected FTS: frame=%s error=%v", early.frame, early.err)
	case <-time.After(5 * time.Second):
		t.Fatal("search_symbols did not enter the injected native FTS query")
	}
	// Setup is not part of the cancellation bound. Full-suite coverage/race
	// scheduling may delay query entry; time only after that entry is witnessed.
	require.NoError(t, client.Conn.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	canceledAt := time.Now()
	require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)))
	result := <-responseCh
	require.NoError(t, result.err)
	elapsed := time.Since(canceledAt)
	require.Less(t, elapsed, 100*time.Millisecond)
	require.Contains(t, string(result.frame), `"code":-32800`)
	require.Contains(t, string(result.frame), `"delivery_unknown":true`)
	require.Contains(t, string(result.frame), `"retryable":false`)
	require.ErrorIs(t, queryContext.Err(), context.Canceled, "request context must reach the native FTS interface")
	select {
	case <-held.finished:
		t.Fatal("injected FTS query completed before release")
	default:
	}
	status := daemon.MCPDispatchSnapshot()
	require.Zero(t, status.Lanes["fast"].InFlight)
	require.Equal(t, 1, status.Lanes["fast"].Draining)
	// Another real fast request can run while the canceled SQL call still
	// physically drains. It does not reuse or release that orphan's lease.
	require.NoError(t, client.Conn.SetReadDeadline(time.Now().Add(time.Second)))
	require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)))
	ping, err := client.ReadMCPFrame()
	require.NoError(t, err)
	require.True(t, bytes.Contains(ping, []byte(`"id":3`)) && bytes.Contains(ping, []byte(`"result"`)), "ping=%s", ping)
	unblock()
	select {
	case <-held.finished:
	case <-time.After(time.Second):
		t.Fatal("released FTS query did not finish")
	}
	require.Eventually(t, func() bool { return daemon.MCPDispatchSnapshot().Lanes["fast"].Draining == 0 }, time.Second, time.Millisecond)
}
