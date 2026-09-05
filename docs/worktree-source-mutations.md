# Source edits in selected worktrees

An exact, live automatic worktree supports `edit.file`, `edit.write`, and
`edit.symbol` (and their legacy tool equivalents). The session may already be
inside the worktree, or select it explicitly with `view.kind: worktree` and a
registered checkout ID or absolute root path. No explicit tracking or dedicated
graph is required.

The old mutation guard refused every routed source tool, including dry runs,
and suggested editing from the checkout's own working copy. That suggestion
could not work: an automatic checkout's own session also receives a routed view.
The replacement admits only the audited checkout-aware write paths.

## Safety and freshness

- Admission obtains the existing checkout coordinator's interactive build
  admission and reconciliation lock. It verifies that the checkout and the
  route epoch still match the view used to interpret the edit.
- File targets must belong to that checkout, including after resolving symlinks
  and existing parent directories. Absolute primary/sibling paths, nested Git
  repositories, and Git metadata are refused rather than redirected.
- Dry runs validate against the selected working copy without committing bytes,
  withdrawing its route, or building a generation.
- Immediately before a real write, the old dirty route becomes pending. Existing
  leased readers remain valid; new requests cannot mistake the old generation
  for the updated working copy.
- The existing single-file commit ledger still distinguishes disk commit from
  graph refresh. Refresh uses the selected checkout coordinator, never the
  primary repository's watcher or incremental indexer. A bounded refresh failure
  leaves a stale/failed graph outcome and schedules reconciliation; it must not
  report that a successful disk write did not happen.
- Coordinator shutdown joins admitted writes before checkout state can retire.

All inexact fallbacks and immutable ref/commit views remain read-only. A missing
coordinator refuses the operation before touching files. Save or discard active
session editor buffers before editing on-disk worktree content; buffer-derived
symbol locations are not authority to modify disk.

## Deliberately unsupported write paths

Batch editing/recovery, filesystem lifecycle operations, and LSP refactors have
separate write and refresh machinery. They remain refused through routed views
until their checkout ownership and recovery are integrated. The refusal states
this limitation rather than recommending a CWD change that cannot solve it.

Ordinary canonical-checkout editing is unchanged. This change requires no schema
migration, daemon restart procedure, new tracking intent, or additional database.

## Regression validation and benchmark scope

The regression fixtures create real temporary Git families, automatically
register/activate a linked checkout through the lifecycle, and invoke MCP
handlers and the registered facade. They cover own-CWD and explicit path/ID
selection, explicit and inferred operations, dry runs, disk isolation, and
subsequent exact graph/source reads. Separate tests cover stale epochs, dirty
state, cancellation, shutdown, panic cleanup, root replacement, and failed
publication after a successful disk commit.

On an Apple M1 Pro, three 10-iteration runs against a tiny fixture measured:

| Operation | Time per operation |
| --- | --- |
| Checkout admission and release (no build) | 10.06–10.21 ms |
| Complete MCP worktree dry run | 12.35–19.55 ms |

These figures measure the new admission path, not cold indexing or large-repo
scaling. Tests additionally assert that a dry run leaves the route epoch and
generation IDs unchanged. Reproduce with
`go test ./internal/indexer -run '^$' -bench '^BenchmarkCheckoutMutation' -benchmem`
and
`go test ./internal/mcp -run '^$' -bench '^BenchmarkWorktreeMutationDryRun$' -benchmem`.
