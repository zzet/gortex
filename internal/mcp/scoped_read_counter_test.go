package mcp

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"sync"
	"sync/atomic"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

// scopedReadCounter counts what a scoped handler reads from a SQLite store.
// Embedding the store keeps every optional capability it implements visible,
// so the handler takes its production path; only the reads below are counted.
//
// Whole-store reads are counted per call: a scoped request must make none.
// Every other counted read records the rows it returned, per method, so a
// test can require that adding unrelated repositories changes neither the
// response nor what was read to build it. A symbol search backend built over
// the counter has its index reads counted as well.
//
// One production path stays out of reach: the contract-core exclusion cache
// serves only a concrete store, so over the counter, edge endpoints are
// classified by per-batch membership reads, which are counted, instead of the
// cached contract-owned set. That set spans the whole store, is read once per
// store revision and has its own tests.
type scopedReadCounter struct {
	*store_sqlite.Store
	tally *scopedReadTally
}

type scopedReadTally struct {
	mu    sync.Mutex
	scans map[string]int
	rows  map[string]int
}

func newScopedReadCounter(store *store_sqlite.Store) *scopedReadCounter {
	return &scopedReadCounter{Store: store, tally: &scopedReadTally{scans: map[string]int{}, rows: map[string]int{}}}
}

func (r *scopedReadCounter) scan(method string) {
	r.tally.mu.Lock()
	defer r.tally.mu.Unlock()
	r.tally.scans[method]++
}

func (r *scopedReadCounter) read(method string, rows int) {
	r.tally.mu.Lock()
	defer r.tally.mu.Unlock()
	r.tally.rows[method] += rows
}

// scans returns the whole-store reads made since the last reset, by method.
func (r *scopedReadCounter) scans() map[string]int {
	r.tally.mu.Lock()
	defer r.tally.mu.Unlock()
	return maps.Clone(r.tally.scans)
}

// rows returns the rows the counted reads returned since the last reset, by
// method.
func (r *scopedReadCounter) rows() map[string]int {
	r.tally.mu.Lock()
	defer r.tally.mu.Unlock()
	return maps.Clone(r.tally.rows)
}

func (r *scopedReadCounter) reset() {
	r.tally.mu.Lock()
	defer r.tally.mu.Unlock()
	clear(r.tally.scans)
	clear(r.tally.rows)
}

// BindReadContext keeps a context-bound handle on the same tally; the store's
// own binding would return an uncounted handle.
func (r *scopedReadCounter) BindReadContext(ctx context.Context) graph.Reader {
	return &scopedReadCounter{Store: r.WithReadContext(ctx), tally: r.tally}
}

func (r *scopedReadCounter) AllNodes() []*graph.Node {
	r.scan("AllNodes")
	return r.Store.AllNodes()
}

func (r *scopedReadCounter) AllNodesLight() []*graph.Node {
	r.scan("AllNodesLight")
	return r.Store.AllNodesLight()
}

func (r *scopedReadCounter) AllEdges() []*graph.Edge {
	r.scan("AllEdges")
	return r.Store.AllEdges()
}

func (r *scopedReadCounter) AllEdgesLight(kinds ...graph.EdgeKind) []*graph.Edge {
	r.scan("AllEdgesLight")
	return r.Store.AllEdgesLight(kinds...)
}

func (r *scopedReadCounter) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	r.scan("NodesByKind")
	return r.Store.NodesByKind(kind)
}

func (r *scopedReadCounter) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	r.scan("EdgesByKind")
	return r.Store.EdgesByKind(kind)
}

func (r *scopedReadCounter) EdgesByKinds(kinds []graph.EdgeKind) iter.Seq[*graph.Edge] {
	r.scan("EdgesByKinds")
	return r.Store.EdgesByKinds(kinds)
}

func (r *scopedReadCounter) GetNode(id string) *graph.Node {
	n := r.Store.GetNode(id)
	r.read("GetNode", nonNilNodes(n))
	return n
}

func (r *scopedReadCounter) GetNodeContext(ctx context.Context, id string) (*graph.Node, error) {
	n, err := r.Store.GetNodeContext(ctx, id)
	r.read("GetNodeContext", nonNilNodes(n))
	return n, err
}

func (r *scopedReadCounter) GetNodesByIDs(ids []string) map[string]*graph.Node {
	out := r.Store.GetNodesByIDs(ids)
	r.read("GetNodesByIDs", len(out))
	return out
}

func (r *scopedReadCounter) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	out, err := r.Store.GetNodesByIDsContext(ctx, ids)
	r.read("GetNodesByIDsContext", len(out))
	return out, err
}

func (r *scopedReadCounter) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	out, err := r.Store.GetNodeKindsByIDsContext(ctx, ids)
	r.read("GetNodeKindsByIDsContext", len(out))
	return out, err
}

func (r *scopedReadCounter) GetNodeIDsByKindsContext(ctx context.Context, ids []string, kinds []graph.NodeKind) (map[string]struct{}, error) {
	out, err := r.Store.GetNodeIDsByKindsContext(ctx, ids, kinds)
	r.read("GetNodeIDsByKindsContext", len(out))
	return out, err
}

func (r *scopedReadCounter) GetInEdges(id string) []*graph.Edge {
	out := r.Store.GetInEdges(id)
	r.read("GetInEdges", len(out))
	return out
}

func (r *scopedReadCounter) GetOutEdges(id string) []*graph.Edge {
	out := r.Store.GetOutEdges(id)
	r.read("GetOutEdges", len(out))
	return out
}

func (r *scopedReadCounter) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	out := r.Store.GetInEdgesByNodeIDs(ids)
	r.read("GetInEdgesByNodeIDs", edgeRows(out))
	return out
}

func (r *scopedReadCounter) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	out := r.Store.GetOutEdgesByNodeIDs(ids)
	r.read("GetOutEdgesByNodeIDs", edgeRows(out))
	return out
}

func (r *scopedReadCounter) GetInEdgesByNodeIDsFromRepos(ids, repos []string) (map[string][]*graph.Edge, bool, error) {
	out, handled, err := r.Store.GetInEdgesByNodeIDsFromRepos(ids, repos)
	r.read("GetInEdgesByNodeIDsFromRepos", edgeRows(out))
	return out, handled, err
}

func (r *scopedReadCounter) GetInEdgesByNodeIDsContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	out, truncated, err := r.Store.GetInEdgesByNodeIDsContext(ctx, ids, limit)
	r.read("GetInEdgesByNodeIDsContext", edgeRows(out))
	return out, truncated, err
}

func (r *scopedReadCounter) GetOutEdgesByNodeIDsContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	out, truncated, err := r.Store.GetOutEdgesByNodeIDsContext(ctx, ids, limit)
	r.read("GetOutEdgesByNodeIDsContext", edgeRows(out))
	return out, truncated, err
}

func (r *scopedReadCounter) BFSAdmissionFilteredContext(ctx context.Context, request graph.BFSAdmissionRequest, filter graph.OutlineEdgeFilter) (graph.BFSAdmissionSnapshot, bool, error) {
	snapshot, handled, err := r.Store.BFSAdmissionFilteredContext(ctx, request, filter)
	rows := len(snapshot.Nodes)
	for _, adjacency := range snapshot.Adjacency {
		rows += len(adjacency.Edges)
	}
	r.read("BFSAdmissionFilteredContext", rows)
	return snapshot, handled, err
}

func (r *scopedReadCounter) FindNodesByName(name string) []*graph.Node {
	out := r.Store.FindNodesByName(name)
	r.read("FindNodesByName", len(out))
	return out
}

func (r *scopedReadCounter) FindNodesByNameContext(ctx context.Context, name string) ([]*graph.Node, error) {
	out, err := r.Store.FindNodesByNameContext(ctx, name)
	r.read("FindNodesByNameContext", len(out))
	return out, err
}

func (r *scopedReadCounter) FindNodesByNames(names []string) map[string][]*graph.Node {
	out := r.Store.FindNodesByNames(names)
	rows := 0
	for _, nodes := range out {
		rows += len(nodes)
	}
	r.read("FindNodesByNames", rows)
	return out
}

func (r *scopedReadCounter) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	out := r.Store.FindNodesByNameInRepo(name, repoPrefix)
	r.read("FindNodesByNameInRepo", len(out))
	return out
}

func (r *scopedReadCounter) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	out := r.Store.FindNodesByNameContaining(substr, limit)
	r.read("FindNodesByNameContaining", len(out))
	return out
}

func (r *scopedReadCounter) FindNodesByNameContainingContext(ctx context.Context, substr string, limit int) ([]*graph.Node, error) {
	out, err := r.Store.FindNodesByNameContainingContext(ctx, substr, limit)
	r.read("FindNodesByNameContainingContext", len(out))
	return out, err
}

func (r *scopedReadCounter) FindNodesByNameContainingFilteredContext(ctx context.Context, substr string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, error) {
	out, err := r.Store.FindNodesByNameContainingFilteredContext(ctx, substr, limit, filter)
	r.read("FindNodesByNameContainingFilteredContext", len(out))
	return out, err
}

func (r *scopedReadCounter) FindNodesByNameBounded(ctx context.Context, name string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
	out, err := r.Store.FindNodesByNameBounded(ctx, name, scope, limit)
	r.read("FindNodesByNameBounded", len(out.Nodes))
	return out, err
}

func (r *scopedReadCounter) OutlineNodesByName(scope graph.NodeScope, name string) ([]*graph.Node, error) {
	out, err := r.Store.OutlineNodesByName(scope, name)
	r.read("OutlineNodesByName", len(out))
	return out, err
}

func (r *scopedReadCounter) FindFileNodesBounded(ctx context.Context, filePath string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
	out, err := r.Store.FindFileNodesBounded(ctx, filePath, scope, limit)
	r.read("FindFileNodesBounded", len(out.Nodes))
	return out, err
}

func (r *scopedReadCounter) NodesLightByKindsInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		rows := 0
		defer func() { r.read("NodesLightByKindsInScopeSeq", rows) }()
		for n := range r.Store.NodesLightByKindsInScopeSeq(repos, files, kinds...) {
			rows++
			if !yield(n) {
				return
			}
		}
	}
}

func (r *scopedReadCounter) FileLocationsInScopeSeq(repos []string) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		rows := 0
		defer func() { r.read("FileLocationsInScopeSeq", rows) }()
		for n := range r.Store.FileLocationsInScopeSeq(repos) {
			rows++
			if !yield(n) {
				return
			}
		}
	}
}

func (r *scopedReadCounter) SearchSymbolsContext(ctx context.Context, text string, limit int) ([]graph.SymbolHit, error) {
	out, err := r.Store.SearchSymbolsContext(ctx, text, limit)
	r.read("SearchSymbolsContext", len(out))
	return out, err
}

func (r *scopedReadCounter) SearchSymbolBundlesContext(ctx context.Context, text string, limit int) ([]graph.SymbolBundle, error) {
	out, err := r.Store.SearchSymbolBundlesContext(ctx, text, limit)
	r.read("SearchSymbolBundlesContext", bundleRows(out))
	return out, err
}

func (r *scopedReadCounter) SearchSymbolBundlesRepoScopedContext(ctx context.Context, text string, repoAllow []string, limit int) ([]graph.SymbolBundle, error) {
	out, err := r.Store.SearchSymbolBundlesRepoScopedContext(ctx, text, repoAllow, limit)
	r.read("SearchSymbolBundlesRepoScopedContext", bundleRows(out))
	return out, err
}

func (r *scopedReadCounter) SearchSymbolBundlesPathScopedContext(ctx context.Context, text string, repos, paths []string, limit int) ([]graph.SymbolBundle, error) {
	out, err := r.Store.SearchSymbolBundlesPathScopedContext(ctx, text, repos, paths, limit)
	r.read("SearchSymbolBundlesPathScopedContext", bundleRows(out))
	return out, err
}

func nonNilNodes(n *graph.Node) int {
	if n == nil {
		return 0
	}
	return 1
}

func edgeRows(byID map[string][]*graph.Edge) int {
	rows := 0
	for _, edges := range byID {
		rows += len(edges)
	}
	return rows
}

// bundleRows counts a symbol search's hits with the edges bundled with them.
func bundleRows(bundles []graph.SymbolBundle) int {
	rows := 0
	for _, bundle := range bundles {
		rows += nonNilNodes(bundle.Node) + len(bundle.InEdges) + len(bundle.OutEdges)
	}
	return rows
}

// requireScopedReadsStayFlat runs one scoped request before and after
// unrelated repositories are added, and requires the same response, no
// whole-store read, and the same rows from every counted read.
//
// The rows are those a read returns to Go. Rows a statement visits and
// rejects inside SQLite are not counted, so a flat count bounds the decoded
// and returned work, not what each statement costs.
func requireScopedReadsStayFlat(t *testing.T, counter *scopedReadCounter, addUnrelated func(), request func() string) {
	t.Helper()
	counter.reset()
	before := request()
	require.Empty(t, counter.scans(), "a scoped request read the whole store")
	baseline := counter.rows()
	require.NotEmpty(t, baseline, "the scoped request read nothing through the counter")
	addUnrelated()
	counter.reset()
	after := request()
	require.JSONEq(t, before, after, "unrelated repositories changed the scoped response")
	require.Empty(t, counter.scans(), "a scoped request read the whole store")
	require.Equal(t, baseline, counter.rows(), "unrelated repositories changed the rows a scoped request read")
}

// Scoped row fixtures give every repository the same call graph under the
// same names and repository-relative paths.
const (
	scopedRowService   = "svc/service.go::Service"
	scopedRowMutex     = "svc/service.go::Service.mu"
	scopedRowTarget    = "svc/service.go::Service.Target"
	scopedRowRefresh   = "svc/service.go::Service.Refresh"
	scopedRowInterface = "svc/api.go::API.Target"
	scopedRowHandler   = "svc/handler.go::Handle"
	scopedRowMain      = "cmd/main.go::main"
	scopedRowWorker    = "svc/worker.go::Run"
	scopedRowRoutes    = "svc/routes.go::Register"
	scopedRowLoad      = "svc/store.go::Load"
	scopedRowTest      = "svc/service_test.go::TestTarget"
)

// scopedRowID spells a fixture symbol in repo.
func scopedRowID(repo, id string) string { return repo + "/" + id }

// addScopedRowRepo adds one repository's call graph. A method that
// overrides an interface method has a function caller with four callees, a
// caller's caller, a lock-holding method caller, a method-value reference and
// a test. Call sites the resolver could not bind are parked on the method
// name's bare `unresolved::` placeholders, which every repository shares, and
// on the repository's own qualified placeholder.
func addScopedRowRepo(t *testing.T, store *store_sqlite.Store, repo string) {
	t.Helper()
	id := func(local string) string { return scopedRowID(repo, local) }
	file := func(local string) string { return repo + "/" + local }
	node := func(local, name string, kind graph.NodeKind, path string, start, end int) *graph.Node {
		return &graph.Node{ID: id(local), Name: name, Kind: kind, FilePath: file(path), RepoPrefix: repo, Language: "go", StartLine: start, EndLine: end}
	}
	mutex := node(scopedRowMutex, "mu", graph.KindField, "svc/service.go", 4, 4)
	mutex.Meta = map[string]any{"field_type": "sync.Mutex"}
	nodes := []*graph.Node{
		node(scopedRowService, "Service", graph.KindType, "svc/service.go", 3, 6),
		mutex,
		node(scopedRowTarget, "Target", graph.KindMethod, "svc/service.go", 8, 14),
		node(scopedRowRefresh, "Refresh", graph.KindMethod, "svc/service.go", 16, 20),
		node(scopedRowInterface, "Target", graph.KindMethod, "svc/api.go", 4, 4),
		node(scopedRowHandler, "Handle", graph.KindFunction, "svc/handler.go", 5, 12),
		node(scopedRowMain, "main", graph.KindFunction, "cmd/main.go", 3, 6),
		node(scopedRowWorker, "Run", graph.KindFunction, "svc/worker.go", 3, 12),
		node(scopedRowRoutes, "Register", graph.KindFunction, "svc/routes.go", 3, 8),
		node(scopedRowLoad, "Load", graph.KindFunction, "svc/store.go", 3, 9),
		node(scopedRowTest, "TestTarget", graph.KindFunction, "svc/service_test.go", 5, 9),
	}
	call := func(from, to, path string, line int) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file(path), Line: line, Origin: graph.OriginASTResolved, Confidence: .9}
	}
	edges := []*graph.Edge{
		{From: id(scopedRowMutex), To: id(scopedRowService), Kind: graph.EdgeMemberOf, FilePath: file("svc/service.go"), Line: 4},
		{From: id(scopedRowTarget), To: id(scopedRowService), Kind: graph.EdgeMemberOf, FilePath: file("svc/service.go"), Line: 8},
		{From: id(scopedRowRefresh), To: id(scopedRowService), Kind: graph.EdgeMemberOf, FilePath: file("svc/service.go"), Line: 16},
		{From: id(scopedRowTarget), To: id(scopedRowInterface), Kind: graph.EdgeOverrides, FilePath: file("svc/service.go"), Line: 8},
		call(id(scopedRowMain), id(scopedRowHandler), "cmd/main.go", 4),
		call(id(scopedRowHandler), id(scopedRowTarget), "svc/handler.go", 7),
		call(id(scopedRowHandler), id(scopedRowLoad), "svc/handler.go", 8),
		call(id(scopedRowHandler), id(scopedRowWorker), "svc/handler.go", 9),
		call(id(scopedRowHandler), id(scopedRowRoutes), "svc/handler.go", 10),
		call(id(scopedRowRefresh), id(scopedRowTarget), "svc/service.go", 18),
		call(id(scopedRowTest), id(scopedRowTarget), "svc/service_test.go", 7),
		{From: id(scopedRowRoutes), To: id(scopedRowTarget), Kind: graph.EdgeReferences, FilePath: file("svc/routes.go"), Line: 5, Origin: graph.OriginASTResolved, Confidence: .8},
		call(id(scopedRowTarget), id(scopedRowLoad), "svc/service.go", 10),
		call(id(scopedRowLoad), graph.UnresolvedMarker+"Flush", "svc/store.go", 6),
		call(id(scopedRowWorker), graph.UnresolvedMarker+"Target", "svc/worker.go", 6),
		call(id(scopedRowWorker), graph.UnresolvedMarker+"*.Target", "svc/worker.go", 8),
		call(id(scopedRowWorker), repo+"::"+graph.UnresolvedMarker+"Target", "svc/worker.go", 10),
	}
	require.NoError(t, store.AddBatchChecked(nodes, edges))
}

// addUnrelatedScopedRowRepos adds count repositories sharing the scoped
// repository's names, paths and bare placeholders.
func addUnrelatedScopedRowRepos(t *testing.T, store *store_sqlite.Store, count int) {
	t.Helper()
	for i := range count {
		addScopedRowRepo(t, store, fmt.Sprintf("unrelated%d", i))
	}
}

// scopedRowServer serves one fixture repository through the production
// request reader: the contract runtime wraps every request reader in the
// core-edge filter, over the counted store.
func scopedRowServer(t *testing.T) (*Server, *store_sqlite.Store, *scopedReadCounter) {
	t.Helper()
	store := openContractCoreCapStore(t)
	addScopedRowRepo(t, store, "a")
	counter := newScopedReadCounter(store)
	srv := NewServer(query.NewEngine(counter), counter, nil, nil, zap.NewNop(), nil)
	installContractCoreKindTestRuntime(t, srv)
	require.Equal(t, graph.Reader(counter), contractCoreSelectedReader(srv.readerFor(t.Context())), "the request reader must wrap the counter")
	return srv, store, counter
}

// scopedRowSessions numbers the sessions scopedRowCall opens.
var scopedRowSessions atomic.Int64

// scopedRowCall runs one request through the production tool middleware and
// returns its text payload. Each request opens its own session, so per-session
// notes and cues read the same on every pass.
func scopedRowCall(t *testing.T, srv *Server, handler mcpserver.ToolHandlerFunc, ctx context.Context, tool string, args map[string]any) string {
	t.Helper()
	ctx = WithSessionID(ctx, fmt.Sprintf("scoped-row-%d", scopedRowSessions.Add(1)))
	result, err := srv.wrapToolHandler(handler)(ctx, makeReq(tool, args))
	require.NoError(t, err)
	require.False(t, result.IsError, toolResultText(result))
	return toolResultText(result)
}
