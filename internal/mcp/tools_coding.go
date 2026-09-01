package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/elide"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
	"github.com/zzet/gortex/internal/tokens"
)

func (s *Server) registerCodingTools() {
	s.addTool(
		mcp.NewTool("get_editing_context",
			mcp.WithDescription("The primary tool to call before editing any file. Returns all symbols defined in the file, their signatures, direct dependencies, and immediate callers — everything needed to code without reading raw source lines."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Relative file path")),
			mcp.WithString("detail", mcp.Description("brief or full (default: brief)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description(tokenBudgetParamDescription)),
			mcp.WithString("if_none_match", mcp.Description("ETag from a previous response — returns not_modified if content unchanged")),
			mcp.WithBoolean("compress_bodies", mcp.Description("Also return a source_compressed view of the whole file with every function/method body replaced by a `{ /* N lines elided */ }` stub. Signatures, imports, types, and comments are preserved verbatim. Roughly 60-70% fewer tokens than raw source. Composable with format:\"gcx\". Default: false.")),
			mcp.WithString("keep", mcp.Description("Comma-separated symbol names, IDs, or node kinds (function / method / type) whose bodies stay verbatim when compress_bodies is set — every other body in the file is still stubbed. Use to keep the symbols you are about to edit at full source while compressing the rest. Ignored unless compress_bodies is true.")),
			mcp.WithString("fidelity_globs", mcp.Description(fidelityGlobsParamDescription)),
		),
		s.handleGetEditingContext,
	)

	s.addTool(
		mcp.NewTool("find_import_path",
			mcp.WithDescription("Given a symbol name you want to use in a file, returns the correct import path. Use instead of reading files or guessing package paths."),
			mcp.WithString("name", mcp.Required(), mcp.Description("Name of the symbol to import")),
			mcp.WithString("path", mcp.Required(), mcp.Description("File where you want to use the symbol (relative path)")),
		),
		s.handleFindImportPath,
	)

	s.addTool(
		mcp.NewTool("explain_change_impact",
			mcp.WithDescription("Given a list of symbols you plan to modify, returns risk-tiered blast radius: d=1 will break, d=2 likely affected, d=3 needs testing. Includes affected processes and communities."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of symbol IDs to modify")),
			mcp.WithBoolean("summary_only", mcp.Description("Return only by_depth_counts (e.g. {1:3, 2:12}) and drop the per-depth row lists — the cheapest blast-radius shape when you only need the headline counts.")),
			mcp.WithNumber("offset", mcp.Description("Skip this many affected rows (in depth order) before returning by_depth — pairs with limit to page a large blast radius.")),
			mcp.WithNumber("limit", mcp.Description("Max affected rows to return in by_depth (default 100, depth order). by_depth_counts always reports the full per-depth totals regardless.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleEnhancedChangeImpact,
	)

	s.addTool(
		mcp.NewTool("get_symbol_source",
			mcp.WithDescription("Returns the source code of a specific symbol (function, method, type) without reading the entire file. Use instead of Read when you know which symbol you need — saves 70-80% of tokens compared to reading the whole file. Fetching several symbols, or still localizing a task? batch_symbols reads many bodies in one call; explore returns the whole ranked neighborhood (source + call paths) for a task in one call."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol node ID (e.g. pkg/server.go::HandleRequest)")),
			mcp.WithNumber("context_lines", mcp.Description("Extra lines above/below the symbol (default: 3)")),
			mcp.WithString("if_none_match", mcp.Description("ETag from a previous response — returns not_modified if content unchanged")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithBoolean("compress_bodies", mcp.Description("Replace function/method bodies in the returned source with a `{ /* N lines elided */ }` stub. Signatures, doc-comments, and structure stay intact. ~30-40% of original tokens. Useful when you only need the surface signature of the symbol, not its implementation. Default: false.")),
			mcp.WithBoolean("allow_secrets", mcp.Description("Serve secret-shaped values verbatim when the symbol is a config / data-leaf value (.env, *.yaml, *.properties, ...). By default such values are withheld. Default: false.")),
			mcp.WithNumber("max_lines", mcp.Description("When the returned source exceeds this many lines, collapse runs of leaf statements inside function bodies into `… N lines elided …` markers while keeping the signature and the full control-flow skeleton — a structure-preserving alternative to a hard line cut. Omit or 0 to disable.")),
		),
		s.handleGetSymbolSource,
	)

	s.addTool(
		mcp.NewTool("batch_symbols",
			mcp.WithDescription("Returns signatures, source, and capped 1-hop callers/callees for multiple symbols; truncation flags mark cuts. Use get_callers/get_call_chain for the full neighbourhood. Replaces repeated get_symbol or get_symbol_source calls."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of symbol IDs")),
			mcp.WithBoolean("include_source", mcp.Description("Include source code for each symbol (default: false)")),
			mcp.WithNumber("context_lines", mcp.Description("Extra lines above/below source (default: 3, only if include_source)")),
			mcp.WithString("if_none_match", mcp.Description("ETag from a previous response — returns not_modified if content unchanged")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleBatchSymbols,
	)

	s.addTool(
		mcp.NewTool("get_test_targets",
			mcp.WithDescription("Given changed symbol IDs, traces the call graph to find test files and test functions that exercise those symbols. Use after editing to know exactly which tests to run — no guessing, no running the entire suite."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of changed symbol IDs")),
			mcp.WithNumber("depth", mcp.Description("Caller traversal depth (default: 3)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-test-file text output (file then its test functions), then the run commands, then the uncovered symbol IDs")),
		),
		s.handleGetTestTargets,
	)

	s.addTool(
		mcp.NewTool("suggest_pattern",
			mcp.WithDescription("Given an existing symbol as an example, extracts the structural pattern for creating similar code. Returns the example source, sibling symbols with the same pattern, registration/wiring code, test patterns, and files to edit. Source blocks longer than `max_source_lines` are elided to keep the response compact; pass a larger value when you need the full body. Use when adding a new function/handler/extractor that follows an existing convention."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol ID to use as the pattern example")),
			mcp.WithNumber("max_source_lines", mcp.Description("Per-source elision threshold (default: 40). Each example / registration / test source block longer than this collapses to its head + a `... N lines elided ...` marker. Pass 0 to disable elision.")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for the project default.")),
		),
		s.handleSuggestPattern,
	)

	s.addTool(
		mcp.NewTool("get_edit_plan",
			mcp.WithDescription("Given symbols you plan to change, returns a dependency-ordered list of files and symbols to edit — definitions first, then implementations, then callers, then tests. Eliminates manual dependency reasoning. Use before any multi-file refactor."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of symbol IDs to change")),
			mcp.WithNumber("depth", mcp.Description("Dependent traversal depth (default: 3)")),
		),
		s.handleGetEditPlan,
	)

	s.addTool(
		mcp.NewTool("edit_symbol",
			mcp.WithDescription("Edit a symbol's source code directly by ID — no Read needed. Gortex resolves the file and line range, finds the old_source fragment, replaces it with new_source, and writes the file. Matching is line-ending tolerant: an LF-authored old_source matches a CRLF file (and vice versa), and new_source is written with the file's own line endings — the response carries eol_normalized:true when that fallback fired. Eliminates the Read→Edit roundtrip for ~80% of edits. Pass base_sha to guard against stale writes: if the on-disk file no longer matches the SHA you observed at read time, the call fails with `base_sha mismatch — re-read and resubmit` and the file is untouched. On success the response carries new_sha so you can pipeline the next edit without re-reading."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol ID (e.g. server.go::NewServer)")),
			mcp.WithString("old_source", mcp.Required(), mcp.Description("Exact source fragment to replace (must be unique within the symbol). CRLF/LF line-ending differences against the file are tolerated.")),
			mcp.WithString("new_source", mcp.Required(), mcp.Description("Replacement source fragment")),
			mcp.WithString("base_sha", mcp.Description("Optional git blob SHA-1 the caller observed at read time. When set, the call refuses to write if the on-disk file's current SHA differs (drift guard against silent clobbers).")),
			mcp.WithBoolean("dry_run", mcp.Description("Validate the edit and return a unified-diff preview without writing (status: would_apply). Use to review the change before committing it.")),
			mcp.WithBoolean("physical_evidence", mcp.Description(mutationEvidenceParamDescription)),
			mcp.WithString("digest", mcp.Description(evidenceDigestParamDescription)),
			mcp.WithString("mutation_id", mcp.Description(mutationIDParamDescription)),
		),
		s.handleEditSymbol,
	)

	s.addTool(
		mcp.NewTool("read_file",
			mcp.WithDescription("Reads a file by path and returns its content. Pass offset (1-based start line) and/or limit (line count) to read a specific line WINDOW of a large file instead of the whole thing. Use sparingly — prefer get_symbol_source / get_editing_context for code. Useful when you need a non-indexed file (config, fixture, raw markdown) or when you genuinely need the full body. With compress_bodies=true, every function/method body is replaced by a `{ /* N lines elided */ }` stub — signatures + structure preserved, ~30-40% of original tokens. Composable with format:\"gcx\"."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path, or repo-prefixed / repo-root-relative path")),
			mcp.WithNumber("offset", mcp.Description("1-based line number to start reading from. Pair with limit to read a window of a large file. Omit or 0 to start at the first line.")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of lines to return starting at offset. Omit or 0 to read to end of file. Set this (with or without offset) to read a bounded window instead of the whole file.")),
			mcp.WithBoolean("compress_bodies", mcp.Description("Replace function/method bodies with elided stubs (default: false)")),
			mcp.WithBoolean("allow_secrets", mcp.Description("Serve secret-shaped values in config / data-leaf files (.env, *.yaml, *.toml, *.properties, ...) verbatim. By default such values are withheld and only their keys are shown. Default: false.")),
			mcp.WithBoolean("physical_evidence", mcp.Description("Return disk evidence. Includes SHA-256, byte count, resolved path, provenance, and same-buffer status for the full regular file. The digest covers raw disk bytes before transforms or editor overlays. Default: false.")),
			mcp.WithString("digest", mcp.Description(evidenceDigestParamDescription)),
			mcp.WithString("keep", mcp.Description("Comma-separated symbol names, IDs, or node kinds whose bodies stay verbatim when compress_bodies is set — every other body in the file is still stubbed. Ignored unless compress_bodies is true.")),
			mcp.WithString("fidelity_globs", mcp.Description(fidelityGlobsParamDescription)),
			mcp.WithNumber("max_lines", mcp.Description("When the file exceeds this many lines, collapse runs of leaf statements inside function bodies into `… N lines elided …` markers while keeping declarations and the control-flow skeleton. Falls back to a plain head cut for non-code files. Omit or 0 to disable.")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation flag rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_chars", mcp.Description("Cap the returned content at this many encoded bytes, cut at a valid UTF-8 boundary; a truncation note rides on the response. Omit or 0 to disable.")),
			mcp.WithString("if_none_match", mcp.Description("ETag from a previous response — returns not_modified if content unchanged")),
		),
		s.handleReadFile,
	)

	s.addTool(
		mcp.NewTool("edit_file",
			mcp.WithDescription("Edit any file (markdown, config, spec, template, source) by exact string replacement — no Read needed. Accepts absolute paths or paths relative to the indexed repo root. Matching is line-ending tolerant: an LF-authored old_string matches a CRLF file (and vice versa), and new_string is written with the file's own line endings — the response carries eol_normalized:true when that fallback fired. Writes atomically (temp+rename) and re-indexes the file so the graph stays fresh. Pass dry_run=true to validate the replacement without writing. Pass base_sha to guard against stale writes: if the on-disk file no longer matches the SHA you observed at read time, the call fails with `base_sha mismatch — re-read and resubmit` and the file is untouched. On success the response carries new_sha so you can pipeline the next edit without re-reading. Complements edit_symbol for non-code files that have no symbol ID."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path, or repo-prefixed / repo-root-relative path")),
			mcp.WithString("old_string", mcp.Required(), mcp.Description("Exact text to replace (must be unique unless replace_all=true). CRLF/LF line-ending differences against the file are tolerated.")),
			mcp.WithString("new_string", mcp.Required(), mcp.Description("Replacement text")),
			mcp.WithBoolean("replace_all", mcp.Description("Replace every occurrence instead of requiring uniqueness (default: false)")),
			mcp.WithNumber("expected_occurrences", mcp.Description("Guard: refuse the edit unless old_string matches exactly this many locations. Pairs with replace_all to assert the cardinality of a sweep (e.g. expected_occurrences:7 replace_all:true). Omit or 0 to disable the check.")),
			mcp.WithBoolean("dry_run", mcp.Description("Validate the replacement and report what would change without writing (default: false)")),
			mcp.WithString("base_sha", mcp.Description("Optional git blob SHA-1 the caller observed at read time. When set, the call refuses to write if the on-disk file's current SHA differs (drift guard against silent clobbers).")),
			mcp.WithBoolean("allow_parse_errors", mcp.Description("Bypass the pre-write parse gate. By default an edit that would introduce new tree-sitter parse errors (leaving the file more syntactically broken than before) is refused before the atomic write; set true to write anyway.")),
			mcp.WithBoolean("physical_evidence", mcp.Description(mutationEvidenceParamDescription)),
			mcp.WithString("digest", mcp.Description(evidenceDigestParamDescription)),
			mcp.WithString("mutation_id", mcp.Description(mutationIDParamDescription)),
		),
		s.handleEditFile,
	)

	s.addTool(
		mcp.NewTool("write_file",
			mcp.WithDescription("Create a new file or overwrite an existing one with the given content — no Read needed. Accepts absolute paths or paths relative to the indexed repo root. Writes atomically (temp+rename) and re-indexes the file so the graph stays fresh. Pass dry_run=true to report what would happen without writing. Pass base_sha when overwriting to guard against stale writes: if the on-disk file no longer matches the SHA you observed at read time (or has been deleted), the call fails with `base_sha mismatch — re-read and resubmit` and the file is untouched. On success the response carries new_sha so you can pipeline the next edit without re-reading. Use for new docs, configs, specs, scaffolded files; prefer edit_symbol or edit_file when a symbol/string target exists."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path, or repo-prefixed / repo-root-relative path")),
			mcp.WithString("content", mcp.Required(), mcp.Description("Full file content")),
			mcp.WithBoolean("dry_run", mcp.Description("Report would_create / would_overwrite without writing (default: false)")),
			mcp.WithString("base_sha", mcp.Description("Optional git blob SHA-1 the caller observed at read time. When set, write_file refuses to overwrite a divergent on-disk file (or write to a path the caller expected to exist but no longer does). Drift guard against silent clobbers on existing files; leave empty when creating a new file.")),
			mcp.WithBoolean("allow_parse_errors", mcp.Description("Bypass the pre-write parse gate. By default a write that would introduce new tree-sitter parse errors (relative to the prior content, or any error in a brand-new file) is refused before the atomic write; set true to write anyway.")),
			mcp.WithBoolean("physical_evidence", mcp.Description(mutationEvidenceParamDescription)),
			mcp.WithString("digest", mcp.Description(evidenceDigestParamDescription)),
			mcp.WithString("mutation_id", mcp.Description(mutationIDParamDescription)),
		),
		s.handleWriteFile,
	)

	s.addTool(
		mcp.NewTool("mutation_status",
			mcp.WithDescription("Reports what a file mutation actually did, after the fact. Use it when an edit_file / write_file / edit_symbol call was abandoned at its deadline (\"the work may still complete in the background\"): instead of retrying blindly, ask here. Returns disk_status (committed / not_applied / failed / in_flight) separately from graph_status (fresh / pending / stale / failed), because the bytes reaching disk and the graph catching up are different questions. disk_status=committed means the edit is already applied — re-applying it by any route would duplicate it. Select a record by receipt, by mutation_id, or by path; with no argument it lists the most recent mutations. Receipts are retained for 30 minutes."),
			mcp.WithString("receipt", mcp.Description("Mutation receipt id from an edit response (`mutation_receipt`) or from the `mutation_commit=` block on an abandoned-call error.")),
			mcp.WithString("mutation_id", mcp.Description("Caller-chosen idempotency key passed to the original edit.")),
			mcp.WithString("path", mcp.Description("File path — returns the most recent mutation recorded for it. Use when the response was lost entirely and no receipt id was ever seen.")),
		),
		s.handleMutationStatus,
	)

	s.addTool(
		mcp.NewTool("rename_symbol",
			mcp.WithDescription("Applies a coordinated multi-file rename for a symbol. Rewrites the definition, every graph usage (calls / references / instantiates), receiver lines when renaming a type, and test-function names that embed the old identifier. Returns status (applied / would_apply / no_edits), the {file, line, old_text, new_text, confidence, reason} edit list, and per-file {bytes_written, new_sha, reindexed}. An existing but unindexed target returns a structured symbol_not_indexed error only when the configured extractor anchors the requested declaration; its guarded edit/file fallback is an exact declaration-line edit and does not claim reference completeness. The refusal itself writes nothing. Every affected line is re-verified against disk and parse-gated before anything is written, so the rename either lands completely or is refused. Pass dry_run=true to preview the identical edit list without writing."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol ID to rename (e.g. auth/token.go::validateToken)")),
			mcp.WithString("new_name", mcp.Required(), mcp.Description("New name for the symbol")),
			mcp.WithBoolean("dry_run", mcp.Description("Preview only: compute and verify every edit but write nothing. Returns status \"would_apply\" with the same edits and per-file new_sha the real call would produce. Default false — the call writes.")),
			mcp.WithBoolean("allow_parse_errors", mcp.Description("Bypass the pre-write parse gate. By default a rename that would leave any affected file with new tree-sitter parse errors is refused before anything is written; set true to rename anyway.")),
		),
		s.handleRenameSymbol,
	)

	s.addTool(
		mcp.NewTool("smart_context",
			mcp.WithDescription("Assembles the minimal context needed for a task in one call. Searches for relevant symbols, gets their source and relationships, finds patterns to follow, and builds an edit plan. Replaces an entire exploration phase of 5-10 tool calls."),
			mcp.WithString("task", mcp.Required(), mcp.Description("Natural language description of what you want to do (e.g. 'add a new MCP tool called list_files')")),
			mcp.WithString("entry_point", mcp.Description("Optional symbol ID or file path to start from")),
			mcp.WithNumber("max_symbols", mcp.Description("Max symbols to include source for (default: 5)")),
			mcp.WithString("fidelity", mcp.Description("Set to \"graded\" to add a context_manifest: focus symbols at full source, their caller/callee adjacency ring as elided signature stubs, and the keyword-match remainder as outline-only entries — all packed under one token budget. Default \"flat\" keeps the legacy relevant_symbols shape.")),
			mcp.WithNumber("token_budget", mcp.Description("Token ceiling for the graded-fidelity manifest (default 8000). Entries are demoted full → compressed → outline as the budget fills. Ignored unless fidelity is \"graded\".")),
			mcp.WithBoolean("estimate", mcp.Description("Dry-run: skip assembling the payload and return only a token-cost projection (projected_tokens plus per-tier counts) for the task at the chosen fidelity, so the caller can budget before fetching the real context.")),
			mcp.WithString("if_none_match", mcp.Description("Pack-root etag from a previous smart_context response. When the recomputed pack root matches — nothing the pack covers has changed — the tool returns not_modified instead of resending the payload, turning a repeated call on an unchanged repo into a near-zero-token no-op.")),
			mcp.WithString("delta_from", mcp.Description("Pack root (etag) of a prior smart_context pack the caller still holds. Instead of re-emitting the full pack, the tool returns a `pack_delta` block — symbols/files/edges added, removed, or changed vs that prior pack — so an incrementally-shifted working set re-delivers only the diff. When the delta is materially smaller than the full pack the bulky symbol lists are dropped (merge the delta into your held pack); the response's `etag` still identifies the full pack for the next delta_from. If the prior pack is no longer cached the full pack is returned with `pack_delta.available=false`.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories.")),
			mcp.WithString("path", mcp.Description("Restrict the assembled context to one or more sub-paths (comma-separated) -- a monorepo-service slice. Anchored, slash-segment-boundary prefixes relative to the repo root. Unions with an inline path: clause in the task and a scope's saved paths.")),
			mcp.WithBoolean("include_call_paths", mcp.Description("Opt in to the in-pack anchored call-paths section (off by default). Overrides the project's smart_context.in_pack config for this call.")),
			mcp.WithBoolean("include_flows", mcp.Description("Opt in to the in-pack flow-spine and dynamic-boundary section (off by default). Overrides the project's smart_context.in_pack config for this call.")),
			mcp.WithBoolean("include_confidence", mcp.Description("Opt in to the in-pack retrieval-confidence verdict (off by default). Overrides the project's smart_context.in_pack config for this call.")),
		),
		s.handleSmartContext,
	)

	s.addTool(
		mcp.NewTool("plan_turn",
			mcp.WithDescription("Opening-move router. Returns a short ranked list of recommended tool calls (with pre-filled args) for a task — 'what should I call first?'. Use before smart_context when you want a cheaper routing decision; call smart_context directly when you're committed to exploring."),
			mcp.WithString("task", mcp.Required(), mcp.Description("Natural-language description of the task")),
			mcp.WithNumber("max_calls", mcp.Description("Max recommended calls (default: 4)")),
		),
		s.handlePlanTurn,
	)

	s.addTool(
		mcp.NewTool("get_untested_symbols",
			mcp.WithDescription("Returns functions and methods in non-test files that no test file reaches via the call graph — the inverse of get_test_targets. Ranked by fan_in descending so the most-used untested symbols surface first. Use to prioritize where to add test coverage."),
			mcp.WithNumber("limit", mcp.Description("Max entries returned (default: 50)")),
			mcp.WithString("file_prefix", mcp.Description("Restrict to symbols whose file path starts with this prefix (e.g. 'internal/auth/')")),
			mcp.WithNumber("min_fan_in", mcp.Description("Only flag symbols with at least this many callers; filters trivial helpers (default: 0)")),
		),
		s.handleGetUntestedSymbols,
	)

	s.addTool(
		mcp.NewTool("get_recent_changes",
			mcp.WithDescription("Returns files and symbols that changed since the last call (watch mode only). Use to re-orient after the user edits files outside of Claude Code's view, without re-reading anything. Rows are clamped to the session workspace and narrowed further by repo/project/scope."),
			mcp.WithString("since", mcp.Description("ISO 8601 timestamp (omit for all changes since index)")),
			mcp.WithString("repo", mcp.Description("Narrow to a single repository prefix (tracked name or path), clamped to the session workspace.")),
			mcp.WithString("project", mcp.Description("Narrow to the repositories in a project, clamped to the session workspace.")),
			mcp.WithString("workspace", mcp.Description("Restrict to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — its repositories narrow the feed, clamped to the session workspace.")),
		),
		s.handleGetRecentChanges,
	)
}

type editingContext struct {
	File     map[string]any   `json:"file"`
	Defines  []map[string]any `json:"defines"`
	Imports  []map[string]any `json:"imports"`
	CalledBy []map[string]any `json:"called_by"`
	Calls    []map[string]any `json:"calls"`
}

// resolveKeepPredicate turns the comma-separated `keep` parameter into
// an elide.Keep predicate. Each token is matched against the supplied
// indexed symbols by ID, by name, or as a node kind (function /
// method / type / …); every matched symbol contributes its line range
// — the precise path. Tokens are additionally offered as bare names
// so `keep` still works on a file with no indexed symbols. The second
// return value lists the distinct symbol names that resolved, for the
// response envelope. Returns (nil, nil) when keep is empty.
func resolveKeepPredicate(keep string, symbols []*graph.Node) (func(elide.Decl) bool, []string) {
	tokens := splitCSV(keep)
	if len(tokens) == 0 {
		return nil, nil
	}
	want := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		want[t] = struct{}{}
	}
	var ranges [][2]int
	var resolved []string
	seen := make(map[string]struct{})
	for _, n := range symbols {
		if n == nil || n.Kind == graph.KindFile {
			continue
		}
		_, byID := want[n.ID]
		_, byName := want[n.Name]
		_, byKind := want[string(n.Kind)]
		if !byID && !byName && !byKind {
			continue
		}
		if n.StartLine > 0 && n.EndLine >= n.StartLine {
			ranges = append(ranges, [2]int{n.StartLine, n.EndLine})
		}
		if n.Name != "" {
			if _, dup := seen[n.Name]; !dup {
				seen[n.Name] = struct{}{}
				resolved = append(resolved, n.Name)
			}
		}
	}
	pred := elide.KeepAny(elide.KeepLineRanges(ranges), elide.KeepNames(tokens))
	return pred, resolved
}

// editingContextSymbolNodes reconstructs the *graph.Node slice the
// elide.KeepAny predicate needs from the editing-context Defines
// rows. We carry the node IDs only on the wire, but a `keep` token
// can target a node by id, name, or kind — so we re-resolve every
// defines row to a node here. Used only when compress_bodies=true.
func (s *Server) editingContextSymbolNodes(ctx context.Context, filePath string, defines []map[string]any) []*graph.Node {
	if len(defines) == 0 {
		return nil
	}
	ids := make([]string, 0, len(defines))
	for _, d := range defines {
		if id, _ := d["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	nodes := s.readerFor(ctx).GetNodesByIDs(ids)
	out := make([]*graph.Node, 0, len(ids))
	for _, id := range ids {
		if n, ok := nodes[id]; ok && n != nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) handleGetEditingContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fp, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	// Admission before any work: a fidelity_globs value that breaks a size
	// bound refuses the request. Dropping the offending rule would rewrite
	// a first-match policy silently — an over-budget `omit` disappearing
	// lets a later `full` win, and the content the caller asked to hide
	// comes back in a response that looks like a normal success. Parsed
	// here rather than at the point of use so a malformed request cannot
	// be served by a path that happens not to reach the compressor.
	fidelityRules, fidelityErr := parseFidelityGlobs(req.GetString("fidelity_globs", ""))
	if fidelityErr != nil {
		return mcp.NewToolResultError("get_editing_context: " + fidelityErr.Error()), nil
	}
	// Normalise to the graph's stored path form so a repo-relative path
	// (internal/x.go) doesn't miss the repo-prefixed nodes in multi-repo
	// mode — the cause of spurious "no symbols found for file" misses.
	fp = s.graphRelPath(ctx, fp)

	// Auto re-index stale file before querying.
	s.ensureFreshForRequest(ctx, []string{fp})

	s.sessionFor(ctx).recordFile(fp)
	// Tool-call observer: credit the recent search for the symbols in
	// the file the agent is about to edit.
	s.creditFileConsumption(ctx, fp)
	out := editingContext{}
	var fileNodeForScope *graph.Node
	callerCap := 20
	calleeCap := 20

	// Fast path: when the backend implements FileEditingContext we
	// take all five projections in a small fixed number of
	// round-trips instead of the per-symbol GetCallers / GetCallChain
	// loop. The fallback retains the previous engine-based shape so
	// the in-memory backend is unaffected.
	if fc, ok := s.readerFor(ctx).(graph.FileEditingContext); ok {
		bundle := fc.FileEditingContext(fp, []graph.NodeKind{graph.KindFunction, graph.KindMethod})
		if bundle == nil || (bundle.FileNode == nil && len(bundle.Defines) == 0) {
			return mcp.NewToolResultError("no symbols found for file: " + fp), nil
		}
		fileNodeForScope = bundle.FileNode
		if fileNodeForScope == nil && len(bundle.Defines) > 0 {
			fileNodeForScope = bundle.Defines[0]
		}
		if !s.nodeInSessionScope(ctx, fileNodeForScope) {
			return mcp.NewToolResultError("no symbols found for file: " + fp), nil
		}
		for _, n := range bundle.Defines {
			s.frecency.Record(n.ID)
		}
		if bundle.FileNode != nil {
			out.File = map[string]any{"id": bundle.FileNode.ID, "language": bundle.FileNode.Language}
		}
		for _, n := range bundle.Defines {
			entry := map[string]any{
				"id":         n.ID,
				"kind":       n.Kind,
				"name":       n.Name,
				"start_line": n.StartLine,
			}
			if n.Meta != nil {
				if sig, ok := n.Meta["signature"]; ok {
					entry["signature"] = sig
				}
			}
			out.Defines = append(out.Defines, entry)
		}
		for _, e := range bundle.Imports {
			out.Imports = append(out.Imports, map[string]any{
				"id":       e.To,
				"external": strings.HasPrefix(e.To, "external::"),
			})
		}
		// Workspace-scope post-filter mirrors the legacy GetCallers /
		// GetCallChain WorkspaceID gate.
		sessWS, _, bound := s.sessionScope(ctx)
		var opts query.QueryOptions
		if bound {
			opts.WorkspaceID = sessWS
		}
		for _, n := range bundle.CalledBy {
			if bound && !opts.ScopeAllows(n) {
				continue
			}
			if len(out.CalledBy) >= callerCap {
				break
			}
			out.CalledBy = append(out.CalledBy, map[string]any{
				"id":         n.ID,
				"name":       n.Name,
				"file_path":  n.FilePath,
				"start_line": n.StartLine,
			})
		}
		for _, n := range bundle.Calls {
			if bound && !opts.ScopeAllows(n) {
				continue
			}
			if len(out.Calls) >= calleeCap {
				break
			}
			out.Calls = append(out.Calls, map[string]any{
				"id":         n.ID,
				"name":       n.Name,
				"file_path":  n.FilePath,
				"start_line": n.StartLine,
			})
		}
	} else {
		sg := s.engineFor(ctx).GetFileSymbols(fp)
		if len(sg.Nodes) == 0 {
			return mcp.NewToolResultError("no symbols found for file: " + fp), nil
		}
		if !s.nodeInSessionScope(ctx, sg.Nodes[0]) {
			return mcp.NewToolResultError("no symbols found for file: " + fp), nil
		}
		sessWS, _, _ := s.sessionScope(ctx)
		for _, n := range sg.Nodes {
			if n.Kind == graph.KindFile {
				continue
			}
			s.frecency.Record(n.ID)
		}
		for _, n := range sg.Nodes {
			if n.Kind == graph.KindFile {
				out.File = map[string]any{"id": n.ID, "language": n.Language}
				// The same anchor the fast path keeps: without it the
				// whole-file compressed view below has no node to resolve a
				// path from, so every backend without a FileEditingContext
				// bundle answered compress_bodies with nothing at all.
				fileNodeForScope = n
				break
			}
		}
		if fileNodeForScope == nil {
			fileNodeForScope = sg.Nodes[0]
		}
		for _, n := range sg.Nodes {
			if n.Kind == graph.KindFile {
				continue
			}
			entry := map[string]any{
				"id":         n.ID,
				"kind":       n.Kind,
				"name":       n.Name,
				"start_line": n.StartLine,
			}
			if sig, ok := n.Meta["signature"]; ok {
				entry["signature"] = sig
			}
			out.Defines = append(out.Defines, entry)
		}
		for _, e := range sg.Edges {
			if e.Kind == graph.EdgeImports {
				out.Imports = append(out.Imports, map[string]any{
					"id":       e.To,
					"external": strings.HasPrefix(e.To, "external::"),
				})
			}
		}
		callerSeen := make(map[string]bool)
		for _, n := range sg.Nodes {
			if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
				callers := s.engineFor(ctx).GetCallers(n.ID, query.QueryOptions{Depth: 1, Limit: callerCap, Detail: "brief", WorkspaceID: sessWS})
				for _, cn := range callers.Nodes {
					if cn.FilePath != fp && !callerSeen[cn.ID] {
						callerSeen[cn.ID] = true
						out.CalledBy = append(out.CalledBy, map[string]any{
							"id":         cn.ID,
							"name":       cn.Name,
							"file_path":  cn.FilePath,
							"start_line": cn.StartLine,
						})
					}
				}
			}
		}
		callSeen := make(map[string]bool)
		for _, n := range sg.Nodes {
			if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
				chain := s.engineFor(ctx).GetCallChain(n.ID, query.QueryOptions{Depth: 1, Limit: calleeCap, Detail: "brief", WorkspaceID: sessWS})
				for _, cn := range chain.Nodes {
					if cn.FilePath != fp && !callSeen[cn.ID] {
						callSeen[cn.ID] = true
						out.Calls = append(out.Calls, map[string]any{
							"id":         cn.ID,
							"name":       cn.Name,
							"file_path":  cn.FilePath,
							"start_line": cn.StartLine,
						})
					}
				}
			}
		}
	}

	// Optional: full-file compressed view. When compress_bodies=true,
	// emit a `source_compressed` field carrying the whole file
	// through the tree-sitter elider so the agent gets signatures +
	// structure in one call without paying the cost of raw bodies.
	// Failures (no grammar, parse error) are swallowed — the caller
	// still gets the structural sections that fired above. A view that
	// cannot produce the file's bytes is not: it is either the typed
	// source_object_missing answer or an omission saying so.
	var sourceCompressed string
	var keptSymbols []string
	var viewURI string
	var viewReadFailure string
	if req.GetBool("compress_bodies", false) {
		var language string
		if out.File != nil {
			if lang, ok := out.File["language"].(string); ok {
				language = lang
			}
		}
		if language != "" && elide.IsSupported(language) {
			// Use the file node (cached above from the editing-context
			// bundle) to find the on-disk path. Falls back to the first
			// defines node if no file node materialised (defensive — the
			// FileEditingContext implementation always returns one when
			// the file is indexed).
			var fileBytes []byte
			anchor := fileNodeForScope
			if files := refViewFilesFor(ctx); files != nil {
				// A view of committed state has no working copy, so the whole
				// file comes out of the tree it pins.
				content, rel, viewErr := readViewFile(ctx, files, fp)
				switch {
				case viewErr == nil:
					fileBytes = content
					viewURI = files.uri(rel)
				case errors.Is(viewErr, graphview.ErrSourceObjectMissing):
					// The blob the view is pinned to is gone from the object
					// store. Every other read surface answers that verbatim,
					// and the read has already withdrawn the view's source
					// capability, so degrading to the structural sections here
					// would hide a view that can no longer serve bytes at all.
					return mcp.NewToolResultError(viewErr.Error()), nil
				default:
					viewReadFailure = viewErr.Error()
				}
			} else if anchor != nil {
				if absPath, rerr := s.resolveNodePath(ctx, anchor); rerr == nil {
					if content, ok := s.overlayContentFor(ctx, absPath); ok {
						fileBytes = []byte(content)
					} else if b, ferr := os.ReadFile(absPath); ferr == nil {
						fileBytes = b
					}
				}
			}
			if len(fileBytes) > 0 {
				// `keep` pins a chosen subset of symbols to their
				// verbatim bodies while the rest of the file is still
				// stubbed — keep the functions being edited at full
				// source and compress everything else.
				keepNodes := s.editingContextSymbolNodes(ctx, fp, out.Defines)
				keepPred, resolved := resolveKeepPredicate(req.GetString("keep", ""), keepNodes)
				keptSymbols = resolved
				decide := fidelityDecideForPath(fidelityRules, fp)
				if compressed, cerr := elide.CompressWith(fileBytes, language, elide.Options{Keep: keepPred, Decide: decide}); cerr == nil {
					sourceCompressed = string(compressed)
				}
			}
		}
	}

	// ETag conditional fetch.
	etag := computeETag(out)
	if sourceCompressed != "" {
		// Fold the compressed view into the etag so a flag flip
		// invalidates the cached entry on the caller side.
		etag = computeETag([2]any{out, sourceCompressed})
	}
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}

	// Server-side accounting only — an editing-context bundle stands in
	// for reading the whole file before editing it.
	ctxLang := ""
	if fileNodeForScope != nil {
		ctxLang = fileNodeForScope.Language
	}
	if payload, merr := json.Marshal(out); merr == nil {
		s.recordFileBaselineSavings(ctx, "get_editing_context", fp, ctxLang, string(payload)+sourceCompressed)
	}

	// Omission notes: flag vendored/generated provenance and body
	// compression so the model doesn't over-trust the payload.
	omissions := pathOmissions(fp)
	if sourceCompressed != "" {
		omissions = append(omissions, omission("compressed",
			"source_compressed replaces function and method bodies with elided stubs; signatures kept"))
	}
	if viewReadFailure != "" {
		// Without this the caller cannot tell a file with nothing to compress
		// from one whose bytes the view could not produce.
		omissions = append(omissions, omission("source_unavailable",
			"source_compressed is absent: the view could not produce this file's bytes — "+viewReadFailure))
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeEditingContext(out.File, out.Defines, out.Imports, out.CalledBy, out.Calls, etag, omissionKindsCSV(omissions)))
	}

	// Add etag to response by marshaling to map.
	result := map[string]any{
		"file":      out.File,
		"defines":   out.Defines,
		"imports":   out.Imports,
		"called_by": out.CalledBy,
		"calls":     out.Calls,
		"etag":      etag,
	}
	if sourceCompressed != "" {
		result["source_compressed"] = sourceCompressed
		result["bodies_elided"] = true
		if len(keptSymbols) > 0 {
			result["kept_symbols"] = keptSymbols
		}
	}
	if viewURI != "" {
		result["served_from"] = "view"
		result["view_uri"] = viewURI
	}
	if len(omissions) > 0 {
		result["omissions"] = omissions
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// findImportLanguageByExt maps the target file's extension to the
// language label graph nodes carry. Covers the languages that have a
// real import system (the rest fall through to an empty string,
// which disables language filtering rather than blocking the lookup).
var findImportLanguageByExt = map[string]string{
	".go":   "go",
	".ts":   "typescript",
	".tsx":  "typescript",
	".mts":  "typescript",
	".cts":  "typescript",
	".js":   "javascript",
	".jsx":  "javascript",
	".mjs":  "javascript",
	".cjs":  "javascript",
	".py":   "python",
	".rb":   "ruby",
	".rs":   "rust",
	".java": "java",
	".kt":   "kotlin",
	".kts":  "kotlin",
	".php":  "php",
	".cs":   "csharp",
}

func languageForExtension(path string) string {
	return findImportLanguageByExt[strings.ToLower(filepath.Ext(path))]
}

func (s *Server) handleFindImportPath(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbolName, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name is required"), nil
	}
	targetFile, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	// Accept qualified Go names ("graph.Node", "pkg.Sym") by splitting
	// on the final dot. The first segment carries the package
	// qualifier the lookup should bias toward; the second is the
	// actual identifier the graph stores. Falls through unchanged for
	// bare names.
	packageHint, bareName := "", symbolName
	if dot := strings.LastIndex(symbolName, "."); dot > 0 && dot < len(symbolName)-1 {
		packageHint = symbolName[:dot]
		bareName = symbolName[dot+1:]
	}

	candidates := s.scopedNodeSlice(ctx, s.engineFor(ctx).FindSymbols(bareName))
	if len(candidates) == 0 {
		return mcp.NewToolResultError("symbol not found: " + symbolName), nil
	}

	// Filter by the target file's language so a Go import lookup
	// never resolves to a TypeScript declaration that just happens to
	// share the identifier. Falls back to the unfiltered candidate
	// set when we can't infer a language for the target — better an
	// approximate hit than no hit at all.
	targetLang := languageForExtension(targetFile)
	if targetLang != "" {
		filtered := candidates[:0:len(candidates)]
		for _, c := range candidates {
			if c.Language == "" || c.Language == targetLang {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) > 0 {
			candidates = filtered
		}
	}

	// Find the best match (prefer different directory from target;
	// when a package qualifier was supplied, bias toward candidates
	// whose home directory's leaf matches it).
	targetDir := filepath.Dir(targetFile)
	var best *graph.Node
	for _, c := range candidates {
		if c.Kind == graph.KindFile || c.Kind == graph.KindImport {
			continue
		}
		if best == nil {
			best = c
		}
		candDir := filepath.Dir(c.FilePath)
		// Package-qualifier bias: when the caller wrote `graph.Node`,
		// prefer a candidate whose directory leaf is `graph`. Matches
		// the Go convention that the import alias defaults to the
		// last path segment.
		if packageHint != "" && filepath.Base(candDir) == packageHint && candDir != targetDir {
			best = c
			break
		}
		// Prefer symbols NOT in the same directory (actual imports).
		if candDir != targetDir {
			best = c
		}
	}

	if best == nil {
		return mcp.NewToolResultError("no importable symbol found: " + symbolName), nil
	}

	// Check if already imported.
	alreadyImported := false
	fileSymbols := s.engineFor(ctx).GetFileSymbols(targetFile)
	if len(fileSymbols.Nodes) > 0 && !s.nodeInSessionScope(ctx, fileSymbols.Nodes[0]) {
		fileSymbols = nil
	}
	if fileSymbols != nil {
		for _, e := range fileSymbols.Edges {
			if e.Kind == graph.EdgeImports && strings.Contains(e.To, filepath.Dir(best.FilePath)) {
				alreadyImported = true
				break
			}
		}
	}

	// Defensive: if the matched node carries an absolute file path
	// (older snapshots produced before applyRepoPrefix was hardened
	// could land abs paths in the graph), fold it back to a
	// repo-relative form so the response stays consistent with every
	// other graph tool. Without this, agents see a leaked
	// `/Users/...` import_path that confuses code generation.
	importDir := filepath.Dir(best.FilePath)
	if filepath.IsAbs(importDir) {
		importDir = s.repoRelative(importDir)
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"symbol_id":        best.ID,
		"import_path":      importDir,
		"already_imported": alreadyImported,
	})
}

// handleGetRecentChanges returns the watcher's change feed, narrowed to the
// repositories the caller may see.
//
// Under the daemon the watcher is a MultiWatcher whose History merges the
// per-repo feeds of every tracked repo — across workspaces — and each row's
// `file` is an absolute path. `analyze kind=recent_changes` is a facade alias
// that forwards straight to this tool, so nothing upstream narrows it: the
// clamp belongs here. Rows are attributed by the repo prefix the emitting
// watcher stamps on the event.
func (s *Server) handleGetRecentChanges(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	watcher := s.currentWatcher()
	if watcher == nil {
		return mcp.NewToolResultError("watch mode is not active"), nil
	}
	resolved, errResult := s.resolveScope(ctx, req, IntentAnalyze)
	if errResult != nil {
		return errResult, nil
	}
	ctx = withRepoAllow(ctx, resolved.RepoAllow)

	// Exactly one of the two reads runs: each copies every tracked repo's
	// ring buffer under its own lock, and the merged form sorts the union.
	var events []indexer.GraphChangeEvent
	if sinceStr := req.GetString("since", ""); sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return mcp.NewToolResultError("invalid timestamp: " + sinceStr), nil
		}
		events = watcher.HistorySince(t)
	} else {
		events = watcher.History()
	}

	var changes []map[string]any
	for _, ev := range events {
		if !s.repoPrefixVisible(ctx, ev.RepoPrefix) {
			continue
		}
		row := map[string]any{
			"file":          ev.FilePath,
			"kind":          ev.Kind,
			"nodes_added":   ev.NodesAdded,
			"nodes_removed": ev.NodesRemoved,
			"timestamp":     ev.Timestamp.Format(time.RFC3339),
		}
		// Only present in multi-repo mode; a single-repo watcher leaves the
		// prefix empty and the row shape is unchanged.
		if ev.RepoPrefix != "" {
			row["repo"] = ev.RepoPrefix
		}
		changes = append(changes, row)
	}

	return s.respondScopedJSONOrTOON(ctx, req, map[string]any{
		"changes":             changes,
		"graph_current_as_of": time.Now().Format(time.RFC3339),
	}, resolved)
}

// suggestSymbolIDs builds a short "did you mean" hint for a symbol id that
// could not be resolved. It offers in-scope ids that share the requested
// name anywhere in the graph (the "right name, wrong path" case) and, when
// the id carries a file path, the symbols defined in that file ranked by
// name similarity (the "right path, stale name" case). Returns "" when
// nothing plausible is nearby, so callers can append it unconditionally.
func (s *Server) suggestSymbolIDs(ctx context.Context, id string) string {
	eng := s.engineFor(ctx)
	if eng == nil || s.graph == nil {
		return ""
	}
	pathPart, namePart := "", id
	if parts := strings.SplitN(id, "::", 2); len(parts) == 2 {
		pathPart, namePart = parts[0], parts[1]
	}
	// Drop a receiver qualifier so Foo.Bar can match a stored "Bar".
	base := namePart
	if i := strings.LastIndex(base, "."); i >= 0 && i < len(base)-1 {
		base = base[i+1:]
	}
	seen := map[string]bool{id: true}
	var out []string
	add := func(n *graph.Node) bool {
		if n == nil || seen[n.ID] || !s.nodeInSessionScope(ctx, n) {
			return false
		}
		seen[n.ID] = true
		out = append(out, n.ID)
		return len(out) >= 5
	}
	// 1. Same-named symbols anywhere — the common wrong-path case.
	if base != "" {
		for _, n := range eng.FindSymbols(base) {
			if add(n) {
				break
			}
		}
	}
	// 2. Nearest-named symbols in the same file — the stale-name case.
	if pathPart != "" && len(out) < 5 {
		if sg := eng.GetFileSymbols(s.graphRelPath(ctx, pathPart)); sg != nil {
			type scored struct {
				n    *graph.Node
				dist int
			}
			ranked := make([]scored, 0, len(sg.Nodes))
			for _, n := range sg.Nodes {
				if n == nil || n.Kind == graph.KindFile {
					continue
				}
				ranked = append(ranked, scored{n, levenshtein(strings.ToLower(n.Name), strings.ToLower(base))})
			}
			sort.Slice(ranked, func(i, j int) bool { return ranked[i].dist < ranked[j].dist })
			for _, r := range ranked {
				if add(r.n) {
					break
				}
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	return " — did you mean: " + strings.Join(out, ", ")
}

func (s *Server) handleGetSymbolSource(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}

	// Auto re-index stale file before querying.
	if parts := strings.SplitN(id, "::", 2); len(parts) == 2 {
		s.ensureFreshForRequest(ctx, []string{parts[0]})
	}

	node := s.engineFor(ctx).GetSymbol(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id + s.suggestSymbolIDs(ctx, id)), nil
	}
	// A by-id fetch must not cross the session's workspace boundary —
	// reported the same as a genuine miss so the boundary isn't
	// probeable.
	if !s.nodeInSessionScope(ctx, node) {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}
	sess := s.sessionFor(ctx)
	sess.recordSymbol(id)
	sess.recordFile(node.FilePath)
	// Credit this consume back to the most recent matching search_symbols,
	// if any; no-op when the combo tracker isn't initialised or no search
	// window is active.
	if q := sess.attributedQuery(id); q != "" {
		s.combo.Record(q, id)
	}
	// Unconditionally record the access for frecency — this is the "symbols
	// the agent actually reads" signal, useful even when no prior search
	// sourced it (agents also fetch symbols by ID from recent history).
	s.frecency.Record(id)

	if node.StartLine == 0 || node.EndLine == 0 {
		return mcp.NewToolResultError("symbol has no line range: " + id), nil
	}

	contextLines := req.GetInt("context_lines", 3)

	// Resolve the file path against whichever indexer owns the repo.
	// Multi-repo mode is the source of truth — node.RepoPrefix names
	// the owning repo and MultiIndexer holds its RootPath. Single-repo
	// mode falls back to the lone indexer's RootPath. Bare-relative
	// paths must never reach readLines: os.Open would resolve them
	// against the daemon process cwd, which is unrelated to any repo
	// and silently produces wrong results.
	var (
		source         string
		startLine      int
		totalFileChars int
		viewURI        string
		absPath        string
	)
	if files := refViewFilesFor(ctx); files != nil {
		// The symbol's lines belong to the committed tree the view pins, not
		// to whatever the canonical checkout has on disk at that path.
		content, rel, viewErr := readViewFile(ctx, files, node.FilePath)
		if viewErr != nil {
			return mcp.NewToolResultError(viewErr.Error()), nil
		}
		viewURI = files.uri(rel)
		source, startLine, totalFileChars, _ = extractLinesFromContent(
			string(content), node.StartLine, node.EndLine, contextLines)
	} else {
		var resolveErr error
		absPath, resolveErr = s.resolveNodePath(ctx, node)
		if resolveErr != nil {
			return mcp.NewToolResultError(resolveErr.Error()), nil
		}
		if guardErr := s.guardSymlinkWithinRepo(ctx, absPath); guardErr != nil {
			return mcp.NewToolResultError(guardErr.Error()), nil
		}
		var err error
		source, startLine, totalFileChars, err = s.readLinesForCtx(ctx, absPath, node.StartLine, node.EndLine, contextLines)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not read source: %v", err)), nil
		}
	}

	compressBodies := req.GetBool("compress_bodies", false)
	bodiesElided := false
	if compressBodies && elide.IsSupported(node.Language) {
		if out, eerr := elide.CompressString(source, node.Language); eerr == nil && out != source {
			source = out
			bodiesElided = true
		}
	}

	// Salience truncation: when the symbol is still larger than
	// max_lines, keep its control-flow skeleton and collapse runs of
	// leaf statements rather than cutting the tail off blind.
	salienceTruncated := false
	if maxLines := req.GetInt("max_lines", 0); maxLines > 0 {
		if out, truncated, _ := elide.SalienceTruncate([]byte(source), node.Language, maxLines); truncated {
			source = string(out)
			salienceTruncated = true
		}
	}

	// Withhold secret-shaped values when serving a config / data-leaf symbol
	// (e.g. a populated .env / yaml key) unless the caller opts out.
	secretsRedacted := false
	if red, did := s.maybeRedactConfigLeaf(node.Language, node.FilePath, req.GetBool("allow_secrets", false), source); did {
		source = red
		secretsRedacted = true
	}

	// Server-side accounting only — the savings value isn't returned to
	// the caller (agents don't act on it and it burns tokens in every
	// response). Aggregated stats remain available via the `savings` tool.
	returned := tokens.CachedCountInt64(source)
	fullFile := int64(tokens.EstimateFromSample(totalFileChars, source))
	symStats := s.tokenStatsFor(ctx)
	symStats.creditFile(absPath)
	symStats.record(s.savingsAttributionNode(node), "get_symbol_source", returned, fullFile)

	result := map[string]any{
		"id":         node.ID,
		"kind":       node.Kind,
		"name":       node.Name,
		"file_path":  node.FilePath,
		"start_line": node.StartLine,
		"end_line":   node.EndLine,
		"source":     source,
		"from_line":  startLine,
	}
	if sig, ok := node.Meta["signature"]; ok {
		result["signature"] = sig
	}
	if viewURI != "" {
		result["served_from"] = "view"
		result["view_uri"] = viewURI
	}
	if bodiesElided {
		result["bodies_elided"] = true
	}
	if salienceTruncated {
		result["salience_truncated"] = true
	}
	if secretsRedacted {
		result["secrets_redacted"] = true
	}

	// Omission notes: flag what the payload leaves out or reshapes so
	// the model doesn't reason about absent code.
	omissions := pathOmissions(node.FilePath)
	if bodiesElided {
		omissions = append(omissions, omission("compressed",
			"function and method bodies replaced with elided stubs; signatures and structure kept"))
	}
	if salienceTruncated {
		omissions = append(omissions, omission("truncated",
			"oversized source reduced toward its control-flow skeleton; runs of leaf statements collapsed"))
	}
	if secretsRedacted {
		omissions = append(omissions, omission("secrets_withheld",
			"secret-shaped values in this config symbol were withheld; pass allow_secrets:true to read them"))
	}
	if len(omissions) > 0 {
		result["omissions"] = omissions
	}

	// Capture the validated typed owner before any output encoding branch. The
	// dispatcher consumes it only when this reserved call itself succeeds.
	captureLocalizationReadSource(ctx, node)

	// ETag conditional fetch.
	etag := computeETag(result)
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}
	result["etag"] = etag

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeGetSymbolSource(node, source, startLine, etag, omissionKindsCSV(omissions)))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}

	return s.respondJSONOrTOON(ctx, req, result)
}

// readLines reads lines from a file, with optional context lines above/below.
// Returns the source text, the first line number, the total file size in characters
// (for token savings estimation), and any error.
//
// Disk path only — used by helpers that have no MCP request context.
// Production handlers should call (*Server).readLinesForCtx so the
// editor-buffer overlay is honoured when active.
func readLines(path string, startLine, endLine, contextLines int) (string, int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()

	from := startLine - contextLines
	if from < 1 {
		from = 1
	}
	to := endLine + contextLines

	var lines []string
	var totalChars int
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		text := scanner.Text()
		totalChars += len(text) + 1 // +1 for newline stripped by Scanner
		if lineNum >= from && lineNum <= to {
			lines = append(lines, text)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", 0, 0, err
	}

	return strings.Join(lines, "\n"), from, totalChars, nil
}

// readLinesForCtx is the overlay-aware counterpart to readLines.
// When ctx carries an editor-overlay view AND the path is covered by
// the overlay, the buffer content is used instead of reading from
// disk — so get_symbol_source / get_editing_context / smart_context
// return the editor's unsaved view of the file. Falls back to
// readLines transparently when no overlay applies.
func (s *Server) readLinesForCtx(ctx context.Context, absPath string, startLine, endLine, contextLines int) (string, int, int, error) {
	content, ok := s.overlayContentFor(ctx, absPath)
	if !ok {
		return readLines(absPath, startLine, endLine, contextLines)
	}
	return extractLinesFromContent(content, startLine, endLine, contextLines)
}

// extractLinesFromContent applies the same line-slicing logic readLines
// uses to an in-memory buffer. Kept separate from readLines so the
// disk path stays a single os.Open / Scanner loop.
func extractLinesFromContent(content string, startLine, endLine, contextLines int) (string, int, int, error) {
	from := startLine - contextLines
	if from < 1 {
		from = 1
	}
	to := endLine + contextLines

	lines := strings.Split(content, "\n")
	totalChars := 0
	for _, l := range lines {
		totalChars += len(l) + 1
	}
	if totalChars > 0 {
		// strings.Split adds a phantom trailing entry when the
		// content ends with a newline; account for the over-count by
		// trimming one byte (matches readLines which counts the
		// trailing \n only when Scanner produced a line for it).
		totalChars--
	}

	var picked []string
	for i, l := range lines {
		lineNum := i + 1
		if lineNum >= from && lineNum <= to {
			picked = append(picked, l)
		}
	}
	return strings.Join(picked, "\n"), from, totalChars, nil
}

func parseBatchSymbolIDs(raw any) ([]string, bool) {
	var ids []string
	switch value := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "[") {
			// Facade arrays cross the legacy string schema as JSON so commas in
			// generic parameter lists remain part of one opaque symbol ID.
			if err := json.Unmarshal([]byte(trimmed), &ids); err != nil {
				return nil, false
			}
		} else {
			// Backward compatibility for the original CLI/MCP shorthand. Scalar
			// values are the only shape where commas mean multiple IDs.
			ids = strings.Split(value, ",")
		}
	case []string:
		ids = append(ids, value...)
	case []any:
		ids = make([]string, 0, len(value))
		for _, item := range value {
			id, ok := item.(string)
			if !ok {
				return nil, false
			}
			ids = append(ids, id)
		}
	default:
		return nil, false
	}
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
		if ids[i] == "" {
			return nil, false
		}
	}
	return ids, len(ids) > 0
}

func (s *Server) handleBatchSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ids, ok := parseBatchSymbolIDs(req.GetArguments()["ids"])
	if !ok {
		return mcp.NewToolResultError("ids is required"), nil
	}

	includeSource := false
	if v, ok := req.GetArguments()["include_source"].(bool); ok {
		includeSource = v
	}
	contextLines := req.GetInt("context_lines", 3)
	// Confine the caller/callee neighbourhoods below to the session
	// workspace.
	sessWS, _, _ := s.sessionScope(ctx)

	var results []map[string]any
	for _, id := range ids {
		node := s.engineFor(ctx).GetSymbol(id)
		// A node outside the session's workspace is reported as a
		// miss — identical to a genuinely absent ID so the boundary
		// stays opaque.
		if node == nil || !s.nodeInSessionScope(ctx, node) {
			results = append(results, map[string]any{
				"id":    id,
				"error": "symbol not found",
			})
			continue
		}

		entry := map[string]any{
			"id":         node.ID,
			"kind":       node.Kind,
			"name":       node.Name,
			"file_path":  node.FilePath,
			"start_line": node.StartLine,
			"end_line":   node.EndLine,
		}
		if sig, ok := node.Meta["signature"]; ok {
			entry["signature"] = sig
		}

		// Callers (depth 1).
		if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
			callers := s.engineFor(ctx).GetCallers(node.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief", WorkspaceID: sessWS})
			var callerIDs []string
			for _, cn := range callers.Nodes {
				if cn.ID != node.ID {
					callerIDs = append(callerIDs, cn.ID)
				}
			}
			if len(callerIDs) > 0 {
				entry["callers"] = callerIDs
			}
			// Stamp the cut. Do not emit a total: TotalNodes is the
			// capped subgraph (seed+9), not the neighbourhood.
			if callers.Truncated {
				entry["callers_truncated"] = true
			}

			// Callees (depth 1).
			callees := s.engineFor(ctx).GetCallChain(node.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief", WorkspaceID: sessWS})
			var calleeIDs []string
			for _, cn := range callees.Nodes {
				if cn.ID != node.ID {
					calleeIDs = append(calleeIDs, cn.ID)
				}
			}
			if len(calleeIDs) > 0 {
				entry["callees"] = calleeIDs
			}
			if callees.Truncated {
				entry["callees_truncated"] = true
			}
		}

		// Source code (optional).
		if includeSource && node.StartLine > 0 && node.EndLine > 0 {
			if absPath, err := s.resolveNodePath(ctx, node); err == nil {
				if source, fromLine, totalFileChars, err := s.readLinesForCtx(ctx, absPath, node.StartLine, node.EndLine, contextLines); err == nil {
					entry["source"] = source
					entry["from_line"] = fromLine
					returned := tokens.CachedCountInt64(source)
					fullFile := int64(tokens.EstimateFromSample(totalFileChars, source))
					batchStats := s.tokenStatsFor(ctx)
					batchStats.creditFile(absPath)
					batchStats.record(s.savingsAttributionNode(node), "batch_symbols", returned, fullFile)
				}
			}
		}

		results = append(results, entry)
	}

	batchResult := map[string]any{
		"symbols": results,
		"total":   len(results),
	}

	// ETag conditional fetch.
	etag := computeETag(batchResult)
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}
	batchResult["etag"] = etag

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeBatchSymbols(results, includeSource))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(batchResult)
	}

	return s.respondJSONOrTOON(ctx, req, batchResult)
}

// Test file patterns by language.
var testFilePatterns = []struct {
	suffix string
	lang   string
}{
	{"_test.go", "go"},
	{".test.ts", "typescript"},
	{".test.tsx", "typescript"},
	{".spec.ts", "typescript"},
	{".test.js", "javascript"},
	{".spec.js", "javascript"},
	{"_test.py", "python"},
	{"test_", "python"},
	{"_test.rs", "rust"},
	{"Test.java", "java"},
	{"_test.rb", "ruby"},
	{"_test.exs", "elixir"},
	{"_test.kt", "kotlin"},
	{"Tests.swift", "swift"},
	{"Test.scala", "scala"},
	{"Test.php", "php"},
	{"Test.cs", "csharp"},
}

func isTestFile(path string) bool {
	for _, p := range testFilePatterns {
		if strings.Contains(path, p.suffix) {
			return true
		}
	}
	return strings.Contains(path, "__tests__/") || strings.Contains(path, "/test/")
}

// partitionProductionFirst returns a stable partition of nodes with
// every production-code symbol ahead of every test symbol, preserving
// the relative order within each group. Used to bias the limited
// smart_context source-embed slots toward implementation over the
// tests that exercise it without disturbing the upstream rerank order.
func partitionProductionFirst(nodes []*graph.Node) []*graph.Node {
	if len(nodes) < 2 {
		return nodes
	}
	prod := make([]*graph.Node, 0, len(nodes))
	tests := make([]*graph.Node, 0)
	for _, n := range nodes {
		if n != nil && isTestFile(n.FilePath) {
			tests = append(tests, n)
			continue
		}
		prod = append(prod, n)
	}
	if len(tests) == 0 {
		return nodes
	}
	return append(prod, tests...)
}

// buildWorkingSetClusters groups a smart_context working set by source
// file, production files first, returning one entry per file as
// {file, symbols:[ids], is_test}. Symbol IDs within a file and the file
// groups themselves are sorted so the block is byte-stable and feeds a
// deterministic pack root.
func buildWorkingSetClusters(nodes []*graph.Node) []map[string]any {
	if len(nodes) == 0 {
		return nil
	}
	byFile := make(map[string][]string)
	isTest := make(map[string]bool)
	for _, n := range nodes {
		if n == nil {
			continue
		}
		byFile[n.FilePath] = append(byFile[n.FilePath], n.ID)
		isTest[n.FilePath] = isTestFile(n.FilePath)
	}
	prodFiles := make([]string, 0, len(byFile))
	testFiles := make([]string, 0)
	for f := range byFile {
		if isTest[f] {
			testFiles = append(testFiles, f)
			continue
		}
		prodFiles = append(prodFiles, f)
	}
	sort.Strings(prodFiles)
	sort.Strings(testFiles)
	ordered := append(prodFiles, testFiles...)
	clusters := make([]map[string]any, 0, len(ordered))
	for _, f := range ordered {
		ids := byFile[f]
		sort.Strings(ids)
		clusters = append(clusters, map[string]any{
			"file":    f,
			"symbols": ids,
			"is_test": isTest[f],
		})
	}
	return clusters
}

func (s *Server) handleGetTestTargets(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("ids")
	if err != nil {
		return mcp.NewToolResultError("ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	depth := req.GetInt("depth", 3)

	// For each symbol, trace callers and collect test nodes.
	type testTarget struct {
		File      string   `json:"file"`
		Functions []string `json:"functions"`
	}

	// Map: test file -> set of test function names.
	testFiles := make(map[string]map[string]bool)
	// Track which changed symbols are covered.
	coveredSymbols := make(map[string]bool)

	for _, id := range ids {
		node := s.engineFor(ctx).GetSymbol(id)
		if node == nil {
			continue
		}

		// Fast path: use the persistent EdgeTests edges that the
		// indexer's test-edge pass attached to the graph. A direct
		// inverse-edge walk is one hop instead of the BFS-on-EdgeCalls
		// that this tool used to do, and it's exact (no isTestFile
		// post-filter needed).
		if testers := s.engineFor(ctx).GetTesters(id); len(testers) > 0 {
			for _, tn := range testers {
				if tn == nil {
					continue
				}
				coveredSymbols[id] = true
				if testFiles[tn.FilePath] == nil {
					testFiles[tn.FilePath] = make(map[string]bool)
				}
				if tn.Kind == graph.KindFunction || tn.Kind == graph.KindMethod {
					testFiles[tn.FilePath][tn.Name] = true
				}
			}
			continue
		}

		// Fallback for graphs that haven't been re-indexed since the
		// EdgeTests pass shipped, or for indirect coverage (depth > 1).
		callers := s.engineFor(ctx).GetCallers(id, query.QueryOptions{Depth: depth, Limit: 100, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if !isTestFile(cn.FilePath) {
				continue
			}
			coveredSymbols[id] = true
			if testFiles[cn.FilePath] == nil {
				testFiles[cn.FilePath] = make(map[string]bool)
			}
			if cn.Kind == graph.KindFunction || cn.Kind == graph.KindMethod {
				testFiles[cn.FilePath][cn.Name] = true
			}
		}

		// Also check if the symbol itself is in a test file (e.g. test helper).
		if isTestFile(node.FilePath) {
			coveredSymbols[id] = true
			if testFiles[node.FilePath] == nil {
				testFiles[node.FilePath] = make(map[string]bool)
			}
		}
	}

	// Build result.
	var targets []testTarget
	for file, funcs := range testFiles {
		var names []string
		for name := range funcs {
			names = append(names, name)
		}
		targets = append(targets, testTarget{
			File:      file,
			Functions: names,
		})
	}

	// Both testFiles and its inner func sets are maps, so the loop above
	// yields a different order on every call — which run_commands then bakes
	// into its `-run TestA|TestB` join. Sort so the response (and anything
	// hashing or diffing it) is stable.
	sort.Slice(targets, func(i, j int) bool { return targets[i].File < targets[j].File })
	for i := range targets {
		sort.Strings(targets[i].Functions)
	}

	// Build run commands (Go-specific for now, extensible later).
	var runCommands []string
	for _, t := range targets {
		if strings.HasSuffix(t.File, "_test.go") {
			dir := filepath.Dir(t.File)
			if len(t.Functions) > 0 {
				runCommands = append(runCommands,
					fmt.Sprintf("go test -run %s ./%s/", strings.Join(t.Functions, "|"), dir))
			} else {
				runCommands = append(runCommands,
					fmt.Sprintf("go test ./%s/", dir))
			}
		}
	}

	// Uncovered symbols (no test found).
	var uncovered []string
	for _, id := range ids {
		if !coveredSymbols[id] {
			uncovered = append(uncovered, id)
		}
	}

	// Compact text for callers that paste this into a prompt or a hook
	// briefing — notably the Stop hook, which has always passed compact:true
	// and, until this branch existed, got a JSON blob pasted into markdown.
	if isCompact(req) {
		var b strings.Builder
		// Sentinel first, so a caller can recognize "nothing covers this"
		// from the leading line rather than having to scan to the end.
		if len(targets) == 0 {
			b.WriteString("no test targets found\n")
		}
		for _, t := range targets {
			if len(t.Functions) > 0 {
				fmt.Fprintf(&b, "%s %s\n", t.File, strings.Join(t.Functions, " "))
			} else {
				fmt.Fprintf(&b, "%s\n", t.File)
			}
		}
		for _, c := range runCommands {
			fmt.Fprintf(&b, "run: %s\n", c)
		}
		if len(uncovered) > 0 {
			fmt.Fprintf(&b, "uncovered: %s\n", strings.Join(uncovered, " "))
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"test_targets":  targets,
		"run_commands":  runCommands,
		"total_files":   len(targets),
		"uncovered":     uncovered,
		"coverage_note": fmt.Sprintf("%d/%d changed symbols have test coverage", len(coveredSymbols), len(ids)),
	})
}

// elideSourceForPattern keeps the first head + last tail of a source
// block when it exceeds maxLines, joined by `... N lines elided ...`.
// suggest_pattern emits up to seven source blocks per call (example +
// up to ten registration callers + up to three test patterns) — full
// bodies were the main reason a single call could exceed 5KB. Pass
// maxLines <= 0 to disable elision.
func elideSourceForPattern(source string, maxLines int) string {
	if maxLines <= 0 || source == "" {
		return source
	}
	lines := strings.Split(source, "\n")
	if len(lines) <= maxLines {
		return source
	}
	// Keep ~2/3 of the budget at the head (declarations + body
	// opening), 1/3 at the tail (closing scope / return). A few-line
	// preview of each end is enough for an agent to follow the
	// pattern; the elision marker tells the caller they can ask for
	// more by raising max_source_lines.
	head := (maxLines * 2) / 3
	if head < 1 {
		head = 1
	}
	tail := maxLines - head
	if tail < 1 {
		tail = 1
	}
	if head+tail >= len(lines) {
		return source
	}
	elided := len(lines) - head - tail
	var b strings.Builder
	b.WriteString(strings.Join(lines[:head], "\n"))
	b.WriteString("\n")
	fmt.Fprintf(&b, "... %d lines elided (raise max_source_lines to see) ...\n", elided)
	b.WriteString(strings.Join(lines[len(lines)-tail:], "\n"))
	return b.String()
}

func (s *Server) handleSuggestPattern(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	exampleID, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}

	node := s.engineFor(ctx).GetSymbol(exampleID)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + exampleID), nil
	}

	// Per-source elision threshold. The handler stitches together
	// the example, every registration caller, and up to three test
	// patterns — each with full source — which used to balloon the
	// response to several KB on real handlers. Elide each source
	// past this many lines so the default response stays compact;
	// pass `max_source_lines: 0` to opt out.
	maxSourceLines := req.GetInt("max_source_lines", 40)

	result := map[string]any{
		"example": map[string]any{
			"id":        node.ID,
			"kind":      node.Kind,
			"name":      node.Name,
			"file_path": node.FilePath,
		},
	}

	// 1. Get the example source.
	if node.StartLine > 0 && node.EndLine > 0 {
		if absPath, err := s.resolveNodePath(ctx, node); err == nil {
			if source, _, _, err := s.readLinesForCtx(ctx, absPath, node.StartLine, node.EndLine, 0); err == nil {
				result["example_source"] = elideSourceForPattern(source, maxSourceLines)
			}
		}
	}
	if sig, ok := node.Meta["signature"]; ok {
		result["signature"] = sig
	}

	// 2. Find siblings — same kind, same file, similar naming pattern.
	fileSymbols := s.engineFor(ctx).GetFileSymbols(node.FilePath)
	if len(fileSymbols.Nodes) > 0 && !s.nodeInSessionScope(ctx, fileSymbols.Nodes[0]) {
		fileSymbols = &query.SubGraph{}
	}
	var siblings []map[string]any
	prefix := extractPrefix(node.Name)
	for _, sn := range fileSymbols.Nodes {
		if sn.ID == node.ID || sn.Kind != node.Kind {
			continue
		}
		siblings = append(siblings, map[string]any{
			"id":         sn.ID,
			"name":       sn.Name,
			"start_line": sn.StartLine,
		})
	}
	if len(siblings) > 10 {
		siblings = siblings[:10]
	}
	result["siblings"] = siblings
	result["siblings_count"] = len(fileSymbols.Nodes) - 1 // exclude file node

	// 3. Find how the example is wired/registered (callers at depth 1).
	callers := s.engineFor(ctx).GetCallers(exampleID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
	var registration []map[string]any
	for _, cn := range callers.Nodes {
		if cn.ID == exampleID {
			continue
		}
		entry := map[string]any{
			"id":         cn.ID,
			"name":       cn.Name,
			"file_path":  cn.FilePath,
			"start_line": cn.StartLine,
		}
		// Get the registration source (the caller function that wires this symbol).
		if cn.StartLine > 0 && cn.EndLine > 0 {
			if absPath, err := s.resolveNodePath(ctx, cn); err == nil {
				if source, _, _, err := s.readLinesForCtx(ctx, absPath, cn.StartLine, cn.EndLine, 0); err == nil {
					entry["source"] = elideSourceForPattern(source, maxSourceLines)
				}
			}
		}
		registration = append(registration, entry)
	}
	result["registration"] = registration

	// 4. Find test patterns — look for test symbols with matching name prefix.
	var testPatterns []map[string]any
	if prefix != "" {
		// Search for test functions that match the example name.
		testSearch := s.scopedNodeSlice(ctx, s.engineFor(ctx).SearchSymbols(node.Name, 20))
		for _, tn := range testSearch {
			if !isTestFile(tn.FilePath) {
				continue
			}
			if tn.Kind != graph.KindFunction && tn.Kind != graph.KindMethod {
				continue
			}
			entry := map[string]any{
				"id":         tn.ID,
				"name":       tn.Name,
				"file_path":  tn.FilePath,
				"start_line": tn.StartLine,
			}
			// Get test source.
			if tn.StartLine > 0 && tn.EndLine > 0 {
				if absPath, err := s.resolveNodePath(ctx, tn); err == nil {
					if source, _, _, err := s.readLinesForCtx(ctx, absPath, tn.StartLine, tn.EndLine, 0); err == nil {
						entry["source"] = elideSourceForPattern(source, maxSourceLines)
					}
				}
			}
			testPatterns = append(testPatterns, entry)
			if len(testPatterns) >= 3 {
				break
			}
		}
	}
	result["test_patterns"] = testPatterns

	// 5. Files to edit — where would you add a new instance of this pattern?
	filesToEdit := []map[string]any{
		{"file": node.FilePath, "reason": "add new symbol here (same file as example)"},
	}
	for _, reg := range registration {
		if fp, ok := reg["file_path"].(string); ok && fp != node.FilePath {
			filesToEdit = append(filesToEdit, map[string]any{
				"file":   fp,
				"reason": "update registration/wiring",
			})
		}
	}
	for _, tp := range testPatterns {
		if fp, ok := tp["file_path"].(string); ok {
			filesToEdit = append(filesToEdit, map[string]any{
				"file":   fp,
				"reason": "add test for new symbol",
			})
			break // one test file is enough
		}
	}
	result["files_to_edit"] = filesToEdit

	return s.respondJSONOrTOON(ctx, req, result)
}

func (s *Server) handleGetEditPlan(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("ids")
	if err != nil {
		return mcp.NewToolResultError("ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	depth := req.GetInt("depth", 3)

	type editStep struct {
		File    string   `json:"file"`
		Symbols []string `json:"symbols"`
		Reason  string   `json:"reason"`
		Order   int      `json:"order"`
	}

	// Track files by category and depth.
	type fileInfo struct {
		symbols map[string]bool
		reason  string
		order   int // lower = edit first
	}
	files := make(map[string]*fileInfo)

	addFile := func(filePath, symbol, reason string, order int) {
		if fi, ok := files[filePath]; ok {
			fi.symbols[symbol] = true
			// Keep the lowest (highest priority) order.
			if order < fi.order {
				fi.order = order
				fi.reason = reason
			}
		} else {
			files[filePath] = &fileInfo{
				symbols: map[string]bool{symbol: true},
				reason:  reason,
				order:   order,
			}
		}
	}

	changedFiles := make(map[string]bool)

	// Order 0: The changed symbols themselves (definitions).
	for _, id := range ids {
		node := s.engineFor(ctx).GetSymbol(id)
		if node == nil {
			continue
		}
		addFile(node.FilePath, node.Name, "definition — change starts here", 0)
		changedFiles[node.FilePath] = true

		// Check if symbol is an interface — implementations need updating.
		if node.Kind == graph.KindInterface {
			impls := s.scopedNodeSlice(ctx, s.engineFor(ctx).FindImplementations(id))
			for _, impl := range impls {
				addFile(impl.FilePath, impl.Name, "implements "+node.Name+" — must conform to changes", 1)
			}
		}

		// Check MemberOf — if changing a type, its methods may need updating.
		if node.Kind == graph.KindType || node.Kind == graph.KindInterface {
			inEdges := s.engineFor(ctx).GetInEdges(id)
			for _, e := range inEdges {
				if e.Kind == graph.EdgeMemberOf {
					memberNode := s.engineFor(ctx).GetSymbol(e.From)
					if memberNode != nil {
						addFile(memberNode.FilePath, memberNode.Name, "member of "+node.Name, 1)
					}
				}
			}
		}
	}

	// Order 2-N: Dependents at increasing depth (callers/importers).
	for _, id := range ids {
		dependents := s.engineFor(ctx).GetDependents(id, query.QueryOptions{Depth: depth, Limit: 100, Detail: "brief"})
		for _, dn := range dependents.Nodes {
			if dn.Kind == graph.KindFile {
				continue
			}
			// Skip the changed symbols themselves.
			isChanged := false
			for _, cid := range ids {
				if dn.ID == cid {
					isChanged = true
					break
				}
			}
			if isChanged {
				continue
			}

			if isTestFile(dn.FilePath) {
				addFile(dn.FilePath, dn.Name, "test — verify after changes", 100)
			} else if changedFiles[dn.FilePath] {
				// Same file as a changed symbol, already covered.
				addFile(dn.FilePath, dn.Name, "definition — change starts here", 0)
			} else {
				addFile(dn.FilePath, dn.Name, "dependent — may need updating", 2)
			}
		}
	}

	// Sort by order, then by file path.
	type sortableStep struct {
		filePath string
		info     *fileInfo
	}
	var sorted []sortableStep
	for fp, fi := range files {
		sorted = append(sorted, sortableStep{fp, fi})
	}
	// Stable sort: order first, then alphabetical.
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].info.order < sorted[i].info.order ||
				(sorted[j].info.order == sorted[i].info.order && sorted[j].filePath < sorted[i].filePath) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	var steps []editStep
	for _, s := range sorted {
		var symbols []string
		for sym := range s.info.symbols {
			symbols = append(symbols, sym)
		}
		steps = append(steps, editStep{
			File:    s.filePath,
			Symbols: symbols,
			Reason:  s.info.reason,
			Order:   s.info.order,
		})
	}

	// Separate test files.
	var editSteps, testSteps []editStep
	for _, step := range steps {
		if isTestFile(step.File) {
			testSteps = append(testSteps, step)
		} else {
			editSteps = append(editSteps, step)
		}
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"edit_order":  editSteps,
		"test_after":  testSteps,
		"total_files": len(steps),
		"summary":     fmt.Sprintf("%d files to edit, %d test files to verify", len(editSteps), len(testSteps)),
	})
}

// extractPrefix returns the common prefix of a camelCase/PascalCase name.
// e.g. "handleGetSymbol" -> "handle", "TestNewServer" -> "Test"
func (s *Server) handleSmartContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task, err := req.RequireString("task")
	if err != nil {
		return mcp.NewToolResultError("task is required"), nil
	}
	// Lift any inline path: clause out of the task text so a
	// caller can scope smart_context to a monorepo sub-path the
	// same way search_symbols does. The residual text drives
	// keyword extraction.
	taskFQ := parseFieldQuery(task)
	if taskFQ.Text != "" {
		task = taskFQ.Text
	}

	entryPoint := req.GetString("entry_point", "")
	// Seed count defaults adaptively to repo size — req.GetInt can't
	// tell an absent argument from an explicit value, so detect
	// presence on the raw arguments map and only scale when the caller
	// left it out.
	maxSymbols := req.GetInt("max_symbols", 5)
	if _, present := req.GetArguments()["max_symbols"]; !present {
		nodes := 0
		if s.graph != nil {
			nodes = s.graph.NodeCount()
		}
		maxSymbols = smartContextSeedCount(nodes)
	}
	graded := req.GetString("fidelity", "") == "graded"

	// Token budget for the graded manifest defaults adaptively to repo
	// size — same absent-vs-explicit detection as max_symbols, since
	// req.GetInt can't distinguish them.
	tokenBudget := req.GetInt("token_budget", defaultManifestBudget)
	if _, present := req.GetArguments()["token_budget"]; !present {
		nodes := 0
		if s.graph != nil {
			nodes = s.graph.NodeCount()
		}
		tokenBudget = manifestBudgetForNodeCount(nodes)
	}

	result := map[string]any{
		"task": task,
	}

	// 1. Extract keywords from task description. Kept internal — the
	// caller doesn't need the derivation to act on the result, and
	// echoing 5-10 keywords per response is pure token bloat.
	keywords := extractKeywords(task)

	// 2. Search for relevant symbols using each keyword.
	seen := make(map[string]bool)
	var relevantSymbols []*graph.Node
	for _, kw := range keywords {
		if len(kw) < 3 {
			continue
		}
		matches := s.scopedNodeSlice(ctx, s.engineFor(ctx).SearchSymbols(kw, 10))
		for _, m := range matches {
			if m.Kind == graph.KindFile || m.Kind == graph.KindImport {
				continue
			}
			if !seen[m.ID] {
				seen[m.ID] = true
				relevantSymbols = append(relevantSymbols, m)
			}
		}
	}

	// 3. If entry point given, resolve it and prioritize.
	var entryNode *graph.Node
	if entryPoint != "" {
		// Try as symbol ID first.
		entryNode = s.engineFor(ctx).GetSymbol(entryPoint)
		if entryNode == nil {
			// Try as file path — get the most important symbol in the file.
			fileSym := s.engineFor(ctx).GetFileSymbols(entryPoint)
			if len(fileSym.Nodes) > 0 && !s.nodeInSessionScope(ctx, fileSym.Nodes[0]) {
				fileSym = &query.SubGraph{}
			}
			if len(fileSym.Nodes) > 0 {
				for _, n := range fileSym.Nodes {
					if n.Kind != graph.KindFile {
						entryNode = n
						break
					}
				}
			}
		}
		if entryNode != nil && !seen[entryNode.ID] {
			relevantSymbols = append([]*graph.Node{entryNode}, relevantSymbols...)
			seen[entryNode.ID] = true
		}
	}

	// 3b. Apply repo/project filter to relevant symbols.
	allowed, filterErr := s.resolveRepoFilter(ctx, req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	relevantSymbols = filterNodes(relevantSymbols, allowed)
	// Sub-path scoping: a `path` argument, an inline path: clause, or
	// a scope's saved paths narrow smart_context to a monorepo
	// service slice.
	if pathFilter := s.resolvePathFilter(req, taskFQ); len(pathFilter) > 0 {
		relevantSymbols = applyPathFilter(relevantSymbols, pathFilter)
	}

	// 3c. Fold frequently-missed symbols (that match task keywords)
	// into the working set before ranking, so feedback can pull a
	// previously-dropped result back into the embedded slots — not
	// just append it at the tail.
	if s.feedback != nil && s.feedback.HasData() {
		missed := s.feedback.MissedSymbolsForQuery(task, 3)
		injected := 0
		for _, missedID := range missed {
			if injected >= 2 {
				break
			}
			if seen[missedID] {
				continue
			}
			missedNode := s.readerFor(ctx).GetNode(missedID)
			if missedNode == nil {
				continue
			}
			nameLower := strings.ToLower(missedNode.Name)
			for _, kw := range keywords {
				if strings.Contains(nameLower, strings.ToLower(kw)) {
					relevantSymbols = append(relevantSymbols, missedNode)
					seen[missedID] = true
					injected++
					break
				}
			}
		}
	}

	// 3d. Rank the deduped working set through the full rerank
	// pipeline — the same churn / HITS / community / co-change /
	// frecency / feedback / path-penalty / source-bias / provenance
	// scoring that powers search_symbols and winnow. The keyword-merge
	// order seeds TextRank so BM25 provenance survives into the rerank.
	// The pipeline is nil in the test harness (Rerank disabled); fall
	// back to the legacy feedback-aware local sort there so ordering
	// stays sensible without a pipeline.
	if len(relevantSymbols) > 0 {
		pipeline := s.engineFor(ctx).Rerank()
		if pipeline != nil {
			cands := make([]*rerank.Candidate, len(relevantSymbols))
			idxByID := make(map[string]int, len(relevantSymbols))
			for i, sym := range relevantSymbols {
				cands[i] = &rerank.Candidate{Node: sym, TextRank: i, VectorRank: -1}
				idxByID[sym.ID] = i
			}
			rctx := s.buildRerankContext(ctx, task)
			pipeline.Rerank(task, cands, rctx)
			ordered := make([]*graph.Node, 0, len(cands))
			for _, cand := range cands {
				ordered = append(ordered, relevantSymbols[idxByID[cand.Node.ID]])
			}
			relevantSymbols = ordered
		} else if s.feedback != nil && s.feedback.HasData() {
			type scored struct {
				node  *graph.Node
				score float64
			}
			scoredSyms := make([]scored, len(relevantSymbols))
			for i, sym := range relevantSymbols {
				baseScore := 1.0 / float64(i+1) // BM25 rank-based score
				fbScore := s.feedback.GetSymbolScoreForQuery(sym.ID, task)
				scoredSyms[i] = scored{node: sym, score: baseScore + fbScore*0.3}
			}
			sort.SliceStable(scoredSyms, func(i, j int) bool {
				return scoredSyms[i].score > scoredSyms[j].score
			})
			for i, ss := range scoredSyms {
				relevantSymbols[i] = ss.node
			}
		}
	}

	// 3d-bias. Bias production code ahead of tests. A stable partition
	// keeps the rerank order within each group but pulls every
	// non-test symbol in front of every test symbol, so the limited
	// full-source embed slots and the head of files_to_edit favour the
	// implementation over the test that exercises it.
	relevantSymbols = partitionProductionFirst(relevantSymbols)

	// 3e. Re-pin the resolved entry point at the head: a caller that
	// named an entry_point wants it first regardless of rerank order.
	if entryNode != nil && len(relevantSymbols) > 0 && relevantSymbols[0].ID != entryNode.ID {
		reordered := make([]*graph.Node, 0, len(relevantSymbols))
		reordered = append(reordered, entryNode)
		for _, sym := range relevantSymbols {
			if sym.ID != entryNode.ID {
				reordered = append(reordered, sym)
			}
		}
		relevantSymbols = reordered
	}

	// Pack strategy: reorder the working set per GORTEX_PACK_STRATEGY
	// before the count cap / manifest budget fill. The default (top-k,
	// rank-faithful) is identity, so existing callers are unaffected;
	// density / file-grouped re-order by token efficiency or file
	// coherence. This is the same strategy the eval harness A/Bs.
	relevantSymbols = s.applyPackStrategy(relevantSymbols)

	// 4. Limit to top N most relevant symbols. In graded-fidelity mode
	// the overflow seeds the manifest's outline tier instead of being
	// discarded.
	var outlineCandidates []*graph.Node
	if len(relevantSymbols) > maxSymbols {
		outlineCandidates = append(outlineCandidates, relevantSymbols[maxSymbols:]...)
		relevantSymbols = relevantSymbols[:maxSymbols]
	}

	// 4b. Estimate mode short-circuits: project the symbol-delivery
	// token cost and return it without assembling the payload, so the
	// caller can budget before fetching the real context.
	if req.GetBool("estimate", false) {
		estResult := map[string]any{
			"task": task,
			"estimate": s.buildSmartContextEstimate(
				ctx, graded, tokenBudget,
				relevantSymbols, outlineCandidates),
		}
		if s.isGCX(ctx, req) {
			return s.gcxResponseWithBudget(req)(encodeSmartContextEstimate(estResult))
		}
		if s.isTOON(ctx, req) {
			return returnTOON(estResult)
		}
		return s.respondJSONOrTOON(ctx, req, estResult)
	}

	// 5. Get source and signatures for relevant symbols. Source is
	// only embedded for the top smartCtxMaxSource functions/methods —
	// signatures alone cover 80% of agent decision-making and each
	// full source snippet adds several hundred tokens. Callers that
	// need more can follow up with get_symbol_source for specific IDs.
	sourcesEmbedded := 0
	var symbolContexts []map[string]any
	for _, sym := range relevantSymbols {
		entry := map[string]any{
			"id":         sym.ID,
			"kind":       sym.Kind,
			"name":       sym.Name,
			"file_path":  sym.FilePath,
			"language":   sym.Language,
			"start_line": sym.StartLine,
		}
		if sig, ok := sym.Meta["signature"]; ok {
			entry["signature"] = sig
		}
		if !graded && sourcesEmbedded < smartCtxMaxSource &&
			(sym.Kind == graph.KindFunction || sym.Kind == graph.KindMethod) &&
			sym.StartLine > 0 && sym.EndLine > 0 {
			if absPath, err := s.resolveNodePath(ctx, sym); err == nil {
				if source, _, totalFileChars, err := s.readLinesForCtx(ctx, absPath, sym.StartLine, sym.EndLine, 0); err == nil {
					if red, did := s.maybeRedactConfigLeaf(sym.Language, sym.FilePath, false, source); did {
						source = red
					}
					entry["source"] = source
					sourcesEmbedded++
					returned := tokens.CachedCountInt64(source)
					fullFile := int64(tokens.EstimateFromSample(totalFileChars, source))
					ctxStats := s.tokenStatsFor(ctx)
					ctxStats.creditFile(absPath)
					ctxStats.record(s.savingsAttributionNode(sym), "smart_context", returned, fullFile)
				}
			}
		}
		symbolContexts = append(symbolContexts, entry)
	}
	result["relevant_symbols"] = symbolContexts
	// Report the focus-symbol count to the retrieval query log so a
	// zero-result context assembly is recorded as such.
	recordQueryResultCount(ctx, len(symbolContexts))

	// 5b1. Graded-fidelity manifest: focus symbols at full source,
	// their caller/callee adjacency ring as elided signature stubs,
	// and the keyword-match remainder as an outline — all packed
	// under one token budget.
	if graded {
		result["context_manifest"] = s.buildContextManifest(
			ctx, relevantSymbols, outlineCandidates, tokenBudget)
	}

	// 5b. Include cross-repo dependencies when in multi-repo mode.
	if s.multiIndexer != nil && s.multiIndexer.IsMultiRepo() {
		var crossRepoDeps []map[string]any
		crossSeen := make(map[string]bool)
		for _, sym := range relevantSymbols {
			// Check outgoing edges for cross-repo references.
			outEdges := s.engineFor(ctx).GetOutEdges(sym.ID)
			for _, e := range outEdges {
				if !e.CrossRepo || crossSeen[e.To] {
					continue
				}
				crossSeen[e.To] = true
				targetNode := s.engineFor(ctx).GetSymbol(e.To)
				if targetNode == nil {
					continue
				}
				dep := map[string]any{
					"id":          targetNode.ID,
					"kind":        targetNode.Kind,
					"name":        targetNode.Name,
					"file_path":   targetNode.FilePath,
					"repo_prefix": targetNode.RepoPrefix,
					"edge_kind":   e.Kind,
				}
				if sig, ok := targetNode.Meta["signature"]; ok {
					dep["signature"] = sig
				}
				crossRepoDeps = append(crossRepoDeps, dep)
			}
		}
		if len(crossRepoDeps) > 0 {
			result["cross_repo_dependencies"] = crossRepoDeps
		}
	}

	// 6. If we have an entry point, get its pattern (registration, siblings, tests).
	if entryNode != nil {
		// File context: imports and structure.
		fileCtx := s.engineFor(ctx).GetFileSymbols(entryNode.FilePath)
		if len(fileCtx.Nodes) > 0 && !s.nodeInSessionScope(ctx, fileCtx.Nodes[0]) {
			fileCtx = &query.SubGraph{}
		}
		var fileSymbols []string
		for _, n := range fileCtx.Nodes {
			if n.Kind != graph.KindFile {
				fileSymbols = append(fileSymbols, fmt.Sprintf("%s %s (line %d)", n.Kind, n.Name, n.StartLine))
			}
		}
		result["entry_file_symbols"] = fileSymbols

		// Callers and callees.
		callers := s.engineFor(ctx).GetCallers(entryNode.ID, query.QueryOptions{Depth: 1, Limit: 5, Detail: "brief"})
		var callerIDs []string
		for _, cn := range callers.Nodes {
			if cn.ID != entryNode.ID {
				callerIDs = append(callerIDs, cn.ID)
			}
		}
		if len(callerIDs) > 0 {
			result["callers"] = callerIDs
		}

		callees := s.engineFor(ctx).GetCallChain(entryNode.ID, query.QueryOptions{Depth: 1, Limit: 5, Detail: "brief"})
		var calleeIDs []string
		for _, cn := range callees.Nodes {
			if cn.ID != entryNode.ID {
				calleeIDs = append(calleeIDs, cn.ID)
			}
		}
		if len(calleeIDs) > 0 {
			result["callees"] = calleeIDs
		}
	}

	// 7. Find test files related to the keywords.
	var testFiles []string
	testSeen := make(map[string]bool)
	for _, sym := range relevantSymbols {
		callers := s.engineFor(ctx).GetCallers(sym.ID, query.QueryOptions{Depth: 2, Limit: 20, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if isTestFile(cn.FilePath) && !testSeen[cn.FilePath] {
				testSeen[cn.FilePath] = true
				testFiles = append(testFiles, cn.FilePath)
			}
		}
	}
	if len(testFiles) > 5 {
		testFiles = testFiles[:5]
	}
	result["related_test_files"] = testFiles

	// 8. Files likely to edit.
	fileSet := make(map[string]bool)
	for _, sym := range relevantSymbols {
		fileSet[sym.FilePath] = true
	}
	var filesToEdit []string
	for f := range fileSet {
		filesToEdit = append(filesToEdit, f)
	}
	// Sorted so the assembled pack is byte-stable across identical
	// calls — the pack-root etag below depends on it.
	sort.Strings(filesToEdit)
	for _, tf := range testFiles {
		if !fileSet[tf] {
			filesToEdit = append(filesToEdit, tf)
		}
	}
	result["files_to_edit"] = filesToEdit

	// 8b. Clustered working set — the same symbols grouped by their
	// source file (production files first), so an agent sees which
	// files carry the relevant symbols and which are tests without
	// re-deriving it from the flat files_to_edit list. files_to_edit
	// is kept for back-compat.
	if ws := buildWorkingSetClusters(relevantSymbols); len(ws) > 0 {
		result["working_set"] = ws
	}

	// 9. Fused blast radius — computed for every call, not just when an
	// entry point is named. Groups the working set's callers by their
	// source file and lists the tests that cover the working set via
	// the exact EdgeTests inverse walk, so an agent sees what its edit
	// can break and what guards it before it touches a line.
	if br := s.buildBlastRadius(ctx, relevantSymbols); br != nil {
		result["blast_radius"] = br
	}

	// Opt-in in-pack enrichment sections (off by default).
	inPackSections := s.smartContextSections(req.GetArguments(), entryPoint)
	s.attachInPackSections(ctx, result, inPackSections, relevantSymbols)
	if inPackSections.Confidence {
		if v := s.inPackConfidence(ctx, task); v != nil {
			addInPackSection(result, "confidence", v)
		}
	}
	// Always-on low-confidence retrieval note: independent of the opt-in
	// verbose confidence block above, when the retrieval that seeded this
	// pack looks untrustworthy (flat ranked distribution, or a head symbol
	// anchored only by speculative edges) attach a compact, actionable note
	// routing the agent to Gortex's richer escape hatches. Suppressed for
	// single-symbol / distinctive-identifier lookups, so a confident exact
	// query never draws a hedge.
	if note := s.lowConfidenceRetrievalNote(ctx, task, relevantSymbols); note != nil {
		result["retrieval_note"] = note
	}

	// Pack-assembly passes: recover the edges between pack symbols a many-rooted
	// retrieval leaves disconnected, and surface class-hierarchy siblings.
	if rec := s.recoverPackEdges(ctx, relevantSymbols); len(rec) > 0 {
		result["recovered_edges"] = rec
	}
	if sibs := s.packHierarchySiblings(ctx, relevantSymbols); len(sibs) > 0 {
		result["hierarchy_siblings"] = sibs
	}

	// Pack-root dedup: hash the assembled context pack. When the
	// caller passes back the pack root it already holds and nothing
	// the pack covers has changed, return not_modified instead of
	// retransmitting the whole payload.
	etag := computePackRoot(result)
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}
	result["etag"] = etag

	// Delta context packing: cache this pack's canonical view under its
	// root, and when the caller passes a prior root it already holds,
	// return only the added/removed/changed symbols vs that pack instead
	// of re-emitting the whole thing. When the delta is materially
	// smaller than the full pack we drop the bulky symbol lists from the
	// response (the agent merges the delta into its held pack); the pack
	// root still identifies the full pack for the next delta_from.
	currentView := extractPackView(result, true)
	s.packCache.put(etag, currentView)
	if deltaFrom := req.GetString("delta_from", ""); deltaFrom != "" {
		if prior, ok := s.packCache.get(deltaFrom); ok {
			delta := diffPackViews(prior, currentView, deltaFrom, etag)
			result["pack_delta"] = delta
			if worth, _ := delta["worth_it"].(bool); worth {
				result["delta"] = true
				delete(result, "relevant_symbols")
				delete(result, "context_manifest")
				// The delta is structured JSON; bypass the GCX/TOON
				// pack encoders, which assume the full pack shape.
				return mcp.NewToolResultJSON(result)
			}
		} else {
			result["pack_delta"] = map[string]any{
				"available": false,
				"base_root": deltaFrom,
				"new_root":  etag,
				"reason":    "prior pack not in cache (evicted or never seen) — full pack returned",
			}
		}
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeSmartContext(result))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// buildBlastRadius assembles the always-on blast-radius block for a
// smart_context working set: callers grouped by their source file and
// the tests that cover the set. Callers come from a one-hop GetCallers
// walk; covering tests come from the exact EdgeTests inverse walk
// (Engine.GetTesters) — the same edge get_test_targets trusts — not the
// path-name heuristic. Every list is sorted so the block is byte-stable
// and feeds a deterministic pack root. Returns nil when there is no
// graph/engine to walk.
func (s *Server) buildBlastRadius(ctx context.Context, workingSet []*graph.Node) map[string]any {
	eng := s.engineFor(ctx)
	if eng == nil || s.graph == nil || len(workingSet) == 0 {
		return nil
	}

	// Callers grouped by the file each caller lives in.
	callersByFile := make(map[string]map[string]bool)
	for _, sym := range workingSet {
		if sym == nil {
			continue
		}
		callers := eng.GetCallers(sym.ID, query.QueryOptions{Depth: 1, Limit: ringScanLimit, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if cn.ID == sym.ID || cn.Kind == graph.KindFile {
				continue
			}
			set, ok := callersByFile[cn.FilePath]
			if !ok {
				set = make(map[string]bool)
				callersByFile[cn.FilePath] = set
			}
			set[cn.ID] = true
		}
	}
	callerFiles := make([]string, 0, len(callersByFile))
	for f := range callersByFile {
		callerFiles = append(callerFiles, f)
	}
	sort.Strings(callerFiles)
	callerGroups := make([]map[string]any, 0, len(callerFiles))
	for _, f := range callerFiles {
		ids := make([]string, 0, len(callersByFile[f]))
		for id := range callersByFile[f] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		callerGroups = append(callerGroups, map[string]any{
			"file":    f,
			"callers": ids,
		})
	}

	// Covering tests via the exact EdgeTests inverse walk.
	type testRef struct{ file, function string }
	testSeen := make(map[string]bool)
	var tests []testRef
	anyTesters := false
	for _, sym := range workingSet {
		if sym == nil {
			continue
		}
		for _, tn := range eng.GetTesters(sym.ID) {
			if tn == nil {
				continue
			}
			anyTesters = true
			key := tn.FilePath + "\x1f" + tn.Name
			if testSeen[key] {
				continue
			}
			testSeen[key] = true
			tests = append(tests, testRef{file: tn.FilePath, function: tn.Name})
		}
	}
	sort.Slice(tests, func(i, j int) bool {
		if tests[i].file != tests[j].file {
			return tests[i].file < tests[j].file
		}
		return tests[i].function < tests[j].function
	})
	coveringTests := make([]map[string]any, 0, len(tests))
	for _, tr := range tests {
		coveringTests = append(coveringTests, map[string]any{
			"file":     tr.file,
			"function": tr.function,
		})
	}

	br := map[string]any{
		"callers_by_file": callerGroups,
		"covering_tests":  coveringTests,
	}
	if !anyTesters {
		br["warning"] = "no covering tests found"
	}
	return br
}

// extractKeywords splits a task description into searchable keywords.
// Filters out common stop words and short words, then reorders so
// identifier-shape keywords (camelCase, snake_case, anything with an
// uppercase letter after a lowercase one) come first. The BM25 budget
// downstream then fills with the most-likely-to-be-the-actual-target
// candidates before generic English verbs ("find", "caller", …)
// flood it with unrelated hits.
func extractKeywords(task string) []string {
	stopWords := map[string]bool{
		"a": true, "an": true, "the": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"may": true, "might": true, "shall": true, "can": true,
		"for": true, "and": true, "but": true, "or": true, "nor": true,
		"not": true, "so": true, "yet": true, "both": true,
		"to": true, "of": true, "in": true, "on": true, "at": true,
		"by": true, "with": true, "from": true, "into": true, "that": true,
		"this": true, "it": true, "its": true, "as": true, "if": true,
		"add": true, "new": true, "create": true, "make": true, "called": true,
		"like": true, "use": true, "using": true, "how": true, "what": true,
		"want": true, "need": true, "all": true, "each": true, "which": true,
		// Common task verbs / nouns that aren't symbol candidates —
		// a task like "find every caller of FooBar" used to surface
		// `findLocked` / `findDeclaration` first because "find" hit
		// the BM25 budget before the real identifier. Stop them.
		"find": true, "finds": true, "found": true, "finding": true,
		"get": true, "gets": true, "got": true, "getting": true,
		"set": true, "sets": true, "setting": true, "list": true, "lists": true,
		"show": true, "shows": true, "showing": true, "tell": true, "tells": true,
		"every": true, "any": true, "some": true, "most": true,
		"caller": true, "callers": true, "callee": true, "callees": true,
		"user": true, "users": true, "users'": true,
		"function": true, "functions": true, "method": true, "methods": true,
		"file": true, "files": true, "symbol": true, "symbols": true,
		"code": true, "source": true,
	}

	// Split on whitespace and punctuation.
	words := strings.FieldsFunc(task, func(r rune) bool {
		if r >= 'a' && r <= 'z' {
			return false
		}
		if r >= 'A' && r <= 'Z' {
			return false
		}
		if r >= '0' && r <= '9' {
			return false
		}
		return r != '_'
	})

	seen := make(map[string]bool)
	var identifiers, plain []string
	for _, w := range words {
		lower := strings.ToLower(w)
		if len(lower) < 3 || stopWords[lower] || seen[lower] {
			continue
		}
		seen[lower] = true
		if hasIdentifierShape(w) {
			identifiers = append(identifiers, w)
		} else {
			plain = append(plain, w)
		}
	}
	return append(identifiers, plain...)
}

// hasIdentifierShape reports whether w looks like a code identifier
// the user explicitly typed (camelCase, snake_case, has digits) and
// therefore deserves to drive the candidate search before generic
// English verbs.
func hasIdentifierShape(w string) bool {
	hasUnderscore := false
	hasDigit := false
	hasInternalUpper := false
	for i, r := range w {
		switch {
		case r == '_':
			hasUnderscore = true
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				hasInternalUpper = true
			}
		}
	}
	return hasUnderscore || hasDigit || hasInternalUpper
}

func (s *Server) handleRenameSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	newName, err := req.RequireString("new_name")
	if err != nil {
		return mcp.NewToolResultError("new_name is required"), nil
	}
	dryRun := req.GetBool("dry_run", false)
	allowParseErrors := req.GetBool("allow_parse_errors", false)

	node := s.engineFor(ctx).GetSymbol(id)
	if node == nil {
		if recovery := s.unindexedRenameRecovery(ctx, id, newName, dryRun); recovery != nil {
			return NewStructuredErrorResult(StructuredError{
				ErrorCode: ErrCodeSymbolNotIndexed,
				Message:   fmt.Sprintf("semantic rename cannot resolve %s because its file has no indexed symbols", id),
				Data:      recovery,
			}), nil
		}
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}

	oldName := node.Name

	if oldName == newName {
		return mcp.NewToolResultError("new_name is the same as the current name"), nil
	}

	// Resolve abs paths per node/edge — single rootPath is wrong in
	// multi-repo mode where each repo has its own root.
	resolvePath := func(graphPath string) string {
		abs, err := s.resolveGraphPath(ctx, graphPath)
		if err != nil {
			return ""
		}
		return abs
	}

	var edits []renameEdit
	editSeen := make(map[string]bool) // file:line dedup

	// Every planned line is rewritten with whole-identifier replacement of
	// every occurrence. A substring rewrite would corrupt neighbouring names
	// ("Get" inside "GetUser"), and a first-occurrence-only rewrite would miss
	// the second call on lines such as "x := Get(Get(y))". These edits are
	// written to disk, so the plan has to be exactly right.
	rewrite := func(line string) (string, bool) {
		next, n := replaceIdentifierAll(line, oldName, newName)
		return next, n > 0
	}

	// 1. The definition itself.
	// A successful rewrite implies a non-empty line, so an unreadable line
	// (readSingleLineAt returns "") is skipped by the same check.
	defLine := readSingleLineAt(resolvePath(node.FilePath), node.StartLine)
	if newText, ok := rewrite(defLine); ok {
		key := fmt.Sprintf("%s:%d", node.FilePath, node.StartLine)
		if !editSeen[key] {
			editSeen[key] = true
			edits = append(edits, renameEdit{
				File:       node.FilePath,
				Line:       node.StartLine,
				OldText:    defLine,
				NewText:    newText,
				Confidence: "high",
				Reason:     "definition",
			})
		}
	}

	// 2. All graph usages (calls, references, instantiates).
	usages := s.engineFor(ctx).FindUsages(id)
	for _, edge := range usages.Edges {
		if edge.Line == 0 {
			continue
		}
		// Read the source line at the reference.
		srcLine := readSingleLineAt(resolvePath(edge.FilePath), edge.Line)
		newText, ok := rewrite(srcLine)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s:%d", edge.FilePath, edge.Line)
		if editSeen[key] {
			continue
		}
		editSeen[key] = true
		edits = append(edits, renameEdit{
			File:       edge.FilePath,
			Line:       edge.Line,
			OldText:    srcLine,
			NewText:    newText,
			Confidence: "high",
			Reason:     string(edge.Kind),
		})
	}

	// 3. MemberOf edges — if renaming a type, its methods' receiver annotations may reference it.
	if node.Kind == graph.KindType || node.Kind == graph.KindInterface {
		inEdges := s.engineFor(ctx).GetInEdges(id)
		for _, edge := range inEdges {
			if edge.Kind != graph.EdgeMemberOf {
				continue
			}
			memberNode := s.engineFor(ctx).GetSymbol(edge.From)
			if memberNode == nil {
				continue
			}
			// Check if the member's ID contains the old type name (e.g. "file.go::TypeName.MethodName").
			if strings.Contains(memberNode.ID, oldName+".") {
				// The receiver line may mention the type name.
				srcLine := readSingleLineAt(resolvePath(memberNode.FilePath), memberNode.StartLine)
				if newText, ok := rewrite(srcLine); ok {
					key := fmt.Sprintf("%s:%d", memberNode.FilePath, memberNode.StartLine)
					if !editSeen[key] {
						editSeen[key] = true
						edits = append(edits, renameEdit{
							File:       memberNode.FilePath,
							Line:       memberNode.StartLine,
							OldText:    srcLine,
							NewText:    newText,
							Confidence: "high",
							Reason:     "member receiver",
						})
					}
				}
			}
		}
	}

	// 4. Test files that reference the old name (text search fallback).
	for _, edge := range usages.Edges {
		if !isTestFile(edge.FilePath) {
			continue
		}
		// Already covered by graph edges above, but check for test function names
		// like "TestValidateToken" that contain the old name.
		for _, n := range usages.Nodes {
			if n.FilePath == edge.FilePath && strings.Contains(n.Name, oldName) {
				srcLine := readSingleLineAt(resolvePath(n.FilePath), n.StartLine)
				if srcLine == "" {
					continue
				}
				// Here the old name is deliberately embedded in a longer
				// identifier ("TestValidateToken"). Rewrite that whole
				// identifier rather than the bare substring, so the edit
				// cannot land inside some unrelated name on the same line.
				renamed := strings.ReplaceAll(n.Name, oldName, newName)
				if renamed == n.Name {
					continue
				}
				newText, replaced := replaceIdentifierAll(srcLine, n.Name, renamed)
				if replaced == 0 {
					continue
				}
				key := fmt.Sprintf("%s:%d", n.FilePath, n.StartLine)
				if editSeen[key] {
					continue
				}
				editSeen[key] = true
				edits = append(edits, renameEdit{
					File:       n.FilePath,
					Line:       n.StartLine,
					OldText:    srcLine,
					NewText:    newText,
					Confidence: "medium",
					Reason:     "test function name",
				})
			}
		}
	}

	// Collect affected files.
	fileSet := make(map[string]bool)
	for _, e := range edits {
		fileSet[e.File] = true
	}

	resp := map[string]any{
		"old_name":       oldName,
		"new_name":       newName,
		"edits":          edits,
		"total_edits":    len(edits),
		"files_affected": len(fileSet),
		"dry_run":        dryRun,
	}

	if len(edits) == 0 {
		// Nothing to write either way. Say so rather than reporting an
		// applied rename that touched no bytes.
		resp["status"] = "no_edits"
		resp["written"] = false
		return s.respondJSONOrTOON(ctx, req, resp)
	}

	// Lock every affected path before verifying, so the check that each line
	// still matches disk cannot be invalidated between verification and write.
	absPaths := make([]string, 0, len(fileSet))
	for relPath := range fileSet {
		abs, err := s.resolveGraphPath(ctx, relPath)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not resolve %s: %v", relPath, err)), nil
		}
		absPaths = append(absPaths, abs)
	}
	releaseMutation, lockErr := acquireMutationPaths(ctx, absPaths)
	if lockErr != nil {
		return mcp.NewToolResultError("rename cancelled while waiting for exclusive file access: " + lockErr.Error()), nil
	}
	defer releaseMutation()

	writes, planErr := s.planRenameWrites(ctx, edits, allowParseErrors)
	if planErr != nil {
		return mcp.NewToolResultError(planErr.Error()), nil
	}

	if dryRun {
		resp["status"] = "would_apply"
		resp["written"] = false
		resp["files"] = previewRenameWrites(writes)
		return s.respondJSONOrTOON(ctx, req, resp)
	}

	results := s.commitRenameWrites(ctx, writes)
	written := 0
	failed := 0
	for _, r := range results {
		if r["status"] == "applied" {
			written++
			continue
		}
		failed++
	}
	resp["status"] = "applied"
	resp["written"] = written > 0
	resp["files"] = results
	resp["files_written"] = written
	if failed > 0 {
		// Every file passed verification, so a failure here is an I/O fault
		// during the commit itself. Never report it as an applied rename:
		// the caller has to know the tree is now inconsistent.
		resp["files_failed"] = failed
		resp["status"] = "partially_applied"
		if written == 0 {
			resp["status"] = "failed"
		}
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

func (s *Server) handleEditSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	oldSource, err := req.RequireString("old_source")
	if err != nil {
		return mcp.NewToolResultError("old_source is required"), nil
	}
	newSource, err := req.RequireString("new_source")
	if err != nil {
		return mcp.NewToolResultError("new_source is required"), nil
	}
	baseSHA := normalizeExpectedSHA(req.GetString("base_sha", ""))
	dryRun := req.GetBool("dry_run", false)
	evidenceRequested, evidenceErr := parsePhysicalEvidenceRequest(req)
	if evidenceErr != nil {
		return mcp.NewToolResultError(evidenceErr.Error()), nil
	}
	if evidenceRequested && dryRun {
		return mcp.NewToolResultError(errEvidenceDryRun), nil
	}
	mutationID := strings.TrimSpace(req.GetString("mutation_id", ""))

	if oldSource == newSource {
		return mcp.NewToolResultError("old_source and new_source are identical"), nil
	}

	node := s.engineFor(ctx).GetSymbol(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id + s.suggestSymbolIDs(ctx, id)), nil
	}

	if node.StartLine == 0 || node.EndLine == 0 {
		return mcp.NewToolResultError("symbol has no line range: " + id), nil
	}

	// Resolve file path.
	absPath, err := s.resolveNodePath(ctx, node)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	releaseMutation, lockErr := acquireMutationPath(ctx, absPath)
	if lockErr != nil {
		return mcp.NewToolResultError("edit cancelled while waiting for exclusive file access: " + lockErr.Error()), nil
	}
	defer releaseMutation()

	// Replay under the path lock, before the snapshot read: once the edit has
	// landed old_source no longer matches, so an un-replayed retry would be
	// refused rather than answered with the original result.
	fingerprint := mutationFingerprint("edit_symbol", id, absPath, oldSource, newSource)
	if replay, conflict, matched := s.replayMutation(mutationID, fingerprint); matched {
		if conflict != "" {
			return mcp.NewToolResultError(conflict), nil
		}
		return s.respondJSONOrTOON(ctx, req, replay)
	}

	// Read the entire file ONCE — both the drift check and the
	// patch operate on the same byte snapshot so a concurrent
	// writer cannot wedge a diff between the SHA we accept and the
	// content we splice into.
	content, err := os.ReadFile(absPath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not read file: %v", err)), nil
	}
	if baseSHA != "" && gitBlobSHA(content) != baseSHA {
		return mcp.NewToolResultError(errBaseSHADrift), nil
	}
	// Before anything is written, so a withheld receipt never leaves a
	// half-applied edit behind.
	if evidenceRequested {
		if err := s.guardMutationEvidenceIntent(s.detectLanguageForPath(ctx, absPath, node.FilePath), node.FilePath, content); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	fileStr := string(content)

	// Extract the symbol's source lines to verify old_source is within them.
	lines := strings.Split(fileStr, "\n")
	if node.StartLine > len(lines) || node.EndLine > len(lines) {
		return mcp.NewToolResultError("symbol line range exceeds file length"), nil
	}

	symbolSource := strings.Join(lines[node.StartLine-1:node.EndLine], "\n")

	if findEOLMatches(symbolSource, oldSource).count == 0 {
		// Expand search to include preceding doc comments (agents often include
		// them because get_symbol_source returns context_lines above the symbol).
		expandedStart := node.StartLine - 1
		for expandedStart > 0 {
			trimmed := strings.TrimSpace(lines[expandedStart-1])
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") ||
				strings.HasPrefix(trimmed, "*") || trimmed == "" {
				expandedStart--
			} else {
				break
			}
		}
		if expandedStart < node.StartLine-1 {
			symbolSource = strings.Join(lines[expandedStart:node.EndLine], "\n")
		}
	}

	srcMatches := findEOLMatches(symbolSource, oldSource)
	if srcMatches.count == 0 {
		return mcp.NewToolResultError(fmt.Sprintf(
			"old_source not found within symbol %s (lines %d-%d). Use get_symbol_source to see the current code.",
			id, node.StartLine, node.EndLine)), nil
	}

	// Verify old_source is unique within the symbol.
	if srcMatches.count > 1 {
		return mcp.NewToolResultError(
			"old_source appears multiple times within the symbol. Provide a larger fragment to make it unique."), nil
	}

	// Apply the edit to the full file content.
	// Find old_source within the symbol's line range only (not the whole file).
	// Use the expanded start if doc comments were included.
	effectiveStart := node.StartLine
	if findEOLMatches(strings.Join(lines[node.StartLine-1:node.EndLine], "\n"), oldSource).count == 0 {
		// Recalculate expanded start for offset computation.
		expandedStart := node.StartLine - 1
		for expandedStart > 0 {
			trimmed := strings.TrimSpace(lines[expandedStart-1])
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") ||
				strings.HasPrefix(trimmed, "*") || trimmed == "" {
				expandedStart--
			} else {
				break
			}
		}
		effectiveStart = expandedStart + 1
	}

	symbolStart := 0
	for i := 0; i < effectiveStart-1 && i < len(lines); i++ {
		symbolStart += len(lines[i]) + 1 // +1 for newline
	}

	symbolEnd := symbolStart + len(symbolSource)
	if symbolEnd > len(fileStr) {
		symbolEnd = len(fileStr)
	}

	// Find old_source within the symbol region (EOL-tolerant: an
	// LF-authored fragment still matches a CRLF region). Spans are byte
	// offsets into the raw region, so the splice below always lands on the
	// real on-disk bytes.
	regionMatches := findEOLMatches(fileStr[symbolStart:symbolEnd], oldSource)
	if len(regionMatches.spans) == 0 {
		return mcp.NewToolResultError("old_source not found in symbol region"), nil
	}
	span := regionMatches.spans[0]

	// Build the new file content.
	editStart := symbolStart + span.start
	editEnd := symbolStart + span.end
	effectiveNew := newSource
	if regionMatches.normalized {
		// Rewrite new_source's terminators to the matched region's own
		// style so the splice never introduces mixed line endings.
		effectiveNew = adaptToDominantEOL(newSource, fileStr[editStart:editEnd])
	}
	newContent := fileStr[:editStart] + effectiveNew + fileStr[editEnd:]
	if newContent == fileStr {
		return mcp.NewToolResultError(
			"old_source and new_source are identical after line-ending normalization"), nil
	}
	newContentBytes := []byte(newContent)

	if dryRun {
		// Validate everything but skip the write; return a unified-diff
		// preview so the caller can review the change before committing.
		preview := map[string]any{
			"file":         node.FilePath,
			"symbol":       id,
			"lines_before": strings.Count(oldSource, "\n") + 1,
			"lines_after":  strings.Count(newSource, "\n") + 1,
			"start_line":   node.StartLine,
			"status":       "would_apply",
			"dry_run":      true,
			"diff":         unifiedDiff(node.FilePath, fileStr, newContent),
			"new_sha":      gitBlobSHA(newContentBytes),
		}
		if regionMatches.normalized {
			preview["eol_normalized"] = true
		}
		return s.respondJSONOrTOON(ctx, req, preview)
	}

	// Write the file atomically while holding the path mutation lock.
	perm := os.FileMode(0o644)
	if info, statErr := os.Stat(absPath); statErr == nil {
		perm = info.Mode().Perm()
	}
	commit, writeErr := s.commitFileMutation(ctx, "edit_symbol", mutationID, fingerprint, node.FilePath, absPath, newContentBytes, perm)
	if writeErr != nil {
		if errors.Is(writeErr, errMutationNotApplied) {
			return mcp.NewToolResultError(mutationNotAppliedMessage("edit", commit, writeErr)), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("could not write file: %v", writeErr)), nil
	}
	sess := s.sessionFor(ctx)
	sess.recordModified(node.FilePath)
	sess.recordSymbol(id)

	reindexOutcome := s.mutationReindexState(ctx, absPath)
	commit.recordGraph(reindexOutcome)

	// Count lines changed.
	oldLines := strings.Count(oldSource, "\n") + 1
	newLines := strings.Count(newSource, "\n") + 1

	resp := map[string]any{
		"file":         node.FilePath,
		"symbol":       id,
		"lines_before": oldLines,
		"lines_after":  newLines,
		"start_line":   node.StartLine,
		"status":       "applied",
		"new_sha":      gitBlobSHA(newContentBytes),
	}
	if regionMatches.normalized {
		resp["eol_normalized"] = true
	}
	if reindexOutcome.Err != nil {
		resp["reindex_error"] = reindexOutcome.Err.Error()
	}
	s.attachMutationFreshness(ctx, resp, node.FilePath, absPath, reindexOutcome)
	if evidenceRequested {
		s.attachMutationPhysicalEvidence(ctx, resp, absPath, content, true)
	}
	attachMutationCommit(resp, commit)
	// Retained last so a replayed retry returns the complete original payload,
	// physical evidence included.
	commit.retainResponse(resp)
	return s.respondJSONOrTOON(ctx, req, resp)
}

// readSingleLineAt reads a single line from an absolute filesystem path.
// Returns "" on error. Caller is responsible for resolving relative graph
// paths to abs first (via Server.resolveGraphPath / resolveNodePath) so a
// missing repo root surfaces as an error instead of silently opening the
// wrong file relative to the daemon process CWD.
func readSingleLineAt(absPath string, lineNum int) string {
	if absPath == "" {
		return ""
	}
	f, err := os.Open(absPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	current := 0
	for scanner.Scan() {
		current++
		if current == lineNum {
			return scanner.Text()
		}
	}
	return ""
}

// extractPrefix returns the common prefix of a camelCase/PascalCase name.
// e.g. "handleGetSymbol" -> "handle", "TestNewServer" -> "Test"
func extractPrefix(name string) string {
	for i := 1; i < len(name); i++ {
		if name[i] >= 'A' && name[i] <= 'Z' {
			return name[:i]
		}
	}
	return name
}
