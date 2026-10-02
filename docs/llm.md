# LLM features (optional)

Gortex can delegate code-intelligence work to an LLM. Two features, both **off by default** and gated on configuring a provider:

- **`ask` MCP tool** — a research agent that drives Gortex's own tools (search, callers, contracts, dependencies) to answer an open-ended question and returns a synthesized answer, instead of the calling agent issuing many tool calls itself. `chain: true` traces cross-system call chains.
- **`search_symbols` `assist` arg** — LLM-assisted ranking on `search_symbols`: `auto` (engage on natural-language queries only), `on`, `off`, `deep` (adds a body-grounded verification pass that reads candidate code + callers and honestly drops irrelevant matches).

## Providers

The backend is chosen by the `llm.provider` key. Every provider except `local` is pure Go — available in any build; only `local` needs a `-tags llama` build (it embeds llama.cpp). Any OpenAI-compatible endpoint can also be registered as a custom provider (see below).

| `llm.provider` | Backend | Needs |
|----------------|---------|-------|
| `local` | in-process llama.cpp | a `-tags llama` build + a `.gguf` model file |
| `anthropic` | Anthropic Messages API | `ANTHROPIC_API_KEY` |
| `openai` | OpenAI Chat Completions | `OPENAI_API_KEY` |
| `azure` | Azure OpenAI Service | `AZURE_OPENAI_ENDPOINT` (or `llm.azure.endpoint`) + `AZURE_OPENAI_API_KEY` + a deployment name |
| `ollama` | Ollama daemon | a running Ollama + a pulled model |
| `claudecli` | Claude Code CLI subprocess | the `claude` binary on `$PATH` (signed in once). **No API key — reuses your Claude Code subscription.** |
| `codex` | OpenAI Codex CLI subprocess | the `codex` binary on `$PATH` (signed in once). **No API key — reuses your Codex / ChatGPT sign-in.** |
| `copilot` | GitHub Copilot CLI subprocess | the `copilot` binary on `$PATH` (signed in via `gh`). **No API key.** |
| `cursor` | Cursor Agent CLI subprocess | the `cursor-agent` binary on `$PATH` (signed in once). **No API key.** |
| `opencode` | opencode CLI subprocess | the `opencode` binary on `$PATH` (signed in once). **No API key.** |
| `gemini` | Google Gemini `generateContent` REST | `GEMINI_API_KEY` |
| `bedrock` | AWS Bedrock Converse API (SigV4-signed, no AWS SDK) | `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` (+ optional `AWS_SESSION_TOKEN`) |
| `deepseek` | DeepSeek Chat Completions (OpenAI-compatible) | `DEEPSEEK_API_KEY` |
| `requesty` | Requesty gateway (OpenAI-compatible, one key for many vendors) | `REQUESTY_API_KEY` |
| _`<custom>`_ | any OpenAI-compatible endpoint | registered with `gortex provider add` — see [Custom providers](#custom-providers) |

## Configuration

The `llm:` block goes in `~/.gortex/config.yaml` or a per-repo `.gortex.yaml` (repo-local wins per field, global fills the rest). Configure only the provider you use:

```yaml
# ~/.gortex/config.yaml (or per-repo .gortex.yaml)
llm:
  provider: local            # local | anthropic | openai | azure | ollama | claudecli | codex | copilot | cursor | opencode | gemini | bedrock | deepseek | requesty | <custom>
  max_steps: 16              # agent tool-loop cap (provider-agnostic)

  local:                     # provider: local — requires a `-tags llama` build
    model: ~/models/qwen2.5-coder-7b-instruct-q4_k_m.gguf
    ctx: 4096                # context window in tokens
    gpu_layers: 999          # layers to offload to GPU (0 = CPU-only)
    template: chatml         # chatml | llama3

  anthropic:                 # provider: anthropic
    model: claude-sonnet-4-6  # or a tier sentinel: claude-haiku | claude-sonnet | claude-opus
    api_key_env: ANTHROPIC_API_KEY   # env var holding the key (this is the default)
    # base_url: https://api.anthropic.com
    # prompt_caching: true    # opt-in ephemeral caching of the system prompt + tool (off by default)
    # cache_ttl: 5m           # 5m (free refresh) | 1h (2x write cost)
    # thinking_mode: auto      # off | auto | manual | adaptive (freeform requests only)
    # thinking_budget_tokens: 8000   # manual-mode budget (min 1024)
    # effort: high            # output_config.effort: low|medium|high|max|xhigh (model-gated)

  openai:                    # provider: openai
    model: gpt-4o
    api_key_env: OPENAI_API_KEY
    # effort: high            # optional reasoning_effort (minimal|low|medium|high)

  azure:                     # provider: azure — Azure OpenAI Service
    deployment: my-gpt4o     # the Azure deployment name (selects the model)
    endpoint: https://my-resource.openai.azure.com   # or set AZURE_OPENAI_ENDPOINT
    api_version: "2024-10-21"
    api_key_env: AZURE_OPENAI_API_KEY

  ollama:                    # provider: ollama
    model: qwen2.5-coder:7b
    host: http://localhost:11434

  claudecli:                 # provider: claudecli — spawns the `claude` CLI per call
    # binary: claude          # binary name or absolute path (resolved via $PATH; default "claude")
    model: sonnet             # optional — forwarded as `--model`; empty = CLI default
    # args: ["--permission-mode", "plan"]   # extra args appended after our flags; a flag here overrides the matching headless default
    # timeout_seconds: 180    # cap per Complete call; 0 → 120s

  codex:                     # provider: codex — spawns the OpenAI `codex` CLI per call
    # binary: codex           # binary name or absolute path (resolved via $PATH; default "codex")
    model: gpt-5-codex        # optional — forwarded as `--model`; empty = CLI default
    # args: ["--sandbox", "workspace-write"]   # extra args inserted before the prompt
    # timeout_seconds: 180    # cap per Complete call; 0 → 180s

  copilot:                   # provider: copilot — spawns the GitHub `copilot` CLI per call
    model: claude-opus-4.1    # optional — forwarded as `--model`; empty = CLI default
    # timeout_seconds: 180

  cursor:                    # provider: cursor — spawns the `cursor-agent` CLI per call
    model: sonnet             # optional — forwarded as `--model`; empty = CLI default
    # timeout_seconds: 180

  opencode:                  # provider: opencode — spawns the `opencode` CLI per call
    model: anthropic/claude-sonnet-4-6   # opencode's provider/model form
    # timeout_seconds: 180

  gemini:                    # provider: gemini — Google Gemini generateContent REST
    model: gemini-2.5-pro
    api_key_env: GEMINI_API_KEY
    # base_url: https://generativelanguage.googleapis.com

  bedrock:                   # provider: bedrock — AWS Bedrock Converse API (SigV4-signed)
    model_id: anthropic.claude-sonnet-4-20250514-v1:0
    region: us-east-1
    # access_key_env: AWS_ACCESS_KEY_ID
    # secret_key_env: AWS_SECRET_ACCESS_KEY
    # session_token_env: AWS_SESSION_TOKEN     # optional — for STS-issued creds
    # base_url: https://bedrock-runtime.us-east-1.amazonaws.com   # override for VPC endpoints

  deepseek:                  # provider: deepseek — OpenAI-compatible Chat Completions
    model: deepseek-chat
    api_key_env: DEEPSEEK_API_KEY
    # base_url: https://api.deepseek.com

  requesty:                  # provider: requesty, OpenAI-compatible gateway (https://docs.requesty.ai)
    model: openai/gpt-4o-mini   # vendor/model ids, e.g. anthropic/claude-sonnet-4-5, google/gemini-2.5-flash
    api_key_env: REQUESTY_API_KEY   # key from https://app.requesty.ai/api-keys
    # base_url: https://router.requesty.ai/v1   # EU: https://router.eu.requesty.ai/v1
    # effort: high            # optional reasoning_effort, forwarded to models that support it

  routing:                   # optional — model routing for the `ask` agent
    enabled: false           # off by default; every run uses the provider's model
    simple_model: claude-haiku-4-5    # low-complexity runs (empty = configured model)
    complex_model: claude-opus-4-7    # multi-hop / refactor-scale runs
```

**`claudecli` headless defaults.** A `claude -p` spawn otherwise loads the user's whole interactive configuration, none of which a one-shot completion wants. The provider therefore passes, by default:

| Flag | Why |
|---|---|
| `--settings '{"disableAllHooks":true}'` | The user's hooks run inside the spawn. A `SessionEnd` hook that fails in a headless context exits the CLI nonzero — and that failed the entire call. |
| `--tools ""` | Even under `--print` the CLI exposes `Bash` / `Read` / `Grep` / …, and a model that opens with a native tool call spends the single `--max-turns 1` turn on it: the run dies with `Reached max turns (1)`. `--allowed-tools` is **not** a substitute — it governs auto-approval, not availability, so the call is still attempted. |
| `--strict-mcp-config` | Skip the user's MCP servers: startup latency, plus more tools to be tempted by. |
| `--system-prompt` (instead of `--append-system-prompt`) | Appending leaves Claude Code's agentic persona, the discovered `CLAUDE.md` files and the injected environment block (“you are in `<cwd>`, which is not a git repository”) in force, and that context routinely beats the structured-output instruction — the reply comes back as a clarifying question instead of JSON. |

Every one of these is skipped when `llm.claudecli.args` already carries the same flag, so the config file is the final word: `args: ["--tools", "default"]` restores the built-in toolset, and a custom `--system-prompt` keeps gortex's schema instruction as an `--append-system-prompt` rider rather than dropping it.

**Local provider idle unload.** The in-process llama.cpp model is unloaded after sitting idle (freeing its memory), and reloaded lazily on the next call. Default idle TTL is 10 minutes; override with `GORTEX_LLM_IDLE_TTL` (a verbatim Go duration, e.g. `5m`); `0` / `off` / `none` disables idle unloading, keeping the model resident once loaded.

When `llm.routing.enabled` is true, each `ask` run is scored by graph-derived task complexity — chain-tracing mode, multi-hop keywords, and how broad a slice of the multi-repo graph is in scope — and dispatched to `simple_model` or `complex_model` *within the active provider* (a cheap single-hop lookup costs less; a cross-system trace gets the capable model). The chosen `model` and `complexity` ride on the `ask` response. An empty tier model falls back to the provider's configured model.

Env overrides: `GORTEX_LLM_PROVIDER`, `GORTEX_LLM_MODEL` (targets the active provider's model — for `azure` it sets the deployment), `GORTEX_LLM_MAX_STEPS`, `GORTEX_LLM_{CLAUDECLI,CODEX,COPILOT,CURSOR,OPENCODE}_BINARY`, `GORTEX_LLM_BEDROCK_REGION`, `GORTEX_LLM_AZURE_{ENDPOINT,DEPLOYMENT,API_VERSION}`, `GORTEX_LLM_EFFORT`, `GORTEX_LLM_IDLE_TTL` (`local` provider only), and `GORTEX_LLM_ANTHROPIC_{PROMPT_CACHING,THINKING_MODE,HAIKU_MODEL,SONNET_MODEL,OPUS_MODEL}`. API keys are read from the env var named by `api_key_env` — never stored in the config file.

If the active provider can't be constructed (missing model or API key, `local` without a `-tags llama` build, `claudecli` / `codex` without the `claude` / `codex` binary on `$PATH`, `bedrock` without AWS credentials), the daemon logs a warning and the LLM features stay absent — the rest of Gortex is unaffected. If the `ask` tool isn't in `tools/list`, no provider is configured.

The `assist` prompts are tiered automatically — terser for hosted frontier models, rule-heavy for small local ones. `deep` mode in particular benefits from a 7B-class or hosted model; small local models are unreliable on its disambiguation cases.

## Custom providers

Any OpenAI-compatible Chat Completions endpoint — OpenRouter, Groq, Together, a self-hosted vLLM, an internal gateway — can be registered by name and then selected like a built-in:

```bash
gortex provider add groq \
  --base-url https://api.groq.com/openai/v1 \
  --model llama-3.3-70b-versatile \
  --api-key-env GROQ_API_KEY \
  --price-input 0.59 --price-output 0.79
gortex provider list
gortex provider show groq
gortex provider remove groq
```

Then set `llm.provider: groq` (or `GORTEX_LLM_PROVIDER=groq`). Entries are stored in `providers.json` next to your config; a repo-local `.gortex/providers.json` is loaded only when `GORTEX_ALLOW_LOCAL_PROVIDERS=1` (so a cloned repo can't silently repoint your LLM calls). A custom provider may not shadow a built-in name. Per entry: `base_url` (http/https, including any version segment — gortex appends `/chat/completions`), `model`, optional `api_key_env` (omit for keyless local endpoints), `schema_mode` (`json_schema` (default) | `json_object` | `prompt` — use the looser modes for gateways without strict structured-output support), extra headers, and informational USD pricing.
