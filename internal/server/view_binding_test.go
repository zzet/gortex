package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
)

// The /v1 REST surface has three classes of read. `POST /v1/tools/{name}`
// runs the MCP tool middleware, which resolves a request view and holds its
// lease for the call. The tool-backed GETs (/v1/processes, /v1/contracts,
// /v1/communities, /v1/caveats, the dashboard's sub-calls) run that same
// middleware but used to hand it a bare r.Context(), so it had nothing to
// resolve from. And the direct-store GETs — /v1/health, /v1/stats, /v1/graph,
// /v1/subgraph, /v1/repos, /v1/workspaces/{ws}/repos — read Handler.graph with
// no view and no lease at all: /v1/graph scanned the whole store twice
// (AllNodes then AllEdges) with nothing holding the generation those two scans
// came from.
//
// These tests pin the two halves of the fix: every direct-store read holds the
// base corpus for the lifetime of its response, and says truthfully that the
// base corpus is what answered.

// --- test doubles -----------------------------------------------------------

// witnessStore records, at the instant of every store read, whether the base
// generation was pinned. It is how a test asserts "no HTTP read scans an
// unleased store" without having to block inside the handler.
type witnessStore struct {
	graph.Store
	leases *graphview.LeaseManager

	mu     sync.Mutex
	during []bool
}

func (s *witnessStore) note() {
	s.mu.Lock()
	s.during = append(s.during, s.leases.InUse(graphview.BaseCorpusGeneration))
	s.mu.Unlock()
}

func (s *witnessStore) observations() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]bool, len(s.during))
	copy(out, s.during)
	return out
}

func (s *witnessStore) AllNodes() []*graph.Node { s.note(); return s.Store.AllNodes() }
func (s *witnessStore) AllEdges() []*graph.Edge { s.note(); return s.Store.AllEdges() }
func (s *witnessStore) Stats() graph.GraphStats { s.note(); return s.Store.Stats() }
func (s *witnessStore) RepoStats() map[string]graph.GraphStats {
	s.note()
	return s.Store.RepoStats()
}
func (s *witnessStore) GetNode(id string) *graph.Node { s.note(); return s.Store.GetNode(id) }
func (s *witnessStore) GetOutEdges(id string) []*graph.Edge {
	s.note()
	return s.Store.GetOutEdges(id)
}
func (s *witnessStore) GetInEdges(id string) []*graph.Edge {
	s.note()
	return s.Store.GetInEdges(id)
}

// gateStore blocks inside the first AllNodes call so a test can inspect the
// world while a response is half-built.
type gateStore struct {
	graph.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *gateStore) AllNodes() []*graph.Node {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	return s.Store.AllNodes()
}

// seededGraph is the two-node, one-edge corpus every case below reads.
func seededGraph(t *testing.T) graph.Store {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "repo::a.go::Foo", Kind: graph.KindFunction, Name: "Foo",
		FilePath: "a.go", Language: "go", RepoPrefix: "repo", WorkspaceID: "ws",
	})
	g.AddNode(&graph.Node{
		ID: "repo::a.go::Bar", Kind: graph.KindFunction, Name: "Bar",
		FilePath: "a.go", Language: "go", RepoPrefix: "repo", WorkspaceID: "ws",
	})
	g.AddEdge(&graph.Edge{From: "repo::a.go::Foo", To: "repo::a.go::Bar", Kind: graph.EdgeCalls})
	return g
}

func viewBindingHandler(t *testing.T, store graph.Store, leases BaseCorpusLeases,
	opts ...func(*mcpserver.MCPServer)) *Handler {
	t.Helper()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false), mcpserver.WithRecovery())
	for _, opt := range opts {
		opt(srv)
	}
	h := NewHandler(srv, store, "0.0.1-test", zap.NewNop())
	if leases != nil {
		h.SetBaseCorpusLeases(leases)
	}
	return h
}

// withCaveatAnalyze registers the one tool /v1/caveats aggregates, returning a
// hotspot that names a node in seededGraph. Without it the analyze sub-calls
// fail, the caveat list comes back empty and the enrichment walk — the direct
// store read this endpoint actually has — never runs, so a gate over that
// endpoint would pass vacuously.
func withCaveatAnalyze(srv *mcpserver.MCPServer) {
	srv.AddTool(mcp.NewTool("analyze", mcp.WithDescription("stub")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if kind, _ := req.GetArguments()["kind"].(string); kind != "hotspots" {
				return mcp.NewToolResultText(""), nil
			}
			return mcp.NewToolResultText(
				`{"hotspots":[{"id":"repo::a.go::Bar","name":"Bar","kind":"function",` +
					`"file_path":"a.go","start_line":4,"fan_in":1}]}`), nil
		})
}

func getJSON(t *testing.T, h *Handler, target string, headers map[string]string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "body: %s", rec.Body.String())
	return out
}

// --- the lease ---------------------------------------------------------------

// TestEveryDirectStoreReadHoldsTheBaseCorpus is the "no unleased whole-store
// scan" gate. Each of these endpoints reads Handler.graph directly; every one
// of those reads must happen with the base generation pinned, so a retirement
// sweep consulting InUse waits behind the reader instead of collecting the
// generation out from under it.
func TestEveryDirectStoreReadHoldsTheBaseCorpus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		opts   []func(*mcpserver.MCPServer)
	}{
		{"health", "/v1/health", nil},
		{"stats", "/v1/stats", nil},
		{"graph", "/v1/graph", nil},
		{"graph filtered by repo", "/v1/graph?repo=repo", nil},
		{"subgraph", "/v1/subgraph?id=repo%3A%3Aa.go%3A%3AFoo&depth=2", nil},
		{"repos", "/v1/repos", nil},
		{"workspace roster", "/v1/workspaces/ws/repos", nil},
		{"dashboard", "/v1/dashboard", nil},
		// /v1/caveats is tool-backed for its data, but its enrichment walk
		// is a direct store read that runs after every tool lease has been
		// released.
		{"caveats", "/v1/caveats", []func(*mcpserver.MCPServer){withCaveatAnalyze}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leases := graphview.NewLeaseManager()
			store := &witnessStore{Store: seededGraph(t), leases: leases}
			h := viewBindingHandler(t, store, leases, tc.opts...)

			getJSON(t, h, tc.target, nil)

			obs := store.observations()
			require.NotEmpty(t, obs, "%s performed no store read at all", tc.target)
			for i, pinned := range obs {
				require.True(t, pinned,
					"%s: store read #%d ran with the base corpus unleased", tc.target, i)
			}
			require.False(t, leases.InUse(graphview.BaseCorpusGeneration),
				"%s leaked its base-corpus pin past the response", tc.target)
		})
	}
}

// TestGraphDumpHoldsThePinAcrossBothWholeStoreScans is the specific race the
// /v1/graph endpoint had: AllNodes and AllEdges are two separate scans, and
// with nothing pinned between them the response could splice two generations.
// The pin must be live while the handler is parked between them.
func TestGraphDumpHoldsThePinAcrossBothWholeStoreScans(t *testing.T) {
	leases := graphview.NewLeaseManager()
	gate := &gateStore{
		Store:   seededGraph(t),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := viewBindingHandler(t, gate, leases)

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/graph", nil))
		done <- rec.Code
	}()

	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the graph dump never reached the store")
	}
	require.True(t, leases.InUse(graphview.BaseCorpusGeneration),
		"the base corpus was not pinned while /v1/graph was mid-scan")

	close(gate.release)
	select {
	case code := <-done:
		require.Equal(t, http.StatusOK, code)
	case <-time.After(10 * time.Second):
		t.Fatal("the graph dump never finished")
	}
	require.False(t, leases.InUse(graphview.BaseCorpusGeneration),
		"the base-corpus pin outlived the response")
}

// TestUnwiredHandlerStillServesAndSaysItHoldsNothing: a handler built without
// a lease manager (the `gortex mcp --server-api` front door, and every test
// that predates this seam) must keep working — and must not claim a pin it
// does not hold.
func TestUnwiredHandlerStillServesAndSaysItHoldsNothing(t *testing.T) {
	h := viewBindingHandler(t, seededGraph(t), nil)
	body := getJSON(t, h, "/v1/stats", map[string]string{"X-Gortex-Cwd": "/work/checkout"})
	view, ok := body["view"].(map[string]any)
	require.True(t, ok, "a cwd-bound caller got no view rider: %v", body)
	require.Equal(t, false, view["pinned"], "an unwired handler claimed a base-corpus pin")
	require.Equal(t, false, view["exact"])
}

// --- the rider ---------------------------------------------------------------

// TestCWDBoundCallerIsToldTheBaseCorpusAnswered: these endpoints have no
// view-routed graph read, so a caller that names a checkout must be told that
// what it got describes the base corpus — never that its own view answered.
func TestCWDBoundCallerIsToldTheBaseCorpusAnswered(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		caps   []any
		opts   []func(*mcpserver.MCPServer)
	}{
		{"health", "/v1/health", []any{"graph.syntax"}, nil},
		{"stats", "/v1/stats", []any{"graph.syntax"}, nil},
		{"graph", "/v1/graph", []any{"graph.syntax", "graph.resolution.local"}, nil},
		{"subgraph", "/v1/subgraph?id=repo%3A%3Aa.go%3A%3AFoo",
			[]any{"graph.syntax", "graph.resolution.local", "graph.incoming_edges"}, nil},
		{"repos", "/v1/repos", []any{"graph.syntax"}, nil},
		{"workspace roster", "/v1/workspaces/ws/repos", []any{"graph.syntax"}, nil},
		{"dashboard", "/v1/dashboard", []any{"graph.syntax"}, nil},
		{"caveats", "/v1/caveats",
			[]any{"graph.syntax", "graph.resolution.local", "graph.incoming_edges"},
			[]func(*mcpserver.MCPServer){withCaveatAnalyze}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leases := graphview.NewLeaseManager()
			h := viewBindingHandler(t, seededGraph(t), leases, tc.opts...)

			body := getJSON(t, h, tc.target, map[string]string{"X-Gortex-Cwd": "/work/checkout"})
			view, ok := body["view"].(map[string]any)
			require.True(t, ok, "%s answered a cwd-bound caller with no rider: %v", tc.target, body)
			require.Equal(t, "base", view["actual_view"])
			require.Equal(t, "worktree:path:/work/checkout", view["requested_view"])
			require.Equal(t, false, view["exact"],
				"%s presented the base corpus as an exact answer to a checkout request", tc.target)
			require.NotEmpty(t, view["fallback_reason"])
			require.Equal(t, tc.caps, view["base_scoped"])
			require.Equal(t, true, view["pinned"])
		})
	}
}

// TestTheCWDQueryFallbackIsHonoredToo: a browser or curl client that cannot
// set headers names its checkout with ?cwd=, exactly as the tool front door
// accepts it in the body.
func TestTheCWDQueryFallbackIsHonoredToo(t *testing.T) {
	leases := graphview.NewLeaseManager()
	h := viewBindingHandler(t, seededGraph(t), leases)
	body := getJSON(t, h, "/v1/stats?cwd=/work/checkout", nil)
	view, ok := body["view"].(map[string]any)
	require.True(t, ok, "the ?cwd= fallback produced no rider: %v", body)
	require.Equal(t, "worktree:path:/work/checkout", view["requested_view"])
	require.Equal(t, false, view["exact"])
}

// TestAnUnboundReadStaysByteIdentical: a caller that names no view asked for
// the base corpus and got it, so the response must carry exactly the fields it
// carried before this seam existed. The rider is a statement about a
// substitution; with no substitution there is nothing to say.
func TestAnUnboundReadStaysByteIdentical(t *testing.T) {
	leases := graphview.NewLeaseManager()
	h := viewBindingHandler(t, seededGraph(t), leases, withCaveatAnalyze)
	for _, target := range []string{
		"/v1/health", "/v1/stats", "/v1/graph",
		"/v1/subgraph?id=repo%3A%3Aa.go%3A%3AFoo", "/v1/repos",
		"/v1/workspaces/ws/repos", "/v1/dashboard", "/v1/caveats",
	} {
		body := getJSON(t, h, target, nil)
		_, present := body["view"]
		require.False(t, present, "%s grew a rider for a caller that named no view", target)
	}
}

// TestAReadThatStraddlesAMutationSaysSo is the honesty half of the pin. The
// lease protects lifetime, not bytes: a base writer is deliberately not made
// to queue behind in-flight readers, so the corpus CAN move mid-response. When
// it does, the response says base_changed instead of quietly handing back a
// mixed answer.
func TestAReadThatStraddlesAMutationSaysSo(t *testing.T) {
	leases := graphview.NewLeaseManager()
	reg, err := leases.PrepareRawRepositoryOwner(graphview.RawRepositoryOwner{
		RepoPrefix: "repo", RootIdentity: "/src/repo", Incarnation: "i1",
	})
	require.NoError(t, err)
	require.NoError(t, leases.CommitRawRepositoryOwner(reg))
	_, err = leases.CaptureInitialRawRepositorySource(context.Background(), reg, "source-a")
	require.NoError(t, err)

	gate := &gateStore{
		Store:   seededGraph(t),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := viewBindingHandler(t, gate, leases)

	type result struct {
		code int
		body map[string]any
	}
	done := make(chan result, 1)
	go func() {
		// ?repo=repo names one repository, so the pin holds that owner and
		// carries a source witness to compare against on the way out.
		req := httptest.NewRequest(http.MethodGet, "/v1/graph?repo=repo", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		done <- result{code: rec.Code, body: out}
	}()

	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the graph dump never reached the store")
	}
	// A publication opens under the live reader.
	write, err := leases.AcquireRawRepositoryMutation(context.Background(), reg)
	require.NoError(t, err)
	close(gate.release)

	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the graph dump never finished")
	}
	require.NoError(t, write.Complete("source-b"))
	write.Release()

	require.Equal(t, http.StatusOK, got.code)
	view, ok := got.body["view"].(map[string]any)
	require.True(t, ok, "a read that straddled a mutation carried no rider: %v", got.body)
	require.Equal(t, true, view["base_changed"],
		"the response did not report that the corpus moved under it")
	require.Equal(t, false, view["exact"])
	require.NotEmpty(t, view["fallback_reason"])
}

// TestASettledCorpusIsNeverReportedAsChanged is the other side of the same
// contract: "unknown" and "unchanged" must not be rendered as "changed", which
// would make every /v1 answer inexact on no evidence.
func TestASettledCorpusIsNeverReportedAsChanged(t *testing.T) {
	leases := graphview.NewLeaseManager()
	reg, err := leases.PrepareRawRepositoryOwner(graphview.RawRepositoryOwner{
		RepoPrefix: "repo", RootIdentity: "/src/repo", Incarnation: "i1",
	})
	require.NoError(t, err)
	require.NoError(t, leases.CommitRawRepositoryOwner(reg))
	_, err = leases.CaptureInitialRawRepositorySource(context.Background(), reg, "source-a")
	require.NoError(t, err)

	h := viewBindingHandler(t, seededGraph(t), leases)
	body := getJSON(t, h, "/v1/graph?repo=repo", nil)
	_, present := body["view"]
	require.False(t, present, "a settled corpus produced a rider: %v", body)
}

// --- the panic net ------------------------------------------------------------

// panicStore fails every direct store read a pinning handler makes. It is how
// a test reproduces "the handler died between acquiring the pin and rendering
// the response" without having to reach into the handler bodies.
type panicStore struct {
	graph.Store
}

func (s *panicStore) Stats() graph.GraphStats                { panic("boom: Stats") }
func (s *panicStore) AllNodes() []*graph.Node                { panic("boom: AllNodes") }
func (s *panicStore) AllEdges() []*graph.Edge                { panic("boom: AllEdges") }
func (s *panicStore) RepoStats() map[string]graph.GraphStats { panic("boom: RepoStats") }
func (s *panicStore) GetNode(string) *graph.Node             { panic("boom: GetNode") }

// TestAPanicMidResponseDoesNotLeakTheBaseCorpusPin.
//
// close() is what releases on the normal path, and close() is reached by
// falling off the end of the handler — so a panic anywhere in the body used to
// strand the pin forever. Handler.ServeHTTP's own recovery middleware catches
// the panic and answers 500 (handler.go's ServeHTTP), so the process survives
// and the leak is silent: it runs no release of its own, and
// graphview.LeaseManager finalises a closing repository owner only once its
// reader count reaches zero (repository_lease.go's readers == 0 gate). One
// stranded reader therefore wedges that owner's closure for the life of the
// process, one leak per failed request. Every pinning handler defers
// release().
func TestAPanicMidResponseDoesNotLeakTheBaseCorpusPin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		opts   []func(*mcpserver.MCPServer)
	}{
		{"health", "/v1/health", nil},
		{"stats", "/v1/stats", nil},
		{"graph", "/v1/graph", nil},
		{"graph filtered by repo", "/v1/graph?repo=repo", nil},
		{"subgraph", "/v1/subgraph?id=repo%3A%3Aa.go%3A%3AFoo", nil},
		{"repos", "/v1/repos", nil},
		{"workspace roster", "/v1/workspaces/ws/repos", nil},
		{"dashboard", "/v1/dashboard", nil},
		{"caveats", "/v1/caveats", []func(*mcpserver.MCPServer){withCaveatAnalyze}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leases := graphview.NewLeaseManager()
			h := viewBindingHandler(t, &panicStore{Store: seededGraph(t)}, leases, tc.opts...)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))

			require.Equal(t, http.StatusInternalServerError, rec.Code,
				"%s did not reach the failing store read", tc.target)
			require.False(t, leases.InUse(graphview.BaseCorpusGeneration),
				"%s stranded its base-corpus pin when the handler panicked", tc.target)
		})
	}
}

// TestTheNormalPathReleasesThroughCloseNotThroughTheNet keeps the two halves
// distinguishable: close() must do the releasing whenever it runs, so the net
// stays a net. A release() that fired unconditionally would hide a close()
// that stopped releasing.
func TestTheNormalPathReleasesThroughCloseNotThroughTheNet(t *testing.T) {
	leases := graphview.NewLeaseManager()
	h := viewBindingHandler(t, seededGraph(t), leases)

	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	read := h.beginBaseRead(req, "", graphview.CapSyntaxGraph)
	require.True(t, leases.InUse(graphview.BaseCorpusGeneration))

	read.close()
	require.False(t, leases.InUse(graphview.BaseCorpusGeneration),
		"close() did not release the pin")

	// The deferred net now has nothing to do, and must not double-release
	// its way into a negative refcount.
	read.release()
	require.False(t, leases.InUse(graphview.BaseCorpusGeneration))

	// A read that is never closed is exactly what the net exists for.
	unclosed := h.beginBaseRead(req, "", graphview.CapSyntaxGraph)
	require.True(t, leases.InUse(graphview.BaseCorpusGeneration))
	unclosed.release()
	require.False(t, leases.InUse(graphview.BaseCorpusGeneration),
		"release() did not cover a response that never reached close()")
}

// walkGateStore parks inside the Nth GetNode so a test can inspect the world
// while a multi-read walk is half-finished.
type walkGateStore struct {
	graph.Store
	at      int
	mu      sync.Mutex
	seen    int
	entered chan struct{}
	release chan struct{}
}

func (s *walkGateStore) GetNode(id string) *graph.Node {
	s.mu.Lock()
	s.seen++
	n := s.seen
	s.mu.Unlock()
	if n == s.at {
		close(s.entered)
		<-s.release
	}
	return s.Store.GetNode(id)
}

// TestCaveatEnrichmentHoldsThePinBetweenItsReads is the /v1/caveats analogue of
// the /v1/graph two-scan race, and the reason the enrichment walk needed a pin
// of its own.
//
// The walk is not one read: it is GetNode, then GetInEdges, then a GetNode per
// caller, per caveat — and every tool sub-call above it has already released
// the request view it held. A gate that only samples "was the base corpus
// pinned at the instant of each read" cannot tell one pin held across the walk
// from a pin re-taken per read; a sweep that collected the generation while the
// handler sat BETWEEN two of those reads would splice two generations into one
// page. This parks the handler between the first caveat's node lookup and its
// caller lookup and asserts the pin is live there.
func TestCaveatEnrichmentHoldsThePinBetweenItsReads(t *testing.T) {
	leases := graphview.NewLeaseManager()
	gate := &walkGateStore{
		Store:   seededGraph(t),
		at:      2, // the caller lookup, i.e. after GetNode + GetInEdges
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := viewBindingHandler(t, gate, leases, withCaveatAnalyze)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/caveats", nil))
		done <- rec
	}()

	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the caveat enrichment walk never reached its second node read")
	}
	require.True(t, leases.InUse(graphview.BaseCorpusGeneration),
		"the base corpus was unpinned while /v1/caveats was mid-enrichment")

	close(gate.release)
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("/v1/caveats never finished")
	}
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.False(t, leases.InUse(graphview.BaseCorpusGeneration),
		"/v1/caveats leaked its base-corpus pin past the response")

	// The walk actually ran — otherwise the liveness assertion above would
	// have been about an empty page.
	var body struct {
		Caveats []struct {
			Symbol   string `json:"symbol"`
			FilePath string `json:"file_path"`
			FanIn    int    `json:"fan_in"`
		} `json:"caveats"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.Caveats, "the caveats page was empty; the gate proved nothing")
	require.Equal(t, "a.go", body.Caveats[0].FilePath,
		"the enrichment walk did not fill the caveat in")
	require.Equal(t, 1, body.Caveats[0].FanIn)
}
