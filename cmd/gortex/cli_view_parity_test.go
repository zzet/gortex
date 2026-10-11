package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
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
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

// The two front doors resolve a path through different code. `gortex …` reaches
// the control socket, whose realController has its own view resolver
// (daemon_controller_view.go selectProbeView). An agent's tools/call reaches the
// MCP dispatcher, whose server resolves internal/mcp resolveRequestView. This
// file is the only place the two are asked the same question about the same
// path over the same catalog and made to answer the same way.
//
// Nothing here stubs either resolver: one real sqlite store, one real catalog,
// one real MultiIndexer, and both doors built over them.
const (
	parityFamilyID   = "family-cli-parity"
	parityPrimaryID  = "chk-parity-primary"
	parityWorktreeID = "chk-parity-worktree"
	parityFile       = "app.go"
)

type parityFixture struct {
	controller   *realController
	dispatcher   *mcpDispatcher
	server       *gortexmcp.Server
	store        *store_sqlite.Store
	catalog      *store_sqlite.Catalog
	prefix       string
	graphID      string
	primaryRoot  string
	worktreeRoot string
	strangerRoot string
}

// newParityFixture builds ONE store, catalog and MultiIndexer, and puts both
// front doors on it: a realController (the control socket's handler, which is
// what every `gortex` verb's routing pre-flight asks) and an mcpDispatcher over
// a real gortexmcp.Server (what an agent's tools/call reaches).
//
// The worktree root is deliberately outside the tracked root — that is what
// makes it invisible to the repository registry and visible only to the view
// catalog, which is the shape whose admission the two doors used to disagree
// about.
func newParityFixture(t *testing.T) *parityFixture {
	t.Helper()
	dir := t.TempDir()
	primaryRoot := filepath.Join(dir, "repos", "app")
	worktreeRoot := filepath.Join(dir, "worktrees", "feature")
	strangerRoot := filepath.Join(dir, "worktrees", "not-registered")
	for _, d := range []string{primaryRoot, worktreeRoot, strangerRoot} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	store, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(store, reg, config.Default().Index, zap.NewNop())
	mi := indexer.NewMultiIndexer(store, reg, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })

	ctx := context.Background()
	res, err := mi.TrackRepoCtx(ctx, config.RepoEntry{Path: primaryRoot})
	require.NoError(t, err)
	prefix := res.RepoPrefix

	// The family list a view lookup walks is derived from the repo prefixes the
	// indexed graph holds, so the corpus has to carry one.
	store.AddBatch([]*graph.Node{{
		ID:         prefix + "/" + parityFile + "::Run",
		Kind:       graph.KindFunction,
		Name:       "Run",
		FilePath:   prefix + "/" + parityFile,
		RepoPrefix: prefix,
		Language:   "go",
		StartLine:  1,
		EndLine:    3,
	}}, nil)

	catalog := store.Catalog()
	graphID := indexer.GraphIDFor(prefix)
	require.NoError(t, catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          parityFamilyID,
		CommonDirIdentity: filepath.Join(primaryRoot, ".git"),
		State:             reconcile.FamilyStateReady,
		CreatedAt:         100,
		LastSeen:          100,
	}))
	require.NoError(t, catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    parityPrimaryID,
		Incarnation:   "inc-primary",
		FamilyID:      parityFamilyID,
		RootPath:      primaryRoot,
		GitDir:        filepath.Join(primaryRoot, ".git"),
		AdminName:     "primary",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      101,
	}))
	require.NoError(t, catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         graphID,
		OwnerCheckoutID: parityPrimaryID,
		RepoPrefix:      prefix,
		FamilyID:        parityFamilyID,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}))

	f := &parityFixture{
		store:        store,
		catalog:      catalog,
		prefix:       prefix,
		graphID:      graphID,
		primaryRoot:  primaryRoot,
		worktreeRoot: worktreeRoot,
		strangerRoot: strangerRoot,
	}
	f.upsertWorktree(t, store_sqlite.CheckoutStateReady, store_sqlite.CheckoutModeAutomatic)

	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer:  mi,
		ConfigManager: cm,
		Graph:         store,
		Logger:        zap.NewNop(),
	})
	require.NoError(t, err)

	// Door 1: the MCP dispatcher — what an agent's tools/call reaches.
	//
	// The stack builds exactly ONE materializer, over the lifecycle's own lease
	// manager, and the control socket takes it from the server rather than
	// building a second (cmd/gortex/daemon.go:291-301). The fixture wires it the
	// same way: two materializers would give the two doors different lease
	// managers, which is not the shape whose parity is being asserted.
	srv := gortexmcp.NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil,
		gortexmcp.MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm})
	srv.SetMaterializer(&graphview.Materializer{
		Store:   store,
		Catalog: catalog,
		Leases:  lifecycle.ViewLeases(),
	})
	f.server = srv
	f.dispatcher = newMCPDispatcher(srv, mi, zap.NewNop())

	// Door 2: the control socket — what every `gortex` verb's routing
	// pre-flight asks, over the SAME store, catalog and materializer.
	f.controller = &realController{
		graph:            store,
		multiIndexer:     mi,
		configManager:    cm,
		lifecycle:        lifecycle,
		logger:           zap.NewNop(),
		viewMaterializer: srv.Materializer(),
		// A probe of an unrouted working copy asks for a build. The fixture has
		// no repository owner registered, so a real activation would only log a
		// failure; the seams observe the ask without spawning work behind the
		// assertion.
		probeReconcile:        func(string) {},
		probeActivateCheckout: func(string) bool { return true },
	}
	return f
}

func (f *parityFixture) upsertWorktree(
	t *testing.T, state store_sqlite.CheckoutState, mode store_sqlite.CheckoutMode,
) {
	t.Helper()
	require.NoError(t, f.catalog.UpsertCheckout(context.Background(), store_sqlite.Checkout{
		CheckoutID:    parityWorktreeID,
		Incarnation:   "inc-worktree",
		FamilyID:      parityFamilyID,
		RootPath:      f.worktreeRoot,
		GitDir:        filepath.Join(f.primaryRoot, ".git", "worktrees", "feature"),
		AdminName:     "feature",
		State:         state,
		DesiredMode:   mode,
		EffectiveMode: mode,
		LastSeen:      102,
	}))
}

// route publishes the two generations the worktree's route names and activates
// it, which is what turns the worktree's answer from "unrouted" into a composed
// view on BOTH doors.
func (f *parityFixture) route(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	key := f.prefix + "/" + parityFile

	commitID, _, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        f.graphID,
		LayerID:        "layer-parity-commit",
		CheckoutID:     parityWorktreeID,
		GenerationKind: "commit",
		TreeOID:        "tree-parity-commit",
		CreatedAt:      1000,
	})
	require.NoError(t, err)
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, commitID, 2000))

	dirtyID, dirtyHandle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          f.graphID,
		LayerID:          "layer-parity-dirty",
		CheckoutID:       parityWorktreeID,
		GenerationKind:   "dirty",
		BaseGenerationID: commitID,
		TreeOID:          "tree-parity-dirty",
		CreatedAt:        1001,
	})
	require.NoError(t, err)
	dirtyHandle.AddBatch([]*graph.Node{{
		ID:         key,
		Kind:       graph.KindFile,
		Name:       key,
		FilePath:   key,
		RepoPrefix: f.prefix,
		Language:   "go",
	}, {
		ID:         key + "::GenerationOnly",
		Kind:       graph.KindFunction,
		Name:       "GenerationOnly",
		FilePath:   key,
		RepoPrefix: f.prefix,
		Language:   "go",
		StartLine:  1,
		EndLine:    4,
	}}, nil)
	require.NoError(t, dirtyHandle.SetFileMasks([]store_sqlite.FileMask{{
		RepoPrefix: f.prefix,
		FilePath:   key,
		Mode:       store_sqlite.OwnershipReplace,
	}}))
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, dirtyID, 2001))

	require.NoError(t, f.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         parityWorktreeID,
		GraphID:            f.graphID,
		CommitGenerationID: commitID,
		DirtyGenerationID:  dirtyID,
		State:              store_sqlite.RouteActive,
	}))
}

// cliView is what the CLI front door learns about a path: the control socket's
// file_coverage answer, which is exactly what probeCWDReach reads
// (cli_daemon.go checkoutBindsCWD) and what a PreToolUse hook turns into a
// verdict.
func (f *parityFixture) cliView(t *testing.T, path string) *daemon.ProbeView {
	t.Helper()
	out, err := f.controller.FileCoverage(context.Background(),
		daemon.FileCoverageParams{Path: filepath.Join(path, parityFile)})
	require.NoError(t, err)
	return out.View
}

// mcpRider is what the MCP front door reports about the same path: the freshness
// rider a real tools/call carries back, resolved from the session cwd through
// resolveRequestView.
func (f *parityFixture) mcpRider(t *testing.T, cwd string) map[string]any {
	t.Helper()
	sess := &daemon.Session{ID: "sess-parity-" + filepath.Base(cwd), CWD: cwd}
	frame := []byte(`{"jsonrpc":"2.0","id":31,"method":"tools/call",` +
		`"params":{"name":"graph_stats","arguments":{"format":"json"}}}`)
	reply, err := f.dispatcher.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply, "the dispatcher answered nothing for cwd %s", cwd)

	var parsed struct {
		Result struct {
			Meta map[string]any `json:"_meta"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(reply, &parsed))
	require.Empty(t, string(parsed.Error), "tools/call from cwd %s failed: %s", cwd, reply)
	rider, _ := parsed.Result.Meta["freshness"].(map[string]any)
	require.NotNil(t, rider,
		"a routed answer that says nothing about its view is indistinguishable from a base one: %s", reply)
	return rider
}

func riderString(rider map[string]any, field string) string {
	s, _ := rider[field].(string)
	return s
}

func riderBool(rider map[string]any, field string) bool {
	b, _ := rider[field].(bool)
	return b
}

// TestCLIAndMCPResolveTheSameViewForTheSamePath is the parity contract: the
// same working copy, asked through the control socket and through a real
// tools/call, names the same checkout and makes the same exactness claim.
//
// The two answers come from different resolvers over one catalog, so nothing
// but the catalog forces them to agree — which is why this is asserted rather
// than assumed.
func TestCLIAndMCPResolveTheSameViewForTheSamePath(t *testing.T) {
	t.Run("a composed automatic worktree", func(t *testing.T) {
		f := newParityFixture(t)
		f.route(t)

		probe := f.cliView(t, f.worktreeRoot)
		require.NotNil(t, probe)
		rider := f.mcpRider(t, f.worktreeRoot)

		assert.Equal(t, parityWorktreeID, probe.CheckoutID, "the CLI door named another checkout")
		assert.Equal(t, probe.CheckoutID, riderString(rider, "checkout_id"),
			"the two doors named different checkouts for one path: %v", rider)
		assert.True(t, probe.Exact)
		assert.Equal(t, probe.Exact, riderBool(rider, "exact"),
			"the two doors disagree about whether the path's own view answered: %v", rider)
		assert.Equal(t, daemon.ProbeViewWorktree, probe.Kind)
		assert.True(t, strings.HasPrefix(riderString(rider, "actual_view"), daemon.ProbeViewWorktree+":"),
			"the MCP door read %q where the probe read a %s view",
			riderString(rider, "actual_view"), probe.Kind)
	})

	t.Run("a registered automatic worktree with no route yet", func(t *testing.T) {
		f := newParityFixture(t)

		probe := f.cliView(t, f.worktreeRoot)
		require.NotNil(t, probe)
		rider := f.mcpRider(t, f.worktreeRoot)

		assert.Equal(t, parityWorktreeID, probe.CheckoutID)
		assert.Equal(t, probe.CheckoutID, riderString(rider, "checkout_id"),
			"the two doors named different checkouts for one path: %v", rider)
		assert.False(t, probe.Exact, "nothing describes this working copy yet")
		assert.Equal(t, probe.Exact, riderBool(rider, "exact"),
			"one door claimed an exact answer the other refused: %v", rider)
		assert.Equal(t, daemon.FallbackViewBuilding, probe.FallbackReason)
		assert.Equal(t, probe.FallbackReason, riderString(rider, "fallback_reason"),
			"the two doors gave different reasons for the same substitution: %v", rider)
	})
}

// TestCLIAndMCPAdmitTheSamePaths is the admission half. The CLI's pre-flight
// reads a control-surface ProbeView; the dispatcher asks the server's
// CheckoutServesCWDChecked. Both are the checkout arm of their door's gate, and
// they must reach the same verdict for every path.
//
// Before the alignment, "the answer named a checkout" was the CLI's rule, which
// admitted the grace and dedicated rows below — paths the dispatcher refuses.
func TestCLIAndMCPAdmitTheSamePaths(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		setup func(t *testing.T, f *parityFixture) string
	}{
		{
			name: "composed automatic worktree",
			setup: func(t *testing.T, f *parityFixture) string {
				f.route(t)
				return f.worktreeRoot
			},
		},
		{
			name:  "ready automatic worktree, not routed yet",
			setup: func(_ *testing.T, f *parityFixture) string { return f.worktreeRoot },
		},
		{
			name: "automatic worktree in availability grace",
			setup: func(t *testing.T, f *parityFixture) string {
				f.upsertWorktree(t, store_sqlite.CheckoutStateAvailabilityGrace,
					store_sqlite.CheckoutModeAutomatic)
				return f.worktreeRoot
			},
		},
		{
			name: "automatic worktree in removal grace",
			setup: func(t *testing.T, f *parityFixture) string {
				f.upsertWorktree(t, store_sqlite.CheckoutStateRemovalGrace,
					store_sqlite.CheckoutModeAutomatic)
				return f.worktreeRoot
			},
		},
		{
			name: "a worktree promoted to its own dedicated graph",
			setup: func(t *testing.T, f *parityFixture) string {
				f.upsertWorktree(t, store_sqlite.CheckoutStateReady,
					store_sqlite.CheckoutModeDedicated)
				return f.worktreeRoot
			},
		},
		{
			name:  "a sibling directory no checkout owns",
			setup: func(_ *testing.T, f *parityFixture) string { return f.strangerRoot },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newParityFixture(t)
			path := tc.setup(t, f)

			dispatcherAdmits, err := f.server.CheckoutServesCWDChecked(ctx, path)
			require.NoError(t, err)

			// One probe, reused for the verdict and the message. Probing again
			// for the failure text would re-enter the activation nudge an
			// unrouted checkout raises.
			probe := f.cliView(t, path)
			cliAdmits := probeViewServesAutomaticLane(probe)

			assert.Equal(t, dispatcherAdmits, cliAdmits,
				"the CLI pre-flight and the MCP dispatcher disagree about %s: "+
					"dispatcher admits=%v, CLI admits=%v (probe kind %q, checkout %q)",
				path, dispatcherAdmits, cliAdmits,
				probeViewKindOf(probe), checkoutIDOf(probe))
		})
	}
}

func checkoutIDOf(answer *daemon.ProbeView) string {
	if answer == nil {
		return ""
	}
	return answer.CheckoutID
}

// viewStubDaemon is a control-surface-only daemon that answers file_coverage
// with a caller-chosen ProbeView.
//
// The shared stub in executor_daemonfirst_test.go always answers with a
// composed worktree view, which is the one kind whose admission never changed.
// This one exists to vary the KIND, because the kind is what the pre-flight now
// reads its verdict from.
type viewStubDaemon struct {
	ln           net.Listener
	trackedRepos []string

	mu               sync.Mutex
	view             *daemon.ProbeView
	viewRoot         string
	coverageProbes   []string
	lastMCPHandshake daemon.Handshake
}

// startViewStubDaemon binds a unix socket under a short temp dir (the AF_UNIX
// path limit is shorter than a nested t.TempDir()) and points
// GORTEX_DAEMON_SOCKET at it for the duration of the test.
func startViewStubDaemon(t *testing.T, trackedRepos []string, root string, view *daemon.ProbeView) *viewStubDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "gxv")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "d.sock"))
	require.NoError(t, err)
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(dir, "d.sock"))
	t.Cleanup(func() { _ = ln.Close() })

	s := &viewStubDaemon{ln: ln, trackedRepos: trackedRepos, view: view, viewRoot: root}
	go s.serve()
	return s
}

func (s *viewStubDaemon) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *viewStubDaemon) handleConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	hsLine, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var hs daemon.Handshake
	if err := json.Unmarshal(hsLine, &hs); err != nil {
		return
	}
	if hs.Mode == daemon.ModeMCP {
		s.mu.Lock()
		s.lastMCPHandshake = hs
		s.mu.Unlock()
	}
	if err := daemon.WriteJSONLine(conn, daemon.HandshakeAck{OK: true, DaemonVersion: "stub"}); err != nil {
		return
	}
	if hs.Mode != daemon.ModeControl {
		// A ModeMCP connection is opened only after the pre-flight admitted the
		// path; the handshake above is all these tests read from it.
		return
	}
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req daemon.ControlRequest
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		switch req.Kind {
		case daemon.ControlStatus:
			st := daemon.StatusResponse{Version: "stub", Ready: true}
			for _, p := range s.trackedRepos {
				st.TrackedRepos = append(st.TrackedRepos, daemon.TrackedRepoStatus{Path: p})
			}
			raw, _ := json.Marshal(st)
			_ = daemon.WriteJSONLine(conn, daemon.ControlResponse{OK: true, Result: raw})
		case daemon.ControlFileCoverage:
			var p daemon.FileCoverageParams
			_ = json.Unmarshal(req.Params, &p)
			s.mu.Lock()
			s.coverageProbes = append(s.coverageProbes, p.Path)
			out := daemon.FileCoverageResult{}
			if pathkey.HasPathPrefix(p.Path, s.viewRoot) {
				out.View = s.view
			}
			s.mu.Unlock()
			raw, _ := json.Marshal(out)
			_ = daemon.WriteJSONLine(conn, daemon.ControlResponse{OK: true, Result: raw})
		default:
			_ = daemon.WriteJSONLine(conn, daemon.ControlResponse{OK: false, ErrorCode: "unsupported"})
		}
	}
}

func (s *viewStubDaemon) seenCoverageProbes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.coverageProbes...)
}

func (s *viewStubDaemon) seenMCPHandshake() daemon.Handshake {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastMCPHandshake
}

// TestResolveExecutor_NamedButUnservedCheckoutTakesTheFamilyRemedy is the
// production-entrypoint trace for the narrowed arm.
//
// It drives the real `gortex` relay — resolveExecutor → probeCWDReach →
// checkoutBindsCWD — over a real socket against a daemon that answers
// file_coverage with a ProbeView naming a checkout that is NOT served by the
// shared automatic lane (the grace shape). The pre-flight must refuse it with
// the family's reconcile remedy instead of relaying a call the dispatcher
// refuses with repo_not_tracked.
func TestResolveExecutor_NamedButUnservedCheckoutTakesTheFamilyRemedy(t *testing.T) {
	dir := t.TempDir()
	mainRepo, worktree := fakeLinkedWorktree(t, dir)

	stub := startViewStubDaemon(t, []string{mainRepo}, worktree, &daemon.ProbeView{
		Kind:           daemon.ProbeViewBase,
		CheckoutID:     "chk-in-grace",
		RepoPrefix:     "stub",
		Exact:          false,
		FallbackReason: string(store_sqlite.CheckoutStateAvailabilityGrace),
	})

	_, err := resolveExecutor(worktree)
	require.Error(t, err,
		"a checkout the shared lane does not serve must not pass the pre-flight — the dispatcher refuses it")
	msg := err.Error()
	assert.NotContains(t, msg, "gortex track ",
		"the daemon already tracks the family; the remedy must never be a track: %q", msg)
	assert.Contains(t, msg, "gortex repos reconcile "+mainRepo,
		"the remedy must name the family's reconcile: %q", msg)
	require.NotEmpty(t, stub.seenCoverageProbes(), "the pre-flight never asked the daemon")
	assert.Empty(t, stub.seenMCPHandshake().CWD,
		"a refused pre-flight must not have opened an MCP session")
}

// TestResolveExecutor_ServedCheckoutStillReachesTheDaemon guards the narrowing
// from the other side: the shape the dispatcher DOES admit — a ready automatic
// checkout whose view is still building — must still relay, view block and all.
func TestResolveExecutor_ServedCheckoutStillReachesTheDaemon(t *testing.T) {
	dir := t.TempDir()
	mainRepo, worktree := fakeLinkedWorktree(t, dir)

	stub := startViewStubDaemon(t, []string{mainRepo}, worktree, &daemon.ProbeView{
		Kind:           daemon.ProbeViewUnrouted,
		CheckoutID:     "chk-building",
		RepoPrefix:     "stub",
		Exact:          false,
		FallbackReason: daemon.FallbackViewBuilding,
	})

	exec, err := resolveExecutor(worktree)
	require.NoError(t, err,
		"a ready automatic checkout is served by the shared lane whether or not its view is built yet")
	defer exec.Close()
	assert.Equal(t, worktree, stub.seenMCPHandshake().CWD,
		"the call must carry the worktree cwd so the daemon binds the same view")
}
