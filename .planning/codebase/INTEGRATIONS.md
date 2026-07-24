# External Integrations

**Analysis Date:** 2026-07-24

## APIs & External Services

**GitHub Integration:**
- GitHub API v3 (google/go-github/v88)
  - Used for: PR review, contract detection, issue linking
  - OAuth support: Via GitHub CLI (`gh auth`) or personal access token
  - Auth: `GITHUB_TOKEN` env var or stored credentials from GitHub CLI config
  - Endpoints: Repositories, pulls, commits, issues, code search
  - Integration point: `internal/contracts/` detects HTTP route contracts from GitHub issues/PRs

**Code Intelligence & Resolution:**
- IDE/Agent integrations (19 supported agents)
  - Claude Code, Cursor, Windsurf, VS Code/Copilot, Continue.dev, Cline, OpenCode, Zed, Aider, Kilo Code, Hermes, Gemini CLI, Copilot CLI, Codex CLI, Kimi, Kiro, Antigravity, Oh My Pi, Pi
  - Protocol: MCP 2026 (Model Context Protocol)
  - Transport: stdio or HTTP (`port: 8765` default in `.gortex.yaml`)

## Data Storage

**Databases:**
- SQLite (modernc.org/sqlite v1.54.0)
  - Connection: Embedded via pure-Go driver; stored in `.gortex/graph.db`
  - Use: Graph snapshots, session data, conversation logs, memories
  - Client: Direct Go database/sql interface
  - Schema: Managed by `internal/persistence/` package (file-based snapshots on schema changes)

- PostgreSQL (jackc/pgx/v5 v5.10.0) - **Optional, for multi-instance deployments**
  - Connection: `GORTEX_POSTGRES_URL` env var (e.g. `postgresql://user:pass@host/dbname`)
  - Use: Remote graph storage, shared session state across daemon instances
  - Client: pgx/v5 with prepared statements
  - Schema: Auto-migrated via internal migrations

**File Storage:**
- Local filesystem only (no S3/cloud storage integration)
  - `.gortex/` directory: Graph data, snapshots, index metadata
  - Workspace-scoped configuration via `internal/workspace/`
  - Multi-repo support: Each tracked repo has isolated index under `.gortex/`

**Caching:**
- In-process caching only (no Redis/Memcached)
  - Reach index (precomputed depth-3 call graph) cached in memory for O(seeds × reach) query performance
  - Conversation logs cached in SQLite

## Authentication & Identity

**Auth Provider:**
- Custom implementations only (no external identity provider)
  - CLI: File-based daemon token in `.gortex/daemon.lock` (flock-protected)
  - HTTP Server: Optional Bearer token auth via `GORTEX_AUTH_TOKEN` env var
  - GitHub API: OAuth via `github-cli` config or `GITHUB_TOKEN` env var
  - LLM Provider auth: See "LLM Integrations" section below

**Session Management:**
- In-process session state (daemon-local)
- Optional remote PostgreSQL backend for multi-instance deployments
- `internal/persistence/sidecar_migrate.go` - Session history migration
- `internal/daemon/session_reconnect_test.go` - Session reconnection after daemon restart

## Monitoring & Observability

**Error Tracking:**
- None configured (in-process error handling only)

**Logs:**
- zap (go.uber.org/zap v1.28.0) structured logging
  - Log level: Configurable via CLI flags `--log-level`
  - Sinks: stdout (stderr for structured JSON) or file (optional)
  - No log shipping integration; logs remain local

**Telemetry:**
- Off by default - opt-in only
  - `gortex telemetry on|off|status` commands
  - Collects: Anonymous tool/command counts only (no code, paths, or exact counts)
  - Honors: `DO_NOT_TRACK` environment variable
  - Endpoint: User-configured custom endpoint or none
  - See `internal/telemetry/` for implementation

## CI/CD & Deployment

**Hosting:**
- Deployment target: Single static binary (self-contained, no external runtime)
  - Built for: macOS (Intel/ARM64), Linux (x86_64/ARM64), Windows (x86_64)
  - Installation: Direct binary download or via Homebrew, `.deb`, `.rpm`, `.apk`, Scoop

**CI Pipeline:**
- GitHub Actions (`.github/workflows/ci.yml`)
  - Lint: golangci-lint
  - Test: `go test ./...` with race detector
  - Build: Cross-platform via goreleaser-cross (Linux), native runners (macOS/Windows)
  - Release: Signed binaries (cosign via Sigstore, SLSA 3 build provenance)
  - Artifact verification: SHA256 checksums + cosign verification

**Release Process:**
- Goreleaser (`.goreleaser.yml`)
  - Platforms: Linux (amd64/arm64), macOS (Intel/ARM64), Windows
  - Archives: tar.gz (Unix), zip (Windows)
  - Packages: .deb, .rpm, .apk (Linux), Homebrew cask (macOS)
  - Changelog: Auto-generated from git commits

## Environment Configuration

**Required env vars:**
- `GORTEX_POSTGRES_URL` - PostgreSQL connection string (optional; SQLite used if omitted)
- `GITHUB_TOKEN` - GitHub API access (optional; for PR review and contract detection)
- LLM provider credentials (see "LLM Integrations" below)
- `GORTEX_AUTH_TOKEN` - HTTP server bearer token (optional; if auth required)

**Secrets location:**
- Environment variables (`.env` file or shell exports)
- GitHub CLI config (`~/.config/gh/hosts.yml`)
- LLM provider config files (see `internal/llm/config.go`)
- No dedicated secrets manager integration (AWS Secrets Manager, HashiCorp Vault, etc.)

## LLM Integrations

**Supported Providers (9 total):**

- **Anthropic** (`internal/llm/provider/anthropic/`)
  - Models: Claude 3 family (Opus, Sonnet, Haiku)
  - Auth: `ANTHROPIC_API_KEY` env var
  - Endpoint: `https://api.anthropic.com/v1/messages`

- **OpenAI** (`internal/llm/provider/openai/`)
  - Models: GPT-4, GPT-3.5-turbo
  - Auth: `OPENAI_API_KEY` env var
  - Endpoint: `https://api.openai.com/v1/chat/completions`

- **Azure OpenAI** (`internal/llm/provider/azure/`)
  - Models: Azure-hosted OpenAI
  - Auth: `AZURE_OPENAI_API_KEY` + `AZURE_OPENAI_ENDPOINT` env vars
  - Endpoint: User-configured Azure resource endpoint

- **Google Gemini** (`internal/llm/provider/gemini/`)
  - Models: Gemini Pro
  - Auth: `GOOGLE_API_KEY` env var
  - Endpoint: `https://generativelanguage.googleapis.com/`

- **AWS Bedrock** (`internal/llm/provider/bedrock/`)
  - Models: Claude, Titan, Llama (via AWS)
  - Auth: AWS credentials (IAM role or `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`)
  - Endpoint: AWS Bedrock regional endpoint

- **DeepSeek** (`internal/llm/provider/deepseek/`)
  - Models: DeepSeek API
  - Auth: `DEEPSEEK_API_KEY` env var
  - Endpoint: `https://api.deepseek.com/`

- **Ollama** (`internal/llm/provider/ollama/`)
  - Local models (Llama 2, Mistral, etc.)
  - Auth: None (local)
  - Endpoint: `http://localhost:11434/api/generate` (default; configurable)
  - Model management: User-managed via `ollama pull`

- **Local Llama** (`internal/llm/provider/local/`)
  - Local inference via llama.cpp subprocess
  - Build tag: `-tags llama` (default)
  - Model: User-downloaded GGUF file (e.g., Meta Llama 2)
  - No external API call

- **OpenAI-compatible** (`internal/llm/provider/openaicompat/`)
  - Generic OpenAI API-compatible endpoint
  - Auth: `OPENAI_API_KEY`-compatible token
  - Endpoint: User-configured custom base URL

**Fallback routing** (`internal/llm/routing.go`):
- Graceful degradation: Falls back to next configured provider on API failure
- Retry logic: Exponential backoff with configurable max retries
- Provider selection: Community-based routing or explicit selection

## Webhooks & Callbacks

**Incoming:**
- None configured (Gortex is query-only; no webhook endpoints)

**Outgoing:**
- PR Review webhooks: Optional GitHub check suites and status updates
  - Configurable via `internal/contracts/` (contract matching and route guard)

## Semantic Search Integrations

**Embedding Backends:**

- **Hugot (default)** - Pure-Go ONNX runtime
  - Model: MiniLM-L6-v2 (auto-downloaded on first use, ~100MB)
  - Dimensions: 384-dimensional vectors
  - No external API call; in-process inference

- **OpenAI Embeddings (optional)** - `internal/embedding/provider.go`
  - Model: `text-embedding-3-small` or `text-embedding-3-large`
  - Auth: `OPENAI_API_KEY` env var
  - Endpoint: `https://api.openai.com/v1/embeddings`
  - Fallback: Enabled for large batches or if local backend fails

- **Ollama Embeddings (optional)** - Local embedding server
  - Models: Customizable (e.g., `nomic-embed-text`)
  - Endpoint: `http://localhost:11434/api/embed` (configurable)
  - Auth: None

- **Static GloVe (default fallback)** - Pre-trained 50-dimensional vectors
  - Embedded in binary (3.8MB)
  - No external download
  - Degraded semantic quality; adequate for keyword-based matching

**Vector Search:**
- HNSW (Hierarchical Navigable Small World) via coder/hnsw
- Integration: BM25 + vector + reciprocal rank fusion (RRF) hybrid search
- Storage: Bleve full-text index with vector metadata

## GitHub Actions Integration

**Contract Detection:**
- Automatically detects API contracts (HTTP routes, gRPC, GraphQL, message topics) from:
  - Framework annotations (Gin, Express, FastAPI, Spring, etc.)
  - Proto files (gRPC services)
  - GraphQL schema files
  - Env var usage patterns

**PR Review Analysis:**
- `gortex review --base <ref> --audience agent` - Line-anchored findings with verdict
- Findings: NPE, thread-safety, N+1 queries, logic errors
- Output: BLOCK/REVIEW/APPROVE verdict + file:line locations

---

*Integration audit: 2026-07-24*
