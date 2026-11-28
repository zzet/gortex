package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
)

// readBuiltRetirementStore can pause the watcher's eviction of one file, or
// the first projection retirement pass between selecting its entries and
// dropping them.
type readBuiltRetirementStore struct {
	*store_sqlite.Store
	path            string
	holdEviction    atomic.Bool
	holdRetirement  atomic.Bool
	entered         chan struct{}
	release         chan struct{}
	releaseOnceFunc func()
}

func (s *readBuiltRetirementStore) pause() {
	close(s.entered)
	<-s.release
}

func (s *readBuiltRetirementStore) EvictFiles(paths []string) (int, int) {
	if slices.Contains(paths, s.path) && s.holdEviction.CompareAndSwap(true, false) {
		s.pause()
	}
	return s.Store.EvictFiles(paths)
}

func (s *readBuiltRetirementStore) validateProjectionRetirement() error {
	if s.holdRetirement.CompareAndSwap(true, false) {
		s.pause()
	}
	return nil
}

// readBuiltRetirementWatcher records the watcher snapshots the server reads,
// so the test can tell whether a generation was admitted during a read.
type readBuiltRetirementWatcher struct {
	*indexer.Watcher
	relations *relationGenerationWitness
}

func (w *readBuiltRetirementWatcher) TierPaths() map[string]indexer.MutationTierState {
	return w.relations.observe(w.Watcher.TierPaths())
}

const (
	readBuiltHead = "package main\n\nfunc Target() {}\n\n"
	readBuiltOld  = "func Changed() int { return 1 }"
	readBuiltNew  = "func Changed() int {\n\tTarget()\n\treturn 2\n}\n\nfunc Added() {}"
)

// readBuiltReply is one read's evidence: whether it was refused, its
// relation state, and whether its rows are the old or the new file.
type readBuiltReply struct {
	tool    string
	refusal string
	state   string
	reason  string // "store_busy" when the store read ran out of time
	layout  string // "old", "new", or a description of rows that are neither
	body    string
	err     error
}

// TestReadBuiltProjectionClearsAfterPublication covers a file read that
// cannot serve the registered projection as it is: the file's bytes are ahead
// of the graph, or an edit's projection was marked suspect by the edit's own
// native event. The read serves a checked projection of the current bytes,
// registers it and admits a catch-up publication for them. Once the watcher
// has published every generation admitted for the file, the graph holds those
// bytes, and every read of the file must answer from it:
//
//   - with the new rows (a reply with the old rows and no pending label would
//     be silent stale),
//   - without a refusal, including reads that run at the same time, and
//   - without a relation state, unless a new generation was admitted for the
//     file during that read: its own catch-up, or a native event the watcher
//     delivered late (scored on sequential reads).
//
// A projection nobody retires keeps every later read of the file labelled
// pending until the file changes again.
func TestReadBuiltProjectionClearsAfterPublication(t *testing.T) {
	cases := []struct {
		name string
		// write changes the file while its publication or retirement is held.
		write func(t *testing.T, f *readBuiltRetirementFixture)
		// behind: the graph does not hold the new bytes while the held read
		// runs, so that read must disclose pending relations.
		behind bool
	}{
		{
			// An out-of-band write: no edit receipt exists for the
			// projection the read builds.
			name:   "out_of_band_write",
			behind: true,
			write: func(t *testing.T, f *readBuiltRetirementFixture) {
				f.store.holdEviction.Store(true)
				require.NoError(t, os.WriteFile(f.path, []byte(readBuiltHead+readBuiltNew+"\n"), 0o600))
				f.awaitPause(t)
			},
		},
		{
			// A local-ACK edit whose receipt retirement already selected the
			// edit's projection when a read replaces it with a checked copy.
			name: "edit_retirement_race",
			write: func(t *testing.T, f *readBuiltRetirementFixture) {
				f.store.holdRetirement.Store(true)
				result, err := externalVersionRegisteredCall(f.s, f.ctx, 3, "edit_file", map[string]any{
					"path": "main.go", "old_string": readBuiltOld, "new_string": readBuiltNew,
					"expected_occurrences": 1, "format": "json",
				})
				require.NoError(t, err)
				require.False(t, result.IsError, resultText(result))
				f.awaitPause(t)
				edited := pendingFileProjections.snap.Load().entries[f.key]
				require.NotNil(t, edited, "the edit published its projection")
				// A native event for the path marks the projection suspect, so
				// the next read re-checks its bytes before serving it. The edit's
				// own event can arrive before the projection is published, so
				// deliver one through the watcher's source-event callback.
				pendingFileProjections.markSuspect(f.path)
				require.True(t, edited.suspect.Load())
			},
		},
	}
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			f := newReadBuiltRetirementFixture(t, readBuiltHead+readBuiltOld+"\n")
			before := f.read(t, 2, "get_file_summary")
			require.Empty(t, before.refusal)
			require.Equal(t, "old", before.layout)
			require.Empty(t, before.state)

			row.write(t, f)
			published := pendingFileProjections.published.Load()
			for attempt := 0; pendingFileProjections.published.Load() == published; attempt++ {
				require.Less(t, attempt, 100, "the read did not publish a projection of the current bytes")
				held := f.read(t, 4, "get_file_summary")
				if held.refusal == "" {
					if !held.storeBusy() {
						require.Contains(t, []string{"old", "new"}, held.layout, "a held reply invented a file layout: %s", held.body)
					}
					if row.behind || held.layout != "new" {
						require.Equal(t, "pending", held.state, "a read ahead of the graph must disclose pending relations")
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			f.store.releaseOnceFunc()
			f.awaitNoGeneration(t)

			// Reads at the same time, right after publication, through file
			// and relation tools: a relation read binds every registered
			// projection, and get_call_chain has no retry of its own. Two of
			// them can race on the same retirement, and get_call_chain may
			// then label its reply pending, so only refusals and rows are
			// scored here; the sequential reads below score the label.
			tools := []string{"get_file_summary", "get_callers", "get_call_chain", "get_file_summary", "get_callers", "get_call_chain"}
			replies := make([]readBuiltReply, len(tools))
			var wg sync.WaitGroup
			for i, tool := range tools {
				wg.Go(func() { replies[i] = f.readReply(10+i, tool) })
			}
			wg.Wait()
			for _, reply := range replies {
				require.NoError(t, reply.err, reply.tool)
				require.Empty(t, reply.refusal, "%s: a read after publication was refused", reply.tool)
				require.Contains(t, []string{"", "pending"}, reply.state, reply.tool)
				if !reply.storeBusy() {
					require.Equal(t, "new", reply.layout, "%s: a reply after publication returned stale rows: %s", reply.tool, reply.body)
				}
			}

			f.recovered(t)
			require.Nil(t, pendingFileProjections.snap.Load().entries[f.key], "the published file kept its read-built projection")
		})
	}
}

type readBuiltRetirementFixture struct {
	s       *Server
	store   *readBuiltRetirementStore
	watcher *readBuiltRetirementWatcher
	ctx     context.Context
	path    string
	key     string
}

func newReadBuiltRetirementFixture(t *testing.T, content string) *readBuiltRetirementFixture {
	t.Helper()
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_QUERY_LOG", filepath.Join(t.TempDir(), "query-log.jsonl"))
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "read-built.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	store := &readBuiltRetirementStore{Store: sql, path: "main.go", entered: make(chan struct{}), release: make(chan struct{})}
	store.releaseOnceFunc = sync.OnceFunc(func() { close(store.release) })
	logger := zap.NewNop()
	idx := indexer.New(store, testRegistry(), config.Default().Index, logger)
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := NewServer(query.NewEngine(store), store, idx, nil, logger, nil)
	t.Cleanup(func() { s.DrainBackground(); s.queryLog.Close() })
	s.editAckMode = config.EditAckLocal
	native, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, logger)
	require.NoError(t, err)
	require.NoError(t, native.Start([]string{root}))
	t.Cleanup(func() { store.releaseOnceFunc(); require.NoError(t, native.Stop()) })
	watcher := &readBuiltRetirementWatcher{Watcher: native, relations: newRelationGenerationWitness(path)}
	s.SetWatcher(watcher)

	const session = "read-built-retirement"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	s.NoteSessionToolPolicy(session, "all", "defer")
	ctx, cancel := context.WithTimeout(s.WithClientSession(WithSessionID(t.Context(), session), session), 20*time.Second)
	t.Cleanup(cancel)
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"read-built-retirement","version":"1"}}}`)))
	return &readBuiltRetirementFixture{s: s, store: store, watcher: watcher, ctx: ctx, path: path, key: pathkey.MutationKey(path)}
}

func (f *readBuiltRetirementFixture) awaitPause(t *testing.T) {
	t.Helper()
	select {
	case <-f.store.entered:
	case <-f.ctx.Done():
		t.Fatal("the held publication step was not reached")
	}
}

func (f *readBuiltRetirementFixture) awaitNoGeneration(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return f.watcher.TierState(f.path).Generation == 0 },
		5*time.Second, 5*time.Millisecond, "the watcher kept an admitted generation for the file")
}

// recovered reads the file summary until a reply carries no relation state.
// A reply may carry one only when the server's own freshness snapshots for
// that read saw a generation admitted during it (a native event can still mark
// the projection suspect, and its re-check admits a catch-up for the same
// bytes; FSEvents can also deliver the event late, after the file's last
// ticket finished, and the watcher admits it as a new generation); the next
// reply after that generation publishes is scored again. A reply whose store
// read ran out of time is labelled as such and read again.
func (f *readBuiltRetirementFixture) recovered(t *testing.T) {
	t.Helper()
	for attempt := range 5 {
		f.awaitNoGeneration(t)
		admitted := f.watcher.relations.generation()
		reply := f.read(t, 20+attempt, "get_file_summary")
		require.Empty(t, reply.refusal, "a read after publication was refused")
		if reply.storeBusy() {
			continue
		}
		require.Equal(t, "new", reply.layout, "a reply after publication returned stale rows: %s", reply.body)
		if reply.state == "" {
			return
		}
		require.Greater(t, f.watcher.relations.generation(), admitted,
			"a reply after publication kept a relation state no generation admitted during its read explains: %s", reply.state)
	}
	t.Fatal("replies after publication kept admitting catch-up generations")
}

// storeBusy reports a reply whose store read ran out of time. It is labelled
// pending and incomplete, so its rows may be partial; it is never silent.
func (r readBuiltReply) storeBusy() bool {
	return r.state == "pending" && r.reason == "store_busy"
}

func (f *readBuiltRetirementFixture) read(t *testing.T, id int, tool string) readBuiltReply {
	t.Helper()
	reply := f.readReply(id, tool)
	require.NoError(t, reply.err, tool)
	return reply
}

// readReply issues one registered read of main.go in JSON and classifies it.
// The only refusals it accepts are the typed retriable projection refusals;
// they show no rows, so they can never be silent stale.
func (f *readBuiltRetirementFixture) readReply(id int, tool string) readBuiltReply {
	reply := readBuiltReply{tool: tool}
	args := map[string]any{"format": "json", "max_bytes": 0}
	switch tool {
	case "get_file_summary":
		args["path"] = "main.go"
	case "get_callers":
		args["id"] = "main.go::Target"
	case "get_call_chain":
		args["id"] = "main.go::Changed"
		args["depth"] = 2
	}
	result, err := externalVersionRegisteredCall(f.s, f.ctx, id, tool, args)
	if err != nil {
		reply.err = err
		return reply
	}
	if result.IsError {
		reply.refusal = resultText(result)
		if !strings.Contains(reply.refusal, projectionSuperseded.Error()) && !strings.Contains(reply.refusal, errProjectedSourceMoved.Error()) {
			reply.err = fmt.Errorf("%s: unexpected refusal: %s", tool, reply.refusal)
		}
		return reply
	}
	body, ok := singleTextContent(result)
	if !ok {
		reply.err = fmt.Errorf("%s: reply has no single text body", tool)
		return reply
	}
	reply.body = body[:min(len(body), 2048)]
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		reply.err = fmt.Errorf("%s: %w", tool, err)
		return reply
	}
	fields := payload
	if rider, ok := payload["freshness"].(map[string]any); ok && rider["graph_freshness"] != nil {
		fields = rider
	}
	reply.state, _ = fields["graph_freshness"].(string)
	reply.reason, _ = fields["reason"].(string)
	if reply.state != "" && (fields["freshness_scope"] != "relations" || !slices.Equal(stringValues(fields["pending_files"]), []string{"main.go"})) {
		reply.err = fmt.Errorf("%s: relation state %q with scope %v for files %v", tool, reply.state, fields["freshness_scope"], fields["pending_files"])
		return reply
	}
	reply.layout = readBuiltLayout(tool, payload)
	return reply
}

func stringValues(value any) []string {
	values, _ := value.([]any)
	out := make([]string, 0, len(values))
	for _, v := range values {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// readBuiltLayout names which version of main.go a reply's rows describe.
// Old: Changed on line 5 alone, calling nothing. New: Changed on lines 5-8
// calling Target, and Added on line 10.
func readBuiltLayout(tool string, payload map[string]any) string {
	nodes := map[string]map[string]any{}
	values, _ := payload["nodes"].([]any)
	for _, value := range values {
		if node, ok := value.(map[string]any); ok {
			if id, _ := node["id"].(string); id != "" {
				nodes[id] = node
			}
		}
	}
	calls := 0
	edges, _ := payload["edges"].([]any)
	for _, value := range edges {
		if edge, ok := value.(map[string]any); ok && edge["kind"] == "calls" && edge["from"] == "main.go::Changed" && edge["to"] == "main.go::Target" {
			calls++
		}
	}
	switch tool {
	case "get_file_summary":
		changed, added := nodes["main.go::Changed"], nodes["main.go::Added"]
		if changed == nil {
			return fmt.Sprintf("no Changed row in %d nodes", len(nodes))
		}
		switch {
		case changed["end_line"] == float64(5) && added == nil:
			return "old"
		case changed["end_line"] == float64(8) && added != nil && added["start_line"] == float64(10):
			return "new"
		}
		return fmt.Sprintf("Changed ends on %v, Added %v", changed["end_line"], added != nil)
	default:
		switch calls {
		case 0:
			return "old"
		case 1:
			return "new"
		}
		return fmt.Sprintf("%d Changed -> Target edges", calls)
	}
}
