package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

// importerReadCounter counts what check_references reads from a SQLite store.
// Embedding the store keeps every optional capability it implements visible,
// so the handler takes its production path. Whole-store reads are counted per
// call; every other counted read records the rows it returned, per method.
type importerReadCounter struct {
	*store_sqlite.Store
	tally *importerReadTally
}

type importerReadTally struct {
	mu    sync.Mutex
	scans map[string]int
	rows  map[string]int
}

func newImporterReadCounter(store *store_sqlite.Store) *importerReadCounter {
	return &importerReadCounter{Store: store, tally: &importerReadTally{scans: map[string]int{}, rows: map[string]int{}}}
}

func (r *importerReadCounter) scan(method string) {
	r.tally.mu.Lock()
	defer r.tally.mu.Unlock()
	r.tally.scans[method]++
}

func (r *importerReadCounter) read(method string, rows int) {
	r.tally.mu.Lock()
	defer r.tally.mu.Unlock()
	r.tally.rows[method] += rows
}

// counts returns the whole-store reads and the rows of every other counted
// read since the last reset, by method.
func (r *importerReadCounter) counts() (scans, rows map[string]int) {
	r.tally.mu.Lock()
	defer r.tally.mu.Unlock()
	return maps.Clone(r.tally.scans), maps.Clone(r.tally.rows)
}

func (r *importerReadCounter) reset() {
	r.tally.mu.Lock()
	defer r.tally.mu.Unlock()
	clear(r.tally.scans)
	clear(r.tally.rows)
}

// BindReadContext keeps a context-bound handle on the same tally; the store's
// own binding would return an uncounted handle.
func (r *importerReadCounter) BindReadContext(ctx context.Context) graph.Reader {
	return &importerReadCounter{Store: r.WithReadContext(ctx), tally: r.tally}
}

func (r *importerReadCounter) AllNodes() []*graph.Node {
	r.scan("AllNodes")
	return r.Store.AllNodes()
}

func (r *importerReadCounter) AllNodesLight() []*graph.Node {
	r.scan("AllNodesLight")
	return r.Store.AllNodesLight()
}

func (r *importerReadCounter) AllEdges() []*graph.Edge {
	r.scan("AllEdges")
	return r.Store.AllEdges()
}

func (r *importerReadCounter) AllEdgesLight(kinds ...graph.EdgeKind) []*graph.Edge {
	r.scan("AllEdgesLight")
	return r.Store.AllEdgesLight(kinds...)
}

func (r *importerReadCounter) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	r.scan("NodesByKind")
	return r.Store.NodesByKind(kind)
}

func (r *importerReadCounter) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	r.scan("EdgesByKind")
	return r.Store.EdgesByKind(kind)
}

func (r *importerReadCounter) EdgesByKinds(kinds []graph.EdgeKind) iter.Seq[*graph.Edge] {
	r.scan("EdgesByKinds")
	return r.Store.EdgesByKinds(kinds)
}

// FileImporters is the store's physical join. The request reader recomputes
// importers over its own filtered adjacency, so a call here is counted too.
func (r *importerReadCounter) FileImporters(path string) []graph.FileImporterRow {
	out := r.Store.FileImporters(path)
	r.read("FileImporters", len(out))
	return out
}

func (r *importerReadCounter) GetNode(id string) *graph.Node {
	n := r.Store.GetNode(id)
	r.read("GetNode", importerNodeRows(n))
	return n
}

func (r *importerReadCounter) GetNodeContext(ctx context.Context, id string) (*graph.Node, error) {
	n, err := r.Store.GetNodeContext(ctx, id)
	r.read("GetNodeContext", importerNodeRows(n))
	return n, err
}

func (r *importerReadCounter) GetNodesByIDs(ids []string) map[string]*graph.Node {
	out := r.Store.GetNodesByIDs(ids)
	r.read("GetNodesByIDs", len(out))
	return out
}

func (r *importerReadCounter) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	out, err := r.Store.GetNodesByIDsContext(ctx, ids)
	r.read("GetNodesByIDsContext", len(out))
	return out, err
}

func (r *importerReadCounter) GetNodeIDsByKindsContext(ctx context.Context, ids []string, kinds []graph.NodeKind) (map[string]struct{}, error) {
	out, err := r.Store.GetNodeIDsByKindsContext(ctx, ids, kinds)
	r.read("GetNodeIDsByKindsContext", len(out))
	return out, err
}

func (r *importerReadCounter) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	out, err := r.Store.GetNodeKindsByIDsContext(ctx, ids)
	r.read("GetNodeKindsByIDsContext", len(out))
	return out, err
}

func (r *importerReadCounter) GetFileNodes(path string) []*graph.Node {
	out := r.Store.GetFileNodes(path)
	r.read("GetFileNodes", len(out))
	return out
}

func (r *importerReadCounter) GetFileNodesContext(ctx context.Context, path string) []*graph.Node {
	out := r.Store.GetFileNodesContext(ctx, path)
	r.read("GetFileNodesContext", len(out))
	return out
}

func (r *importerReadCounter) GetInEdges(id string) []*graph.Edge {
	out := r.Store.GetInEdges(id)
	r.read("GetInEdges", len(out))
	return out
}

func (r *importerReadCounter) GetOutEdges(id string) []*graph.Edge {
	out := r.Store.GetOutEdges(id)
	r.read("GetOutEdges", len(out))
	return out
}

func (r *importerReadCounter) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	out := r.Store.GetInEdgesByNodeIDs(ids)
	rows := 0
	for _, edges := range out {
		rows += len(edges)
	}
	r.read("GetInEdgesByNodeIDs", rows)
	return out
}

func (r *importerReadCounter) FindNodesByName(name string) []*graph.Node {
	out := r.Store.FindNodesByName(name)
	r.read("FindNodesByName", len(out))
	return out
}

func (r *importerReadCounter) FindNodesByNameContext(ctx context.Context, name string) ([]*graph.Node, error) {
	out, err := r.Store.FindNodesByNameContext(ctx, name)
	r.read("FindNodesByNameContext", len(out))
	return out, err
}

func (r *importerReadCounter) OutlineNodesByName(scope graph.NodeScope, name string) ([]*graph.Node, error) {
	out, err := r.Store.OutlineNodesByName(scope, name)
	r.read("OutlineNodesByName", len(out))
	return out, err
}

func importerNodeRows(n *graph.Node) int {
	if n == nil {
		return 0
	}
	return 1
}

const (
	importerRowsWorkspace = "ws"
	importerRowsTarget    = "svc/service.go::Service.Target"
	importerRowsFile      = "svc/service.go"
)

// addImporterRowsRepo adds one repository whose service file is imported by
// file identity from two files and a test, and by a symbol of the file from
// a fourth. Every repository uses the same names and repository-relative
// paths, in one workspace.
func addImporterRowsRepo(t *testing.T, store *store_sqlite.Store, repo string) {
	t.Helper()
	path := func(local string) string { return repo + "/" + local }
	node := func(id, name string, kind graph.NodeKind, file string) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: kind, FilePath: file, RepoPrefix: repo, WorkspaceID: importerRowsWorkspace, Language: "go", StartLine: 1, EndLine: 9}
	}
	nodes := []*graph.Node{
		node(path(importerRowsFile), "service.go", graph.KindFile, path(importerRowsFile)),
		node(path("svc/service.go::Service"), "Service", graph.KindType, path(importerRowsFile)),
		node(path(importerRowsTarget), "Target", graph.KindMethod, path(importerRowsFile)),
		node(path("svc/handler.go"), "handler.go", graph.KindFile, path("svc/handler.go")),
		node(path("svc/handler.go::Handle"), "Handle", graph.KindFunction, path("svc/handler.go")),
		node(path("cmd/main.go"), "main.go", graph.KindFile, path("cmd/main.go")),
		node(path("api/client.go::Client"), "Client", graph.KindType, path("api/client.go")),
		node(path("svc/service_test.go"), "service_test.go", graph.KindFile, path("svc/service_test.go")),
	}
	imports := func(from, to string) *graph.Edge {
		return &graph.Edge{From: path(from), To: path(to), Kind: graph.EdgeImports, FilePath: path(from), Line: 3}
	}
	edges := []*graph.Edge{
		imports("svc/handler.go", importerRowsFile),
		imports("cmd/main.go", importerRowsFile),
		imports("svc/service_test.go", importerRowsFile),
		imports("api/client.go::Client", "svc/service.go::Service"),
		{From: path("svc/handler.go::Handle"), To: path(importerRowsTarget), Kind: graph.EdgeCalls, FilePath: path("svc/handler.go"), Line: 5, Origin: graph.OriginASTResolved, Confidence: .9},
	}
	require.NoError(t, store.AddBatchChecked(nodes, edges))
}

// importingFilesOracle answers check_references' importing files the way the
// handler did before the capability existed: every import edge in the store
// whose target is the file or one of its symbols, unscoped.
func importingFilesOracle(store *store_sqlite.Store, path string) []string {
	seen := map[string]bool{}
	files := []string{}
	for _, edge := range store.AllEdges() {
		if edge.Kind != graph.EdgeImports {
			continue
		}
		to := store.GetNode(edge.To)
		if to == nil || (to.FilePath != path && to.ID != path) {
			continue
		}
		if from := store.GetNode(edge.From); from != nil && !seen[from.FilePath] {
			seen[from.FilePath] = true
			files = append(files, from.FilePath)
		}
	}
	sort.Strings(files)
	return files
}

var importerRowsSessions atomic.Int64

// A check_references call from a session bound to one repository finds the
// files importing the target's file from that file's own identities. Other
// repositories in the session's workspace sharing its names and paths change
// neither the importing files, which stay equal to the unscoped whole-store
// answer, nor the rows read to find them, and nothing reads the whole store.
func TestCheckReferencesScopedImportersNeverScanAndRowsStayFlat(t *testing.T) {
	store := openContractCoreCapStore(t)
	addImporterRowsRepo(t, store, "a")
	counter := newImporterReadCounter(store)
	srv := NewServer(query.NewEngine(counter), counter, nil, nil, zap.NewNop(), nil)
	installContractCoreKindTestRuntime(t, srv)
	call := func() string {
		ctx := WithSessionID(t.Context(), fmt.Sprintf("check-references-rows-%d", importerRowsSessions.Add(1)))
		session := srv.sessionFor(ctx)
		session.scopeResolved = true
		session.scopeBound = true
		session.scopeWorkspaceID = importerRowsWorkspace
		session.scopeRepoAllow = map[string]bool{"a": true}
		require.Equal(t, graph.Reader(counter), contractCoreSelectedReader(srv.readerFor(ctx)), "the request reader must wrap the counter")
		result, err := srv.wrapToolHandler(srv.handleCheckReferences)(ctx, makeReq("check_references", map[string]any{"symbol_id": "a/" + importerRowsTarget, "format": "json"}))
		require.NoError(t, err)
		require.False(t, result.IsError, toolResultText(result))
		return toolResultText(result)
	}
	importing := func(response string) []string {
		var payload struct {
			ImportingFiles []string `json:"importing_files"`
		}
		require.NoError(t, json.Unmarshal([]byte(response), &payload))
		return payload.ImportingFiles
	}
	want := []string{"a/api/client.go", "a/cmd/main.go", "a/svc/handler.go", "a/svc/service_test.go"}

	counter.reset()
	before := call()
	scans, baseline := counter.counts()
	require.Empty(t, scans, "a scoped request read the whole store")
	require.Equal(t, want, importing(before))
	require.Equal(t, importingFilesOracle(store, "a/"+importerRowsFile), importing(before))
	require.Positive(t, baseline["GetFileNodes"], "importers start from the file's own identities")

	for i := range 5 {
		addImporterRowsRepo(t, store, fmt.Sprintf("unrelated%d", i))
	}
	counter.reset()
	after := call()
	scans, rows := counter.counts()
	require.Empty(t, scans, "a scoped request read the whole store")
	require.JSONEq(t, before, after, "unrelated repositories changed the scoped response")
	require.Equal(t, importingFilesOracle(store, "a/"+importerRowsFile), importing(after))
	require.Equal(t, baseline, rows, "unrelated repositories changed the rows a scoped request read")
}
