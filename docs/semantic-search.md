# Semantic search

**Default-on.** A baked GloVe-50d table (~3.8 MB embedded in the binary, top 20k tokens) gives every install hybrid store-native FTS5/BM25 + vector search out of the box — no flag, no model download, no native dependency. Adaptive alpha-weighted rank fusion blends the two channels, and the text↔vector balance is scored *continuously* from the query's shape (identifier density, separators, stopwords) rather than bucketed into a discrete class — so a half-identifier query lands between the symbol and natural-language blends instead of jumping a whole tier. After ranking, an optional pure-cosine refinement pass re-scores the top results with the exact embedding distance the rank-based fusion discards.

## Configuration

Switch or tune providers in `.gortex.yaml`:

```yaml
embedding:
  enabled: true                # default — pass `false` to disable
  provider: static             # static | local | api  (default: static)
  variant: ""                  # optional named local model (e.g. a Hugot variant); empty = the provider default
  api_url: http://localhost:11434
  api_model: nomic-embed-text
  chunk_threshold_lines: 60    # symbols longer than this get split
  chunk_window_lines: 40       # AST-aware window size
  api_concurrency: 4           # bounded worker pool for hosted providers
```

Selecting a different embedding model (`variant`, or `GORTEX_EMBEDDINGS_VARIANT`) that changes vector dimensionality re-embeds the graph on next index; the persisted index guards against a dimension mismatch.

### Where the config lives — and what wins

The `embedding:` block can sit in more than one place; precedence, highest first:

1. **`--embeddings` / `--embeddings-url` / `--embeddings-model` flags** and the **`GORTEX_EMBEDDINGS*` env vars** — one-shot overrides. A URL forces the `api` provider; `GORTEX_EMBEDDINGS=0/1` toggles the vector channel; `GORTEX_EMBEDDINGS_VARIANT` pins a local model.
2. **Repo-local `.gortex.yaml`** — the per-project `embedding:` block (loaded by viper, so `GORTEX_EMBEDDING_*` env keys also merge here).
3. **Global `~/.gortex/config.yaml`** — a user-level `embedding:` block layered *under* the repo-local one: every field the repo leaves unset inherits the global value (the tri-state `enabled:` too), so one block can serve every repo. An unrecognised top-level key in this file is ignored with a startup warning (`contains keys gortex does not recognize`) rather than silently dropped — the usual cause of an `embedding:` block that "does nothing" is placing it under the wrong key here.

| Provider | Quality | Offline | Native deps | Notes |
|---|---|---|---|---|
| `static` (default) | Good for identifier-shaped queries | Yes | None | Baked GloVe-50d table, CPU-only, zero setup |
| `local` (Hugot MiniLM-L6-v2) | Better for NL queries | After first run | None | Auto-downloads ~90 MB to `~/.gortex/models/` |
| `api` (Ollama / OpenAI) | Best | No | None | Bounded concurrent worker pool — tune via `api_concurrency` |

## AST sub-chunking

Symbols longer than `chunk_threshold_lines` are split into AST-aware windows (block statements, case clauses, field groups) before embedding; each window is vectorised independently and de-duplicated back to the parent symbol at query time, so a large function lands as one hit grounded in the specific chunk that matched — chunk IDs never leak into results.

## Input truncation

Every embedding input is capped at the model's positional budget — `max_position_embeddings` from the model's `config.json` minus the two special-token slots (510 for MiniLM/BERT; larger for wide-context variants) — before it reaches inference. A transformer cannot attend past that window, so trimming the tail is lossless by construction, and it is load-bearing: the pure-Go tokenizer path does not enforce the limit itself, so a single over-budget input would otherwise reach inference at full length and abort the *entire* vector-index build with a tensor shape mismatch — dropping the daemon to text-only search. AST chunking already splits long symbols into sub-budget windows, so truncation only ever trims a pathological single chunk. If the model directory lacks a readable `config.json` the budget falls back to 510; if the tokenizer itself can't load, truncation degrades to a rune clamp rather than disabling the backend.

## Persistent index

The vector index and the chunk → symbol map are persisted in the daemon's graph store; restarts re-warm in milliseconds without re-embedding the graph. The store's schema is forward-compatible — a store written before the vector layer existed opens with an empty one and rebuilds incrementally.

## Vocabulary bridging without an LLM

A curated equivalence table (`auth` ↔ `authentication` ↔ `login`, `delete` ↔ `remove` ↔ `destroy`, …) plus per-repo auto-concept mining from symbol-name token co-occurrence expands queries deterministically — runs alongside (and dedup against) any LLM expansion. Toggle via `search.equivalence_classes`. A weighted concept-relatedness layer sits on top of the flat classes (e.g. `auth` pulls in `token` / `session` at lower priority) without merging the distinct concepts. When an LLM expander *is* configured, `vocab_anchored: true` constrains its invented terms to tokens that actually occur in the repo's symbol vocabulary.

## HITS reranking

A hubs-and-authorities pass over the reference/call graph contributes a `hits` signal to the rerank pipeline — heavily-referenced symbols outrank shallow utility nodes, and the hub penalty (`authority / (1 + hub)`) demotes called-by-everything infra so it doesn't drown the result page.

### Edge-provenance attenuation

Centrality (HITS + PageRank) and a dedicated rerank signal weight call/reference edges by how they were resolved: the abundant LSP-dispatch / framework-wiring tier — and the weak name-only tier — are attenuated relative to the structurally-unambiguous tier. Dense LSP enrichment otherwise inflates the apparent centrality of utility and framework code over genuine domain authorities. The weighting is a no-op on graphs with no resolution provenance recorded, so it never changes ranking where the data is absent.

### Other rerank refinements

- **Generated-file demotion** — a generated file (`*.pb.go`, `mock_*.go`, `*_pb2.py`, …) is ranked below a real same-named hand-written implementation, but only when one exists.
- **Source over test** — when a query surfaces both an implementation and its test, the implementation is lifted above the test (only when both co-occur, so it never shifts the rest of the page).

## Keyword-soup defense

Boolean / OR-soup queries (`A OR B OR 'no access' OR …`) — and operator-free keyword lists (`parse decode unmarshal token jwt cache`) and comma-enumerations — defeat embedding retrieval. The query classifier detects all three, skips wasted LLM expansion, and splits the soup into terms fused via the store-native lexical expansion path; a `query_advice` nudge rides on the response. Genuine natural-language questions stay classified as concept. Tune via `search.keyword_soup_rewrite: split | nudge | off`.

## Prose corpus

Markdown headings + section bodies become first-class searchable nodes (`KindDoc`) — `search_symbols corpus: "docs"` returns ranked README / ADR / design-doc sections; `corpus: "all"` mixes them with code hits. A docs query runs its own retrieval channel (a parallel doc-biased fetch, not merely a post-filter over the code fetch) and applies a prose weight profile that suppresses code-structural rerank signals (API/type-signature, definition-bias) which are meaningless for prose. Section node IDs are derived from the heading path, so incremental reindex of a touched markdown file produces stable IDs.

## Per-keyword TaskMemory

The combo store now keys symbol associations both on the whole query and per keyword, so a new task with similar keywords inherits learned ranking from prior searches even when the exact phrasing differs. Exact-query matches still dominate; per-keyword evidence is the lower-confidence generalisation.

## Build-tag backends

Opt-in faster local backends via build tags:

```bash
go build -tags "embeddings_gomlx XLA" ./cmd/gortex/   # needs libtokenizers.a on the linker path — use `make build-gomlx` (see below)
```

The GoMLX/XLA backend requires **both** tags — `embeddings_gomlx` alone links a disabled XLA stub and always falls through to the pure-Go backend; the `XLA` tag is what compiles the real XLA session. It also statically links the rust tokenizer, so the build needs `libtokenizers.a` on the linker path (a prebuilt archive from [daulet/tokenizers](https://github.com/daulet/tokenizers) releases, at `/usr/lib` or `/usr/local/lib`); `make build-gomlx` downloads it for you. At runtime the XLA/PJRT plugin auto-downloads (~100 MB). XLA/PJRT runtime viability is platform-dependent and still experimental — if the plugin fails to load, the local chain degrades to the pure-Go Hugot backend and the startup log names the failed backend (see Troubleshooting). The default pure-Go backend needs no tags and no native libraries, and is the reliable path.

| Build tag | Backend | Model | Extra dependency | Status |
|---|---|---|---|---|
| _(none)_ | Hugot pure-Go | MiniLM-L6-v2 (auto-download) | none | **default — reliable path** |
| `embeddings_gomlx XLA` | Hugot + XLA/GoMLX | MiniLM-L6-v2 (auto-download) | libtokenizers.a (build) + PJRT plugin (runtime download) | experimental — XLA/PJRT runtime is platform-dependent |

The legacy `--embeddings` / `--embeddings-url` / `--embeddings-model` CLI flags and the `GORTEX_EMBEDDINGS*` env vars still take precedence over the config block — useful for one-shot overrides without editing `.gortex.yaml`.

## Troubleshooting

Semantic search degrading to text-only (BM25 / FTS5) is always logged — match the daemon log line to the cause:

- **`embeddings enabled ... provider: local (hugot/fp32), dim: 384`** — working as intended: the transformer backend is active at its true width.
- **`embeddings enabled ... provider: local → static fallback, dim: 50`** — a `local` config could not construct any transformer backend and fell back to static GloVe. The preceding `embedding backend unavailable — degraded to static fallback` warnings name each backend and why (an uncached model with downloads disabled, …). Fix the named backend or accept static; the width and provider name now tell the truth rather than echoing the configured name.
- **`vector index aborted on chunk failure`** — an embedding call failed (API timeout / auth, or an over-long input on a build without truncation) and the whole vector index was dropped to avoid a half-embedded, mis-scoring index. Text search stays live. `gortex eval embedders` reports the concrete cause as `vector build failed: …` instead of a bare "no vector data".
- **`vector index built ... dropped: N`** (N > 0) — N malformed vectors (nil / wrong width) were skipped; the `sample_ids` warning names the first few. A non-zero `dropped` from a healthy provider is worth investigating.
- **`vector index disabled — embedding text count exceeds threshold`** — the corpus is larger than `embedding.max_symbols`; raise it if you have the memory headroom.
- **`~/.gortex/config.yaml contains keys gortex does not recognize`** — a top-level key (commonly an `embedding:` block nested one level too deep, or a typo) is being ignored; move it to a recognised key.

## `search_symbols` `assist:` modes

- `auto` (default) — skips LLM for identifier queries, expands NL queries
- `on` — forces expansion + rerank
- `off` — store-native FTS5/BM25 only
- `deep` — adds a body-grounded verification pass; +1.5–4 s; quality is highly model-dependent — unreliable on 3B local models, fine on 7B+ or hosted

See [llm.md](llm.md) for provider configuration.
