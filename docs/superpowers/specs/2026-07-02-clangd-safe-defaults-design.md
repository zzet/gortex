# Clangd Safe Defaults Design

## Context

Issue https://github.com/zzet/gortex/issues/220 reports high CPU during semantic enrichment when Gortex auto-registers `clangd` and launches it with `--background-index --header-insertion=never`. On a large C/C++ repository with a broad `.clang-tidy`, clangd can run expensive clang-tidy matchers, crash, reconnect, and repeat substantial enrichment work.

The immediate user-facing problem is that Gortex's default clangd launch does more work than semantic graph enrichment needs. Crash-loop policy is separate follow-up work tracked in https://github.com/zzet/gortex/issues/222.

## Scope

Change only the built-in `clangd` LSP default arguments used by semantic enrichment.

Do not add a new config option. Do not change reconnect behavior. Do not alter `.gortex.yaml` provider override semantics.

## Proposed Default

The built-in `clangd` spec should launch with:

```text
--background-index=false
--clang-tidy=false
--header-insertion=never
-j=1
```

`--clang-tidy=false` prevents repo `.clang-tidy` checks from running during enrichment. `--background-index=false` prevents clangd from building and maintaining a project-wide background index when Gortex only needs request-driven semantic evidence. `-j=1` limits clangd's internal worker concurrency for the enrichment subprocess.

## Architecture

Keep the existing registry/config architecture:

- `internal/semantic/lsp/registry.go` remains the source of built-in LSP server specs.
- `internal/serverstack/shared_server.go` continues to auto-register available LSP specs.
- `SpecWithOverrides` continues to let `.gortex.yaml` replace built-in command, args, and env values.

Users who want clang-tidy diagnostics or background indexing can opt back in with a repo-local provider override.

## Data Flow

Semantic enrichment will still discover C/C++ nodes, ask the LSP router for the `clangd` provider, and lazy-spawn clangd only when there is work to confirm or enrich.

The only behavior change is the argv used when clangd is spawned from the built-in spec. Foreground LSP requests for definition, references, implementations, and hover remain available.

## Error Handling

This design does not change reconnect or crash handling. If an LSP server exits, the existing reconnect path continues to behave as it does today.

Repeated-exit suppression is intentionally deferred to issue https://github.com/zzet/gortex/issues/222 so this fix stays narrow and easy to review.

## Testing

Use TDD for implementation:

1. Add a failing registry test that asserts the built-in `clangd` spec has exactly the safe default args.
2. Run the focused test and confirm it fails before the implementation change.
3. Change the `clangd` built-in args.
4. Re-run the focused test and relevant package tests.

The focused test belongs near the existing registry tests in `internal/semantic/lsp/registry_test.go`. It should verify the built-in spec, not a config override copy, so a future accidental reintroduction of `--background-index` or clang-tidy diagnostics fails quickly.

## Documentation

Update `docs/lsp.md` so the documented clangd command matches the new defaults. Include a short note that Gortex disables clang-tidy and background indexing by default for enrichment because it needs graph signal, not lint diagnostics or a persistent project index.

## Non-Goals

- No generic LSP crash-loop guard.
- No new semantic config key.
- No changes to C/C++ file selection.
- No changes to clangd availability detection.
- No changes to user-provided `semantic.providers` overrides.
