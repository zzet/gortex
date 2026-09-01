package mcp

import (
	"sort"
	"strings"
)

// The guide is the single on-demand home for Gortex reference detail that
// used to be pre-paid in the installed CLAUDE.md section: the LLM-provider
// matrix, the non-obvious capabilities catalog, the token-economy deep-dive,
// the MCP resources list, and pointers into the analyze / search_ast
// catalogs (which live authoritatively in `kind:"help"` / `detector:"help"`
// and the gortex://schema resource). Nothing here is deleted from the
// product — it stops being ambient and becomes reachable via the
// `gortex://guide` MCP resource and the `gortex guide [topic]` CLI verb.
//
// Single-home invariants enforced by tests:
//   - the LLM-provider matrix lives ONLY here (not in the CLAUDE.md sections);
//   - the analyze kind catalog lives in `kind:"help"` / gortex://schema and is
//     rendered here from the same source — never re-inlined into CLAUDE.md;
//   - the wire-FORMAT deep-dive lives ONLY in the server instructions
//     (sharedParamLegend) — the guide points at it, it is not repeated here.

// guideProviders is the LLM-provider matrix + the `ask` delegation surface,
// relocated verbatim out of the installed rule block. The 13-provider
// enumeration is the single-home marker asserted by the gates.
const guideProviders = `## LLM providers (powers ` + "`ask`" + ` and ` + "`search_symbols assist:`" + ` modes)

Selected via ` + "`llm.provider`" + ` in ` + "`.gortex.yaml`" + ` or ` + "`~/.gortex/config.yaml`" + `, or ` + "`GORTEX_LLM_PROVIDER`" + ` / ` + "`GORTEX_LLM_MODEL`" + `. One of:
` + "`local`" + ` / ` + "`anthropic`" + ` / ` + "`openai`" + ` / ` + "`azure`" + ` / ` + "`ollama`" + ` / ` + "`claudecli`" + ` / ` + "`codex`" + ` / ` + "`copilot`" + ` / ` + "`cursor`" + ` / ` + "`opencode`" + ` / ` + "`gemini`" + ` / ` + "`bedrock`" + ` / ` + "`deepseek`" + `.
Only ` + "`local`" + ` needs a ` + "`-tags llama`" + ` build; the HTTP and subprocess adapters are pure Go, available in every binary.

| Provider | Backend | Requires |
|---|---|---|
| ` + "`local`" + ` (default) | in-process llama.cpp | ` + "`-tags llama`" + ` build + ` + "`llm.local.model`" + ` (a ` + "`.gguf`" + `) |
| ` + "`anthropic`" + ` | Messages API | ` + "`llm.anthropic.model`" + ` + ` + "`ANTHROPIC_API_KEY`" + ` |
| ` + "`openai`" + ` | Chat Completions | ` + "`llm.openai.model`" + ` + ` + "`OPENAI_API_KEY`" + ` |
| ` + "`azure`" + ` | Azure OpenAI | ` + "`llm.azure.deployment`" + ` + endpoint + ` + "`AZURE_OPENAI_API_KEY`" + ` |
| ` + "`ollama`" + ` | Ollama daemon | ` + "`llm.ollama.model`" + ` (+ ` + "`llm.ollama.host`" + `) |
| ` + "`claudecli`" + ` / ` + "`codex`" + ` / ` + "`copilot`" + ` / ` + "`cursor`" + ` / ` + "`opencode`" + ` | CLI subprocess | the named CLI on ` + "`$PATH`" + ` (signed in once) |
| ` + "`gemini`" + ` | Gemini ` + "`generateContent`" + ` | ` + "`llm.gemini.model`" + ` + ` + "`GEMINI_API_KEY`" + ` |
| ` + "`bedrock`" + ` | AWS Bedrock Converse (SigV4) | ` + "`llm.bedrock.model_id`" + ` + AWS creds (region default ` + "`us-east-1`" + `) |
| ` + "`deepseek`" + ` | DeepSeek Chat Completions | ` + "`llm.deepseek.model`" + ` + ` + "`DEEPSEEK_API_KEY`" + ` |

Custom OpenAI-compatible endpoints register by name with ` + "`gortex provider add/list/show/remove`" + `. Anthropic accepts the tier sentinels ` + "`claude-haiku`" + ` / ` + "`claude-sonnet`" + ` / ` + "`claude-opus`" + ` and opt-in prompt caching / thinking / effort. ` + "`llm.routing`" + ` (off by default) routes ` + "`ask`" + ` to a cheaper or more capable model by graph-derived task complexity.

### Delegate research to a local agent (` + "`ask`" + `)

When a provider is configured the ` + "`ask`" + ` MCP tool is registered — a grammar-constrained agent that uses gortex tools to research one question and returns a synthesized answer. Reach for it when you'd otherwise issue many ` + "`search_symbols`" + ` / ` + "`get_callers`" + ` / ` + "`contracts`" + ` calls; pass ` + "`chain: true`" + ` to trace a request across repos. If ` + "`ask`" + ` isn't in ` + "`tools/list`" + `, no provider could construct — fall through to direct tools.

### ` + "`search_symbols assist:`" + ` modes

` + "`auto`" + ` (default — skips the LLM for identifier queries, expands NL queries), ` + "`on`" + ` (forces expansion+rerank), ` + "`off`" + ` (pure BM25), ` + "`deep`" + ` (adds a body-grounded verification pass; +1.5-4s; quality is model-dependent).`

// guideCapabilities is the non-obvious capabilities tour, relocated from the
// rule block. Reference material an agent reaches for on demand, not a
// mid-session action list.
const guideCapabilities = `## Non-obvious capabilities

- **` + "`compress_bodies: true`" + `** on ` + "`read_file`" + ` / ` + "`get_symbol_source`" + ` / ` + "`get_editing_context`" + ` elides function bodies to stubs while keeping signatures + doc-comments + structure. ~30-40% of original tokens. Composes with ` + "`format:\"gcx\"`" + `; safe to set unconditionally (no-op on unparseable input).
- **Overlay sessions** (` + "`overlay_push`" + ` / ` + "`overlay_list`" + ` / ` + "`overlay_drop`" + ` / ` + "`compare_with_overlay`" + `) let editor extensions push unsaved buffers as a per-session shadow graph — every subsequent tool call reads through it without mutating base. Idle TTL via ` + "`GORTEX_OVERLAY_IDLE_TTL`" + ` (default 30m).
- **Speculative execution** (` + "`preview_edit`" + ` / ` + "`simulate_chain`" + `) takes an LSP ` + "`WorkspaceEdit`" + ` and returns the graph diff + broken callers/implementors + impact rollup + suggested tests + optional diagnostics — disk untouched. ` + "`simulate_chain keep:true`" + ` promotes the final state into a real overlay.
- **Change-contract pipeline** (` + "`change_contract`" + ` / ` + "`symbols_for_ranges`" + `) lowers a WorkspaceEdit / diff range / symbol set / line-ranges into one verdict ` + "`{allow|warn|refuse}`" + ` with reasons, risk, a verification command, a stop condition, and an edit strategy. The pre-write **parse gate** on ` + "`edit_file`" + ` / ` + "`write_file`" + ` refuses edits that introduce new tree-sitter parse errors (override ` + "`allow_parse_errors`" + `).
- **MCP 2026 Streamable HTTP** at ` + "`POST /mcp`" + ` — ` + "`gortex server`" + ` always mounts it; ` + "`gortex daemon --http-addr <addr>`" + ` opts the daemon in (non-localhost binds require ` + "`--http-auth-token`" + `).
- **Artifacts** — non-code knowledge files (DB schemas, API specs, ADRs, infra configs) declared in ` + "`.gortex.yaml`" + ` ` + "`artifacts:`" + ` are indexed as ` + "`artifact`" + ` nodes; ` + "`search_artifacts`" + ` / ` + "`get_artifact`" + ` surface them.
- **` + "`get_architecture`" + `** — one-call architectural snapshot (languages, communities, hotspots, processes); pass ` + "`resolution`" + ` for a symbol → file → package → service → system rollup.
- **Capability edges** — ` + "`reads_env`" + ` / ` + "`executes_process`" + ` / ` + "`accesses_field`" + ` are first-class traversable edges for supply-chain / least-privilege audits ("what reads $AWS_SECRET", "what shells out", "what writes this field").
- **PR review, end-to-end** — ` + "`gortex prs`" + ` triages open PRs from the graph; ` + "`gortex review [<base>|--diff] [--audience agent|human]`" + ` reviews a diff. MCP mirrors: ` + "`pr_risk`" + ` / ` + "`list_prs`" + ` / ` + "`triage_prs`" + ` / ` + "`review`" + ` / ` + "`review_pack`" + `.
- **Broad ingest** — image + PDF nodes; first-class extractors for Terraform/HCL, Helm, Ansible, .NET ` + "`.sln`" + `/` + "`.csproj`" + `, MCP configs, Quarto, Luau, COBOL/JCL, C/C++ macros. ` + "`gortex db schema --postgres <dsn>`" + ` ingests a live DB schema.`

// guideTokens is the token-economy deep-dive MINUS the wire-format section
// (which lives authoritatively in the server instructions / sharedParamLegend
// and is only pointed at here — the single-home invariant for format).
const guideTokens = `## Token economy

The wire-FORMAT knobs (` + "`format`" + ` = json/gcx/toon, ` + "`max_bytes`" + `, ` + "`cursor`" + `, ` + "`fields`" + `, ` + "`limit`" + `, ` + "`scope`" + `, ` + "`repo`" + `/` + "`project`" + `/` + "`workspace`" + `/` + "`ref`" + `) are documented once in the MCP server ` + "`instructions`" + ` (the shared-parameter legend emitted at initialize). Read them there; this section covers the orthogonal content knobs.

### Content compression (` + "`compress_bodies`" + `)

Replaces every function/method body in returned source with a ` + "`{ /* N lines elided */ }`" + ` stub (language-appropriate). Signatures, doc-comments, imports, top-level constants/types, and structure stay intact. A 200-line file lands at ≤ 60 lines. Wired in 16 languages. Composes with ` + "`format:\"gcx\"`" + ` for stacked savings.

### Graceful degradation

List-shaped tools run through a per-response budget. On overflow the server strips verbose meta, then drops tier-3 rows (params/closures/low-confidence edges), then tier-2 rows, then tail-trims the longest tier-1 list — each escape stamped (` + "`_meta_stripped`" + `, ` + "`_dropped_tier_<N>_<list>`" + `, ` + "`_truncated_by_budget`" + `). Use the metadata to decide whether to narrow the filter, raise ` + "`max_bytes`" + `, or paginate.

### Reuse and freshness

Treat what you fetch as cached for the task — don't re-run the same query. ` + "`read_file`" + ` / ` + "`get_symbol_source`" + ` / ` + "`get_file_summary`" + ` / ` + "`get_editing_context`" + ` / ` + "`smart_context`" + ` return an ` + "`etag`" + ` and accept ` + "`if_none_match`" + ` — an unchanged target comes back ` + "`not_modified`" + ` at ~0 tokens. A ` + "`count: 0`" + ` while the daemon is warming (a ` + "`warming`" + ` block, or ` + "`index_health`" + ` not ready) means *not indexed yet*, not *absent*.`

// guideResources is the MCP resources catalog, relocated from the rule block.
const guideResources = `## MCP resources

Bootstrap-state tools are also exposed as read-only, URI-addressable resources — subscribe via ` + "`resources/subscribe`" + ` once and receive ` + "`notifications/resources/updated`" + ` after each graph re-warm (no polling).

| Resource | Payload |
|---|---|
| ` + "`gortex://stats`" + ` | ` + "`graph_stats`" + ` |
| ` + "`gortex://index-health`" + ` | ` + "`index_health`" + ` |
| ` + "`gortex://workspace`" + ` | ` + "`workspace_info`" + ` |
| ` + "`gortex://repos`" + ` | ` + "`list_repos`" + ` |
| ` + "`gortex://active-project`" + ` | ` + "`get_active_project`" + ` |
| ` + "`gortex://schema`" + ` | graph schema + analyze/search_ast catalogs |
| ` + "`gortex://guide`" + ` | this reference |

Analyzer rollups (read-only summaries of the indexed state): ` + "`gortex://report`" + ` (orientation), ` + "`gortex://god-nodes`" + ` (top hotspots), ` + "`gortex://surprises`" + ` (cycles + dead code + hubs), ` + "`gortex://audit`" + ` (CLAUDE.md drift), ` + "`gortex://questions`" + ` (TODOs), plus ` + "`gortex://communities`" + ` / ` + "`gortex://processes`" + ` templates.`

// guideWorkflow is the full session-start checklist, relocated here so the
// installed policy core keeps only the memory-trigger essentials.
const guideWorkflow = `## Session-start checklist

1. Confirm the daemon with ` + "`index_health`" + ` (cheap liveness + scope). Call ` + "`graph_stats`" + ` only when you need node/edge counts or ` + "`per_repo`" + ` orientation.
2. If ` + "`total_nodes`" + ` is 0, ` + "`index_repository`" + ` with path ` + "`\".\"`" + ` before anything else.
3. Call ` + "`distill_session`" + ` to recover prior-session memory (decisions, pinned notes, recent excerpts).
4. In multi-repo mode, ` + "`get_active_project`" + ` to check scope; ` + "`set_active_project`" + ` to switch.
5. Open a non-trivial task with ` + "`smart_context`" + `; then ` + "`surface_memories task:\"…\" symbol_ids:\"…\"`" + ` to pick up cross-session invariants.
6. Before editing a file, ` + "`get_editing_context`" + `; if you've touched the symbol before, ` + "`query_notes`" + ` / ` + "`query_memories`" + ` by symbol_id.
7. Before a signature change, ` + "`verify_change`" + `. For a refactor, ` + "`get_edit_plan`" + ` then ` + "`batch_edit`" + `.
8. Verify with the project's real build/test. Reserve ` + "`check_guards`" + ` / ` + "`get_test_targets`" + ` for substantive changes.
9. Before committing, ` + "`detect_changes`" + ` for scope + ` + "`diff_context`" + ` for graph-enriched review.

The behavior-critical memory triggers (distill_session, surface_memories, save_note, store_memory) live in the installed policy core (~/.claude/CLAUDE.md). This checklist is the fuller reference.`

const guideViews = `## Worktree and branch views

The session CWD selects an automatically discovered checkout overlay. Do not call track for an ordinary worktree: explicit tracking means the user requested a dedicated logical graph. Overall is one coherent view—the selected overlay plus its designated primary—not a union of branches. Overlay data only wins for duplicate logical identities; tombstones hide deleted base identities.

Every response carries freshness metadata. ` + "`exact:false`" + ` means the requested view was not served, and every fallback is read-only. Set ` + "`require_exact:true`" + ` to reject fallback, ` + "`require_fresh:true`" + ` to wait for current filesystem state, and bound waiting with an absolute RFC3339 ` + "`wait_deadline`" + `.

When CWD is not the target, select it explicitly: ` + "`view:{kind:\"worktree\",checkout_id:\"…\"}`" + ` or ` + "`view:{kind:\"git_ref\",value:\"refs/heads/release\",graph_id:\"…\"}`" + `. Inactive ref/commit views have committed source and structural graph only—no working-copy LSP, ` + "`search.text`" + `, or edits. Exact worktree edits are allowed only through the approved coordinator-backed path; fallback and ref/commit views are read-only.

Checkout administration previews destructive effects by default. Before primary closure, family forget, or set-primary, inspect the preview and obtain explicit user confirmation before sending confirm.`

// guideSection maps a topic keyword to its rendered section. Section bodies
// that need a live catalog (analyze / search_ast) are rendered from the same
// source functions that back kind:"help" / detector:"help", so the catalog
// has a single source of truth.
func guideSection(topic string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(topic)) {
	case "providers", "provider", "llm", "ask":
		return guideProviders, true
	case "capabilities", "capability", "features", "feature":
		return guideCapabilities, true
	case "tokens", "token", "economy", "compress", "compress_bodies":
		return guideTokens, true
	case "resources", "resource":
		return guideResources, true
	case "workflow", "session", "checklist", "session-start":
		return guideWorkflow, true
	case "views", "view", "worktrees", "branches":
		return guideViews, true
	case "analyze", "analyzers", "kinds":
		// Point at the single-source catalog rather than re-inlining it: the
		// full per-kind reference lives in `analyze kind:"help"` and the
		// gortex://schema resource.
		return "## analyze\n\n" + analyzeGroupedSummary, true
	case "search_ast", "ast", "detectors":
		return "## search_ast\n\n" + buildSearchASTDescription() +
			"\n\nFull detector catalog: `search_ast detector:\"help\"` or the gortex://schema resource.", true
	default:
		return "", false
	}
}

// guideTopicOrder is the canonical section order for the full render and the
// topic list.
var guideTopicOrder = []string{"providers", "capabilities", "tokens", "views", "analyze", "search_ast", "resources", "workflow"}

// GuideTopics returns the addressable guide topic keywords, sorted.
func GuideTopics() []string {
	out := append([]string(nil), guideTopicOrder...)
	sort.Strings(out)
	return out
}

// GuideText renders the guide. An empty topic (or "all"/"index") returns the
// full document with a table of contents; a known topic returns just that
// section; an unknown topic returns the overview with the topic list so the
// caller can retry.
func GuideText(topic string) string {
	t := strings.ToLower(strings.TrimSpace(topic))
	if t != "" && t != "all" && t != "index" && t != "toc" {
		if sec, ok := guideSection(t); ok {
			return sec + "\n"
		}
		// Unknown topic: fall through to the overview so the reply is still
		// useful and names the valid topics.
	}

	var b strings.Builder
	b.WriteString("# Gortex Guide\n\n")
	b.WriteString("On-demand reference for Gortex. The mandatory graph-tools policy and the memory workflow live in the installed CLAUDE.md; everything below is reachable via the `gortex://guide` resource or `gortex guide [topic]`.\n\n")
	if t != "" && t != "all" && t != "index" && t != "toc" {
		b.WriteString("(Unknown topic \"" + topic + "\" — showing the full guide. Topics: " + strings.Join(guideTopicOrder, ", ") + ".)\n\n")
	} else {
		b.WriteString("Topics: " + strings.Join(guideTopicOrder, ", ") + ". Address one with `gortex guide <topic>` or `gortex://guide/<topic>`.\n\n")
	}
	for _, name := range guideTopicOrder {
		sec, _ := guideSection(name)
		b.WriteString(sec)
		b.WriteString("\n\n")
	}
	return b.String()
}
