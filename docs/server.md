# HTTP server, MCP 2026 transport, and Web UI

Gortex exposes three transports — stdio MCP (the default `gortex mcp`), a Unix-socket daemon, and an HTTP API. The HTTP layer is what IDE plugins, CI, the web UI, and remote agents talk to.

- [Server mode (`/v1/*` JSON API)](#server-mode-v1-json-api)
- [MCP 2026 Streamable HTTP transport (`/mcp`)](#mcp-2026-streamable-http-transport-mcp)
- [Web UI](#web-ui)

## Server mode (`/v1/*` JSON API)

The daemon exposes all MCP tools as an HTTP/JSON API under versioned `/v1/*` routes once you give it an HTTP address with `--http-addr`. The daemon serves the repos you track, so add the repo first, then bring the HTTP surface up:

```bash
# Track the repo (or run from inside it — the cwd's repo auto-tracks), then start the HTTP backend
gortex track /path/to/repo
gortex daemon start --http-addr 127.0.0.1:7411

# Non-localhost bind requires an auth token
gortex daemon start --http-addr 0.0.0.0:7411 --http-auth-token "$(openssl rand -hex 32)"

# Optional one-shot HTTP API alongside MCP stdio; first enable
# mcp.allow_embedded in the user-level config.
gortex mcp --index /path/to/repo --server --port 8765
```

**Endpoints (all under `/v1/`):**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/health` | GET | Status, node/edge counts, uptime |
| `/v1/tools` | GET | List all available tools with descriptions |
| `/v1/tools/{name}` | POST | Invoke any MCP tool with JSON arguments. Accepts `?format=gcx` or top-level `"format"` in the body |
| `/v1/stats` | GET | Graph statistics by kind and language, plus `server_id` + `started_at` |
| `/v1/graph` | GET | Full brief-graph dump (nodes + edges + stats); accepts `?project=` and/or `?repo=` for scoping |
| `/v1/events` | GET | SSE stream of graph-change events (the daemon watches tracked repos by default). Accepts `?token=<t>` for `EventSource` auth |

**Auth & binding.** A localhost `--http-addr` (e.g. `127.0.0.1:7411`) runs unauthenticated. Pass `--http-auth-token <token>` or set `$GORTEX_DAEMON_HTTP_TOKEN` to require `Authorization: Bearer <token>` on every `/mcp` and `/v1/*` request (constant-time compare; CORS preflights bypass). Non-localhost binds without a token are rejected at startup. CORS origin is configurable via `--cors-origin` (default `*`); it applies to both `/mcp` and `/v1`. `/healthz` is exempt from auth so liveness probes work.

**Scoping the graph.** The daemon serves every tracked repo from one shared index, so the HTTP surface spans all of them. Add or remove repos from the served set with `gortex track <path>` / `gortex untrack <path>`; query-time `?project=` / `?repo=` parameters (see the `/v1/graph` row above) narrow a request to a single workspace or repo without restarting the daemon.

**Multi-server roster.** When the daemon is running, it can route MCP traffic across multiple Gortex servers — a local Unix socket for the repos on this machine, plus one or more remote HTTPS servers for shared / cloud indexes. The roster lives at `~/.gortex/servers.toml`; manage it with `gortex daemon server list / add / remove`. Auth tokens can be embedded directly (`--auth-token`) or pulled from an env var the daemon reads at request time (`--auth-token-env`, preferred). Restart the daemon to pick up roster changes.

## MCP 2026 Streamable HTTP transport (`/mcp`)

`gortex daemon start --http-addr <addr>` exposes the **MCP 2026 Streamable HTTP transport** — the wire format the June 2026 MCP release locks in — on the same TCP address as the `/v1/*` JSON API.

| Verb | Path | Behaviour |
|------|------|-----------|
| `POST` | `/mcp` | Accepts one or more JSON-RPC frames. Requests receive a correlated response or response array; notification-only input returns 202 with an empty body. Unknown sessions return 404 before dispatch. |
| `GET` | `/mcp` | Opens an SSE stream the server uses to push server-initiated notifications (progress, sampling) onto the bound session. |
| `DELETE` | `/mcp` | Terminates a session. Idempotent — returns 204 even when the id is unknown. |
| `OPTIONS` | `/mcp` | CORS preflight; advertises the allowed methods. |

**Session lifecycle and request handling.** POST requests may carry `Mcp-Session-Id`; the transport replays matching state out of a `streamable.SessionStore` (the default in-memory `MemoryStore` is TTL-evicted; swap for a Redis-backed adapter to share state across replicas behind a load balancer). A standalone, non-batched `initialize` begins without an id, mints a fresh one, and returns it on the response header. An unknown or expired id on POST requests, including `initialize`, is rejected before dispatch with HTTP 404 and no `Mcp-Session-Id` response header. JSON-RPC requests receive a correlated `-32001 session not found` body; responses and notifications do not receive JSON-RPC replies. Clients may then reinitialize without an id and retry the rejected request once. Idle sessions are evicted after a TTL (default 30 m, `GORTEX_MCP_SESSION_IDLE_TTL` to tune, a negative value disables expiry); any request on the session resets the clock, so a client issuing calls at least that frequently never expires. The horizon is advertised in whole seconds on every POST response as `Mcp-Session-Idle-Ttl` and inside the initialize result's `_meta` under `gortex/session_idle_ttl_seconds`, so clients can schedule keep-alives (for example a `tools/list` at half the TTL) instead of discovering expiry through the 404. The `Mcp-Protocol-Version` header is echoed when provided; absent, the transport advertises its default. `tools/call` frames flow through the same multi-server router that serves `/v1/tools/<name>`, so workspace scoping carries over unchanged.

**Daemon enablement.** `gortex daemon start --http-addr 127.0.0.1:7411 [--http-auth-token <token>]` brings the transport up alongside the unix-socket dispatcher. Non-localhost binds require an auth token (or `$GORTEX_DAEMON_HTTP_TOKEN`). `/healthz` is exempt so liveness probes work. Once `--http-addr` is set, the daemon mounts `/mcp` alongside the `/v1/*` surface on the same address — no extra flag needed.

## Web UI

The web UI lives in its own repo at [`gortexhq/web`](https://github.com/gortexhq/web) so it can be deployed independently of the backend (Vercel / a static host / your own Next.js deployment). It's a standalone Next.js 15 app that talks to the daemon's HTTP surface over `/v1/*`:

```bash
# 1) Track the repo and start the HTTP backend (bearer-auth required for non-localhost binds)
gortex track /path/to/repo
gortex daemon start --http-addr 127.0.0.1:7411

# 2) Clone and run the UI in another terminal
git clone https://github.com/gortexhq/web.git gortex-web && cd gortex-web
echo 'NEXT_PUBLIC_GORTEX_URL=http://localhost:7411' > .env.local
npm install && npm run dev
# Open http://localhost:3000
```

| Page | Features |
|------|----------|
| **Dashboard** | Health, stats, language pie chart, node kind bar chart |
| **Graph Explorer** | Sigma.js 2D + five react-three-fiber 3D modes (City / Strata / Galaxies / Constellation / Graph3D), node filters, selection, detail panel |
| **Search** | Semantic + BM25 search via `/v1/*`, results grouped by kind |
| **Symbol Detail** | Source code, signature, callers/callees/usages/deps tabs |
| **Communities** | Community cards with cohesion bars, expandable members |
| **Processes** | Collapsible call-tree steps, product vs test process split |
| **Analysis** | Dead code, hotspots, cycles, index health — 4 tabs |
| **Contracts** | API contracts (HTTP, gRPC, GraphQL, topics, WebSocket, env vars) with provider/consumer matching, request/response type tracing, `yours / tests / deps / all` scope filter |
| **Services** | Service-level graph visualization with per-repo stats |
| **AI Chat** | LLM-powered chat with code context (placeholder) |
