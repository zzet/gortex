# Contributing to Gortex

Thank you for considering contributing to Gortex! This guide will help you get
started.

## Come say hello

Most day-to-day discussion happens on Discord:

**[discord.gg/39MFHu3J5d](https://discord.gg/39MFHu3J5d)**

If you're new, join and introduce yourself — what you work on, which languages
or ecosystems you care about, and what brought you to Gortex. It's the fastest
way to:

- find out whether someone is already working on the thing you want to build,
- sanity-check a design before you write the code,
- get unstuck on a build, a tree-sitter grammar, or a failing test,
- coordinate on larger changes that touch several subsystems.

Issues and pull requests are still the source of truth for anything that ships.
Discord is where the coordination happens so PRs don't collide.

Please also read the [Code of Conduct](CODE_OF_CONDUCT.md); it applies to
Discord, issues, and pull requests alike. Security issues go through
[SECURITY.md](SECURITY.md), **not** a public issue.

## Licensing of contributions

Gortex is released under the [Apache License, Version 2.0](LICENSE.md).
By submitting a contribution (a pull request, patch, or other work) you
agree that it is licensed to the project under the same Apache 2.0 terms,
as described in section 5 of the License. You retain copyright in your
contribution; the project retains a perpetual, worldwide, royalty-free
license to use, modify, and redistribute it as part of Gortex.

Contributors are listed in [CONTRIBUTORS.md](CONTRIBUTORS.md). Add yourself
to that file in a dedicated PR, linking your previous contributions, if you'd
like to be credited.

## Getting Started

### Prerequisites

- Go 1.26+ (the toolchain version is pinned in `go.mod`; CI builds with it)
- CGO enabled and a working C/C++ compiler — tree-sitter grammars are C.
  On Windows the mingw-w64 toolchain works; that's what CI uses.
- Git

### Building

```bash
git clone https://github.com/zzet/gortex.git
cd gortex
go build -o gortex ./cmd/gortex/
```

A cold build compiles several hundred vendored tree-sitter grammars, so expect
minutes rather than seconds the first time. Later builds hit the Go build cache
and are fast.

`make build` does the same thing but adds `-tags llama` for the in-process
llama.cpp LLM provider. The plain `go build` above is the configuration CI
checks and is the right default for most work. Other optional build tags:

| Tag | Enables |
|-----|---------|
| `llama` | In-process llama.cpp provider (`llm.provider: local`) |
| `embeddings_onnx` | ONNX Runtime embedding backend |
| `embeddings_gomlx` (+ `XLA`) | GoMLX embedding backend |
| `netgo,osusergo,static_link` | The fully static Linux release build |

### Running Tests

```bash
go test -race ./...
```

CI runs the suite with `-timeout=20m` on Linux and macOS. A few packages —
`internal/persistence` in particular — are slow under `-race`, so if you hit the
default 10-minute panic, raise the timeout rather than assuming a hang.

### Linting

```bash
make lint      # golangci-lint run --timeout=5m
```

**A green test run is not the full CI gate.** CI pins golangci-lint to
**v2.13.1** and fails the build on any finding. Run it locally before opening a
PR — config lives in [`.golangci.yaml`](.golangci.yaml).

```bash
make fmt       # gofmt -s -w .
```

### Running Benchmarks

```bash
make bench
```

Or target a single package:

```bash
go test -bench=. -benchmem ./internal/parser/languages/
go test -bench=. -benchmem ./internal/query/
go test -bench=. -benchmem ./internal/graph/
```

### What CI checks

Your PR has to get past all of these
([`.github/workflows/ci.yml`](.github/workflows/ci.yml)):

- `go build` + `go test -race` on `ubuntu-latest` and `macos-latest`
- a native Windows CGO build, plus focused tests for the agent, path-guard, and
  sidecar code paths whose behaviour differs there
- a static Linux build (`netgo,osusergo,static_link`) verified to be
  self-contained and smoke-tested inside `debian:11` and `alpine:3`
- golangci-lint
- builds with the ONNX and GoMLX embedding tags
- benchmarks for the parser, query, and graph packages

Additional workflows cover security scanning, the install script, init smoke
tests, and skill drift.

## How to Contribute

### Reporting Bugs

- Open an issue with a clear description
- Include the output of `gortex version` and `go version`
- Provide a minimal reproduction if possible

### Suggesting Features

- Open an issue describing the feature and its use case
- For language support requests, say whether an upstream tree-sitter grammar
  exists — see [Adding a Language](#adding-a-language) for the tiers

### Submitting Code

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Write tests for your changes
4. Ensure all tests pass (`go test -race ./...`)
5. Ensure the linter is clean (`make lint`)
6. Commit with a clear message
7. Open a pull request and fill in the template

Small, focused PRs get reviewed fastest. If a change is large or crosses
subsystem boundaries, raise it on Discord or in an issue first — it's much
cheaper to redirect a design than a finished branch.

## Adding a Language

This is one of the most impactful contributions. Gortex indexes languages
through three engine tiers, in order of decreasing extraction depth and
increasing ease:

1. **Bespoke tree-sitter** — a vendored grammar under
   [`internal/parser/tsitter/`](internal/parser/tsitter/) plus a hand-tuned
   extractor in [`internal/parser/languages/`](internal/parser/languages/) with
   per-language S-expression queries. Worth it when you need ORM, contract,
   dataflow, or scope-aware call resolution.
2. **Regex** — pattern-matched line scanning for languages with no upstream
   grammar. Shared block helpers live in `helpers_indent.go`.
3. **Forest signature-only** — if
   [`alexaandru/go-sitter-forest`](https://github.com/alexaandru/go-sitter-forest)
   already ships a grammar, add the module and append one row to
   `forestLanguages` in
   [`forest_registrations.go`](internal/parser/languages/forest_registrations.go).
   Cheapest path, broadest coverage, no deep extraction.

[**docs/languages.md**](docs/languages.md) documents each tier, the current
language matrix, and the exact steps — read it before you start.

Hand-written extractors implement the `parser.Extractor` interface
([`internal/parser/extractor.go`](internal/parser/extractor.go)) and are
registered in `RegisterAll` in
[`register.go`](internal/parser/languages/register.go). Registration order
matters: extractors registered before `registerForestLanguages` claim their
extensions ahead of the generic forest grammars.

**What to extract (in priority order):**

- Functions/methods with `EdgeMemberOf` to their class/type
- Classes/types/interfaces
- Interface method specs in `Meta["methods"]` (enables IMPLEMENTS inference)
- Imports
- Call sites
- Variables/constants

**Reference implementations:**

- `golang.go` — the most complete extractor
- `python.go` — simple OOP language
- `rust.go` — systems language with impl blocks
- `nim.go` / `abap.go` — regex-tier templates
- `yaml.go` — simple config extractor

Every extractor ships a `_test.go` with at least a happy-path and an
empty-input case. Debug the AST first — tree-sitter node types vary between
grammars.

## Code Style

- Follow standard Go conventions (`gofmt -s`, `go vet`, golangci-lint)
- No unnecessary abstractions — three similar lines is better than a premature
  helper
- Comments explain *why*, not *what*; match the density of the surrounding code
- Tests should be self-contained with inline source snippets
- Extractor test helpers (`nodesOfKind`, `edgesOfKind`) are shared across test
  files

## Project Structure

```
cmd/gortex/          CLI entry point and subcommands
pkg/gortex/          Public API for embedding Gortex in another Go program
docs/                Reference documentation (see below)
internal/
  analysis/          Communities, processes, hotspots, architecture rollups
  contracts/         Contract definitions and checking
  daemon/            Long-running daemon: lifecycle, watchers, federation
  graph/             Core graph data structures (Node, Edge, Graph)
  indexer/           Directory walker, file watcher, incremental reindex
  llm/               LLM providers and the `ask` agent
  mcp/               MCP server and tool handlers
  parser/            Extractor interface, tree-sitter plumbing, registry
    languages/       Per-language extractors (one file each)
    forest/          Generic signature-only extractor over go-sitter-forest
    tsitter/         Vendored grammar wrappers for the bespoke tier
  persistence/       SQLite graph store and sidecars
  query/             Query engine (BFS traversal, SubGraph)
  resolver/          Cross-file reference resolution, IMPLEMENTS inference
  review/            PR review and diff analysis pipeline
  search/            FTS adapter, trigram, and semantic search
  server/            HTTP transports and the web dashboard
```

`internal/` holds many more focused packages than the ones above; those are the
load-bearing ones to know first.

## Public API (`pkg/gortex`)

`New(opts ...Option) (*Engine, error)` opens a SQLite graph store, which can
fail — construction returns an error rather than a bare `*Engine`. Pass
`WithStorePath` to keep the store at a path of your choosing and reuse the
index on the next run; without it the store lives in a temp directory.

Every `Engine` must be closed. `Close` checkpoints the write-ahead log, closes
the database handle, and removes the temp directory when `New` created one —
skipping it leaks a file handle and a background goroutine.

## Where to Read Next

| Doc | Covers |
|-----|--------|
| [docs/architecture.md](docs/architecture.md) | How the pieces fit together |
| [docs/languages.md](docs/languages.md) | Language matrix and adding a language |
| [docs/mcp.md](docs/mcp.md) | MCP server, tool presets, transports |
| [docs/cli.md](docs/cli.md) | CLI verb reference |
| [docs/llm.md](docs/llm.md) | LLM provider configuration |
| [docs/features.md](docs/features.md) | Capability tour |
| [CLAUDE.md](CLAUDE.md) | Repo conventions for coding agents |

## Questions?

Ask on [Discord](https://discord.gg/39MFHu3J5d), open an issue, or start a
discussion. We're happy to help!
