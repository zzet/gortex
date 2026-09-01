package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/server/hub"
)

// activityBuffer holds the last N graph-change events so the UI can
// backfill its activity feed without waiting for a fresh event.
//
// The buffer is intentionally small (default 100) — it is meant for
// "what just happened" feedback in the dashboard, not durable history.
// Events are preserved across reconnects but are lost on server restart.
type activityBuffer struct {
	mu     sync.RWMutex
	events []indexer.GraphChangeEvent
	cap    int
}

func newActivityBuffer(cap int) *activityBuffer {
	if cap <= 0 {
		cap = 100
	}
	return &activityBuffer{cap: cap, events: make([]indexer.GraphChangeEvent, 0, cap)}
}

func (b *activityBuffer) add(ev indexer.GraphChangeEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
	if len(b.events) > b.cap {
		b.events = b.events[len(b.events)-b.cap:]
	}
}

func (b *activityBuffer) snapshot(limit int) []indexer.GraphChangeEvent {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if limit <= 0 || limit > len(b.events) {
		limit = len(b.events)
	}
	out := make([]indexer.GraphChangeEvent, limit)
	for i := 0; i < limit; i++ {
		out[i] = b.events[len(b.events)-1-i]
	}
	return out
}

func (h *Handler) startActivityCollector(eh *hub.Hub) {
	if eh == nil || h.activity == nil {
		return
	}
	subID, ch := eh.Subscribe()
	go func() {
		defer eh.Unsubscribe(subID)
		for ev := range ch {
			h.activity.add(ev)
		}
	}()
}

// --- /v1/activity ---

func (h *Handler) handleActivity(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	if h.activity == nil {
		WriteJSON(w, http.StatusOK, map[string]any{"events": []any{}})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"events": h.activity.snapshot(limit)})
}

// --- /v1/repos ---
//
// Returns a flat list of indexed repositories with the per-repo
// breakdown the dashboard's repo cards and the graph filter panel
// expect. All numbers are derived from graph.RepoStats() — no mock
// values. The "color" hint is chosen deterministically from the repo
// prefix so cards stay stable across reloads without storing state.

type repoEntry struct {
	ID         string `json:"id"`
	Owner      string `json:"owner"`
	Lang       string `json:"lang"`
	Nodes      int    `json:"nodes"`
	Edges      int    `json:"edges"`
	Funcs      int    `json:"funcs"`
	Methods    int    `json:"methods"`
	Types      int    `json:"types"`
	Interfaces int    `json:"interfaces"`
	Vars       int    `json:"vars"`
	Files      int    `json:"files"`
	Color      string `json:"color"`
}

// dominantLanguage returns the language with the highest byte/node share
// for a repo, ignoring config-only languages so a Go repo doesn't show
// up as "yaml".
func dominantLanguage(byLang map[string]int) string {
	skip := map[string]bool{"json": true, "yaml": true, "toml": true, "markdown": true, "makefile": true, "dockerfile": true, "bash": true, "hcl": true}
	best := ""
	bestN := -1
	for lang, n := range byLang {
		if skip[lang] {
			continue
		}
		if n > bestN {
			best = lang
			bestN = n
		}
	}
	if best != "" {
		return best
	}
	for lang, n := range byLang {
		if n > bestN {
			best = lang
			bestN = n
		}
	}
	return best
}

// hashColor maps a string to one of the design's accent OKLCH colors.
// Stable, deterministic, no per-repo seeding required.
var repoPalette = []string{
	"oklch(0.82 0.15 155)",
	"oklch(0.80 0.13 230)",
	"oklch(0.78 0.14 300)",
	"oklch(0.80 0.17 10)",
	"oklch(0.82 0.14 45)",
	"oklch(0.82 0.15 80)",
	"oklch(0.72 0.02 252)",
}

func paletteFor(s string) string {
	if s == "" {
		return repoPalette[0]
	}
	var sum uint32
	for _, c := range s {
		sum = sum*31 + uint32(c)
	}
	return repoPalette[int(sum)%len(repoPalette)]
}

// splitOwner pulls "owner/repo" out of a repo prefix when one exists,
// otherwise treats the whole prefix as the repo and leaves owner blank.
func splitOwner(prefix string) (owner, name string) {
	if before, after, ok := strings.Cut(prefix, "/"); ok {
		return before, after
	}
	return "", prefix
}

func reposFromGraph(g graph.Store) []repoEntry {
	stats := g.RepoStats()
	out := make([]repoEntry, 0, len(stats))
	for prefix, s := range stats {
		owner, name := splitOwner(prefix)
		out = append(out, repoEntry{
			ID:         name,
			Owner:      owner,
			Lang:       dominantLanguage(s.ByLanguage),
			Nodes:      s.TotalNodes,
			Edges:      s.TotalEdges,
			Funcs:      s.ByKind["function"],
			Methods:    s.ByKind["method"],
			Types:      s.ByKind["type"],
			Interfaces: s.ByKind["interface"],
			Vars:       s.ByKind["variable"],
			Files:      s.ByKind["file"],
			Color:      paletteFor(prefix),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nodes > out[j].Nodes })
	return out
}

func (h *Handler) handleRepos(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{"repos": reposFromGraph(h.graph)})
}

// --- /v1/overlay/* ---

// handleOverlayRegister handles POST /v1/overlay/sessions.
//
// Body (all fields optional): {
//
//	"workspace_id": "<slug>",
//	"session_id":   "<explicit-id>"   // bind to a known MCP session
//
// }
//
// When `session_id` is supplied, the session is registered under that
// ID instead of a freshly minted one — this is how an MCP client binds
// its overlay session to its MCP session ID, so subsequent tools/call
// frames from the same MCP session automatically see the overlay (the
// MCP tool middleware reads OverlayCohortIDFromContext, which resolves
// to the real session id unless a caller explicitly overrides it via
// X-Gortex-Overlay-Session). Idempotent: registering twice with the
// same (id, workspace) tuple is a no-op; mismatched workspaces return
// 409.
//
// Response: {"session_id": "...", "workspace_id": "..."}.
func (h *Handler) handleOverlayRegister(w http.ResponseWriter, r *http.Request) {
	if h.overlays == nil {
		http.Error(w, "overlay support not enabled on this server", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		WorkspaceID string `json:"workspace_id"`
		SessionID   string `json:"session_id"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	id := body.SessionID
	if id == "" {
		id = h.overlays.Register(body.WorkspaceID)
	} else {
		if err := h.overlays.RegisterWithID(id, body.WorkspaceID); err != nil {
			if errors.Is(err, daemon.ErrSessionExists) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	WriteJSON(w, http.StatusCreated, map[string]any{
		"session_id":   id,
		"workspace_id": body.WorkspaceID,
	})
}

// handleOverlayPush handles PUT /v1/overlay/sessions/{id}/files.
// Body: OverlayFile JSON. Path is required; Content or Deleted=true.
func (h *Handler) handleOverlayPush(w http.ResponseWriter, r *http.Request) {
	if h.overlays == nil {
		http.Error(w, "overlay support not enabled on this server", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	var overlay daemon.OverlayFile
	if err := json.NewDecoder(r.Body).Decode(&overlay); err != nil {
		http.Error(w, "invalid OverlayFile JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.overlays.Push(id, overlay, nil); err != nil {
		switch {
		case errors.Is(err, daemon.ErrSessionNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, daemon.ErrOverlayDrift):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleOverlayDelete handles DELETE /v1/overlay/sessions/{id}/files.
// Body: {"path": "..."}. Removes one overlay file from the session.
func (h *Handler) handleOverlayDelete(w http.ResponseWriter, r *http.Request) {
	if h.overlays == nil {
		http.Error(w, "overlay support not enabled on this server", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if err := h.overlays.Delete(id, body.Path); err != nil {
		if errors.Is(err, daemon.ErrSessionNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleOverlayDrop handles DELETE /v1/overlay/sessions/{id}.
// Drops the entire session, discarding every overlay it held.
func (h *Handler) handleOverlayDrop(w http.ResponseWriter, r *http.Request) {
	if h.overlays == nil {
		http.Error(w, "overlay support not enabled on this server", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	h.overlays.Drop(id)
	w.WriteHeader(http.StatusNoContent)
}

// handleOverlayList handles GET /v1/overlay/sessions/{id}/files.
// Returns the current overlay snapshot for the session.
func (h *Handler) handleOverlayList(w http.ResponseWriter, r *http.Request) {
	if h.overlays == nil {
		http.Error(w, "overlay support not enabled on this server", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	files, err := h.overlays.Files(id)
	if err != nil {
		if errors.Is(err, daemon.ErrSessionNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	out := make([]daemon.OverlayFile, 0, len(files))
	for _, f := range files {
		out = append(out, f)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"files": out})
}

// handleWorkspaceRoster serves `GET /v1/workspaces/{ws}/repos`.
// Returns the deduplicated list of repo prefixes for the requested
// workspace slug, or 404 when no node in
// the graph carries that workspace. The daemon-side
// WorkspaceRosterCache calls this once per (slug, workspace) and
// caches the result for ~1 minute (see daemon.NewWorkspaceRosterCache).
func (h *Handler) handleWorkspaceRoster(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if ws == "" {
		http.Error(w, "workspace slug required", http.StatusBadRequest)
		return
	}
	seen := make(map[string]struct{})
	for _, n := range h.graph.AllNodes() {
		// Effective workspace match — match on either the explicit
		// WorkspaceID or the RepoPrefix fallback. Either way the
		// result list reports RepoPrefix so callers can address the
		// underlying repo without translating slugs.
		nodeWS := n.WorkspaceID
		if nodeWS == "" {
			nodeWS = n.RepoPrefix
		}
		if nodeWS != ws {
			continue
		}
		if n.RepoPrefix == "" {
			continue
		}
		seen[n.RepoPrefix] = struct{}{}
	}
	if len(seen) == 0 {
		http.Error(w, "workspace not found", http.StatusNotFound)
		return
	}
	repos := make([]string, 0, len(seen))
	for p := range seen {
		repos = append(repos, p)
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"workspace": ws,
		"repos":     repos,
	})
}

// --- /v1/processes ---
//
// Thin wrapper around the get_processes MCP tool. Returns processes in
// a UI-friendly shape: each process gets a "crosses" array (unique repo
// prefixes touched) and a "risk" rating derived from the score. When
// the underlying tool isn't registered (analyze-only build), returns
// an empty list rather than 404 so the page can render its empty state.

type processEntry struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Entry    string   `json:"entry"`
	Steps    int      `json:"steps"`
	Files    int      `json:"files"`
	Repos    int      `json:"repos"`
	Score    int      `json:"score"`
	Risk     string   `json:"risk"`     // ok | warn | risk
	Crosses  []string `json:"crosses"`  // repo prefixes this flow touches
	Category string   `json:"category"` // product | tests | internal
}

// rawProcessSummary mirrors the MCP get_processes list response. Files
// and Steps are intentionally omitted — the list now carries precomputed
// step_count / file_count / repo_prefixes so the dashboard shape doesn't
// require a second call per process.
type rawProcessSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	EntryPoint   string   `json:"entry_point"`
	StepCount    int      `json:"step_count"`
	FileCount    int      `json:"file_count"`
	Score        float64  `json:"score"`
	RepoPrefixes []string `json:"repo_prefixes"`
}

func processEntryFromRaw(p rawProcessSummary) processEntry {
	risk := "ok"
	switch {
	case p.Score > 1000:
		risk = "risk"
	case p.Score > 500:
		risk = "warn"
	}
	crosses := p.RepoPrefixes
	if crosses == nil {
		crosses = []string{}
	}
	return processEntry{
		ID:       p.ID,
		Name:     p.Name,
		Entry:    p.EntryPoint,
		Steps:    p.StepCount,
		Files:    p.FileCount,
		Repos:    len(crosses),
		Score:    int(p.Score),
		Risk:     risk,
		Crosses:  crosses,
		Category: categorizeProcess(p.EntryPoint),
	}
}

// categorizeProcess buckets a flow by its entry point path so the
// Investigations UI can tab between production code, test entry points,
// and Go `internal/` package flows.
func categorizeProcess(entry string) string {
	// Split "<repo>/<path>::<sym>" into the path half.
	path, sym, hasSym := strings.Cut(entry, "::")
	if hasSym {
		if strings.HasPrefix(sym, "Test") || strings.HasPrefix(sym, "Benchmark") || strings.HasPrefix(sym, "Example") || strings.HasPrefix(sym, "Fuzz") {
			return "tests"
		}
	}
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, "_test.go") || strings.Contains(lower, "_test.") {
		return "tests"
	}
	if strings.Contains(path, "/internal/") || strings.HasPrefix(path, "internal/") {
		return "internal"
	}
	return "product"
}

func (h *Handler) handleProcesses(w http.ResponseWriter, r *http.Request) {
	raw, err := h.CallToolStrict(r.Context(), "analyze", map[string]any{"kind": "processes"})
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw == "" {
		WriteJSON(w, http.StatusOK, map[string]any{"processes": []processEntry{}})
		return
	}
	type wrap struct {
		Processes []rawProcessSummary `json:"processes"`
	}
	var w0 wrap
	if err := json.Unmarshal([]byte(raw), &w0); err != nil {
		WriteJSON(w, http.StatusOK, map[string]any{"processes": []processEntry{}})
		return
	}
	out := make([]processEntry, 0, len(w0.Processes))
	for _, p := range w0.Processes {
		out = append(out, processEntryFromRaw(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	WriteJSON(w, http.StatusOK, map[string]any{"processes": out})
}

// --- /v1/contracts ---
//
// Flattens the contracts MCP tool's by_repo grouping into a single list
// keyed by canonical contract ID. The UI shows kind / producer /
// consumers / breaking flag; we fold provider+consumer rows into a
// single entry per contract ID so users see one row per route, not two.

type contractEntry struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Kind      string             `json:"kind"`  // REST | EVENT | URL | ENV | DEP (coarse, for badge)
	Type      string             `json:"type"`  // http | grpc | graphql | topic | ws | env | openapi | dependency
	Scope     string             `json:"scope"` // own | external
	Producer  string             `json:"producer"`
	Consumers []string           `json:"consumers"`
	Version   string             `json:"version"`
	Breaking  bool               `json:"breaking"`
	Callers   int                `json:"callers"`
	Last      string             `json:"last"`
	Locations []contractLocation `json:"locations"`

	// IsTestOnly is true when every location of this contract was
	// extracted from a synthetic test/bench fixture (testdata/,
	// bench/fixtures/, __fixtures__/). The UI uses this as a single
	// boolean to hide synthetic rows from the production view by
	// default, while still allowing a "show tests" toggle so drift
	// checks (test pinned to a stale provider contract) stay visible.
	// Mixed contracts — at least one production location plus one
	// test location — keep IsTestOnly=false so they remain in the
	// production view; the per-location Meta carries the granular
	// is_test flag for callers that want to render badges per row.
	IsTestOnly bool `json:"is_test_only,omitempty"`

	// Schema-shape fields promoted from the primary location's meta so
	// the UI can render a structured schema card instead of the raw
	// per-location JSON. Populated from the provider when present,
	// otherwise from the first consumer.
	Schema *contractSchema `json:"schema,omitempty"`
	// ProviderSchema / ConsumerSchema expose each side's meta
	// independently so the UI can render side-by-side comparison.
	// Both are nil when no location of that role has schema info.
	ProviderSchema *contractSchema `json:"provider_schema,omitempty"`
	ConsumerSchema *contractSchema `json:"consumer_schema,omitempty"`
}

// contractSchema summarises the request/response shape of a contract
// for UI consumption. All fields are optional and reflect whatever
// the extractor and post-pass could pin down. `Source` is one of
// "extracted" | "partial" | "none".
type contractSchema struct {
	RequestType    string `json:"request_type,omitempty"`
	ResponseType   string `json:"response_type,omitempty"`
	RequestExpr    string `json:"request_expr,omitempty"`
	ResponseExpr   string `json:"response_expr,omitempty"`
	RequestStream  bool   `json:"request_stream,omitempty"`
	ResponseStream bool   `json:"response_stream,omitempty"`
	// ResponseRepeated is true when the response value's declared
	// type was a slice (e.g. `[]Foo` from `make([]Foo, …)`). The UI
	// renders the response as `[Foo]` instead of `Foo`.
	ResponseRepeated bool `json:"response_repeated,omitempty"`
	// ResponseShape and RequestShape are the snapshotted struct
	// definitions of ResponseType / RequestType — fields, JSON tags,
	// types — so the UI can render the body as an actual JSON
	// object instead of a bare type-symbol-ID. Empty when the type
	// isn't a graph-known struct (primitives, external types).
	ResponseShape map[string]any `json:"response_shape,omitempty"`
	RequestShape  map[string]any `json:"request_shape,omitempty"`
	// ResponseEnvelope is the structured form of an inline map
	// response like `map[string]any{"files": out}`. When present,
	// the UI prefers it over ResponseExpr so the schema view shows
	// the JSON shape (a list of {name, type, expr} fields) instead
	// of the raw helper-call text.
	ResponseEnvelope []envelopeFieldDTO `json:"response_envelope,omitempty"`
	PathParams       []string           `json:"path_params,omitempty"`
	// PathParamNames preserves the developer-written parameter names
	// in the same positional order as PathParams. Contract IDs use
	// positional p1/p2/... so cross-repo matching is naming-agnostic;
	// this field surfaces the actual source-side names (e.g. ["id"])
	// for display, drift detection, and OpenAPI export.
	PathParamNames []string `json:"path_param_names,omitempty"`
	QueryParams    []string `json:"query_params,omitempty"`
	StatusCodes    []int    `json:"status_codes,omitempty"`
	Source         string   `json:"source,omitempty"`
}

// envelopeFieldDTO mirrors one entry of contracts.envelopeField on the
// HTTP wire. Type is best-effort and may be empty when the source
// expression couldn't be traced to a typed binding; Expr is always
// the trimmed source expression that fed the JSON key. Shape is the
// snapshotted struct definition for Type — fields, JSON tags, etc. —
// so the UI can render the response as an actual JSON object instead
// of a bare type-symbol-ID. Empty when the type couldn't be expanded
// (external types, untyped expressions).
type envelopeFieldDTO struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
	Expr string `json:"expr,omitempty"`
	// Repeated is true when the originally-declared type was a slice
	// (e.g. `[]Repo` or `make([]string, …)`). The UI renders the field
	// as an array — `repos: [Repo]` instead of `repos: Repo`.
	Repeated bool           `json:"repeated,omitempty"`
	Shape    map[string]any `json:"shape,omitempty"`
}

// contractLocation is a single on-disk occurrence of a contract —
// either a provider handler or a consumer call site. The UI uses this
// to expand a contract row into a jump-list of file:line entries and
// to resolve symbol IDs back to source via /v1/tools/get_symbol_source.
type contractLocation struct {
	Role       string         `json:"role"` // provider | consumer
	RepoPrefix string         `json:"repo_prefix"`
	SymbolID   string         `json:"symbol_id"`
	FilePath   string         `json:"file_path"`
	Line       int            `json:"line"`
	Meta       map[string]any `json:"meta,omitempty"`
}

func (h *Handler) handleContracts(w http.ResponseWriter, r *http.Request) {
	raw, err := h.CallToolStrict(r.Context(), "analyze", map[string]any{"kind": "contracts", "action": "list"})
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw == "" {
		WriteJSON(w, http.StatusOK, map[string]any{"contracts": []contractEntry{}})
		return
	}
	type rawContract struct {
		ID         string         `json:"id"`
		Type       string         `json:"type"`
		Role       string         `json:"role"`
		SymbolID   string         `json:"symbol_id"`
		FilePath   string         `json:"file_path"`
		Line       int            `json:"line"`
		RepoPrefix string         `json:"repo_prefix"`
		Meta       map[string]any `json:"meta"`
		Confidence float64        `json:"confidence"`
	}
	type wrap struct {
		ByRepo map[string]struct {
			Contracts map[string][]rawContract `json:"contracts"`
			Total     int                      `json:"total"`
		} `json:"by_repo"`
	}
	var w0 wrap
	if err := json.Unmarshal([]byte(raw), &w0); err != nil {
		WriteJSON(w, http.StatusOK, map[string]any{"contracts": []contractEntry{}})
		return
	}
	merged := make(map[string]*contractEntry)
	for _, group := range w0.ByRepo {
		for kind, items := range group.Contracts {
			for _, c := range items {
				// Upstream NormalizeHTTPPath preserves parameter names,
				// so a provider's "/v1/workspaces/{wid}/tags/{id}" and
				// a consumer's "/v1/workspaces/{workspaceId}/tags/{id}"
				// hash to different contract IDs for the same route.
				// Re-canonicalize by positional params so they merge.
				key := canonicalContractKey(c.ID)
				e, ok := merged[key]
				if !ok {
					e = &contractEntry{
						ID:        key,
						Name:      key,
						Kind:      uiContractKind(kind),
						Type:      kind,
						Consumers: []string{},
						Locations: []contractLocation{},
					}
					merged[key] = e
				}
				if c.Role == "provider" && e.Producer == "" {
					e.Producer = c.RepoPrefix
				}
				if c.Role == "consumer" && c.RepoPrefix != "" && !slices.Contains(e.Consumers, c.RepoPrefix) {
					e.Consumers = append(e.Consumers, c.RepoPrefix)
				}
				e.Locations = append(e.Locations, contractLocation{
					Role:       c.Role,
					RepoPrefix: c.RepoPrefix,
					SymbolID:   c.SymbolID,
					FilePath:   c.FilePath,
					Line:       c.Line,
					Meta:       c.Meta,
				})
				e.Callers++
			}
		}
	}
	out := make([]contractEntry, 0, len(merged))
	for _, e := range merged {
		e.Scope = contractScope(e.Type, e.Producer)
		sort.SliceStable(e.Locations, func(i, j int) bool {
			a, b := e.Locations[i], e.Locations[j]
			if a.Role != b.Role {
				return a.Role == "provider" // providers first
			}
			if a.RepoPrefix != b.RepoPrefix {
				return a.RepoPrefix < b.RepoPrefix
			}
			if a.FilePath != b.FilePath {
				return a.FilePath < b.FilePath
			}
			return a.Line < b.Line
		})
		e.Schema = promoteSchemaFromLocations(e.Locations)
		e.ProviderSchema = promoteSchemaForRole(e.Locations, "provider")
		e.ConsumerSchema = promoteSchemaForRole(e.Locations, "consumer")
		e.IsTestOnly = locationsAllTagged(e.Locations, "is_test")
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Producer != out[j].Producer {
			return out[i].Producer < out[j].Producer
		}
		return out[i].Name < out[j].Name
	})
	WriteJSON(w, http.StatusOK, map[string]any{"contracts": out})
}

// --- /v1/contracts/validate ---
//
// Passes through to the contracts MCP tool's `validate` action and
// returns its JSON payload unchanged: `{issues: [...], summary: {...}}`.
// The UI consumes this to badge contract rows with breaking-change
// counts and render a per-contract diff panel.

func (h *Handler) handleContractsValidate(w http.ResponseWriter, r *http.Request) {
	raw, err := h.CallToolStrict(r.Context(), "analyze", map[string]any{"kind": "contracts", "action": "validate"})
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw == "" {
		WriteJSON(w, http.StatusOK, map[string]any{
			"issues":  []any{},
			"summary": map[string]int{"total": 0, "breaking": 0, "warning": 0, "info": 0},
		})
		return
	}
	// The tool returns JSON we can relay verbatim — re-decoding to a
	// Go map would lose the severity enum's string form. Only thing
	// we do is verify it parses as JSON; malformed input falls back
	// to an empty payload rather than propagating a server error.
	var probe any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		WriteJSON(w, http.StatusOK, map[string]any{
			"issues":  []any{},
			"summary": map[string]int{"total": 0, "breaking": 0, "warning": 0, "info": 0},
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(raw))
}

func uiContractKind(raw string) string {
	switch raw {
	case "topic":
		return "EVENT"
	case "ws":
		return "URL"
	case "http", "grpc", "graphql", "openapi":
		return "REST"
	case "env":
		return "ENV"
	case "dependency":
		return "DEP"
	default:
		return strings.ToUpper(raw)
	}
}

// promoteSchemaForRole extracts a schema summary from every location
// matching `role` (provider | consumer). Multiple locations for the
// same role are merged additively — first non-empty value wins for
// type fields, lists union. Returns nil when no matching location has
// any schema info.
func promoteSchemaForRole(locs []contractLocation, role string) *contractSchema {
	var primary *contractLocation
	for i := range locs {
		if locs[i].Role != role || locs[i].Meta == nil {
			continue
		}
		primary = &locs[i]
		break
	}
	if primary == nil {
		return nil
	}
	s := &contractSchema{
		RequestType:      metaString(primary.Meta, "request_type"),
		ResponseType:     metaString(primary.Meta, "response_type"),
		RequestExpr:      metaString(primary.Meta, "request_expr"),
		ResponseExpr:     metaString(primary.Meta, "response_expr"),
		ResponseRepeated: metaBool(primary.Meta, "response_repeated"),
		ResponseShape:    normaliseShape(primary.Meta["response_shape"]),
		RequestShape:     normaliseShape(primary.Meta["request_shape"]),
		ResponseEnvelope: metaEnvelope(primary.Meta, "response_envelope"),
		Source:           metaString(primary.Meta, "schema_source"),
		PathParams:       metaStrings(primary.Meta, "path_params"),
		PathParamNames:   metaStrings(primary.Meta, "path_param_names"),
		QueryParams:      metaStrings(primary.Meta, "query_params"),
		StatusCodes:      metaInts(primary.Meta, "status_codes"),
	}
	if v, _ := primary.Meta["request_stream"].(bool); v {
		s.RequestStream = true
	}
	if v, _ := primary.Meta["response_stream"].(bool); v {
		s.ResponseStream = true
	}
	// Merge secondary same-role locations (multi-consumer case).
	for i := range locs {
		l := &locs[i]
		if l == primary || l.Role != role || l.Meta == nil {
			continue
		}
		if s.RequestType == "" {
			s.RequestType = metaString(l.Meta, "request_type")
		}
		if s.ResponseType == "" {
			s.ResponseType = metaString(l.Meta, "response_type")
		}
		if len(s.StatusCodes) == 0 {
			s.StatusCodes = metaInts(l.Meta, "status_codes")
		}
		if len(s.QueryParams) == 0 {
			s.QueryParams = metaStrings(l.Meta, "query_params")
		}
		if len(s.ResponseEnvelope) == 0 {
			s.ResponseEnvelope = metaEnvelope(l.Meta, "response_envelope")
		}
	}
	if schemaIsEmpty(s) {
		return nil
	}
	return s
}

// locationsAllTagged reports whether every location carries the named
// boolean meta flag (e.g. "is_test"). Empty location list returns
// false — there's nothing to be uniformly tagged. Used to roll up the
// per-location is_test stamp into a single is_test_only contractEntry
// flag.
func locationsAllTagged(locs []contractLocation, key string) bool {
	if len(locs) == 0 {
		return false
	}
	for _, l := range locs {
		if v, _ := l.Meta[key].(bool); !v {
			return false
		}
	}
	return true
}

// promoteSchemaFromLocations folds schema-shape meta from the primary
// location (provider first, consumer fallback) into a flat
// contractSchema for UI rendering. Returns nil when nothing useful
// was pinned down so the wire shape stays compact.
func promoteSchemaFromLocations(locs []contractLocation) *contractSchema {
	var primary contractLocation
	found := false
	for _, l := range locs {
		if l.Role == "provider" {
			primary = l
			found = true
			break
		}
	}
	if !found && len(locs) > 0 {
		primary = locs[0]
		found = true
	}
	if !found || primary.Meta == nil {
		return nil
	}
	s := &contractSchema{
		RequestType:      metaString(primary.Meta, "request_type"),
		ResponseType:     metaString(primary.Meta, "response_type"),
		RequestExpr:      metaString(primary.Meta, "request_expr"),
		ResponseExpr:     metaString(primary.Meta, "response_expr"),
		ResponseRepeated: metaBool(primary.Meta, "response_repeated"),
		ResponseShape:    normaliseShape(primary.Meta["response_shape"]),
		RequestShape:     normaliseShape(primary.Meta["request_shape"]),
		ResponseEnvelope: metaEnvelope(primary.Meta, "response_envelope"),
		Source:           metaString(primary.Meta, "schema_source"),
		PathParams:       metaStrings(primary.Meta, "path_params"),
		PathParamNames:   metaStrings(primary.Meta, "path_param_names"),
		QueryParams:      metaStrings(primary.Meta, "query_params"),
		StatusCodes:      metaInts(primary.Meta, "status_codes"),
	}
	if v, _ := primary.Meta["request_stream"].(bool); v {
		s.RequestStream = true
	}
	if v, _ := primary.Meta["response_stream"].(bool); v {
		s.ResponseStream = true
	}
	// If a provider had nothing but a consumer also has meta, try
	// filling gaps from it — useful for contracts where the consumer
	// (e.g. a generated SDK) carries types the provider didn't
	// annotate.
	for _, l := range locs {
		if l.Role == primary.Role || l.Meta == nil {
			continue
		}
		if s.RequestType == "" {
			s.RequestType = metaString(l.Meta, "request_type")
		}
		if s.ResponseType == "" {
			s.ResponseType = metaString(l.Meta, "response_type")
		}
		if len(s.StatusCodes) == 0 {
			s.StatusCodes = metaInts(l.Meta, "status_codes")
		}
		if len(s.QueryParams) == 0 {
			s.QueryParams = metaStrings(l.Meta, "query_params")
		}
		if len(s.ResponseEnvelope) == 0 {
			s.ResponseEnvelope = metaEnvelope(l.Meta, "response_envelope")
		}
	}
	if schemaIsEmpty(s) {
		return nil
	}
	return s
}

func schemaIsEmpty(s *contractSchema) bool {
	if s == nil {
		return true
	}
	return s.RequestType == "" && s.ResponseType == "" && s.RequestExpr == "" && s.ResponseExpr == "" &&
		len(s.ResponseEnvelope) == 0 &&
		len(s.ResponseShape) == 0 && len(s.RequestShape) == 0 &&
		len(s.PathParams) == 0 && len(s.QueryParams) == 0 && len(s.StatusCodes) == 0 &&
		!s.RequestStream && !s.ResponseStream && !s.ResponseRepeated
}

// metaEnvelope decodes the upstream "response_envelope" meta entry —
// emitted by the contract enrichers as a []map[string]any with name /
// type / expr keys per row. The intermediate JSON pass between
// daemon and dashboard turns the slice into []any of map[string]any,
// so we accept both shapes.
func metaEnvelope(m map[string]any, key string) []envelopeFieldDTO {
	raw := m[key]
	if raw == nil {
		return nil
	}
	var rows []map[string]any
	switch v := raw.(type) {
	case []map[string]any:
		rows = v
	case []any:
		rows = make([]map[string]any, 0, len(v))
		for _, x := range v {
			if row, ok := x.(map[string]any); ok {
				rows = append(rows, row)
			}
		}
	}
	if len(rows) == 0 {
		return nil
	}
	out := make([]envelopeFieldDTO, 0, len(rows))
	for _, row := range rows {
		f := envelopeFieldDTO{}
		if s, ok := row["name"].(string); ok {
			f.Name = s
		}
		if s, ok := row["type"].(string); ok {
			f.Type = s
		}
		if s, ok := row["expr"].(string); ok {
			f.Expr = s
		}
		if v, ok := row["repeated"].(bool); ok {
			f.Repeated = v
		}
		// Shape arrives either as a *contracts.Shape (in-process call)
		// or as map[string]any (post-JSON-roundtrip on the wire). The
		// dashboard re-emits it as JSON either way, so we normalise to
		// map[string]any here.
		f.Shape = normaliseShape(row["shape"])
		if f.Name != "" {
			out = append(out, f)
		}
	}
	return out
}

// normaliseShape coerces whatever the contracts MCP tool emitted for
// a "shape" Meta value into a plain map[string]any so the dashboard
// can re-emit it as JSON. Already-decoded JSON arrives as
// map[string]any; gob-decoded paths produce *contracts.Shape (or its
// equivalent struct) which we marshal+unmarshal once.
func normaliseShape(raw any) map[string]any {
	if raw == nil {
		return nil
	}
	if m, ok := raw.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func metaString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func metaBool(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func metaStrings(m map[string]any, key string) []string {
	switch v := m[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func metaInts(m map[string]any, key string) []int {
	switch v := m[key].(type) {
	case []int:
		return v
	case []any:
		out := make([]int, 0, len(v))
		for _, x := range v {
			switch n := x.(type) {
			case int:
				out = append(out, n)
			case float64:
				out = append(out, int(n))
			}
		}
		return out
	}
	return nil
}

// contractScope decides whether a merged contract row represents something
// defined in the active project ("own") or something it only consumes
// ("external"). Go-module / package dependency contracts are always
// external by construction; for the rest, a row is own iff we found a
// provider inside the active-project repo set.
func contractScope(rawType, producer string) string {
	if rawType == "dependency" {
		return "external"
	}
	if producer == "" {
		return "external"
	}
	return "own"
}

// canonicalContractIDParam matches any {name} path-parameter placeholder
// in a contract ID. We rewrite the name to a positional {p1}, {p2}, …
// so the same HTTP route matches regardless of whether the provider
// called the param {id} and the consumer called it {entityId}.
var canonicalContractIDParam = regexp.MustCompile(`\{[^}]+\}`)

// canonicalContractKey returns a merge-stable key for a raw contract
// ID. Only IDs whose first segment is "http" are rewritten; other
// contract types (grpc, topic, env, dependency) already have structural
// IDs that don't embed param names.
func canonicalContractKey(id string) string {
	if !strings.HasPrefix(id, "http::") {
		return id
	}
	i := 0
	return canonicalContractIDParam.ReplaceAllStringFunc(id, func(string) string {
		i++
		return fmt.Sprintf("{p%d}", i)
	})
}

// --- /v1/communities ---
//
// Returns the community detection result reshaped for the dashboard
// communities card. The MCP get_communities list summary already
// carries the majority repo prefix and file count so we don't need a
// second call per community.

type communityEntry struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Repo     string  `json:"repo"`
	Symbols  int     `json:"symbols"`
	Files    int     `json:"files"`
	Cohesion float64 `json:"cohesion"`
	// ParentID points at the phase-2 super-community this leaf
	// belongs to. Sibling clusters under the same ParentID get
	// grouped under a shared header in the dashboard UI.
	ParentID string `json:"parent_id,omitempty"`
}

func (h *Handler) handleCommunities(w http.ResponseWriter, r *http.Request) {
	raw, err := h.CallToolStrict(r.Context(), "analyze", map[string]any{"kind": "communities"})
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw == "" {
		WriteJSON(w, http.StatusOK, map[string]any{"communities": []communityEntry{}, "modularity": 0.0})
		return
	}
	type rawComm struct {
		ID         string  `json:"id"`
		Label      string  `json:"label"`
		Size       int     `json:"size"`
		FileCount  int     `json:"file_count"`
		Cohesion   float64 `json:"cohesion"`
		RepoPrefix string  `json:"repo_prefix"`
		ParentID   string  `json:"parent_id,omitempty"`
	}
	type wrap struct {
		Communities []rawComm `json:"communities"`
		Modularity  float64   `json:"modularity"`
	}
	var w0 wrap
	if err := json.Unmarshal([]byte(raw), &w0); err != nil {
		WriteJSON(w, http.StatusOK, map[string]any{"communities": []communityEntry{}, "modularity": 0.0})
		return
	}
	out := make([]communityEntry, 0, len(w0.Communities))
	for _, c := range w0.Communities {
		out = append(out, communityEntry{
			ID:       c.ID,
			Name:     c.Label,
			Repo:     c.RepoPrefix,
			Symbols:  c.Size,
			Files:    c.FileCount,
			Cohesion: c.Cohesion,
			ParentID: c.ParentID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbols > out[j].Symbols })
	WriteJSON(w, http.StatusOK, map[string]any{
		"communities": out,
		"modularity":  w0.Modularity,
	})
}

// --- /v1/guards ---
//
// Wraps check_guards into the table shape used by the Guards page. The
// MCP tool returns per-rule violations; we group by rule and report a
// status (ok | warn | violated) with the hit count.

type guardEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Hits   int    `json:"hits"`
	Scope  string `json:"scope"`
}

// handleGuards lists guard rules from configuration. Status is "ok" by
// default — rules don't have a runtime "violated" state until evaluated
// against a change set, which is the job of the check_guards MCP tool
// (callable via /v1/tools/check_guards with an `ids` argument). The
// page shows what's configured; the IDE / agent gets violations
// per-change.
func (h *Handler) handleGuards(w http.ResponseWriter, _ *http.Request) {
	out := make([]guardEntry, 0)
	seen := make(map[string]bool)
	add := func(rules []struct {
		Name, Kind, Source, Target string
	}) {
		for i, r := range rules {
			key := r.Name + "::" + r.Source + "::" + r.Target
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, guardEntry{
				ID:     fmt.Sprintf("%s-%d", r.Name, i),
				Name:   r.Name,
				Kind:   r.Kind,
				Status: "ok",
				Hits:   0,
				Scope:  r.Source + " → " + r.Target,
			})
		}
	}
	if h.configManager != nil {
		// Workspace overrides per active repo + global defaults at "".
		repos := append([]string{""}, repoNames(h.configManager.ActiveRepos())...)
		for _, name := range repos {
			rules := h.configManager.EffectiveGuardRules(name)
			compact := make([]struct {
				Name, Kind, Source, Target string
			}, 0, len(rules))
			for _, r := range rules {
				compact = append(compact, struct {
					Name, Kind, Source, Target string
				}{r.Name, r.Kind, r.Source, r.Target})
			}
			add(compact)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	WriteJSON(w, http.StatusOK, map[string]any{"guards": out})
}

func repoNames(repos []config.RepoEntry) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		if r.Name != "" {
			out = append(out, r.Name)
		}
	}
	return out
}

// --- /v1/caveats ---

type caveatEntry struct {
	ID       string `json:"id"`
	Severity string `json:"severity"` // risk | hot | cycle | unowned | deprecated | boundary
	Symbol   string `json:"symbol"`
	Title    string `json:"title"`
	Desc     string `json:"desc"`
	Owner    string `json:"owner"`
	Age      string `json:"age"`
	// Graph-enriched fields. Populated in handleCaveats after parsing,
	// by looking up the symbol in the graph — all optional because cycle
	// entries and synthetic caveats may have no resolvable node.
	FilePath        string   `json:"file_path,omitempty"`
	RepoPrefix      string   `json:"repo_prefix,omitempty"`
	Kind            string   `json:"kind,omitempty"`
	FanIn           int      `json:"fan_in,omitempty"`
	ExternalCallers int      `json:"external_callers,omitempty"`
	CallerRepos     []string `json:"caller_repos,omitempty"`
}

func (h *Handler) handleCaveats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := make([]caveatEntry, 0, 32)

	// check_guards is intentionally NOT called here — the MCP tool
	// requires a `changed symbol IDs` argument and only returns
	// violations against that change set. The Caveats page is supposed
	// to surface persistent landmines, not what would fire if a
	// specific commit ran. Boundary/ownership violations come from the
	// /v1/guards endpoint instead.
	for _, kind := range []string{"hotspots", "dead_code", "cycles"} {
		raw, err := h.CallToolStrict(ctx, "analyze", map[string]any{"kind": kind, "limit": 20})
		if err != nil {
			h.logger.Warn("caveats: analyze sub-call failed; section will be empty",
				zap.String("kind", kind), zap.Error(err))
			continue
		}
		if raw == "" {
			continue
		}
		switch kind {
		case "hotspots":
			out = append(out, parseHotspots(raw)...)
		case "dead_code":
			out = append(out, parseDeadCode(raw)...)
		case "cycles":
			out = append(out, parseCycles(raw)...)
		}
	}

	severityRank := map[string]int{
		"risk":       0,
		"hot":        1,
		"cycle":      2,
		"boundary":   3,
		"unowned":    4,
		"deprecated": 5,
	}
	sortByRank(out, severityRank)
	enrichCaveats(h.Graph(), out)
	WriteJSON(w, http.StatusOK, map[string]any{"caveats": out})
}

// enrichCaveats fills in file_path, repo_prefix, kind, fan_in, and
// cross-repo caller counts by looking each caveat's symbol up in the
// graph. Entries with an unresolvable symbol (e.g. cycle placeholders
// or stale IDs from a prior index) are left untouched so the caller can
// detect the gap instead of rendering zeros that look like real data.
func enrichCaveats(g graph.Store, caveats []caveatEntry) {
	if g == nil {
		return
	}
	for i := range caveats {
		sym := caveats[i].Symbol
		if sym == "" {
			continue
		}
		node := g.GetNode(sym)
		if node == nil {
			continue
		}
		caveats[i].FilePath = node.FilePath
		caveats[i].RepoPrefix = node.RepoPrefix
		caveats[i].Kind = string(node.Kind)

		inEdges := g.GetInEdges(sym)
		repoSet := make(map[string]struct{}, len(inEdges))
		var fanIn, external int
		for _, e := range inEdges {
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
				continue
			}
			fanIn++
			caller := g.GetNode(e.From)
			if caller == nil {
				continue
			}
			if caller.RepoPrefix != "" && caller.RepoPrefix != node.RepoPrefix {
				external++
				repoSet[caller.RepoPrefix] = struct{}{}
			}
		}
		caveats[i].FanIn = fanIn
		caveats[i].ExternalCallers = external
		if len(repoSet) > 0 {
			repos := make([]string, 0, len(repoSet))
			for r := range repoSet {
				repos = append(repos, r)
			}
			sort.Strings(repos)
			caveats[i].CallerRepos = repos
		}
	}
}

func sortByRank(in []caveatEntry, rank map[string]int) {
	for i := 1; i < len(in); i++ {
		j := i
		for j > 0 && rank[in[j-1].Severity] > rank[in[j].Severity] {
			in[j-1], in[j] = in[j], in[j-1]
			j--
		}
	}
}

// All four parsers below produce IDs that combine a per-parser prefix
// with the entry index. The index is essential — the underlying tools
// can return entries with empty IDs (e.g. cycles, where the natural ID
// is the path), and the React UI uses the ID as a list key. Without
// the index suffix, multiple empty-ID entries collapse to the same key
// and React warns about duplicates.

func parseHotspots(raw string) []caveatEntry {
	// Mirrors analysis.HotspotEntry.
	type hotspot struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		FilePath string `json:"file_path"`
		Line     int    `json:"start_line"`
		FanIn    int    `json:"fan_in"`
	}
	type wrap struct {
		Hotspots []hotspot `json:"hotspots"`
	}
	var w wrap
	if err := json.Unmarshal([]byte(raw), &w); err != nil || len(w.Hotspots) == 0 {
		return nil
	}
	out := make([]caveatEntry, 0, len(w.Hotspots))
	for i, h := range w.Hotspots {
		if i >= 10 {
			break
		}
		name := h.Name
		if name == "" {
			name = h.ID
		}
		out = append(out, caveatEntry{
			ID:       fmt.Sprintf("hs-%d-%s", i, h.ID),
			Severity: "hot",
			Symbol:   h.ID,
			Title:    "Hot path · " + name,
			Desc:     fmt.Sprintf("Fan-in %d — touched by many call sites.", h.FanIn),
			Age:      "ongoing",
		})
	}
	return out
}

func parseDeadCode(raw string) []caveatEntry {
	// Mirrors analysis.DeadCodeEntry.
	type entry struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		FilePath string `json:"file_path"`
	}
	type wrap struct {
		DeadCode []entry `json:"dead_code"`
	}
	var w wrap
	if err := json.Unmarshal([]byte(raw), &w); err != nil || len(w.DeadCode) == 0 {
		return nil
	}
	out := make([]caveatEntry, 0, len(w.DeadCode))
	for i, e := range w.DeadCode {
		if i >= 10 {
			break
		}
		name := e.Name
		if name == "" {
			name = e.ID
		}
		out = append(out, caveatEntry{
			ID:       fmt.Sprintf("dc-%d-%s", i, e.ID),
			Severity: "deprecated",
			Symbol:   e.ID,
			Title:    "Likely unreachable · " + name,
			Desc:     "No incoming references in the indexed graph.",
		})
	}
	return out
}

func parseCycles(raw string) []caveatEntry {
	// Mirrors analysis.Cycle: { path[], kind, severity }. Earlier
	// versions of this parser looked for `id` and `members`, which the
	// real type doesn't have — every cycle ended up with an empty ID,
	// and the dashboard's React keys collided. Now the entry index is
	// part of the ID so duplicate or empty paths still render distinctly.
	type cycle struct {
		Path     []string `json:"path"`
		Kind     string   `json:"kind"`
		Severity int      `json:"severity"`
	}
	type wrap struct {
		Cycles []cycle `json:"cycles"`
	}
	var w wrap
	if err := json.Unmarshal([]byte(raw), &w); err != nil || len(w.Cycles) == 0 {
		return nil
	}
	out := make([]caveatEntry, 0, len(w.Cycles))
	for i, c := range w.Cycles {
		if i >= 10 {
			break
		}
		title := "Circular dependency"
		symbol := ""
		if len(c.Path) > 0 {
			symbol = c.Path[0]
			title = "Cycle: " + symbol
		}
		desc := fmt.Sprintf("%d symbols form a %s cycle.", len(c.Path), nonEmpty(c.Kind, "dependency"))
		out = append(out, caveatEntry{
			ID:       fmt.Sprintf("cy-%d-%s", i, symbol),
			Severity: "cycle",
			Symbol:   symbol,
			Title:    title,
			Desc:     desc,
		})
	}
	return out
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// --- /v1/dashboard ---
//
// Bundles every datum the dashboard hero card needs into one round-trip:
// totals, kinds + languages (as ordered arrays so the UI doesn't sort),
// repo cards, recent activity, and aggregated caveats. Designed to be
// cheap (one pass through stats + cached analyze tool results).

type dashboardSnapshot struct {
	Stats struct {
		TotalNodes int    `json:"total_nodes"`
		TotalEdges int    `json:"total_edges"`
		Repos      int    `json:"repos"`
		Caveats    int    `json:"caveats"`
		Version    string `json:"version"`
	} `json:"stats"`
	Kinds     []kvEntry                  `json:"kinds"`
	Languages []kvEntry                  `json:"languages"`
	Repos     []repoEntry                `json:"repos"`
	Activity  []indexer.GraphChangeEvent `json:"activity"`
	Caveats   []caveatEntry              `json:"caveats"`
	Processes []processEntry             `json:"processes"`
}

type kvEntry struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func mapToOrderedKV(m map[string]int) []kvEntry {
	out := make([]kvEntry, 0, len(m))
	for k, v := range m {
		out = append(out, kvEntry{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	stats := h.graph.Stats()
	snap := dashboardSnapshot{}
	snap.Stats.TotalNodes = stats.TotalNodes
	snap.Stats.TotalEdges = stats.TotalEdges
	snap.Stats.Version = h.version
	snap.Kinds = mapToOrderedKV(stats.ByKind)
	snap.Languages = mapToOrderedKV(stats.ByLanguage)
	snap.Repos = reposFromGraph(h.graph)
	snap.Stats.Repos = len(snap.Repos)

	if h.activity != nil {
		snap.Activity = h.activity.snapshot(20)
	} else {
		snap.Activity = []indexer.GraphChangeEvent{}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Reuse the caveats aggregator so the count and the inline preview
	// both come from the same data — no chance of the dashboard's
	// number disagreeing with the Caveats page on first load.
	cavs := make([]caveatEntry, 0, 32)
	for _, kind := range []string{"hotspots", "dead_code", "cycles"} {
		raw, err := h.CallToolStrict(ctx, "analyze", map[string]any{"kind": kind, "limit": 20})
		if err != nil {
			h.logger.Warn("dashboard: analyze sub-call failed; caveats section will be partial",
				zap.String("kind", kind), zap.Error(err))
			continue
		}
		if raw == "" {
			continue
		}
		switch kind {
		case "hotspots":
			cavs = append(cavs, parseHotspots(raw)...)
		case "dead_code":
			cavs = append(cavs, parseDeadCode(raw)...)
		case "cycles":
			cavs = append(cavs, parseCycles(raw)...)
		}
	}
	snap.Caveats = cavs
	snap.Stats.Caveats = len(cavs)

	// Top processes for the inline preview. The full list is on the
	// Processes page; here we cap at 6 so the dashboard stays compact.
	if raw, err := h.CallToolStrict(ctx, "analyze", map[string]any{"kind": "processes"}); err != nil {
		h.logger.Warn("dashboard: get_processes failed; processes section will be empty",
			zap.Error(err))
	} else if raw != "" {
		type wrap struct {
			Processes []rawProcessSummary `json:"processes"`
		}
		var w0 wrap
		if json.Unmarshal([]byte(raw), &w0) == nil {
			procs := make([]processEntry, 0, len(w0.Processes))
			for _, p := range w0.Processes {
				procs = append(procs, processEntryFromRaw(p))
			}
			sort.Slice(procs, func(i, j int) bool { return procs[i].Score > procs[j].Score })
			if len(procs) > 6 {
				procs = procs[:6]
			}
			snap.Processes = procs
		}
	}
	if snap.Processes == nil {
		snap.Processes = []processEntry{}
	}

	WriteJSON(w, http.StatusOK, snap)
}
