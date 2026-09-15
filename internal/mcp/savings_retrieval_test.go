package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/savings"
	"github.com/zzet/gortex/internal/search"
)

// newRetrievalSavingsServer indexes a repo of several non-trivial Go files.
// The read-family fixtures elsewhere in this package use a single 30-byte
// file, which cannot exercise a file-SET baseline: the point of these tests is
// that one retrieval page stands in for several whole files, so the fixture
// has to have several.
func newRetrievalSavingsServer(t *testing.T, files int) (*Server, *savings.Store, string) {
	t.Helper()
	// These tests drive the facade names an agent actually calls, so the
	// session must be on the facade-v1 surface rather than the legacy default.
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	dir := filepath.Join(t.TempDir(), "myrepo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for i := range files {
		var b strings.Builder
		fmt.Fprintf(&b, "package myrepo\n\nimport \"strings\"\n\n")
		for j := range 12 {
			fmt.Fprintf(&b, `
// Widget%d%d walks the shared registry and normalises every entry it is
// handed, returning the joined form plus whether anything matched at all.
func Widget%d%d(key string, depth int) (string, bool) {
	if key == "" {
		return "", false
	}
	parts := make([]string, 0, depth)
	for i := 0; i < depth; i++ {
		parts = append(parts, strings.ToLower(strings.TrimSpace(key)))
	}
	return strings.Join(parts, "/"), len(parts) > 0
}
`, i, j, i, j)
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("mod%d.go", i)), []byte(b.String()), 0o644))
	}

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}}}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
	store, err := savings.Open("")
	require.NoError(t, err)
	srv.InitSavings(store, "")
	return srv, store, dir
}

func ledgerByTool(t *testing.T, store *savings.Store) map[string]int64 {
	t.Helper()
	totals, err := store.ToolTotals(time.Time{})
	require.NoError(t, err)
	out := make(map[string]int64, len(totals))
	for _, row := range totals {
		out[row.Tool] = row.TokensSaved
	}
	return out
}

// The user-visible bug: an agent that navigates entirely through the facade —
// search, relations, trace — saw a flat ledger, because none of those
// operations reached a recording site. Each must now book its file-set
// baseline under its LEGACY tool name, so the facade and the legacy surface
// land in the same per-tool bucket.
func TestFacadeRetrievalOperationsRecordSavings(t *testing.T) {
	srv, store, _ := newRetrievalSavingsServer(t, 4)
	ctx := context.Background()

	res := callToolByName(t, srv, ctx, "search", map[string]any{
		"operation": "symbols", "query": "Widget",
	})
	require.False(t, res.IsError, "search.symbols must succeed: %s", textOfResult(t, res))

	byTool := ledgerByTool(t, store)
	require.Contains(t, byTool, "search_symbols",
		"search.symbols must book under its legacy tool name, got %v", byTool)
	require.Greater(t, byTool["search_symbols"], int64(0),
		"a search page citing whole files it stands in for must book real savings")
}

// relations.usages is documented as the replacement for grepping a symbol's
// call sites. It cites files the caller would otherwise have opened, so it
// carries the same baseline as any other retrieval page.
func TestFacadeRelationsRecordsSavings(t *testing.T) {
	srv, store, _ := newRetrievalSavingsServer(t, 3)
	ctx := context.Background()

	res := callToolByName(t, srv, ctx, "relations", map[string]any{
		"operation": "usages", "target": map[string]any{"symbol": "myrepo/mod0.go::Widget00"},
	})
	require.False(t, res.IsError, "relations.usages must succeed: %s", textOfResult(t, res))
	require.Greater(t, ledgerByTool(t, store)["find_usages"], int64(0))
}

// A file's whole-file baseline is the cost of reading it ONCE. An agent that
// searches the same corner of the repo twice did not avoid reading those files
// twice, so the second page must book nothing — otherwise a polling or
// re-querying client mints savings out of nothing, the same failure the
// not-modified guards exist to prevent on the read-family tools.
func TestRetrievalSavingsCreditsEachFileOncePerSession(t *testing.T) {
	srv, store, _ := newRetrievalSavingsServer(t, 3)
	ctx := WithSessionID(context.Background(), "session-repeat")

	first := callToolByName(t, srv, ctx, "search", map[string]any{"operation": "symbols", "query": "Widget"})
	require.False(t, first.IsError)
	afterFirst := ledgerByTool(t, store)["search_symbols"]
	require.Greater(t, afterFirst, int64(0), "the first page must book the files it surfaced")

	second := callToolByName(t, srv, ctx, "search", map[string]any{"operation": "symbols", "query": "Widget"})
	require.False(t, second.IsError)
	require.Equal(t, afterFirst, ledgerByTool(t, store)["search_symbols"],
		"a repeat page over already-surfaced files must add no savings")
}

// The two halves of the ledger must not bill the same file twice. A read-family
// call already charges the whole file as its counterfactual; a later retrieval
// page that merely cites that file has displaced nothing further.
func TestReadFamilyBaselineBlocksLaterRetrievalCredit(t *testing.T) {
	srv, store, _ := newRetrievalSavingsServer(t, 1)
	ctx := WithSessionID(context.Background(), "session-shared")

	read := callToolByName(t, srv, ctx, "read", map[string]any{
		"operation": "file", "target": map[string]any{"file": "mod0.go"},
	})
	require.False(t, read.IsError, "read.file must succeed: %s", textOfResult(t, read))
	require.Contains(t, ledgerByTool(t, store), "read_file")

	found := callToolByName(t, srv, ctx, "search", map[string]any{"operation": "symbols", "query": "Widget"})
	require.False(t, found.IsError)
	require.NotContains(t, ledgerByTool(t, store), "search_symbols",
		"the only cited file was already billed by read_file; the search must add nothing")
}

// Sessions are the unit of the credited-file set: two agents working the same
// repo have each genuinely avoided their own reads, so the second session must
// be credited even though the first already surfaced the same files.
func TestRetrievalSavingsCreditIsPerSession(t *testing.T) {
	srv, store, _ := newRetrievalSavingsServer(t, 2)
	args := map[string]any{"operation": "symbols", "query": "Widget"}

	first := callToolByName(t, srv, WithSessionID(context.Background(), "A"), "search", args)
	require.False(t, first.IsError)
	afterA := ledgerByTool(t, store)["search_symbols"]
	require.Greater(t, afterA, int64(0))

	second := callToolByName(t, srv, WithSessionID(context.Background(), "B"), "search", args)
	require.False(t, second.IsError)
	require.Greater(t, ledgerByTool(t, store)["search_symbols"], afterA,
		"a second session avoided its own reads and must be credited for them")
}

// The per-call cap is the guard against a wide page minting a baseline nobody
// would have paid: a 50-hit usages page does not mean the caller would have
// opened 50 files. Credit stops at retrievalBaselineMaxFiles distinct files.
func TestRetrievalBaselineCapsFilesPerCall(t *testing.T) {
	files := retrievalBaselineMaxFiles + 6
	srv, _, dir := newRetrievalSavingsServer(t, files)
	ctx := WithSessionID(context.Background(), "session-cap")

	// Graph paths are '/'-joined on every platform (the indexer folds every
	// key through filepath.ToSlash), and so is every citation lifted off the
	// wire — filepath.Join here would hand resolveGraphPath `myrepo\mod0.go`,
	// which matches no repo prefix on Windows and credits nothing.
	cited := make([]string, 0, files)
	for i := range files {
		cited = append(cited, fmt.Sprintf("myrepo/mod%d.go", i))
	}
	srv.recordFileSetBaselineSavings(ctx, "search_symbols", cited, "a retrieval page")

	credited := 0
	for i := range files {
		abs := filepath.Join(dir, fmt.Sprintf("mod%d.go", i))
		// creditFile returns false for a file already claimed by the call above.
		if !srv.tokenStatsFor(ctx).creditFile(abs) {
			credited++
		}
	}
	require.Equal(t, retrievalBaselineMaxFiles, credited,
		"exactly the cap may be credited, however many files the page cites")
}

// A tool outside the retrieval allow-list must never book, however many files
// its output happens to name — the allow-list is the judgement about which
// responses actually displace a file read.
func TestRetrievalSavingsIgnoresNonRetrievalTools(t *testing.T) {
	srv, store, _ := newRetrievalSavingsServer(t, 2)
	ctx := context.Background()

	res := callToolByName(t, srv, ctx, "workspace", map[string]any{"operation": "repos"})
	require.False(t, res.IsError, "workspace.repos must succeed: %s", textOfResult(t, res))
	require.Empty(t, ledgerByTool(t, store),
		"a workspace listing displaces no file read and must book nothing")
}

// citedFilesFromResult is the one place a response is turned back into the set
// of files it stands in for, across all three wire formats Gortex emits. A
// format it cannot read books nothing, which is why every format matters.
func TestCitedFilesFromResult_WireFormats(t *testing.T) {
	t.Run("gcx1 locates path columns by name across blocks", func(t *testing.T) {
		payload := strings.Join([]string{
			"GCX1 tool=get_call_chain.nodes fields=id,kind,name,path,path_abs,line total=2",
			"repo/a.go::F\tfunction\tF\trepo/a.go\t/abs/repo/a.go\t6",
			"repo/b.go::G\tfunction\tG\trepo/b.go\t/abs/repo/b.go\t9",
			"GCX1 tool=get_call_chain.edges fields=from,to,kind,line,file_path count=1",
			"repo/a.go::F\trepo/b.go::G\tcalls\t12\trepo/c.go",
		}, "\n")
		require.Equal(t, []string{"repo/a.go", "repo/b.go", "repo/c.go"}, citedFilesFromResult(payload))
	})

	t.Run("gcx1 comment and blank lines are not rows", func(t *testing.T) {
		payload := "GCX1 tool=search_symbols fields=id,path total=1\n# 1 result(s)\n\nrepo/a.go::F\trepo/a.go"
		require.Equal(t, []string{"repo/a.go"}, citedFilesFromResult(payload))
	})

	t.Run("prose riders after a block are not rows", func(t *testing.T) {
		payload := strings.Join([]string{
			"GCX1 tool=search_symbols fields=path,kind,name total=1",
			"repo/a.go\tfunction\tF",
			"(Session note: graph read #6. Every location returned so far is real and citeable.)",
		}, "\n")
		require.Equal(t, []string{"repo/a.go"}, citedFilesFromResult(payload))
	})

	t.Run("json object and array", func(t *testing.T) {
		require.Equal(t, []string{"repo/a.go", "repo/b.go"},
			citedFilesFromResult(`{"hits":[{"path":"repo/a.go"},{"file":"repo/b.go"}]}`))
		require.Equal(t, []string{"repo/a.go"},
			citedFilesFromResult(`[{"file_path":"repo/a.go"}]`))
	})

	t.Run("toon", func(t *testing.T) {
		payload := "count: 2\nmatches[2]:\n  - line: 3\n    path: repo/a.go\n  - line: 7\n    path: repo/b.go\nquery: x"
		require.Equal(t, []string{"repo/a.go", "repo/b.go"}, citedFilesFromResult(payload))
	})

	t.Run("absolute paths are not citations", func(t *testing.T) {
		payload := "GCX1 tool=search_symbols fields=id,path,path_abs total=1\nx\t/abs/only.go\t/abs/only.go"
		require.Empty(t, citedFilesFromResult(payload))
	})

	t.Run("unparseable payloads book nothing", func(t *testing.T) {
		require.Empty(t, citedFilesFromResult("some prose with no structure at all"))
		require.Empty(t, citedFilesFromResult(""))
	})
}

// creditFile is the single choke point that stops a file being billed twice.
func TestTokenStatsCreditFile(t *testing.T) {
	ts := &tokenStats{}
	require.True(t, ts.creditFile("/repo/a.go"), "first claim wins")
	require.False(t, ts.creditFile("/repo/a.go"), "second claim on the same file is refused")
	require.True(t, ts.creditFile("/repo/b.go"), "a different file is independent")
	require.False(t, ts.creditFile(""), "an empty path is never creditable")

	full := &tokenStats{creditedFiles: make(map[string]struct{}, maxCreditedFiles)}
	for i := range maxCreditedFiles {
		full.creditedFiles[fmt.Sprintf("/f/%d.go", i)] = struct{}{}
	}
	require.False(t, full.creditFile("/f/overflow.go"), "a saturated set stops crediting rather than growing")
}

// The production entrypoint, end to end: a read-only tool call arriving over
// the MCP surface must not cost a durable sidecar transaction just to book
// its own accounting. Before this, server.record() -> savings.AddObservation
// -> SidecarStore.AddSavingsObservation opened, wrote and committed one
// transaction (~37 KB of sidecar WAL) per call, which made an idle agent
// session the dominant writer on the machine. The calls must now buffer, and
// the shutdown path (Server.FlushSavings, registered in the serverstack
// teardown chain) must be what persists them — in ONE transaction.
func TestReadOnlyToolCallsCoalesceIntoOneLedgerTransaction(t *testing.T) {
	srv, _, _ := newRetrievalSavingsServer(t, 4)

	// A sidecar-backed ledger: the in-memory store the fixture wires has no
	// transactions to count.
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	store, err := savings.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	srv.InitSavings(store, "")

	// OpenSidecar returns the process-cached handle for this path, i.e. the
	// very store the ledger writes through.
	sc, err := persistence.OpenSidecar(path)
	require.NoError(t, err)
	before := sc.SavingsCommitCount()

	const calls = 6
	for i := range calls {
		ctx := WithSessionID(context.Background(), fmt.Sprintf("agent-%d", i))
		res := callToolByName(t, srv, ctx, "search", map[string]any{
			"operation": "symbols", "query": "Widget",
		})
		require.False(t, res.IsError, "search.symbols must succeed: %s", textOfResult(t, res))
	}

	require.Equal(t, int64(0), sc.SavingsCommitCount()-before,
		"%d read-only tool calls must open no sidecar transaction", calls)
	pending := store.Pending()
	require.GreaterOrEqual(t, pending, calls,
		"every recorded call must be buffered, not dropped")

	require.NoError(t, srv.FlushSavings())
	require.Equal(t, int64(1), sc.SavingsCommitCount()-before,
		"the whole window must commit as exactly one transaction")
	require.Equal(t, 0, store.Pending())

	snap, err := store.Snapshot()
	require.NoError(t, err)
	require.Equal(t, int64(pending), snap.Totals.CallsCounted,
		"the flushed totals must account for every buffered observation")
	require.Greater(t, ledgerByTool(t, store)["search_symbols"], int64(0),
		"the per-tool breakdown must survive coalescing")
}

// Exactness through the reader the dashboard uses: graph_stats renders
// cumulativeSavingsSnapshot, which reads the ledger through the same store.
// Buffering must be invisible to it — no flush call in between.
func TestCumulativeSavingsSnapshotIsExactWithoutAnExplicitFlush(t *testing.T) {
	srv, _, _ := newRetrievalSavingsServer(t, 3)
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	store, err := savings.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	srv.InitSavings(store, "")

	ctx := WithSessionID(context.Background(), "agent-stats")
	res := callToolByName(t, srv, ctx, "search", map[string]any{"operation": "symbols", "query": "Widget"})
	require.False(t, res.IsError, "search.symbols must succeed: %s", textOfResult(t, res))
	require.Greater(t, store.Pending(), 0, "precondition: the observation is still buffered")

	out := srv.cumulativeSavingsSnapshot()
	require.NotNil(t, out)
	require.Greater(t, out["calls_counted"].(int64), int64(0),
		"graph_stats must see buffered observations — a read flushes first")
	require.Greater(t, out["tokens_saved"].(int64), int64(0))
}

// --- the flush window is settable from the entry point --------------------

// The daemon's minute-scale coalescing is wrong for the one-shot stdio
// server, whose host SIGKILLs it. Server.SetSavingsFlushBounds is the seam
// `gortex mcp` uses to tighten it; it has to reach the ledger, and the
// tightened count bound has to govern the real recording path, not just the
// stored field.
func TestSetSavingsFlushBoundsReachesTheLedgerAndBoundsRealCalls(t *testing.T) {
	srv, _, _ := newRetrievalSavingsServer(t, 3)
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	store, err := savings.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	srv.InitSavings(store, "")

	every, max := store.FlushBounds()
	require.Equal(t, savings.DefaultFlushInterval, every, "precondition: the daemon default")
	require.Equal(t, savings.DefaultFlushMax, max)

	srv.SetSavingsFlushBounds(savings.OneshotFlushInterval, 2)
	every, max = store.FlushBounds()
	require.Equal(t, savings.OneshotFlushInterval, every,
		"the entry point's interval must reach the ledger")
	require.Equal(t, 2, max)
	require.Less(t, savings.OneshotFlushInterval, savings.DefaultFlushInterval,
		"the one-shot window must be shorter than the daemon's")

	sc, err := persistence.OpenSidecar(path)
	require.NoError(t, err)
	before := sc.SavingsCommitCount()

	for i := range 3 {
		ctx := WithSessionID(context.Background(), fmt.Sprintf("oneshot-%d", i))
		res := callToolByName(t, srv, ctx, "search", map[string]any{"operation": "symbols", "query": "Widget"})
		require.False(t, res.IsError, "search.symbols must succeed: %s", textOfResult(t, res))
	}
	require.GreaterOrEqual(t, sc.SavingsCommitCount()-before, int64(1),
		"the tightened count bound must commit inside the session, not only at shutdown")
	require.LessOrEqual(t, store.Pending(), 2,
		"no more than the count bound may stay exposed to a SIGKILL")

	require.NoError(t, srv.FlushSavings())
	snap, err := store.Snapshot()
	require.NoError(t, err)
	require.Equal(t, int64(3), snap.Totals.CallsCounted,
		"every call must be accounted for across the bounded windows")
	require.Zero(t, snap.DroppedObservations)
}

// Deferrable blind: the entry point calls this before it knows whether the
// stack wired a sidecar-backed ledger at all. An in-memory ledger (the
// fixture's, and every embedded/eval server's) buffers nothing and has no
// bounds to set, so both calls must be quiet no-ops rather than panics.
func TestSetSavingsFlushBoundsOnAnInMemoryLedgerIsANoOp(t *testing.T) {
	srv, store, _ := newRetrievalSavingsServer(t, 1)
	every, max := store.FlushBounds()
	require.Zero(t, every, "precondition: an in-memory ledger has no flush window")
	require.Zero(t, max)

	srv.SetSavingsFlushBounds(savings.OneshotFlushInterval, savings.OneshotFlushMax)
	every, max = store.FlushBounds()
	require.Zero(t, every, "an in-memory ledger must not acquire a window")
	require.Zero(t, max)
	require.NoError(t, srv.FlushSavings())
}
