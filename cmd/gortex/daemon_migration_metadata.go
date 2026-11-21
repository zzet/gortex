package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

// These records retain connection protocol state only, never cwd ownership.
// An old accepted connection is attached to the actual server on its first
// post-migration frame, without reconnecting or serializing other sessions.
type migrationMetadataSession struct {
	mu                      sync.Mutex
	session                 *daemon.Session
	write                   func([]byte) error
	initialize, initialized []byte
	bound                   *mcpDispatcher
	closed                  bool
}

type migrationMetadataDispatcher struct {
	catalog       *mcpDispatcher
	target        *mcpDispatcher // immutable in a published dispatcher
	reporter      *daemonStartupReporter
	sessions      *sync.Map
	declaredRepos *indexer.MultiIndexer // only the lexical load declarations are populated
}

func newMigrationMetadataDispatcher(cfg *config.Config, repos []config.RepoEntry, logger *zap.Logger, reporter *daemonStartupReporter) *migrationMetadataDispatcher {
	policy := gortexmcp.ToolPolicyConfig{Preset: cfg.MCP.Tools.Preset, Mode: cfg.MCP.Tools.Mode, Allow: cfg.MCP.Tools.Allow, Deny: cfg.MCP.Tools.Deny, OperatorPinned: cfg.MCP.Tools.Explicit}
	// Metadata registration needs no graph or query engine. Every graph call
	// is refused by this dispatcher until the real durable runtime is handed
	// over, so constructing an in-memory persistence substitute is unnecessary.
	server := gortexmcp.NewServer(nil, nil, nil, nil, logger, nil, gortexmcp.MultiRepoOptions{ToolPolicy: &policy})
	declared := &indexer.MultiIndexer{}
	declared.DeclareRepos(repos)
	return &migrationMetadataDispatcher{catalog: newMCPDispatcher(server, nil, logger), reporter: reporter, sessions: &sync.Map{}, declaredRepos: declared}
}

func (d *migrationMetadataDispatcher) handoff(target *mcpDispatcher) *migrationMetadataDispatcher {
	return &migrationMetadataDispatcher{catalog: d.catalog, target: target, reporter: d.reporter, sessions: d.sessions}
}

func (d *migrationMetadataDispatcher) DispatchClass(name string, args map[string]any) daemon.MCPDispatchClass {
	if d.target != nil {
		return d.target.DispatchClass(name, args)
	}
	return d.catalog.DispatchClass(name, args)
}

func (d *migrationMetadataDispatcher) SessionStarted(session *daemon.Session, write func([]byte) error) {
	rec := &migrationMetadataSession{session: session, write: write}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	d.sessions.Store(session.ID, rec)
	if d.target != nil {
		d.target.SessionStarted(session, write)
		rec.bound = d.target
	} else {
		d.catalog.SessionStarted(session, write)
	}
}

func (d *migrationMetadataDispatcher) SessionEnded(session *daemon.Session) {
	value, ok := d.sessions.Load(session.ID)
	if !ok {
		return
	}
	rec := value.(*migrationMetadataSession)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.session != session || !d.sessions.CompareAndDelete(session.ID, value) {
		return
	}
	rec.closed = true
	if rec.bound != nil {
		rec.bound.SessionEnded(session)
	}
	d.catalog.SessionEnded(session)
}

func (d *migrationMetadataDispatcher) phase() string {
	d.reporter.mu.Lock()
	defer d.reporter.mu.Unlock()
	if d.reporter.state.StartupPhase == daemon.StartupMigrating {
		return "migrating"
	}
	return "opening_store"
}

func (d *migrationMetadataDispatcher) indexHealth(frame []byte, cwd string) []byte {
	// Use only a containing configured root. Neither linked worktree discovery
	// nor symlink resolution should touch a repository mount in this phase.
	phase := d.phase()
	for dir := filepath.Clean(cwd); filepath.IsAbs(dir); {
		if repo, ok := d.declaredRepos.RepoLoadFor(dir); ok {
			return loadingIndexHealthFrame(frame, repo, phase, 0, gortexmcp.RepoLoadingSuggestion(repo.Label()), "")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return loadingIndexHealthFrame(frame, indexer.RepoLoad{}, phase, 0,
		"The store is opening or migrating. Repository identity is pending; retry shortly.", "identity_pending")
}

func (d *migrationMetadataDispatcher) Dispatch(ctx context.Context, session *daemon.Session, frame []byte) ([]byte, error) {
	value, ok := d.sessions.Load(session.ID)
	if !ok {
		return nil, context.Canceled
	}
	rec := value.(*migrationMetadataSession)
	rec.mu.Lock()
	if rec.closed {
		rec.mu.Unlock()
		return nil, context.Canceled
	}
	if d.target != nil {
		if rec.bound != d.target {
			d.target.SessionStarted(session, rec.write)
			for _, replay := range [][]byte{rec.initialize, rec.initialized} {
				if len(replay) > 0 {
					if _, err := d.target.Dispatch(ctx, session, replay); err != nil {
						rec.mu.Unlock()
						return nil, err
					}
				}
			}
			d.catalog.SessionEnded(session)
			rec.bound = d.target
		}
		rec.mu.Unlock()
		return d.target.Dispatch(ctx, session, frame)
	}
	method := peekFrameMethod(frame)
	if method == "initialize" {
		rec.initialize = append([]byte(nil), frame...)
	}
	if method == "notifications/initialized" {
		rec.initialized = append([]byte(nil), frame...)
	}
	rec.mu.Unlock()
	srv := d.catalog.srv
	session.SetLiveCWD(peekFrameClientCWD(frame))
	cwd := requestCWD(session, frame)
	allowed := loadingMetadataMethod(method)
	if method == "tools/call" {
		name, args := peekFrameToolCall(frame)
		if srv.LegacyToolForCall(name, args) == "index_health" {
			return d.indexHealth(frame, cwd), nil
		}
		// The catalogs remain subject to the negotiated tool surface. Neither
		// handler grants access to a graph or writes repo configuration.
		allowed = name == "capabilities" || name == "tools_search"
	}
	if !allowed {
		return jsonRPCErrorFrame(frame, "store schema is opening or migrating", map[string]any{"error_code": string(gortexmcp.ErrCodeRepoLoading), "retryable": true, "path": cwd, "phase": d.phase(), "percent": 0}), nil
	}
	ctx = gortexmcp.WithClientConnection(gortexmcp.WithSessionCWD(gortexmcp.WithSessionID(ctx, session.ID), cwd))
	ctx = srv.WithClientSession(ctx, session.ID)
	srv.NoteSessionToolPolicy(session.ID, session.ToolSpec, session.ToolMode)
	d.catalog.maybeSnoopInitialize(session, frame)
	reply := srv.MCPServer().HandleMessage(ctx, json.RawMessage(frame))
	if reply == nil {
		return nil, nil
	}
	out, err := json.Marshal(reply)
	if err == nil {
		out = rewriteMCPAdmissionInstructions(method, out, gortexmcp.ServerInstructionsLoading("configured repositories", d.phase(), 0))
	}
	return out, err
}

// The optional HTTP listener is configured before Listen too. Its ordinary
// handler is published only when the store-backed runtime exists.
type migrationHTTPHandler struct {
	mu        sync.RWMutex
	target    http.Handler
	available func() bool
}

func (h *migrationHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	target := h.target
	h.mu.RUnlock()
	if target == nil || h.available != nil && !h.available() {
		http.Error(w, "store is opening or migrating; retry", http.StatusServiceUnavailable)
		return
	}
	target.ServeHTTP(w, r)
}
func (h *migrationHTTPHandler) publish(target http.Handler) {
	h.mu.Lock()
	h.target = target
	h.mu.Unlock()
}
