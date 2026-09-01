# Token savings

Gortex tracks how many tokens it saves compared to naive file reads — per-call, per-session, and cumulative across restarts:

- **Per-call:** two shapes of call book an observation server-side. The per-call value is deliberately not echoed in responses (agents don't act on it and it would burn tokens on every reply); it lands in the ledger.
  - **Read-family** — `read_file`, `get_file_summary`, `get_editing_context`, `get_symbol_source`, `batch_symbols` (with `include_source`), `smart_context`: tokens actually returned vs the *one* full file the response stands in for.
  - **Retrieval** — `explore`, `context_closure`, `prefetch_context`, `get_repo_outline`, `get_artifact`, the `search_*` family, the relations family (`find_usages`, `get_callers`, `get_dependencies`, `get_dependents`, `find_implementations`, …), the trace family (`get_call_chain`, `flow_between`, `taint_paths`, `walk_graph`, …), `nav`, `suggest_pattern`: tokens returned vs the *set* of whole files the page cites. These are the calls that replace a grep-then-read sweep, so their counterfactual is the files the caller would otherwise have opened.

- **Session-level:** `graph_stats` returns a `token_savings` object with `calls_counted`, `tokens_returned`, `tokens_saved`, `efficiency_ratio`.
- **Cumulative (cross-session):** `graph_stats` also returns `cumulative_savings` when persistence is wired — includes `first_seen`, `last_updated`, and `cost_avoided_usd` per model — Anthropic (Opus/Sonnet/Haiku), OpenAI (GPT-5, GPT-4.1, GPT-4o, o3/o4-mini), Google Gemini (3.x and 2.5), DeepSeek, and Zhipu GLM (5.x and 4.5/4.6/4.7 tiers). Backed by the machine-global sidecar database (`~/.gortex/sidecar.sqlite` — the same file that holds notes/memories): `savings_totals` carries top-line + per-repo + per-language aggregates and `savings_events` one session-tagged row per call, powering the windowed buckets and the per-tool breakdown. Each observation commits transactionally, so the ledger survives SIGKILLed MCP servers and concurrent writer processes. Flat-file ledgers from older releases (`~/.gortex/cache/savings.json` + `savings.jsonl`) are imported once on first open and renamed `*.bak`.

### What the retrieval baseline deliberately does not claim

A retrieval page names more files than its caller would have opened, and naming a file twice does not save reading it twice. Two limits keep the number honest, and both under-report on purpose — the ledger's failure mode must be "Gortex looks worse than it is", never "Gortex invented savings":

- **Once per session, per file.** A file's whole-file baseline is credited at most once per MCP session, whichever call surfaces it first — read-family or retrieval. A repeated search over the same corner of the repo books nothing. This bounds everything a session can ever claim to the cost of reading its repo once.
- **Capped per call.** At most 8 distinct cited files are credited by any single call. A 50-hit `find_usages` page does not mean the caller would have opened 50 files; it would have opened the few hits it cared about, and we cannot know which.

A call that credits no new file books nothing at all, rather than a zero-baseline row: we did not measure that call's counterfactual, so we do not assert one. Reading a whole file through `read_file` still books `saved = 0` — it displaces nothing — which is why a healthy ledger shows read_file at 0% next to the retrieval tools that carry the real savings.

`gortex savings` renders a three-bucket dashboard:

```text
Gortex Token Savings
====================
Cost avoided:   $168.69 (claude-opus-4) across 1,878 calls · 11,246,094 tokens saved

Today       ████████░░░░░░░░   50.0%  saved 9,200 / 18,400 tokens   $0.14
Last 7 days ██████████░░░░░░   62.5%  saved 60,100 / 96,200 tokens  $0.90
All time    ███████████████░   93.3%  saved 11,246,094 / 12,050,716 tokens  $168.69
```

```bash
# Three-bucket dashboard with USD on top
gortex savings

# Per-tool breakdown inside each bucket
gortex savings --verbose

# Headline a single model (fuzzy match: "opus" → claude-opus-4)
gortex savings --model opus

# Bucket "Today" by UTC instead of local time
gortex savings --utc

# Machine-readable output (mirrors the dashboard structure: buckets[].per_tool, cost_avoided_usd, etc.)
gortex savings --json

# Wipe cumulative totals and the event history
gortex savings --reset

# Override pricing (JSON array of {model, usd_per_m_input})
GORTEX_MODEL_PRICING_JSON='[{"model":"mycorp","usd_per_m_input":5}]' gortex savings
```

Token counts use **tiktoken (`cl100k_base`)** — the tokenizer Claude and GPT-4 actually use — via `github.com/pkoukk/tiktoken-go` with an embedded offline BPE loader, so no runtime downloads. The BPE is lazy-loaded on first call. If init fails for any reason, the package falls back to the legacy `chars/4` heuristic so metrics stay usable.
