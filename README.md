<div align="center">
<p align="center">
  <img src="assets/wall.svg" alt="Gortex" width="500">
</p>


### High-performance and efficient code-intelligence engine for AI agents and IDE
#### Indexes code into graph and exposes it via CLI, MCP Server, and web UI. Multi-repository support by default.
#### Single static binary for macOS, Linux, and Windows — no dependency chain, simple installation and use.

---
[![CI](https://github.com/zzet/gortex/actions/workflows/ci.yml/badge.svg)](https://github.com/zzet/gortex/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/zzet/gortex?logo=github&sort=semver)](https://github.com/zzet/gortex/releases/latest)
[![Sigstore signed](https://img.shields.io/badge/sigstore-signed-66D4FF?logo=sigstore&logoColor=white)](docs/installation.md#verifying-releases-supply-chain-security)
[![SLSA 3](https://img.shields.io/badge/SLSA-Level%203-green)](https://slsa.dev/spec/v1.0/levels#build-l3)
[![VirusTotal](https://img.shields.io/badge/VirusTotal-0%2F91-brightgreen?logo=virustotal)](https://www.virustotal.com/gui/url/00e1094b39c9bd7db4d5a179b1d56173f85c915075057fd3cc64bfbb9b735b11/detection)
[![macOS](https://img.shields.io/badge/macOS-supported-blue.svg)](#)
[![Linux](https://img.shields.io/badge/Linux-supported-blue.svg)](#)
[![Windows](https://img.shields.io/badge/Windows-supported-blue.svg)](#)

[![MCP Toplist](https://mcptoplist.com/badge/glama%2Fzzet%2Fgortex.svg)](https://mcptoplist.com/server/glama%2Fzzet%2Fgortex)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/zzet/gortex/badge)](https://scorecard.dev/viewer/?uri=github.com/zzet/gortex)
[![Go Reference](https://pkg.go.dev/badge/github.com/zzet/gortex.svg)](https://pkg.go.dev/github.com/zzet/gortex)
  <br />
  <a href="https://trendshift.io/repositories/36832" target="_blank"><img src="https://trendshift.io/api/badge/repositories/36832" alt="zzet%2Fgortex | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/></a>
</div>

High-quality parsing 257 languages/grammars through tree-sitter AST analysis, in-process resolvers, enhanced with [compiler-grade resolution](https://github.com/zzet/gortex/blob/main/docs/lsp.md) for Python, TypeScript / JavaScript, PHP, C#, Go, C, C++, Java, Kotlin, Swift, Zig, Rust, Ruby, Elixir, Ocaml, Haskell, and [others](https://github.com/zzet/gortex/blob/main/docs/languages.md#at-a-glance) - producing a persistent provenance-tiered knowledge graph of functions, classes, call chains, HTTP routes, and cross-service contracts and calls with a strong confidence model. 175 (configurable) MCP tools - use only what you need. Zero dependencies. Plug and play across 19 coding agents. **Up to 50× fewer tokens per response**. Reproducible [benchmarks](BENCHMARK.md).


> 19 AI coding agents (Claude Code, Kiro, Cursor, Windsurf, VS Code / Copilot, Continue.dev, Cline, OpenCode, Antigravity, Codex CLI, Gemini CLI, Zed, Aider, Kilo Code, OpenClaw, Hermes, Oh My Pi, Pi, Kimi) supported out of the box.
>
> One install configures every one detected on your machine — see [docs/agents.md](docs/agents.md).

<details>
<summary>Gortex Web UI — force-directed knowledge graph visualization</summary>

![Gortex Web UI — force-directed knowledge graph visualization](assets/graph.png)

</details>

## Why it matters

- **50× fewer tokens per response** — graph-native lookups beat naive file reads. Agents read **just what they need**, not the full file, not the 500-line file around it.
- **Full development cycle** - no 'read file before edit'. Agents ask for sources, tell what to change, and don't waste context of reading noise.
- **257 languages/grammars** - every file in the repository reachable; no mix of tools and bloating/hallycinating agents. Three tiers (bespoke tree-sitter, regex, forest-backed signatures) plus Jupyter and Databricks notebooks → [docs/languages.md](docs/languages.md)
- **Cross-repo by default** — N repos in one graph; contracts, references, and call chains span repo boundaries with evidence-gated resolution, contract matching, impact analysis, per-session isolation → [docs/multi-repo.md](docs/multi-repo.md)
- **Extreamly fast analysis** — a precomputed depth-3 reach index turns blast-radius queries into O(seeds × reach) map lookups. Safe to ask "what breaks if I change this?" on every edit. No dozens of tool calls to grasp context.
- **Zero external dependencies** — single binary, everything in-process. No network, no model download to get started. Install, start daemon, use.
- **Agent integrations (19)** — `gortex init` configures every detected coding assistant on the machine → [docs/agents.md](docs/agents.md)
- **100+ MCP tools, 16 resources, 3 prompts** — symbol lookup, call chains, blast radius, dataflow, clone detection, refactoring, code actions → [docs/mcp.md](docs/mcp.md)
- **Semantic search default-on** — baked GloVe-50d (3.8 MB embedded), hybrid BM25 + vector + RRF, zero deps; opt-in MiniLM / Ollama / OpenAI → [docs/semantic-search.md](docs/semantic-search.md)
- **Speculative execution** — `preview_edit` / `simulate_chain` answer "what would change if I applied this WorkspaceEdit?" without touching disk
- **Live editor overlays** — push unsaved buffers as a shadow graph; tools read through it. Branching for parallel speculative sessions
- **GCX1 wire format** — published, round-trippable. **An additional −27% tokens vs JSON** at same fidelity → [docs/wire-format.md](docs/wire-format.md)
- **Long-living daemon** — one process serves every IDE window; live fsnotify, on-disk snapshots, restart, OS-supervised lifecycle
- **9 LLM providers (optional)** — local llama.cpp, Anthropic, OpenAI, Ollama, Claude / Codex CLI subprocess, Gemini, Bedrock, DeepSeek → [docs/llm.md](docs/llm.md)
- **Composable safety** — `verify_change`, `check_guards`, `audit_agent_config` flag broken callers, guard violations, stale docs before they ship
- **PR review, end to end** — `gortex prs` triages open PRs (per-PR blast radius, merge-order conflicts via shared communities, AI-ranked queue, reviewer suggestions); `gortex review` emits line-anchored findings with a BLOCK/REVIEW/APPROVE verdict from a graph-grounded rulepack; MCP tools (`pr_risk`, `get_pr_impact`, `review`, `review_pack`, `post_review`, …) expose it to agents → [docs/cli.md](docs/cli.md)
- **HTTP server + Web UI** — versioned `/v1/*` API + MCP 2026 Streamable HTTP; standalone Next.js 15 UI with five 3D graph modes → [docs/server.md](docs/server.md)
- **Telemetry off by default** — opt-in anonymous tool/command counts only (no code, paths, names, or exact counts); nothing transmitted unless you configure an endpoint. `gortex telemetry on|off|status`; honours `DO_NOT_TRACK` → [docs/telemetry.md](docs/telemetry.md)

Full catalog of features: [docs/features.md](docs/features.md). Complete CLI reference: [docs/cli.md](docs/cli.md).

## Install

```bash
# macOS / Linux
curl -fsSL https://get.gortex.dev | sh

# Windows (PowerShell)
irm https://get.gortex.dev/install.ps1 | iex
```

Detects OS/arch, verifies SHA256 + cosign, installs to PATH. Re-run to upgrade. Homebrew, `.deb` / `.rpm` / `.apk`, scoop, signed binaries, and from-source builds: [docs/installation.md](docs/installation.md).

## Quick Start

```bash
gortex install                          # one-time machine setup (MCP, skills, slash commands)
gortex daemon start --detach            # background daemon
gortex track ~/projects/myapp           # add a repo
cd ~/projects/myapp && gortex init      # per-repo: .mcp.json, hooks, community routing
```

Your AI assistant now uses graph queries. Full 15-minute walkthrough: [docs/onboarding.md](docs/onboarding.md).

## Cross-Repo API Contracts

Gortex auto-detects API contracts across repos and matches providers to consumers, surfaced via the `contracts` MCP tool and the web UI Contracts page.

| Contract type | Detection | Provider | Consumer |
|--------------|-----------|----------|----------|
| **HTTP routes** | Framework annotations (gin, Express, FastAPI, Spring, …) | Route handler | HTTP client calls (`fetch`, `http.Get`) |
| **gRPC** | Proto service definitions | Service RPC | Client stub calls |
| **GraphQL** | Schema type/field definitions | Schema | Query/mutation strings |
| **Message topics** | Kafka / RabbitMQ / NATS / Redis pub/sub | Publish calls | Subscribe calls |
| **WebSocket** | Event emit/listen patterns | `emit()` | `on()` |
| **Env vars** | `os.Getenv`, `process.env`, `.env` files | `Setenv` / `.env` | `Getenv` / `process.env` |
| **OpenAPI** | Swagger / OpenAPI spec files | Spec paths | (linked to HTTP routes) |
| **Temporal workflows** | Go / Java SDK annotations | Activity / workflow function | `ExecuteActivity` / `ExecuteChildWorkflow` |

Contracts are normalised to canonical IDs (e.g. `http::GET::/api/users/{id}`) and matched across repos to detect orphan providers / consumers and mismatches. See [docs/contracts.md](docs/contracts.md).

## Scale — battle-tested on large repos

Measured on an Apple Silicon laptop with the default CGO build.

| Repository | Files | Nodes | Edges | Index time | Throughput | Peak heap |
| ---------- | ----: | ----: | ----: | ---------: | ---------: | --------: |
| [torvalds/linux](https://github.com/torvalds/linux) | 70,333 | 1,690,174 | 6,239,570 | ~3 min | 300 files/s | 5.07 GB |
| [microsoft/vscode](https://github.com/microsoft/vscode) | 10,762 | 204,501 | 808,902 | ~1 min | 143 files/s | 580 MB |
| zzet/gortex (self) | 430 | 5,583 | 53,830 | 3.4s | 127 files/s | 52 MB |

Parsing dominates wall time (65–80 %); reference resolution and search-index build scale sub-linearly.

## Comparison with CodeGraph

Both tools turn a codebase into a queryable graph, but they target different workflows: CodeGraph centres on visualizing and chatting with a code graph stored in an external graph database, while Gortex is a zero-dependency engine built for the full agent development cycle (read → edit → verify) over MCP and CLI.

| | Gortex | [CodeGraph](https://github.com/FalkorDB/code-graph) |
| --- | --- | --- |
| **Deployment** | Single static binary, everything in-process, no external services | Web app backed by a FalkorDB graph database (Docker / hosted DB) |
| **Languages** | 257 tree-sitter grammars; compiler-grade resolution for Python, TypeScript / JavaScript, Go, Java, C#, Rust, and [others](docs/languages.md) | Tree-sitter analyzers for a small language set (Python, Java, C, …) |
| **Graph storage** | In-memory graph with gob+gzip snapshots; provenance-tiered edges with a confidence model | Persisted in FalkorDB; queried via its graph query layer |
| **Scope** | Multi-repo by default — cross-repo contracts, references, and call chains in one graph | Per-repository graph |
| **Agent interface** | MCP server (175 configurable tools), CLI verbs, versioned HTTP API | Web UI visualization plus a chat interface |
| **Editing & safety** | Graph-aware edits (`edit_symbol`, `batch_edit`) with a pre-write parse gate, `verify_change`, blast-radius analysis | Read / explore / visualize — no editing surface |
| **LLM dependency** | Optional — 9 pluggable providers; every core tool works without one | OpenAI API key required for natural-language querying |
| **Token efficiency** | Up to 50× fewer tokens per response; GCX1 wire format (−27% vs JSON) | Not a design goal (human-facing UI) |

Feature sets move fast — this reflects both projects as of mid-2026; check each repo for the current state.

## Token savings dashboard

`gortex savings` reports tokens saved vs naive file reads — per-call, per-session, and cumulative across restarts, priced in USD against the headline model.

```text
Gortex Token Savings
====================
Cost avoided:   $168.69 (claude-opus-4) across 1,878 calls · 11,246,094 tokens saved

Today       ████████░░░░░░░░   50.0%  saved 9,200 / 18,400 tokens   $0.14
Last 7 days ██████████░░░░░░   62.5%  saved 60,100 / 96,200 tokens  $0.90
All time    ███████████████░   93.3%  saved 11,246,094 / 12,050,716 tokens  $168.69
```

`--verbose` adds the per-tool breakdown; `--json` is machine-readable. Full reference: [docs/savings.md](docs/savings.md).

## Architecture

```
gortex binary
  CLI (cobra)    ──> MultiIndexer ──> In-Memory Graph (shared, per-repo indexed)
  MCP (stdio)    ──────────────────> Query Engine (repo/project/ref scoping)
  HTTP /v1/*     ──────────────────> same tools + /v1/graph + /v1/events (SSE)
  Daemon (unix)  ──────────────────> shared graph for every MCP client, session isolation
                  MultiWatcher    <── filesystem events (fsnotify, per-repo)
                  CrossRepoResolver ──> cross-repo edge creation (type-aware)
                  Persistence     ──> gob+gzip snapshot (pluggable backend)
```

Data flow, graph schema (node and edge kinds, multi-repo fields, test taxonomy), persistence model: [docs/architecture.md](docs/architecture.md).

## Documentation

| Topic | Reference |
| --- | --- |
| First-time walkthrough | [onboarding.md](docs/onboarding.md) |
| Installation & supply-chain verification | [installation.md](docs/installation.md) |
| Full feature catalog | [features.md](docs/features.md) |
| CLI reference | [cli.md](docs/cli.md) |
| MCP tools, resources, prompts | [mcp.md](docs/mcp.md) |
| Multi-repo workspaces | [multi-repo.md](docs/multi-repo.md) |
| HTTP server + Web UI + MCP 2026 transport | [server.md](docs/server.md) |
| Cross-repo API contracts | [contracts.md](docs/contracts.md) |
| Semantic search | [semantic-search.md](docs/semantic-search.md) |
| Optional LLM features | [llm.md](docs/llm.md) |
| LSP integration | [lsp.md](docs/lsp.md) |
| Per-community skills & agent usage | [skills.md](docs/skills.md) |
| AI agent adapters (19) | [agents.md](docs/agents.md) |
| Supported languages (257) | [languages.md](docs/languages.md) |
| Token savings | [savings.md](docs/savings.md) |
| GCX1 wire format | [wire-format.md](docs/wire-format.md) |
| Architecture & graph schema | [architecture.md](docs/architecture.md) |
| Evaluation methodology | [04-evaluation/](docs/04-evaluation/) |
| Telemetry & privacy | [telemetry.md](docs/telemetry.md) |
| Versioning policy | [versioning.md](docs/versioning.md) |

## Building from source

```bash
make build          # binary with version stamping
make test           # go test -race ./...
make lint           # golangci-lint
```

Requires Go 1.26+ and CGO (for tree-sitter C bindings).

## License

Apache License 2.0. See [LICENSE.md](LICENSE.md). Copyright 2024-2026 Andrey Kumanyaev <me@zzet.org>.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on adding features, language extractors, and submitting PRs.
