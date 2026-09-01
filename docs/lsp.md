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

The core registry (`internal/semantic/lsp/registry.go`):

| Spec name                    | Command                          | Languages                   | Default priority |
| ---------------------------- | -------------------------------- | --------------------------- | ---------------- |
| `gopls`                      | `gopls`                          | go                          | 3                |
| `tsgo`                       | `tsgo --lsp`                     | typescript, javascript      | 5                |
| `typescript-language-server` | `typescript-language-server`     | typescript, javascript      | 6                |
| `pyright`                    | `pyright-langserver`             | python                      | 5                |
| `rust-analyzer`              | `rust-analyzer`                  | rust                        | 5                |
| `clangd`                     | `clangd --background-index`      | c, c++, objc, objc++        | 5                |
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

Lower priority numbers win when more than one provider serves the same
language. `gopls` is `3` so it beats SCIP-based providers (`5`) for Go;
`jdtls` is `6` so it's lower-priority than the SCIP-java path that
ships separately.

When several specs cover the same file extension, routing prefers the
highest-priority spec whose binary is actually installed. `tsgo` — the
native-Go TypeScript compiler's LSP mode — is the default TS/JS winner
when its binary is on `PATH` (one native process instead of a
node + tsserver + typingsInstaller tree); `typescript-language-server`
serves wherever `tsgo` is missing or fails to spawn. The same rule
gives `.py` a `pyrefly` fallback when `pyright` is not installed.

`clangd` is the one server that needs a compilation database for full
enrichment. Point it at a `compile_commands.json` (or a `compile_flags.txt`
/ `.clangd`) at the repo root; without one, gortex degrades that repository's
enrichment to reference confirmation — see
[Enrichment cost model](#enrichment-cost-model).

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

# TypeScript / JavaScript (pick one)
npm install -g @typescript/native-preview          # ships tsgo (preferred)
npm install -g typescript typescript-language-server  # node-based fallback

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

Verify with `gortex daemon status` — the `lsp` row reports `alive`
against the live-provider cap plus the eviction count, followed by one
line per running server carrying its workspace, `last_used` age, and
`in_use` pin count. Newly enabled specs show up only after the first
request that needs them.

## Lifecycle

The router applies these defaults in `cmd/gortex/server.go` and
`cmd/gortex/mcp.go`:

| Knob              | Default      | What it does                                                                                          |
| ----------------- | ------------ | ----------------------------------------------------------------------------------------------------- |
| `WithIdleTimeout` | 10 minutes   | Subprocess closes if no `For()` / `ForSpec()` call lands in this window.                              |
| `WithReaperInterval` | 1 minute   | Background tick invokes `Reap()` to enforce the idle timeout.                                          |
| `WithMaxAlive`    | 6 servers    | LRU eviction kicks in when a seventh distinct server would spawn — the least-recently-used one closes. |

These defaults suit a polyglot workspace where most languages are
touched only intermittently. The idle timeout is overridable at runtime:
`GORTEX_LSP_IDLE_TTL` accepts a Go duration (`45m`, `2h`); `0` or a
negative value disables reaping entirely. Useful when a long enrichment
window should keep a warm server around — a big Roslyn workspace is
expensive to reload. The reaper interval and max-alive bound are still
compile-time (`lsp.NewRouter(...).With...`).

### Workspace roots

Each cached provider is keyed by `(spec, workspace)`, and the workspace
is what the language server is initialised against **and chdir'd into**.
It always comes from the tracked repo's own recorded path — never from
the directory the daemon process happened to be launched in. A daemon
tracking several repos is normally started from none of them, so a
workspace derived from its launch cwd would name a directory that exists
nowhere and fail the spawn.

The router therefore requires an absolute, existing directory and
rejects anything else before spawning. `gortex daemon --detach` can be
started from anywhere; no repo needs to be the daemon's cwd.

## Enrichment cost model

The resolution path (use 1 at the top of this page) runs as a batch
**enrichment pass** — one pass per (repository × language server) — that walks
the repo's nodes for that language and upgrades edges the AST resolver left
ambiguous. A pass runs up to five phases:

1. **Interface pass** — for each interface / abstract declaration, asks the
   server for its implementations (`textDocument/implementation`) and links the
   dispatch edges.
2. **Confirm pass** — for every ambiguous edge (one whose confidence the
   resolver could not raise to certainty), asks for the referent's references
   (`textDocument/references`) and confirms or refutes the edge. Grouped by
   referent file so each file opens once.
3. **Definition-rebind fallback** — for edges the confirm pass could not settle
   from references alone, asks for the call site's definition
   (`textDocument/definition`) and rebinds the edge to the concrete target.
   An answer that agrees with the heuristic target counts as
   `edges_confirmed`; an answer naming a different same-name declaration
   rewrites the edge (tagged `rebound_from`) and counts as `edges_rebound` —
   a correction of the heuristic graph, not a confirmation of it. One
   exception: an answer naming the *declared* dispatch member (interface /
   abstract base) while the stored target is one of its concrete
   implementations does not rewrite anything — the stored edge is a
   devirtualization guess definition cannot vouch for, so the compiler-proven
   edge to the declared member is added (`edges_added`) and the guess keeps
   its heuristic tier.
4. **References-add pass** — only for servers that expose references but not a
   call hierarchy; recovers the caller edges a declaration's references imply.
5. **Per-file sweep** — the whole-repo hover / hierarchy phase. Per function or
   method it prepares a call hierarchy and reads outgoing (and, where it helps,
   incoming) calls; per type or interface it reads the super/subtype hierarchy;
   per symbol it hovers for a type string.

Each phase opens the files it touches on the language server (`didOpen`) and
closes them when done (`didClose`). The first four phases carry most of the
precision gain; the sweep carries most of the cost — on a warm restart, where
every node already reloads with its resolved edges and type stamp, the sweep is
almost pure churn.

### Bounded document session

All five phases share one **document session** per pass. It opens each file on a
server at most once while the file is in use, keeps recently-closed files warm
in an LRU tail so a later phase reuses the open document instead of reopening
it, and closes everything at pass end. The simultaneously-open ceiling is
`2 × max_parallel` documents per server (floor 4): the pinned working set stays
within `max_parallel` (bounded by the pass's file semaphore) and the extra
headroom is the warm tail. `didOpen` / `didClose` stay paired per (file,
server) — a file opened on a server that later dies still gets its close attempt
on that server — while the server's open-document set stays bounded.

This matters most for `clangd` without a compilation database: every `didOpen`
triggers a full fallback-preamble + AST rebuild, so reopening the same file
across phases multiplies that cost.

### Servers that skip the lifecycle entirely

A server whose spec sets `NoDidOpen` answers position requests (hover,
references, call hierarchy) for files it loaded from its own workspace,
without any `didOpen` — and for one server family that is worth far more
than the saved notification. csharp-ls schedules read-only requests
concurrently but treats every `didOpen` / `didClose` as an exclusive write:
it waits for all in-flight reads to retire and blocks every read queued
behind it. A pass that interleaves opens with its queries therefore
serializes to single-request throughput no matter how many requests it
keeps in flight — measured on a real C# monorepo as ~8 req/s with the
lifecycle against ~1,000 req/s without it. With `NoDidOpen` the document
session degrades to a pure content cache (file bytes still read from disk
once per file) and the pass sends zero document notifications. The
`GORTEX_LSP_OPEN_DOCS` env var overrides in both directions: `1` forces
the lifecycle back on, `0` skips it for every server.

### Servers that skip the heavy request classes

A server whose spec sets `NoHeavyRequests` never receives the two request
classes that ride its find-references machinery: `textDocument/references`
and `callHierarchy/incomingCalls`. For the one spec that sets it today — C#
(`omnisharp`, including its `csharp-ls` fallback command) — the reason is a
memory leak, not throughput: released csharp-ls builds (≤ 0.26) hold ~10MB
plus a set of OS handles per references round trip and ~0.7MB per
incomingCalls, released only at process exit. A full enrichment pass over a
large repo pushes tens of GB through that leak, and a long-lived daemon
answering usage queries accumulates it one request at a time.

What changes under the opt-out:

- **Edge confirmation moves to definition.** The references confirm pass
  (phase 2) is skipped and the definition pass (phase 3) becomes the primary
  confirm source, covering every sited ambiguous edge — the same verdicts at
  position-request cost. Edge tiers and recall are unaffected.
- **`callHierarchy/incomingCalls` is never sent.** Dispatch fan-out stays
  with the graph-side interface-dispatch synthesizer; the outgoing side of
  the call hierarchy still runs.
- **The references-add pass (phase 4) is skipped** — it exists for servers
  without a call hierarchy and is references-driven by definition.
- **On-demand confirmation is disabled.** The query-time path that upgrades
  `find_usages` / `get_callers` answers with a live LSP round trip returns
  immediately for these servers, so C# usage queries answer from the stored
  graph tiers alone.

The `GORTEX_LSP_HEAVY` env var overrides the spec in both directions, with
the same value vocabulary as `GORTEX_LSP_OPEN_DOCS`: `on` / `1` / `true`
restores references and incomingCalls for an opted-out server, `off` / `0`
/ `false` disables them for every server, and an empty or unrecognised
value falls through to the spec. The knob is deliberately env-only: it
exists to match a specific server *build*, not a workspace, and a durable
config key would outlive the build it was set for.

> **Warning:** set `GORTEX_LSP_HEAVY=on` for C# only on a csharp-ls build
> that carries the FindReferences leak fix
> ([razzmatazz/csharp-language-server#410](https://github.com/razzmatazz/csharp-language-server/pull/410)).
> On any released build up to 0.26, a full heavy pass over a large repo can
> push ~30GB through the leak and OOM the server mid-pass.

### Sweep modes

The per-file sweep (phase 5) is gated by a **sweep mode**:

| Mode     | Behaviour                                                                                                                                                                                                                                                                                                                                                              |
| -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `demand` | **Default.** Sweep a file only when its declarations still carry unresolved same-name call candidates (enrichment demand) or it carries a dispatch-relevant declaration: a callable taking part in dynamic dispatch, an interface, or a type involved in a super/subtype hierarchy (an `implements` / `extends` edge in either direction — a bare data type does not admit its file). In a language whose edge set carries no extractor-produced hierarchy edges at all (e.g. C, C++, Objective-C, Swift — the sweep's `typeHierarchy` hop is the only producer there), the type check falls back to admitting every type. A file with no signal is skipped, so a warm restart pays no sweep for it. Within a swept file, a node that already carries a `semantic_type` stamp from an earlier pass is not re-hovered. |
| `full`   | Sweep every file of the language — maximal hover and hierarchy coverage, the right choice for a first cold index.                                                                                                                                                                                                                                                       |
| `off`    | Skip the per-file sweep entirely. The confirm / rebind / references-add / interface passes still run, so edge tiers and recall are unaffected — only hover type strings and sweep-recovered hierarchy edges are dropped.                                                                                                                                                 |

Resolution precedence, highest first:

1. The `GORTEX_LSP_SWEEP` environment variable (`demand` / `full` / `off`, with
   `none` accepted as an alias for `off`; case- and whitespace-insensitive). Set
   it where you launch `gortex daemon` / `gortex server` to dial one run without
   editing config.
2. The `.gortex.yaml` key `semantic.lsp_sweep` (same values).
3. A per-server default. Most servers have none and fall through to the global
   default; **rust-analyzer defaults to `full`**. Rust method calls bind
   overwhelmingly to standard-library receiver types the graph never indexes, so
   static confirmation leaves rust-analyzer's net-new call-hierarchy edges on the
   table — its recall lives in the full sweep. Either operator source above
   overrides it, so `semantic.lsp_sweep: demand` (or the env) puts rust-analyzer
   back on the demand gate.
4. The `demand` default.

An unrecognised value at any level is ignored and the next source applies.

```yaml
semantic:
  enabled: true
  lsp_sweep: demand   # demand (default) | full | off
```

### Incoming-calls policy

In the sweep's call-hierarchy step, outgoing calls are always fetched — the
sweep visits every caller, so a declaration's outgoing hops alone reconstruct
every intra-repo static call edge. Incoming calls are fetched only where the
outgoing side is blind: a dispatch-relevant declaration (whose concrete callers
land on the incoming side of an interface method, not its outgoing side) or one
that still carries unresolved demand — or under `full` mode. Files are swept
demand-first, so a declaration whose incoming calls are skipped at a deadline
cut still loses no reachable edge. The count of declined round trips rides on
the pass-complete log as `incoming_calls_skipped`.

### Compile-database preflight (clangd)

Before enriching a repository with a server that needs a compilation database
(`clangd`), gortex checks the workspace root for one of:

- `compile_commands.json` — canonical CMake / Bear output
- `build*/compile_commands.json` — out-of-tree build directories
- `compile_flags.txt` — clangd's flat-flags fallback
- `.clangd` — a hand-written clangd config

If none is present, clangd rebuilds a full fallback AST on every `didOpen`, and
opening a header directly makes it a standalone translation unit with no
cross-file signal to show for the cost. Rather than drive that churn, the pass
**degrades to reference confirmation**: it runs the confirm and rebind passes
(which work inside the fallback translation unit on fallback flags) and skips
the interface pass, the references-add pass, the entire per-file sweep, and all
header files. Edge tiers are unaffected, and the pass's yield lands under
`edges_confirmed` / `edges_rebound` as usual — a degraded pass that settles
most of its edges through the definition fallback reports mostly rebinds, not
zero yield. Hover type strings and call / type-hierarchy edges are absent for
that pass.

A degraded pass warns once with the remediation and marks its result
`degraded`. `index_health` surfaces a recommendation naming the repository and
provider:

> generate compile_commands.json (cmake -DCMAKE_EXPORT_COMPILE_COMMANDS=ON,
> bear -- make, or meson) at the repo root, then reindex

Degradation is deliberate, not a failure — `semantic_enrichment_ok` stays true.

### Telemetry

The pass-complete log line (`LSP enrich: hover phase complete`) carries the cost
accounting for the pass:

| Field | Meaning |
| ----- | ------- |
| `sweep_mode` | The effective sweep mode for the pass. |
| `did_opens` | Total `didOpen` calls across every phase. |
| `reopened_files` | Files opened more than once (a warm-tail miss). |
| `doc_evictions` | LRU evictions — a `didClose` forced to make room. |
| `peak_open_docs` | Peak simultaneously-open documents on any one server. |
| `req_references`, `req_implementations`, `req_definitions`, `req_hovers` | Per-method request counts. |
| `req_prepare_call_hierarchy`, `req_outgoing_calls`, `req_incoming_calls` | Call-hierarchy request counts. |
| `incoming_calls_skipped` | Incoming-calls round trips the policy above declined. |
| `req_prepare_type_hierarchy`, `req_supertypes`, `req_subtypes` | Type-hierarchy request counts. |
| `skipped_already_stamped` | Nodes whose hover was skipped because they already carried a `semantic_type`. |

A degraded pass logs `LSP enrich: degraded pass complete (reference
confirmation only)` instead, with `degraded=true` and the document-open counters
but no hover / hierarchy counts.

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
| `GORTEX_LSP_CSHARP_RESTORE` | on | Before spawning the C# server, run `dotnet restore <resolved solution> -p:NuGetAudit=false` in the workspace (falling back to a bare directory restore when no solution resolves) so the MSBuild workspace loads every project (root-cause fix). Best-effort: a failure logs and falls through to a normal spawn; skipped on passive IDE attach and when `dotnet` is not on `PATH`. |
| `GORTEX_LSP_CSHARP_DIAG_FILTER` | on | Strip diagnostics whose code is the `NU####` NuGet family from `publishDiagnostics` before storing / fanning out (symptom fix). Deliberately narrow — real `CS####` compiler diagnostics always pass through. |
| `GORTEX_LSP_CSHARP_SOLUTION` | unset | Solution csharp-ls loads and the pre-restore targets — a PATH-style list of `.sln`/`.slnx` paths (relative to a workspace root, or absolute inside one); each tracked root uses the first entry that resolves inside it, so one daemon-global value serves a multi-repo daemon. Unset auto-detects a lone root-level solution; a falsy value (`0` / `off` / `false` / `no` / `none`) disables injection and auto-detect, leaving csharp-ls's own recursive discovery untouched. The resolved pin and any ignored entries are logged at spawn. |
| `GORTEX_LSP_RESOLVER_CSHARP` | off | Opt-in: add csharp-ls to the resolve-time LSP helper for repos with C# intent (root-level `.sln`/`.slnx`/`.csproj`). Any set, non-falsy value enables (`1` / `true` / `on` / `yes`); off by default because a Roslyn workspace load costs real memory and startup time. |

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
- **`workspace ... is not usable as a working directory` error:** the
  caller supplied a workspace root that names no directory. The router
  refuses it rather than spawning the server there, because that value
  becomes the subprocess's working directory. A rejection is local to
  the one workspace — the spec stays available for every other tracked
  repo.
- **Server keeps restarting:** the idle reaper closed it, then the
  next request re-spawned. Raise `GORTEX_LSP_IDLE_TTL` (Go duration;
  `0` disables reaping) if this hurts warm-cache benchmarks.
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
  `semantic.lsp_max_parallel` in config overrides the spec's cap for
  every router-spawned server — the durable knob for a machine whose
  servers multiplex better than the conservative default assumes. The
  `GORTEX_LSP_MAX_PARALLEL` env override wins over both for one-run
  experiments (and also reaches resolver-pool providers, which take env +
  spec only; legacy config-declared providers keep their explicit
  setting). Non-positive or unparseable values fall through.
