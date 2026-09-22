# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Gortex, please report it responsibly:

1. **Do not** open a public issue.
2. Use GitHub's [private vulnerability reporting](https://github.com/zzet/gortex/security/advisories/new), or email the maintainer directly.
3. Include a description, the affected version or commit, and steps to reproduce.

We aim to acknowledge receipt within 48 hours and will provide a timeline for a fix.

## Overview

Gortex is a code-intelligence engine. It indexes repositories into a knowledge
graph held in a local SQLite store and exposes that graph over a CLI and an MCP
server. Running
locally — on the user's machine, with the user's privileges — is the default and
the assumption of this policy, but Gortex can also be deployed remotely (for
example, a daemon bound to a non-localhost address), where the network-exposure
and authentication considerations below carry more weight.

The MCP tools are typically driven by an LLM agent, so the agent should be
treated as **potentially adversarial**: prompt injection through indexed content
(a crafted README, source comment, test fixture, or dependency) can cause the
agent to invoke tools with attacker-influenced arguments. The boundaries
described below — file-path confinement, opt-in network egress, and explicit
process execution — are the security boundary, not the agent's good behavior.

## Two channels, two levels of trust

Gortex's enforcement rests on one distinction, and understanding it explains
every exemption in this document:

- The **operator channel** — the `gortex` CLI and the daemon's control surface —
  is you, typing a command. It is deliberately **not** confined: `gortex export
  --out /any/path` writes where you say, and `gortex init --force` will index a
  root the safety check otherwise refuses. You asked in your own name, on your
  own machine.
- The **agent channel** — MCP tool calls — is confined, because its arguments
  can be steered by content Gortex indexed. Path arguments are checked against
  the tracked roots, and the operations that would widen those roots are refused.

When something below says "operator only" or "you opted in", it means exactly
this: Gortex stopped enforcing at that point because you made the decision, not
because the operation is safe. Those decisions are enumerated next so you can
see which ones you have actually made.

## Decisions that grant authority

Each of these hands out capability. None is a vulnerability; all of them are
choices, and the risk is yours once made.

| You do this | It grants | Note |
|---|---|---|
| Run the daemon | Any process that can open its socket gets **full authority over every tracked repository** — reads, writes, and the control surface | There is no per-connection authentication beyond the socket's `0600` mode. A second local *account* cannot reach it; another process running as *you* can |
| `gortex track <path>` | Every agent session on the machine can read **and write** that tree by absolute path, in any workspace | Persisted to your global config. Tracking is the act that grants file access — treat it as such |
| Use the CLI / control channel | Unconfined file writes (`gortex export --out <anywhere>`) and indexing of a root the safety check refuses (`gortex init --force`) | Intentional. The equivalent operations are refused on the agent channel |
| Give several repos the same `workspace:` slug | A session opened *above* your repos binds to the whole slug — including repos that share it from **elsewhere on disk**, outside the directory you opened | A slug is an explicit grouping you declared, so it is honoured over the narrower "what this folder contains" inference |
| Set `mcp.allow_embedded: true` | Each MCP client may create a private one-shot server and index a tree when the shared daemon is unavailable | Off by default to avoid duplicate processes and indexes. An explicit `gortex mcp --index <path>` may also create the repo-local notebook there; an inferred root never does |
| `--http-addr` (even `127.0.0.1`) | **Every local process, and any page your browser loads**, can reach the MCP surface | The unix socket's permissions do not cover a TCP port. Prefer the socket |
| Omit `--http-auth-token` on a loopback bind | Unauthenticated access to that port | Permitted by design; a non-loopback bind refuses to start without a token |
| `--http-allowed-origin <origin>` | That website can drive the full tool surface **with your privileges** | Off by default: cross-origin browser requests to `/mcp` are refused |
| `--cors-origin '*'` (current default) | Any origin may read `/v1` responses when it can reach the port | Narrow it if you enable `--http-addr` |
| `gortex eval-server` | An HTTP tool surface for benchmarking | Loopback-only unless you pass a token |
| `GORTEX_DAEMON_PPROF_ADDR` | Unauthenticated heap dumps (**containing indexed source**) and `/debug/pprof/cmdline` (**containing your auth token**) | Loopback-only; opt-in |
| Configure a hosted or subprocess **LLM provider** | Prompts derived from your source leave the machine | No provider is configured by default |
| Enable **federation / proxy** | Graph queries go to the daemons you configure | Off unless configured; read-only by default |
| Index a repository you do not trust | Its content reaches the agent's context, widening the prompt-injection surface | This is the threat the confinement boundaries exist for |

## What Gortex does not protect you from

Stated plainly, because the absence of a boundary is easy to mistake for the
presence of one:

- **Agent sessions are not isolated from each other on the filesystem.** Workspace
  and project scoping bound *graph queries*. A session working in one tracked
  repository can read and write another tracked repository by absolute path.
- **A repository you track is a repository you have shared** with every agent
  session on the machine, for the lifetime of that config entry.
- **Editing is a first-class feature**, so an agent that can write source can
  influence what runs the next time you build, test, or open the project. Review
  agent-authored changes the way you would review a pull request.
- **Anything reachable at a local TCP port is reachable by your browser.** A
  loopback bind is a convenience, not an isolation mechanism.
- **Nothing here defends against a local attacker already running as you.** Such
  a process can read your files without involving Gortex at all.

## Scope

### File system access

- Gortex **reads and writes files within indexed repository roots.** Editing is
  a first-class feature: tools such as `write_file`, `edit_file`, `edit_symbol`,
  `rename_symbol`, `batch_edit`, `move_inline`, `safe_delete_symbol`, and the LSP
  code-action tools modify source files in the repositories Gortex has indexed.
  After a write, the affected file is re-indexed to keep the graph fresh.
- File-path resolution is **confined to indexed repository roots.** A path —
  relative or absolute — is resolved against the roots of the tracked
  repositories, and access outside every indexed root is refused. Symlinks are
  resolved before the check so a link cannot be used to escape a root.
- Gortex does not require, and does not request, access to files outside the
  repositories you index.
- The confinement boundary is the **union of every tracked repository root**,
  not the workspace of the session making the call. A session working in one
  tracked repository can read and write files in another tracked repository by
  absolute path. Workspace and project scoping narrow *graph queries*; they are
  a relevance and context boundary, not a filesystem one. Do not track a
  repository you would not let every agent session on this machine read and
  modify.
- Because the tracked-root set defines that boundary, the tools that extend it
  (`track_repository` / `workspace_admin(operation:"track")`) are part of the
  boundary. Tracking a new root widens file access for every tool and persists
  to your global config; the `force` option, which would allow tracking `/` or
  your home directory, is refused for agent sessions and available only from
  the CLI you run yourself.

### The daemon socket is the trust boundary

The daemon runs as **you**, never with elevated privileges, and its unix socket
is created `0600` inside a `0700` directory. Access control is those filesystem
permissions and nothing else — there is no per-connection authentication, and
one connection grants both the tool surface and the control surface (track /
untrack / shutdown).

The useful consequence: **Gortex never grants access to a directory you could
not already read yourself.** It cannot be used to reach another user's files,
because a daemon that can read them is running as someone who can read them, and
only that someone can open its socket.

See [Decisions that grant authority](#decisions-that-grant-authority) for what
the HTTP surfaces change, and [What Gortex does not protect you
from](#what-gortex-does-not-protect-you-from) for what this boundary is not.

### Network access

- **No telemetry.** Gortex sends no usage data, analytics, or crash reports, and
  performs no update or "phone-home" checks. With no LLM provider, federation, or
  forge tooling configured, Gortex makes **no outbound network requests.**
- Outbound network access happens only through these **opt-in** features:
  - **LLM providers** (`llm.provider`): the `ask` agent and `search_symbols`
    assist modes can call an LLM. The default provider is `local` (in-process,
    no network). When configured for a hosted provider (Anthropic, OpenAI, Azure
    OpenAI, Google Gemini, AWS Bedrock, DeepSeek, Requesty, or a remote Ollama) or a
    subprocess CLI provider (Claude, Codex, Copilot, Cursor, opencode), prompts
    **derived from your source code** are sent to that endpoint or third-party
    tool. No provider is configured by default, and `ask` / assist stay disabled
    when none is available.
  - **Federation** (`.gortex.yaml` `federation:` / `gortex proxy`): fans
    **read-only** graph queries out to other Gortex daemons you configure. It is
    off unless configured and read-only by default; the `federation.edges`
    cross-daemon edge feature (which fetches remote subgraphs) is off by default.
  - **PR / review tooling** (`gortex prs`, `gortex review --post`, and the
    matching MCP tools): call the GitHub API / the `gh` CLI when you invoke them.
- **Inbound HTTP.** `gortex server` mounts a Streamable-HTTP MCP endpoint at
  `POST /mcp`; the daemon exposes it only when started with `--http-addr`. The
  listener binds to **localhost by default**; binding to a non-localhost address
  requires an authentication token (`--http-auth-token`). The default stdio
  transport communicates only with the parent process.

### Process execution

- Gortex executes external programs only for features you opt into:
  - **Git**, for history-derived features (blame, churn, co-change, diff review).
  - **Language servers** (e.g. `tsserver`), for cross-file resolution and LSP
    code actions, when an LSP is configured and available.
  - **Subprocess LLM providers** and **forge tools** (`claude`, `codex`,
    `copilot`, `cursor-agent`, `opencode`, `gh`), when configured.
- These run with your privileges and may make their own network calls; they are
  invoked only when the corresponding feature is configured or requested.

### Data at rest

- The graph, along with session notes and development memories, is persisted
  locally under `~/.gortex` (and per-repo `.gortex/`). Notes and memories may
  contain excerpts of your source. Nothing is transmitted off the machine except
  through the opt-in network features above.

### Build / supply chain

- **CGO.** Tree-sitter grammars are compiled via CGO from
  `github.com/alexaandru/go-sitter-forest`. The optional in-process LLM (the
  `local` provider) is compiled only with the `llama` build tag.
- SQLite persistence uses the pure-Go `modernc.org/sqlite` driver (no CGO).

## Hardening checklist

[Decisions that grant authority](#decisions-that-grant-authority) lists what each
choice hands out. This is the short actionable form:

- **Track only what you would share.** Every tracked repository is readable and
  writable by every agent session on the machine. Prune the list with
  `gortex untrack`; it is a standing grant, not a per-session one.
- **Prefer the unix socket.** Only pass `--http-addr` if you need HTTP. If you
  do: set `--http-auth-token` even on loopback, narrow `--cors-origin` from its
  `*` default, and add `--http-allowed-origin` only for a web front end you run.
- **Never bind a non-loopback address without a token.** Gortex refuses to start
  that way; do not work around it. Use an SSH tunnel instead.
- **Leave `GORTEX_DAEMON_PPROF_ADDR` unset** outside a debugging session — it
  serves your argv (including auth tokens) and heap unauthenticated.
- **Treat a configured LLM provider as data egress**: prompts derived from your
  source go to that endpoint or subprocess. None is configured by default.
- **Review agent-authored edits.** Write tools are first-class, and changed
  source runs the next time you build or test.
- **Point Gortex at repositories you trust.** Indexed content reaches the agent's
  context, which is the prompt-injection surface the confinement boundaries
  exist to contain.
