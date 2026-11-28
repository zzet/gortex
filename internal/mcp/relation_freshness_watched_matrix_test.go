package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	toon "github.com/toon-format/toon-go"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// watchedMatrixHold names the real publication step a fixture pauses in.
type watchedMatrixHold int

const (
	// Before the watcher evicts the file's old rows: the pre-change graph
	// stays queryable while the file on disk already changed.
	holdBeforeEviction watchedMatrixHold = iota
	// After the file's new rows and mtime are written, before the resolver
	// binds its calls: a new caller edge is still unresolved.
	holdAfterApply
)

type watchedMatrixStore struct {
	*store_sqlite.Store
	hold    watchedMatrixHold
	path    string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *watchedMatrixStore) pause() {
	close(s.entered)
	<-s.release
}

func (s *watchedMatrixStore) EvictFiles(paths []string) (int, int) {
	if s.hold == holdBeforeEviction && slices.Contains(paths, s.path) && s.armed.CompareAndSwap(true, false) {
		s.pause()
	}
	return s.Store.EvictFiles(paths)
}

func (s *watchedMatrixStore) BulkSetFileMtimes(repo string, mtimes map[string]int64) error {
	if err := s.Store.BulkSetFileMtimes(repo, mtimes); err != nil {
		return err
	}
	if _, ok := mtimes[s.path]; ok && s.hold == holdAfterApply && s.armed.CompareAndSwap(true, false) {
		s.pause()
	}
	return nil
}

// watchedMatrixFixture is one indexed repository with a live watcher whose
// publication of a single file can be held, and one registered client
// session on either the facade surface or the full legacy tool surface.
type watchedMatrixFixture struct {
	s         *Server
	store     *watchedMatrixStore
	watcher   *indexer.Watcher
	relations *relationGenerationWitness
	ctx       context.Context
	path      string
	file      string
	facade    bool
	unblock   func()
}

func newWatchedMatrixFixture(t *testing.T, files map[string]string, file string, hold watchedMatrixHold, facade bool) *watchedMatrixFixture {
	t.Helper()
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_QUERY_LOG", filepath.Join(t.TempDir(), "query-log.jsonl"))
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
	}
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "watched-matrix.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	store := &watchedMatrixStore{Store: sql, hold: hold, path: file, entered: make(chan struct{}), release: make(chan struct{})}
	f := &watchedMatrixFixture{store: store, path: filepath.Join(root, file), file: file, facade: facade,
		unblock: sync.OnceFunc(func() { close(store.release) })}
	logger := zap.NewNop()
	idx := indexer.New(store, testRegistry(), config.Default().Index, logger)
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	f.s = NewServer(query.NewEngine(store), store, idx, nil, logger, nil)
	t.Cleanup(func() { f.s.DrainBackground(); f.s.queryLog.Close() })
	f.s.editAckMode = config.EditAckLocal
	f.watcher, err = indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, logger)
	require.NoError(t, err)
	require.NoError(t, f.watcher.Start([]string{root}))
	t.Cleanup(func() { f.unblock(); require.NoError(t, f.watcher.Stop()) })
	f.relations = newRelationGenerationWitness(f.path)
	f.s.SetWatcher(&sharedReceiptWatcher{Watcher: f.watcher, relations: f.relations})

	session := "watched-matrix-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	require.NoError(t, f.s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { f.s.DisconnectSession(session) })
	if facade {
		f.s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	} else {
		f.s.NoteSessionToolPolicy(session, "all", "defer")
	}
	ctx, cancel := context.WithTimeout(f.s.WithClientSession(WithSessionID(t.Context(), session), session), 15*time.Second)
	t.Cleanup(cancel)
	f.ctx = ctx
	require.NotNil(t, f.s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"watched-matrix","version":"1"}}}`)))
	return f
}

// edit applies one local-ACK tool edit and returns the edit's own watcher
// ticket and publication receipt. The facade session writes through the
// facade edit tool; the legacy session through edit_file.
func (f *watchedMatrixFixture) edit(t *testing.T, id int, args map[string]any) (*indexer.MutationTicket, *mutationReceipt) {
	t.Helper()
	tool := "edit_file"
	if f.facade {
		tool = "edit"
	}
	offer := &sharedReceiptOffer{ticket: make(chan *indexer.MutationTicket, 1)}
	editCtx := context.WithValue(f.ctx, sharedReceiptOfferKey{}, offer)
	f.store.armed.Store(true)
	ack, err := sharedReceiptJSON(f.s, editCtx, id, tool, args)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(ack["mutation_receipt"], &offer.receipt))
	require.NotEmpty(t, offer.receipt)
	require.JSONEq(t, `"local"`, string(ack["ack_mode"]))
	return sharedReceiptFreshContractPublication(t, editCtx, &sharedReceiptFixture{server: f.s}, offer)
}

func (f *watchedMatrixFixture) awaitHold(t *testing.T) {
	t.Helper()
	select {
	case <-f.store.entered:
	case <-f.ctx.Done():
		t.Fatal("watched publication did not reach its real pause")
	}
}

// release lets the held publication finish and waits for its receipt.
func (f *watchedMatrixFixture) release(t *testing.T, receipt *mutationReceipt) {
	t.Helper()
	f.unblock()
	if receipt != nil {
		select {
		case <-receipt.done:
		case <-f.ctx.Done():
			t.Fatal("original watched receipt did not finish after release")
		}
	}
	f.awaitNoGeneration(t)
}

func (f *watchedMatrixFixture) awaitNoGeneration(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return f.watcher.TierState(f.path).Generation == 0 },
		5*time.Second, 5*time.Millisecond, "the watcher kept an admitted generation for the file")
}

// recovered reads once the watcher has no admitted generation left for the
// file, until a reply carries no relation state. read scores the reply's edge
// set itself and returns its relation state. A read just after publication can
// still see a new generation for the same bytes: the edit's projection is
// retired only after its receipt completes, so the read can re-verify it and
// admit a catch-up, and FSEvents can deliver the edit's own native event after
// the edit's ticket has finished. The reply then truthfully reports that
// generation as pending. A reply may therefore carry a relation state only
// when the server's own freshness snapshots for that read saw a generation
// admitted during it, and the next reply after that generation publishes is
// scored again.
func (f *watchedMatrixFixture) recovered(t *testing.T, read func(id int) string) {
	t.Helper()
	for attempt := range 3 {
		f.awaitNoGeneration(t)
		admitted := f.relations.generation()
		state := read(10 + attempt)
		if state == "" {
			return
		}
		require.Greater(t, f.relations.generation(), admitted,
			"a reply after publication kept a relation state no generation admitted during its read explains: %s", state)
	}
	t.Fatal("replies after publication kept admitting catch-up generations")
}

// call issues one registered read in the session and decodes its body in the
// requested format. state is the TOON label line, or the JSON
// graph_freshness value; fields holds the JSON freshness fields. While a
// publication is held, the read can return the typed retriable projection
// refusal instead of data; that refusal shows no relation rows, so it cannot
// be silent stale. A held read retries it until the reply carries data, so
// every held phase still scores a real edge set. Any other refusal, and any
// refusal outside the held phase, fails the test.
func (f *watchedMatrixFixture) call(t *testing.T, id int, tool string, args map[string]any, asTOON, held bool) (payload map[string]any, state string, fields map[string]any) {
	t.Helper()
	var result *mcplib.CallToolResult
	for attempt := 1; ; attempt++ {
		var err error
		result, err = externalVersionRegisteredCall(f.s, f.ctx, id, tool, args)
		require.NoError(t, err)
		if !result.IsError {
			break
		}
		text := resultText(result)
		require.True(t, held, "only a held read may refuse: %s", text)
		require.Contains(t, text, projectionSuperseded.Error(), "unexpected refusal")
		if label, _, ok := strings.Cut(text, "\n"); ok && strings.HasPrefix(label, "relations: ") {
			require.Regexp(t, `^relations: pending \([0-9]+ files\)$`, label)
		}
		require.Less(t, attempt, 50, "held read kept refusing")
		time.Sleep(10 * time.Millisecond)
	}
	if asTOON {
		require.Nil(t, result.StructuredContent, "only the requested TOON text is evidence")
		body, ok := singleTextContent(result)
		require.True(t, ok)
		if strings.HasPrefix(body, "relations: ") {
			state, body, ok = strings.Cut(body, "\n")
			require.True(t, ok)
		}
		require.False(t, json.Valid([]byte(body)), "requested TOON returned JSON")
		decoded, err := toon.Decode([]byte(body), toon.WithStrictMode(true))
		require.NoError(t, err)
		payload, ok = decoded.(map[string]any)
		require.True(t, ok)
	} else {
		body, ok := singleTextContent(result)
		require.True(t, ok)
		require.NoError(t, json.Unmarshal([]byte(body), &payload))
	}
	require.Equal(t, false, payload["truncated"])
	rider, _ := payload["freshness"].(map[string]any)
	fields = rider
	if fields["graph_freshness"] == nil {
		fields = payload
	}
	if !asTOON {
		state, _ = fields["graph_freshness"].(string)
	}
	watchedMatrixNoFreshClaim(t, result, payload, rider, state)
	return payload, state, fields
}

// watchedMatrixNoFreshClaim checks that a reply disclosing pending relations
// does not also claim fresh data in its body, rider or metadata.
func watchedMatrixNoFreshClaim(t *testing.T, result *mcplib.CallToolResult, payload, rider map[string]any, state string) {
	t.Helper()
	if state == "" {
		return
	}
	var meta map[string]any
	if result.Meta != nil {
		meta, _ = result.Meta.AdditionalFields["freshness"].(map[string]any)
	}
	for _, claims := range []map[string]any{payload, rider, meta} {
		require.NotEqual(t, "fresh", claims["graph_freshness"])
		require.NotEqual(t, true, claims["fresh"])
	}
}

// requirePending checks the in-band pending disclosure for the held file.
func (f *watchedMatrixFixture) requirePending(t *testing.T, asTOON bool, state string, fields map[string]any) {
	t.Helper()
	if asTOON {
		require.Equal(t, "relations: pending (1 files)", state)
		return
	}
	require.Equal(t, "pending", state)
	require.Equal(t, "relations", fields["freshness_scope"])
	require.Equal(t, []any{f.file}, fields["pending_files"])
}

// TestWatchedRelationFreshnessMatrix holds a real watcher publication for one
// changed file and reads the callers of a fixed target through a registered
// machine output while the relations are stale. A held reply must return one
// of the two real edge sets with pending relations disclosed in band and no
// fresh claim. After publication every reply must hold the exact edge set,
// and once no generation is pending for the file it carries no relation state.
func TestWatchedRelationFreshnessMatrix(t *testing.T) {
	const (
		target    = "main.go::Target"
		control   = "main.go::Control"
		mainFile  = "package main\nfunc Target() {}\nfunc Control() { Target() }\n"
		withCall  = "func Changed() int { Target(); return 1 }"
		withoutIt = "func Changed() int { return 2 }"
	)
	crossFile := map[string]string{
		"main.go":   "package main\nfunc Target() {}\nfunc Other() {}\nfunc Control() { Target() }\n",
		"caller.go": "package main\nfunc Changed() { Other() }\n",
	}
	cases := []struct {
		name   string
		toon   bool // facade relations in TOON; otherwise get_callers in JSON
		files  map[string]string
		file   string // the changed file
		hold   watchedMatrixHold
		old    string // empty: the file is deleted out of band
		new    string
		caller string
		// caller -> target edges before the change and after publication
		before, published int
	}{
		{
			name: "intra_file_call/json", files: map[string]string{"main.go": mainFile + withoutIt + "\n"},
			file: "main.go", hold: holdBeforeEviction, old: withoutIt, new: withCall,
			caller: "main.go::Changed", before: 0, published: 1,
		},
		{
			name: "intra_file_call/facade_toon", toon: true, files: map[string]string{"main.go": mainFile + withoutIt + "\n"},
			file: "main.go", hold: holdBeforeEviction, old: withoutIt, new: withCall,
			caller: "main.go::Changed", before: 0, published: 1,
		},
		{
			name: "caller_body_change/json", files: map[string]string{"main.go": mainFile + withCall + "\n"},
			file: "main.go", hold: holdBeforeEviction, old: withCall, new: withoutIt,
			caller: "main.go::Changed", before: 1, published: 0,
		},
		{
			name: "caller_body_change/facade_toon", toon: true, files: map[string]string{"main.go": mainFile + withCall + "\n"},
			file: "main.go", hold: holdBeforeEviction, old: withCall, new: withoutIt,
			caller: "main.go::Changed", before: 1, published: 0,
		},
		{
			name: "cross_file_call/json", files: crossFile,
			file: "caller.go", hold: holdAfterApply, old: "func Changed() { Other() }", new: "func Changed() { Target() }",
			caller: "caller.go::Changed", before: 0, published: 1,
		},
		{
			name: "cross_file_call/facade_toon", toon: true, files: crossFile,
			file: "caller.go", hold: holdAfterApply, old: "func Changed() { Other() }", new: "func Changed() { Target() }",
			caller: "caller.go::Changed", before: 0, published: 1,
		},
		{
			name: "out_of_band_delete/facade_toon", toon: true, files: map[string]string{
				"main.go": mainFile,
				"gone.go": "package main\nfunc Gone() { Target() }\n",
			},
			file: "gone.go", hold: holdBeforeEviction,
			caller: "gone.go::Gone", before: 1, published: 0,
		},
	}
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			f := newWatchedMatrixFixture(t, row.files, row.file, row.hold, row.toon)
			stored := func() int {
				n := 0
				for _, edge := range f.store.GetInEdges(target) {
					if edge.From == row.caller && edge.Kind == graph.EdgeCalls {
						n++
					}
				}
				return n
			}
			probe := func(id int, phase string) string {
				t.Helper()
				tool, args := "get_callers", map[string]any{"id": target, "format": "json", "max_bytes": 0}
				if row.toon {
					tool, args = "relations", map[string]any{
						"operation": "callers",
						"target":    map[string]any{"symbol": target},
						"output":    map[string]any{"format": "toon", "max_bytes": 0},
					}
				}
				payload, state, fields := f.call(t, id, tool, args, row.toon, phase == "held")
				changed, controls := 0, 0
				edges, _ := payload["edges"].([]any)
				for _, value := range edges {
					edge, ok := value.(map[string]any)
					require.True(t, ok)
					if edge["to"] != target || edge["kind"] != "calls" {
						continue
					}
					switch edge["from"] {
					case row.caller:
						changed++
					case control:
						controls++
					}
				}
				require.Equal(t, 1, controls, "%s: the control caller must stay in the relation set", phase)
				switch phase {
				case "held":
					require.Contains(t, []int{row.before, row.published}, changed, "held reply invented an edge set")
					f.requirePending(t, row.toon, state, fields)
				case "before":
					require.Equal(t, row.before, changed)
					require.Empty(t, state)
				case "recovered":
					require.Equal(t, row.published, changed, "a reply after publication returned a stale edge set")
					if state != "" {
						f.requirePending(t, row.toon, state, fields)
					}
				}
				return state
			}

			require.Equal(t, row.before, stored())
			probe(2, "before")
			var ticket *indexer.MutationTicket
			var receipt *mutationReceipt
			if row.old == "" {
				f.store.armed.Store(true)
				require.NoError(t, os.Remove(f.path))
			} else {
				args := map[string]any{
					"path": row.file, "old_string": row.old, "new_string": row.new,
					"expected_occurrences": 1, "format": "json", "max_bytes": 0,
				}
				if row.toon {
					args = map[string]any{
						"operation": "file", "target": map[string]any{"file": row.file},
						"match": row.old, "replacement": row.new,
						"guard":  map[string]any{"expected_occurrences": 1},
						"output": map[string]any{"format": "json", "max_bytes": 0},
					}
				}
				ticket, receipt = f.edit(t, 3, args)
			}
			f.awaitHold(t)
			if row.old == "" {
				_, err := os.Stat(f.path)
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				bytes, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.Equal(t, strings.Replace(row.files[row.file], row.old, row.new, 1), string(bytes))
				require.True(t, ticket.TierState().RelationsAt.IsZero())
			}
			held := f.watcher.TierState(f.path)
			require.NotZero(t, held.Generation)
			require.True(t, held.RelationsAt.IsZero())
			require.Equal(t, row.before, stored())
			probe(4, "held")
			require.Equal(t, row.before, stored(), "publication must stay held through the reply")

			f.release(t, receipt)
			if ticket != nil {
				require.False(t, ticket.TierState().RelationsAt.IsZero())
			} else {
				require.Nil(t, f.store.GetNode(row.caller))
			}
			require.Equal(t, row.published, stored())
			f.recovered(t, func(id int) string { return probe(id, "recovered") })
		})
	}
}

// TestWatchedSignatureChangeFreshness changes a function's signature so the
// following function moves down two lines, and reads the call chain through
// the facade in JSON and TOON while the watcher publication is held before
// eviction. A held reply must disclose pending relations; one serving the old
// line layout without that disclosure would be silent stale. After
// publication both formats carry the new layout and, once no generation is
// pending, no relation state.
func TestWatchedSignatureChangeFreshness(t *testing.T) {
	const (
		target      = "main.go::Target"
		following   = "main.go::Following"
		example     = "main.go::Example"
		head        = "package main\n\nfunc Example() int { return 1 }\n\n"
		old         = "func Target() int { return Example() }"
		replacement = "func Target(optional ...int) int {\n return Example()\n}"
		tail        = "\n\nfunc Following() int { return Target() }\n"
	)
	f := newWatchedMatrixFixture(t, map[string]string{"main.go": head + old + tail}, "main.go", holdBeforeEviction, true)
	storedGap := func() int {
		t.Helper()
		first, second := f.store.GetNode(target), f.store.GetNode(following)
		require.NotNil(t, first)
		require.NotNil(t, second)
		return second.StartLine - first.StartLine
	}
	probe := func(id int, phase, format string) string {
		t.Helper()
		payload, state, fields := f.call(t, id, "trace", map[string]any{
			"operation": "call_chain", "target": map[string]any{"symbol": following},
			"options": map[string]any{"depth": 2, "limit": 1000},
			"output":  map[string]any{"format": format, "max_bytes": 0},
		}, format == "toon", phase == "held")
		nodes, ok := payload["nodes"].([]any)
		require.True(t, ok)
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
		edges, ok := payload["edges"].([]any)
		require.True(t, ok)
		for from, to := range map[string]string{following: target, target: example} {
			count := 0
			for _, value := range edges {
				edge, ok := value.(map[string]any)
				require.True(t, ok)
				if edge["kind"] == "calls" && edge["from"] == from && edge["to"] == to {
					count++
				}
			}
			require.Equal(t, 1, count, "%s -> %s", from, to)
		}
		switch phase {
		case "before":
			require.Equal(t, 2, gap)
			require.Empty(t, state)
		case "held":
			require.Contains(t, []int{2, 4}, gap, "held reply invented a line layout")
			f.requirePending(t, format == "toon", state, fields)
		case "recovered":
			require.Equal(t, 4, gap, "a reply after publication returned the old line layout")
			if state != "" {
				f.requirePending(t, format == "toon", state, fields)
			}
		}
		return state
	}

	require.Equal(t, 2, storedGap())
	probe(2, "before", "json")
	probe(3, "before", "toon")
	ticket, receipt := f.edit(t, 4, map[string]any{
		"operation":   "symbol",
		"target":      map[string]any{"symbol": target},
		"match":       old,
		"replacement": replacement,
		"guard":       map[string]any{"expected_occurrences": 1},
		"output":      map[string]any{"format": "json", "max_bytes": 0},
	})
	f.awaitHold(t)
	bytes, err := os.ReadFile(f.path)
	require.NoError(t, err)
	require.Equal(t, head+replacement+tail, string(bytes))
	require.True(t, ticket.TierState().RelationsAt.IsZero())
	require.Equal(t, 2, storedGap())
	probe(5, "held", "json")
	probe(6, "held", "toon")
	require.Equal(t, 2, storedGap(), "publication must stay held through the replies")
	require.True(t, ticket.TierState().RelationsAt.IsZero())

	f.release(t, receipt)
	require.False(t, ticket.TierState().RelationsAt.IsZero())
	require.Equal(t, 4, storedGap())
	for _, format := range []string{"json", "toon"} {
		f.recovered(t, func(id int) string { return probe(id, "recovered", format) })
	}
}
