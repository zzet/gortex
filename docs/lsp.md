# LSP Routing

Gortex uses the Language Server Protocol (LSP) for two things:

1. **Compiler-grade resolution during enrichment.** When the resolver
   leaves an edge as `ast_inferred` or `text_matched`, an LSP server
   (`textDocument/definition` + `textDocument/implementation`) upgrades
   it to `lsp_resolved` / `lsp_dispatch`. This raises precision on
   `find_usages`, `get_callers`, `find_implementations`, and the
   contract pipeline's binding resolver.
2. **On-demand actions** via the four MCP tools that wrap the LSP
   action surface: `get_diagnostics`, `get_code_actions`,
   `apply_code_action`, `fix_all_in_file`.

Both paths route through the same per-daemon `lsp.Router` — one
subprocess per language server, lazy-spawned on first request, idle
reaper at ten minutes, LRU eviction at six concurrent.

## Server registry

Sixteen servers ship in the registry today
(`internal/semantic/lsp/registry.go`):

| Spec name                    | Command                          | Languages                   | Default priority |
| ---------------------------- | -------------------------------- | --------------------------- | ---------------- |
| `gopls`                      | `gopls`                          | go                          | 3                |
| `typescript-language-server` | `typescript-language-server`     | typescript, javascript      | 5                |
| `pyright`                    | `pyright-langserver`             | python                      | 5                |
| `rust-analyzer`              | `rust-analyzer`                  | rust                        | 5                |
| `clangd`                     | `clangd --background-index=false --clang-tidy=false --header-insertion=never -j=1` | c, c++, objc, objc++ | 5                |
| `jdtls`                      | `jdtls`                          | java                        | 6                |
| `kotlin-language-server`     | `kotlin-language-server`         | kotlin                      | 6                |
| `omnisharp`                  | `omnisharp -lsp`                 | csharp                      | 5                |
| `ruby-lsp`                   | `ruby-lsp`                       | ruby                        | 5                |
| `phpactor`                   | `phpactor language-server`       | php                         | 5                |
| `lua-language-server`        | `lua-language-server`            | lua                         | 5                |
| `sourcekit-lsp`              | `sourcekit-lsp`                  | swift                       | 5                |
| `haskell-language-server`    | `haskell-language-server-wrapper`| haskell                     | 5                |
| `elixir-ls`                  | `elixir-ls`                      | elixir                      | 5                |
| `ocamllsp`                   | `ocamllsp`                       | ocaml                       | 5                |
| `zls`                        | `zls`                            | zig                         | 5                |

Several specs declare `AlternativeCommands` — Gortex picks the first
binary on `PATH`:

- `pyright` → falls back to `jedi-language-server` or `pylsp`.
- `ruby-lsp` → falls back to `solargraph stdio`.
- `phpactor` → falls back to `intelephense --stdio`.

The built-in `clangd` spec disables background indexing and clang-tidy by
default because semantic enrichment needs request-driven graph evidence,
not lint diagnostics or a persistent project index. Repositories that want
those clangd features can override the `clangd` provider args in
`.gortex.yaml`.

Lower priority numbers win when more than one provider serves the same
language. `gopls` is `3` so it beats SCIP-based providers (`5`) for Go;
`jdtls` is `6` so it's lower-priority than the SCIP-java path that
ships separately.

## Enabling a server

Add it to `.gortex.yaml`:

```yaml
semantic:
  enabled: true
  mode: typecheck     # or "callgraph"
  providers:
    - name: gopls
      enabled: true
    - name: rust-analyzer
      enabled: true
    - name: pyright
      enabled: true
```

Names match the **Spec name** column above. The router pre-registers
every enabled spec at boot but does not spawn anything yet —
subprocesses start the first time a tool calls into them.

## Installing the underlying servers

Gortex does not ship the LSP binaries. Install the ones you want to
use; the router falls back gracefully when a binary is missing
(`SpecAvailable(name)` returns false → tool returns a structured
`no_lsp_for` error instead of hanging).

```bash
# Go
go install golang.org/x/tools/gopls@latest

# Rust
rustup component add rust-analyzer

# Python (pick one)
npm install -g pyright            # recommended
pip install jedi-language-server  # alt
pip install python-lsp-server     # alt (pylsp)

# TypeScript / JavaScript
npm install -g typescript typescript-language-server

# C / C++ / Objective-C
brew install llvm                 # ships clangd
# or apt install clangd

# Java
brew install jdtls

# Kotlin
brew install kotlin-language-server

# C#
dotnet tool install --global Microsoft.OmniSharp

# Ruby (pick one)
gem install ruby-lsp              # recommended
gem install solargraph            # alt

# PHP (pick one)
composer global require phpactor/phpactor
npm install -g intelephense       # alt

# Lua
brew install lua-language-server

# Swift
# Bundled with Xcode toolchain on macOS; no separate install.

# Haskell
ghcup install hls

# Elixir
brew install elixir-ls

# OCaml
opam install ocaml-lsp-server

# Zig
brew install zls
```

Verify with `gortex daemon status` — the LSP-router section lists
`alive`, `last_used`, and the resolved command for each running
server. Newly enabled specs show up only after the first request that
needs them.

## Lifecycle

The router applies these defaults in `cmd/gortex/server.go` and
`cmd/gortex/mcp.go`:

| Knob              | Default      | What it does                                                                                          |
| ----------------- | ------------ | ----------------------------------------------------------------------------------------------------- |
| `WithIdleTimeout` | 10 minutes   | Subprocess closes if no `For()` / `ForSpec()` call lands in this window.                              |
| `WithReaperInterval` | 1 minute   | Background tick invokes `Reap()` to enforce the idle timeout.                                          |
| `WithMaxAlive`    | 6 servers    | LRU eviction kicks in when a seventh distinct server would spawn — the least-recently-used one closes. |

These defaults suit a polyglot workspace where most languages are
touched only intermittently. Override them by editing the
`lsp.NewRouter(...).With...` chain in your build if you need a longer
warm pool or a tighter memory bound.

## Diagnostics

Two surfaces:

### Pull (poll-based)

`get_diagnostics` returns the most recent `publishDiagnostics` payload
the LSP server produced for a file. Use it for one-shot reads, batch
checks, or contexts where the agent doesn't maintain a long-lived
session.

### Push (opt-in)

`subscribe_diagnostics` opts the calling MCP session into
`notifications/diagnostics` push events. After subscribing, every LSP
`publishDiagnostics` for any router-managed server is forwarded to the
session as an MCP notification with this shape:

```json
{
  "method": "notifications/diagnostics",
  "params": {
    "uri":         "file:///abs/path/to/main.go",
    "path":        "/abs/path/to/main.go",
    "server":      "gopls",
    "diagnostics": [
      { "range": {"start": {"line": 41, "character": 4}, "end": {...}},
        "severity": 1, "message": "missing return", "source": "gopls" }
    ]
  }
}
```

Push semantics:

- **Opt-in per session.** Sessions that never call `subscribe_diagnostics`
  receive nothing — no broadcast spam.
- **Delta-only.** Identical re-publishes (which some servers emit on
  every save even when nothing changed) are suppressed at the
  broadcaster — your subscribers only see real changes.
- **All-router-managed servers.** One subscription covers every spec
  the user enabled in config. The `server` field on each notification
  identifies which LSP produced the payload.
- **Non-blocking.** Notifications use `SendNotificationToAllClients`
  which drops to an error hook when a session's notification channel
  is full — slow consumers don't block the LSP message-pump.

Call `unsubscribe_diagnostics` to opt back out (idempotent).

Pair with `get_code_actions` + `apply_code_action` + `fix_all_in_file`
for the full edit-time diagnostic loop without polling.

## Language-specific behaviour

A few servers need per-language handling beyond the generic router. These
knobs are **environment variables** read by the daemon process — set them
where you launch `gortex daemon` / `gortex server`. There is no `.gortex.yaml`
key for them today.

### C# — NuGet audit advisories (`omnisharp` / `csharp-ls`)

The Roslyn `MSBuildWorkspace` these servers build escalates a NuGet audit
*warning* (the `NU19xx` family — e.g. a transitively vulnerable package) to a
**fatal** project-load failure and drops every project that references the
flagged package. Those projects then have no compilation, so the server emits
false `CS####` "unresolved type / namespace" diagnostics — even though
`dotnet build` / `dotnet test` keep `NU19xx` a non-fatal warning and succeed.

Gortex applies two complementary, C#-scoped fixes, **both on by default**:

| Env var | Default | Effect |
| --- | --- | --- |
| `GORTEX_LSP_CSHARP_RESTORE` | on | Before spawning the C# server, run `dotnet restore -p:NuGetAudit=false` in the workspace so the MSBuild workspace loads every project (root-cause fix). Best-effort: a failure logs and falls through to a normal spawn; skipped on passive IDE attach and when `dotnet` is not on `PATH`. |
| `GORTEX_LSP_CSHARP_DIAG_FILTER` | on | Strip diagnostics whose code is the `NU####` NuGet family from `publishDiagnostics` before storing / fanning out (symptom fix). Deliberately narrow — real `CS####` compiler diagnostics always pass through. |

Set either to a falsey value (`0` / `off` / `false` / `none`) to disable it —
e.g. `GORTEX_LSP_CSHARP_RESTORE=0` for offline / air-gapped indexing or to
keep indexing off the network. Restore is on by default because gortex only
indexes repositories you explicitly add (it never auto-discovers), and
spawning the C# server already evaluates the project's MSBuild graph — so the
restore adds no execution surface beyond the workspace load it precedes. A
successful restore logs `lsp: csharp pre-restore complete (NuGetAudit
suppressed)`; a failure logs `lsp: csharp pre-restore failed; spawning server
anyway` with the restore output tail.

### Java — build-backed resolution (`jdtls`)

By default `jdtls` runs in a **no-build** mode (JRE-only classpath, Maven /
Gradle import and autobuild disabled) so indexing an untrusted Java repo never
runs its build. Resolution is more limited in this mode (jdtls falls back to
an "invisible project"). Set `GORTEX_LSP_JDTLS_TRUST_BUILD=1` (or `true`) to
allow full Maven / Gradle import + autobuild for higher-fidelity resolution —
**opt-in**, because it executes the repository's build tooling. Enable it only
for repositories you trust.

## Troubleshooting

- **`no_lsp_for` error:** the file extension didn't match any
  registered spec. Either the spec isn't enabled in `.gortex.yaml`, or
  the binary isn't on `PATH`. Run the spec's `--version` directly to
  confirm install.
- **`router spawn <name>: ...` error:** the binary was on `PATH` at
  boot but the subprocess failed to initialise (commonly a missing
  dependency such as `node` for `pyright`, or a workspace-config
  mismatch). The error surfaces the LSP server's stderr.
- **Server keeps restarting:** the idle reaper closed it, then the
  next request re-spawned. Increase `WithIdleTimeout` if this hurts
  warm-cache benchmarks.
- **High memory under polyglot load:** lower `WithMaxAlive` from 6 to
  3-4. The LRU evicts the least-recent server transparently.

## Implementation notes

- The router lives at `internal/semantic/lsp/router.go`. It satisfies
  the `semantic.LSPRouter` interface so `semantic.Manager` can drive
  batch enrichment without taking a hard import dependency on the lsp
  package (which would create a cycle — lsp already imports semantic
  for the Provider interface).
- `tools_lsp.go::lspProviderForPath` queries the router first; if no
  router is wired (legacy boot paths, tests), it falls back to a scan
  through `Manager.AllProviders()` so user-defined daemons (specs not
  in the registry) still work.
- One `*lsp.Provider` per spec, regardless of how many MCP sessions
  hit it. Concurrency is bounded by `ServerSpec.MaxParallel` (6-10
  inflight requests per server depending on the spec).
