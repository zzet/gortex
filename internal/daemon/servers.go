package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/zzet/gortex/internal/platform"
)

// ServerEntry describes one Gortex server reachable from the daemon.
// Lifted out of `~/.gortex/servers.toml`.
//
// The "slug" is the local identity used in CLI output, log lines, and
// daemon-side routing. URL accepts both TCP (`http://host:port`,
// `https://...`) and Unix-domain socket forms (`unix:///path/to.sock`).
//
// Auth: prefer AuthTokenEnv (an env-var name the daemon resolves at
// connect time) over AuthToken (a literal value). Putting raw
// secrets in `servers.toml` is allowed for parity with how
// `~/.gortex/config.yaml` already gets written by `gortex
// track`, but the env-var form is the recommended path.
//
// Workspaces is the optional pre-declared roster: when set, the
// daemon trusts this list without making the `GET
// /v1/workspaces/<ws>/repos` roundtrip on first use. Empty means
// "discover at runtime"; `WorkspaceRosterCache` falls back to
// querying the server for the roster on demand.
type ServerEntry struct {
	Slug         string   `toml:"slug"`
	URL          string   `toml:"url"`
	AuthToken    string   `toml:"auth_token,omitempty"`
	AuthTokenEnv string   `toml:"auth_token_env,omitempty"`
	Workspaces   []string `toml:"workspaces,omitempty"`
	// Default flips a single ServerEntry to the "use me when no
	// workspace context disambiguates" pick. Conflict (multiple
	// entries marked Default=true) is rejected at load time.
	Default bool `toml:"default,omitempty"`

	// Enabled gates whether the daemon federates/proxies to this
	// remote. Pointer-bool so absent-in-TOML (nil) is distinguished
	// from an explicit false: absent ⇒ enabled (default-on, so a
	// pre-existing roster that has no `enabled` key keeps working).
	// `gortex proxy off <slug>` writes `enabled = false`; `on` deletes
	// the key (back to default-on) — it never writes `enabled = true`
	// so a hand-edited roster without the key stays clean.
	Enabled *bool `toml:"enabled,omitempty"`

	// ReadOnly marks the remote as accepting only read tools; the
	// federation write-gate refuses to route a mutating tool to it.
	// In v1 every remote is effectively read-only regardless; this
	// field is the persisted intent and the surface for
	// `--read-only`. An unadvertised capability is treated as
	// read-only at runtime (fail-safe).
	ReadOnly bool `toml:"read_only,omitempty"`

	// Namespace is the origin-namespaced node-ID keying prefix used
	// when minting proxy nodes for this remote. Keying field only —
	// reserved here so the struct is edited exactly once.
	Namespace string `toml:"namespace,omitempty"`
}

// IsEnabled reports the effective enabled state of the remote: a nil
// Enabled (absent from TOML) means enabled, preserving backward
// compatibility with rosters written before the key existed.
func (e ServerEntry) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

// ServersConfig is the on-disk schema for `~/.gortex/servers.toml`.
type ServersConfig struct {
	Server []ServerEntry `toml:"server"`
}

// ServersConfigPath returns the path the daemon reads / writes for
// the multi-server roster. Order of preference mirrors SocketPath:
//
//  1. $GORTEX_DAEMON_SERVERS — explicit override (tests, custom
//     deployments).
//  2. $HOME/.gortex/servers.toml — the canonical user-level file.
//     It lives in the unified `~/.gortex/` tree alongside the global
//     `config.yaml`, the same place tracking scripts and `gortex
//     daemon` already write to. An absolute $XDG_CONFIG_HOME relocates
//     this to <XDG_CONFIG_HOME>/gortex/servers.toml.
//  3. $TEMPDIR/gortex-servers.toml — last-resort fallback so the
//     daemon can still come up in an environment with no $HOME.
func ServersConfigPath() string {
	if override := os.Getenv("GORTEX_DAEMON_SERVERS"); override != "" {
		return override
	}
	if _, err := os.UserHomeDir(); err != nil {
		if v := os.Getenv("XDG_CONFIG_HOME"); v == "" || !filepath.IsAbs(v) {
			return filepath.Join(os.TempDir(), "gortex-servers.toml")
		}
	}
	return filepath.Join(platform.ConfigDir(), "servers.toml")
}

// LoadServersConfig reads and validates ~/.gortex/servers.toml. A
// missing file is not an error — the daemon may run with zero
// configured servers (e.g. before a user sets one up). Empty/zero
// ServersConfig is returned in that case.
//
// Validation rejects:
//   - duplicate slugs
//   - empty URLs
//   - URLs that aren't `http(s)://...` or `unix://...`
//   - more than one ServerEntry marked Default=true
func LoadServersConfig(path string) (*ServersConfig, error) {
	if path == "" {
		path = ServersConfigPath()
	}
	cfg := &ServersConfig{}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read servers config %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse servers config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate servers config %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes the config back to `path` (or ServersConfigPath() when
// empty) atomically: marshal to TOML, write to a sibling temp file,
// fsync, rename into place. The parent directory is created with 0700
// and the file with 0600 — matching the daemon's existing convention
// for files that can hold auth tokens.
//
// Validate is run first; an invalid config is never persisted.
func (c *ServersConfig) Save(path string) error {
	if c == nil {
		return fmt.Errorf("nil ServersConfig")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if path == "" {
		path = ServersConfigPath()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	data, err := toml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal servers config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "servers-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("fsync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}

// AddServer appends an entry, rejecting on duplicate slug or any other
// invariant violation (so callers can rely on Validate succeeding
// after a successful AddServer). Setting Default=true on an entry
// when another is already default is rejected here rather than
// auto-flipping the existing one — the explicit error keeps the
// "exactly one default" rule visible to the user.
func (c *ServersConfig) AddServer(entry ServerEntry) error {
	if c == nil {
		return fmt.Errorf("nil ServersConfig")
	}
	if entry.Slug == "" {
		return fmt.Errorf("slug is required")
	}
	if c.FindBySlug(entry.Slug) != nil {
		return fmt.Errorf("server %q already exists", entry.Slug)
	}
	c.Server = append(c.Server, entry)
	if err := c.Validate(); err != nil {
		// Roll back so the in-memory config matches what would have
		// been persisted.
		c.Server = c.Server[:len(c.Server)-1]
		return err
	}
	return nil
}

// RemoveServer drops the entry with the given slug. Returns false
// (no error) when the slug isn't found — matches the typical CLI
// idempotency expectation. Returns an error only on a nil receiver.
func (c *ServersConfig) RemoveServer(slug string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("nil ServersConfig")
	}
	for i := range c.Server {
		if c.Server[i].Slug == slug {
			c.Server = append(c.Server[:i], c.Server[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// Validate enforces the ServersConfig invariants.
func (c *ServersConfig) Validate() error {
	if c == nil {
		return nil
	}
	seenSlug := make(map[string]bool, len(c.Server))
	defaultCount := 0
	for i, s := range c.Server {
		if s.Slug == "" {
			return fmt.Errorf("server[%d]: slug is required", i)
		}
		if s.Slug == LocalServerSentinel {
			return fmt.Errorf("server[%d]: slug %q is reserved for the local daemon's own graph", i, s.Slug)
		}
		if seenSlug[s.Slug] {
			return fmt.Errorf("server[%d]: duplicate slug %q", i, s.Slug)
		}
		seenSlug[s.Slug] = true
		if s.URL == "" {
			return fmt.Errorf("server[%q]: url is required", s.Slug)
		}
		if !isSupportedServerURL(s.URL) {
			return fmt.Errorf("server[%q]: url %q must start with http://, https://, or unix://", s.Slug, s.URL)
		}
		if s.Default {
			defaultCount++
			if defaultCount > 1 {
				return fmt.Errorf("server[%q]: only one entry may set default=true", s.Slug)
			}
		}
	}
	return nil
}

// isSupportedServerURL allows http(s) for TCP and unix:// for socket
// transports — the same shape gortex server's --bind flag accepts.
func isSupportedServerURL(u string) bool {
	switch {
	case strings.HasPrefix(u, "http://"),
		strings.HasPrefix(u, "https://"),
		strings.HasPrefix(u, "unix://"):
		return true
	}
	return false
}

// FindBySlug returns the ServerEntry with the given slug, or nil.
func (c *ServersConfig) FindBySlug(slug string) *ServerEntry {
	if c == nil {
		return nil
	}
	for i := range c.Server {
		if c.Server[i].Slug == slug {
			return &c.Server[i]
		}
	}
	return nil
}

// DefaultServer returns the entry marked Default=true, falling back
// to the first server in the list when no explicit default is set.
// Returns nil only when the list is empty.
func (c *ServersConfig) DefaultServer() *ServerEntry {
	if c == nil || len(c.Server) == 0 {
		return nil
	}
	for i := range c.Server {
		if c.Server[i].Default {
			return &c.Server[i]
		}
	}
	return &c.Server[0]
}

// SetEnabled flips a remote's enabled state in the roster. enabled=true
// CLEARS the key (back to default-on) rather than writing
// `enabled = true`, so a hand-edited roster without the key stays
// minimal and the default-on contract is the single source of truth;
// enabled=false writes an explicit false. It returns whether the value
// actually changed and errors on an unknown slug.
func (c *ServersConfig) SetEnabled(slug string, enabled bool) (changed bool, err error) {
	if c == nil {
		return false, fmt.Errorf("no server roster loaded")
	}
	for i := range c.Server {
		if c.Server[i].Slug != slug {
			continue
		}
		was := c.Server[i].IsEnabled()
		if enabled {
			// Clear the key: nil ⇒ default-on.
			c.Server[i].Enabled = nil
		} else {
			v := false
			c.Server[i].Enabled = &v
		}
		return was != enabled, nil
	}
	return false, fmt.Errorf("unknown server slug %q", slug)
}

// ServerClient is the daemon-side HTTP client targeting one
// ServerEntry. Holds a transport configured for the entry's URL form
// (TCP vs unix:// chooses Dial vs DialUnix transparently).
//
// The client is goroutine-safe; the daemon shares one per slug
// across multiple worker goroutines and reuses connections via the
// underlying http.Transport.
type ServerClient struct {
	Entry      ServerEntry
	BaseURL    string // canonical form passed to http.NewRequest
	httpClient *http.Client
}

// NewServerClient builds a ServerClient. For unix:// entries the
// returned client dials the socket on every request; HTTP keep-alive
// works the same as for TCP. The auth token is resolved at request
// time so an env-var rotation lands on the next call.
func NewServerClient(entry ServerEntry) (*ServerClient, error) {
	if !isSupportedServerURL(entry.URL) {
		return nil, fmt.Errorf("server %q: unsupported url scheme: %s", entry.Slug, entry.URL)
	}
	cli := &ServerClient{Entry: entry}
	if strings.HasPrefix(entry.URL, "unix://") {
		// Strip the scheme so http.Request takes a relative path; the
		// dialer ignores the host part. Use a synthetic
		// http://unix/<path> base so url.Parse downstream stays
		// happy.
		socket := strings.TrimPrefix(entry.URL, "unix://")
		cli.BaseURL = "http://unix"
		cli.httpClient = &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		}
	} else {
		// TCP / TLS — Go's default transport handles both http:// and
		// https:// URLs; we just plug a sane timeout so a hung server
		// doesn't wedge the daemon.
		cli.BaseURL = strings.TrimRight(entry.URL, "/")
		cli.httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return cli, nil
}

// resolveAuthToken returns the bearer token for the next request,
// resolving AuthTokenEnv against os.Getenv each call so rotated
// tokens take effect without a daemon restart.
func (c *ServerClient) resolveAuthToken() string {
	if c.Entry.AuthToken != "" {
		return c.Entry.AuthToken
	}
	if c.Entry.AuthTokenEnv != "" {
		return os.Getenv(c.Entry.AuthTokenEnv)
	}
	return ""
}

// ProxyIdentity is the caller-side view identity a proxied tool call carries
// to the remote.
//
// Without it the remote resolves its own view from the body's `cwd` argument
// alone, so the SAME tools/call answers about a different view depending on
// whether it was served locally or proxied: the local seam binds the request
// view from the session's cwd (which arrives as the `X-Gortex-Cwd` header or
// the transport's session state, not necessarily as a tool argument), while
// the remote — seeing neither header — falls back to its base corpus. CWD is
// the session's workspace boundary; SessionID is the caller's real MCP session
// id, which the remote's /v1/tools handler reads from `Mcp-Session-Id`.
//
// Both halves are optional: a zero identity forwards nothing and leaves the
// remote exactly where it was.
type ProxyIdentity struct {
	SessionID string
	CWD       string
}

// Empty reports whether this identity would forward nothing.
func (p ProxyIdentity) Empty() bool { return p.SessionID == "" && p.CWD == "" }

// proxyIdentityCtxKey is the private key ProxyIdentity rides on. It is
// deliberately a daemon-package key rather than a read of the MCP package's
// session context values: internal/mcp imports internal/daemon, so the
// dependency cannot run the other way. Every transport that already knows the
// caller's session id and cwd attaches it here before it asks the router to
// decide.
type proxyIdentityCtxKey struct{}

// WithProxyIdentity returns a context carrying the caller's view identity for
// any tool call proxied to a remote server. An empty identity returns ctx
// unchanged so a caller never has to branch.
func WithProxyIdentity(ctx context.Context, id ProxyIdentity) context.Context {
	id.SessionID = strings.TrimSpace(id.SessionID)
	id.CWD = strings.TrimSpace(id.CWD)
	if ctx == nil || id.Empty() {
		return ctx
	}
	return context.WithValue(ctx, proxyIdentityCtxKey{}, id)
}

// ProxyIdentityFromContext returns the identity attached by
// WithProxyIdentity, or the zero identity when none is present.
func ProxyIdentityFromContext(ctx context.Context) ProxyIdentity {
	if ctx == nil {
		return ProxyIdentity{}
	}
	id, _ := ctx.Value(proxyIdentityCtxKey{}).(ProxyIdentity)
	return id
}

// proxyIdentityForCall is the identity a proxied tool call actually carries:
// whatever the caller's transport attached to ctx, completed — never
// overridden — from the call's own `cwd` argument.
//
// The completion half exists because not every producer of a proxied call has
// a transport-level cwd to attach. A front door that only reads headers (the
// /mcp mount before the body-cwd fold below, and the unix-socket dispatcher's
// tryProxyToolCall) leaves ctx carrying no CWD at all, and the remote then
// resolves its view from the body — which is fine for peekRouteContext (it
// prefers the body's cwd) but NOT for the remote's requestViewCWD /
// requestToolContext pair (internal/server/handler.go), which read the header
// and the `?cwd=` query only. So a call whose cwd lives solely in the body
// reaches the remote's view seam with nothing to bind and is answered from the
// remote's base corpus.
//
// Completion is strictly a fallback: a ctx identity always wins, so a caller
// that deliberately scoped the hop (the /v1 front door, which already folded
// the body's cwd into ctx) is never second-guessed here.
func proxyIdentityForCall(ctx context.Context, body []byte) ProxyIdentity {
	id := ProxyIdentityFromContext(ctx)
	if id.CWD == "" {
		id.CWD = toolCallBodyCWD(body)
	}
	return id
}

// toolCallBodyCWD peeks the `cwd` argument off a `POST /v1/tools/<name>` body
// in both shapes the endpoint accepts: nested under `arguments` (what every
// router caller marshals — see the streamable transport's tryRouteToolCall and
// the daemon dispatcher's tryProxyToolCall, which both wrap the raw arguments
// under that key) and top-level (what a hand-written client posts). The
// precedence mirrors internal/server.Handler.peekRouteContext exactly, so the
// value forwarded here is the one the remote would have peeked anyway.
//
// `arguments` is decoded through json.RawMessage because the MCP layer treats
// a missing key and an explicit `null` alike; decoding it as a struct would
// make a `"arguments":null` body fail the whole unmarshal and lose a
// legitimate top-level cwd.
func toolCallBodyCWD(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var envelope struct {
		Arguments json.RawMessage `json:"arguments"`
		Cwd       string          `json:"cwd"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	if len(envelope.Arguments) > 0 {
		var args struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(envelope.Arguments, &args); err == nil {
			if cwd := strings.TrimSpace(args.Cwd); cwd != "" {
				return cwd
			}
		}
	}
	return strings.TrimSpace(envelope.Cwd)
}

// ProxyTool forwards a single MCP tool invocation to this server's
// `POST /v1/tools/<name>` endpoint and returns the raw response
// bytes. Used by the daemon's hybrid-read router when a query's
// scope routes to a remote workspace.
//
// The body is passed through verbatim — the caller (typically the
// daemon's RouteToolCall) is responsible for shape, the server
// handler is responsible for parsing. Returning bytes (not a
// decoded struct) keeps this client agnostic to the per-tool
// response shapes.
func (c *ServerClient) ProxyTool(toolName string, body []byte) ([]byte, int, error) {
	return c.ProxyToolCtx(context.Background(), toolName, body)
}

// ProxyToolCtx is the context-aware successor to ProxyTool. The
// outbound request is built with http.NewRequestWithContext so a
// caller's deadline / cancellation propagates to the in-flight HTTP
// call, instead of being bounded only by the client's coarse timeout.
// The federation read fan-out derives a short per-remote deadline from
// this ctx so a slow or dead remote can never block the query hot path
// for the full HTTP-client timeout.
func (c *ServerClient) ProxyToolCtx(ctx context.Context, toolName string, body []byte) ([]byte, int, error) {
	u, err := url.JoinPath(c.BaseURL, "v1", "tools", toolName)
	if err != nil {
		return nil, 0, fmt.Errorf("join proxy URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, fmt.Errorf("build proxy request: %w", err)
	}
	if tok := c.resolveAuthToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Forward the caller's view identity so the remote resolves the same
	// view a local dispatch would have. These are the two headers the
	// remote's own /v1/tools handler reads (internal/server/handler.go:
	// handleToolCall reads Mcp-Session-Id, requestViewCWD / peekRouteContext
	// read X-Gortex-Cwd); anything absent here is simply not set, leaving the
	// remote where it was. The identity is completed from the call's own
	// `cwd` argument when the caller's transport had none of its own to
	// attach — see proxyIdentityForCall.
	if id := proxyIdentityForCall(ctx, body); !id.Empty() {
		if id.CWD != "" {
			req.Header.Set("X-Gortex-Cwd", id.CWD)
		}
		if id.SessionID != "" {
			req.Header.Set("Mcp-Session-Id", id.SessionID)
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("proxy %s/%s: %w", c.Entry.Slug, toolName, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read proxy response: %w", err)
	}
	return out, resp.StatusCode, nil
}

// FetchWorkspaceRoster calls `GET /v1/workspaces/<ws>/repos` on the
// server and returns the list of repo prefixes in that workspace.
// Used by the daemon's lookup logic to know which server owns a
// given workspace.
func (c *ServerClient) FetchWorkspaceRoster(workspace string) ([]string, error) {
	u, err := url.JoinPath(c.BaseURL, "v1", "workspaces", workspace, "repos")
	if err != nil {
		return nil, fmt.Errorf("join roster URL: %w", err)
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build roster request: %w", err)
	}
	if tok := c.resolveAuthToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch roster from %q: %w", c.Entry.Slug, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrWorkspaceNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server %q roster %s: status %d", c.Entry.Slug, workspace, resp.StatusCode)
	}
	var payload struct {
		Repos []string `json:"repos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode roster: %w", err)
	}
	return payload.Repos, nil
}

// ErrWorkspaceNotFound is returned by FetchWorkspaceRoster when the
// server doesn't host the requested workspace. Distinct from a
// transport error so the daemon's lookup loop can keep iterating
// servers without surfacing a noisy connectivity warning.
var ErrWorkspaceNotFound = errors.New("workspace not found on server")

// WorkspaceRosterCache caches roster lookups per (slug, workspace).
// TTL is intentionally short — the alpha use case is a developer
// adding/removing repos in one of their tracked workspaces and
// expecting the daemon to notice within a minute or two without a
// restart.
type WorkspaceRosterCache struct {
	mu      sync.RWMutex
	entries map[rosterKey]rosterEntry
	ttl     time.Duration
}

type rosterKey struct {
	slug      string
	workspace string
}

type rosterEntry struct {
	repos    []string
	expires  time.Time
	notFound bool
}

// NewWorkspaceRosterCache builds an in-memory cache with the given
// TTL. ttl <= 0 means "always fetch" (no caching).
func NewWorkspaceRosterCache(ttl time.Duration) *WorkspaceRosterCache {
	return &WorkspaceRosterCache{
		entries: make(map[rosterKey]rosterEntry),
		ttl:     ttl,
	}
}

// Lookup returns the cached roster for (slug, workspace) and whether
// the entry is still fresh. notFound==true means the server told us
// the workspace isn't hosted there; callers should not retry until
// the entry expires.
func (c *WorkspaceRosterCache) Lookup(slug, workspace string) (repos []string, found bool, fresh bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[rosterKey{slug: slug, workspace: workspace}]
	if !ok {
		return nil, false, false
	}
	if c.ttl > 0 && time.Now().After(e.expires) {
		return nil, false, false
	}
	return e.repos, !e.notFound, true
}

// Set stores a positive (workspace exists, repos list) result.
func (c *WorkspaceRosterCache) Set(slug, workspace string, repos []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[rosterKey{slug: slug, workspace: workspace}] = rosterEntry{
		repos:   append([]string(nil), repos...),
		expires: time.Now().Add(c.ttl),
	}
}

// SetNotFound stores a negative result (server doesn't host the
// workspace) so callers don't retry until the TTL expires.
func (c *WorkspaceRosterCache) SetNotFound(slug, workspace string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[rosterKey{slug: slug, workspace: workspace}] = rosterEntry{
		notFound: true,
		expires:  time.Now().Add(c.ttl),
	}
}

// Invalidate drops every cached entry — used after a `servers.toml`
// reload or when the daemon learns a server is down.
func (c *WorkspaceRosterCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[rosterKey]rosterEntry)
}

// LookupResult is what RouteForCwd returns: the chosen ServerEntry
// (nil when no server claims the workspace), the workspace slug
// resolved from cwd, and an explanation of which priority fired.
type LookupResult struct {
	Server    *ServerEntry
	Workspace string
	// Source is one of:
	//   "scope-override" — the caller passed an explicit scope (priority 1)
	//   "config-yaml"    — `.gortex.yaml::workspace` walking up from cwd (priority 2)
	//   "roster"         — the daemon found the cwd's repo in a server's roster (priority 3)
	//   "default"        — the servers.toml `default = "<slug>"` entry (priority 4)
	Source string
}

// CwdResolver looks up a workspace slug for a current working directory
// by walking up to find a `.gortex.yaml` with a `workspace:` key. Lifted
// out as an interface so tests can inject a stub instead of touching
// the filesystem.
type CwdResolver func(cwd string) (workspace string, ok bool)

// DefaultCwdResolver walks parent directories from `cwd` looking for
// a `.gortex.yaml`. Returns the first non-empty `workspace:` value it
// finds. Stops at filesystem root or after 32 levels (defensive).
//
// The implementation is deliberately tiny — the daemon's hot path
// will hit it on every routed query, so we don't shell out to the
// full config loader for one field. yaml-shaped scan that just looks
// for `workspace: <slug>` at the top level matches the schema.
func DefaultCwdResolver(cwd string) (string, bool) {
	dir := cwd
	for i := 0; i < 32; i++ {
		path := filepath.Join(dir, ".gortex.yaml")
		if data, err := os.ReadFile(path); err == nil {
			if ws := scanWorkspaceField(data); ws != "" {
				return ws, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// scanWorkspaceField looks for a top-level `workspace: <slug>` line
// in the file. The slug is whatever follows up to a comment / EOL,
// trimmed of quotes and whitespace. We intentionally do NOT pull in
// the full yaml decoder here — this gets called on every query and
// the .gortex.yaml schema is already fully validated at config load
// time. If a malformed file ends up here, we fall back to "no
// workspace" rather than failing the lookup.
func scanWorkspaceField(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "workspace:") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(trimmed, "workspace:"))
		// Strip inline comments.
		if idx := strings.Index(v, "#"); idx >= 0 {
			v = strings.TrimSpace(v[:idx])
		}
		// Strip quotes.
		v = strings.Trim(v, `"' `)
		if v == "" {
			// Empty or mapping-valued workspace declarations do not identify
			// a workspace slug.
			continue
		}
		// A workspace slug may never collide with the reserved local
		// sentinel, nor contain ~ or / (which would alias path/home
		// expansion and the proxy-node origin segment). A rejected slug
		// degrades to "no workspace" so routing falls back to local
		// rather than re-opening the localSlug foot-gun from the
		// workspace side.
		if v == LocalServerSentinel || strings.ContainsAny(v, "~/") {
			continue
		}
		return v
	}
	return ""
}

// RouteForCwd implements the priority chain:
//
//  1. ScopeOverride (caller-supplied workspace/server slug) wins.
//  2. `.gortex.yaml::workspace` resolved from cwd.
//  3. Roster discovery — the daemon walks every configured server's
//     roster and picks the one that owns the cwd's workspace. Cached.
//  4. servers.toml's `default` entry.
//
// `scopeOverride` is the workspace slug the caller passed via the
// query's scope field — empty means "no override". When the
// caller passed a slug, we still resolve which ServerEntry hosts it
// via the same roster lookup the cwd path would use.
//
// The function does NOT make HTTP calls — it consults the supplied
// roster cache and returns an empty Server when the cache doesn't
// know yet. Callers that want roster-fetch behaviour should call
// ServerClient.FetchWorkspaceRoster on cache misses and then
// re-invoke this routine.
func RouteForCwd(
	cfg *ServersConfig,
	rosters *WorkspaceRosterCache,
	resolver CwdResolver,
	cwd string,
	scopeOverride string,
) LookupResult {
	if cfg == nil {
		return LookupResult{}
	}
	resolveServerForWorkspace := func(ws string) *ServerEntry {
		// Pre-declared `workspaces = [...]` lists in servers.toml win
		// — the user told us authoritatively which server hosts what.
		for i := range cfg.Server {
			for _, w := range cfg.Server[i].Workspaces {
				if w == ws {
					return &cfg.Server[i]
				}
			}
		}
		// Fall back to the cached roster (populated lazily by the
		// caller's prior FetchWorkspaceRoster calls).
		if rosters != nil {
			for i := range cfg.Server {
				if _, exists, fresh := rosters.Lookup(cfg.Server[i].Slug, ws); exists && fresh {
					return &cfg.Server[i]
				}
			}
		}
		return nil
	}

	if scopeOverride != "" {
		if s := resolveServerForWorkspace(scopeOverride); s != nil {
			return LookupResult{Server: s, Workspace: scopeOverride, Source: "scope-override"}
		}
		// Caller-supplied scope but we don't yet know which server
		// hosts it — return the workspace slug so the caller can
		// trigger a roster fetch and retry. This is preferable to
		// silently falling through to the default server, which
		// would mask a typo'd scope.
		return LookupResult{Workspace: scopeOverride, Source: "scope-override"}
	}

	if resolver != nil {
		if ws, ok := resolver(cwd); ok {
			if s := resolveServerForWorkspace(ws); s != nil {
				return LookupResult{Server: s, Workspace: ws, Source: "config-yaml"}
			}
			// .gortex.yaml says workspace = <ws> but no server claims
			// it — same shape as scope-override, return the slug for
			// the caller to fetch+retry.
			return LookupResult{Workspace: ws, Source: "config-yaml"}
		}
	}

	// Fall through: pick the default server from servers.toml. The
	// returned LookupResult.Workspace is empty because we don't know
	// what cwd's workspace actually is — the daemon may want to
	// surface this as a warning ("running unscoped").
	if def := cfg.DefaultServer(); def != nil {
		return LookupResult{Server: def, Source: "default"}
	}

	return LookupResult{}
}

// AllSlugs returns every server slug in declaration order. Useful
// when a caller wants to walk every configured server (e.g. when the
// roster cache is empty and the daemon needs to discover which one
// hosts a workspace via FetchWorkspaceRoster).
func (c *ServersConfig) AllSlugs() []string {
	if c == nil {
		return nil
	}
	out := make([]string, len(c.Server))
	for i := range c.Server {
		out[i] = c.Server[i].Slug
	}
	return out
}
