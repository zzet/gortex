# CI test selection

Pull requests normally run every test in each affected Go package, including
transitive reverse dependencies through ordinary imports, internal test imports,
and external test imports. A test-only edit still selects its complete package
and reverse dependents. This is a conservative package trial, not test-name
selection. Pushes to `main` and manual `workflow_dispatch` runs use the full
cross-platform suite. Manual runs also retain the extended parser stress gate.

## Force the full suite

- Add the **`ci:full`** label to a PR. Adding or removing it starts a new CI run;
  the usual opened, synchronize and reopened events remain enabled.
- Set the repository Actions variable **`CI_FULL_TESTS`** to **`true`** to roll
  all PRs back to full testing. The default when absent is selective PR testing.
  Remove it or set it to `false` to resume the trial.

These overrides take precedence over package selection. Dependabot PRs always
run the full suite, using the PR author's `dependabot[bot]` identity rather than
who triggers a rerun. Dependency changes also independently trigger full mode:
`go.mod`, `go.sum`, `go.work`, `go.work.sum`, nested module manifests, dependency
configuration, and workflow/action version bumps are non-Go inputs.

Changes to the variable affect subsequent planning. **Rerun the entire workflow**
to regenerate an existing run's plan; rerunning only failed jobs reuses the old
plan. A selective job that detects a changed runner context fails closed; use a
full override and rerun the entire workflow while the target policy is updated.
No repository protection or security rules are changed by this trial.

## Conservative boundaries

The selector optimizes ordinary added or modified `.go` inputs whose owning
package appears in supported build metadata. Added or modified Markdown under
`docs/` may be omitted from the Go closure only after qualification: canonical
paths, ordinary tracked file modes, regular UTF-8 text without symlink ancestors,
and no embedded, containing-package or nested-module input ownership. Unknown
ownership remains full. A mixed Go/docs change keeps the same reverse dependency
closure as the Go changes alone. Verified docs-only changes produce a sealed,
explicit empty test selection; builds, lint, smoke and security remain enabled.

It requests the full suite for:

- Deleted/renamed/copied files, including both old and new paths. It does not
  infer old dependencies from a head-only graph.
- Other non-Go files, including unqualified documentation, fixtures, embeds, C/C++ inputs,
  generators, CI scripts and configuration. This intentionally makes changes to
  the selector itself run the existing full cross-platform suite. Go files under
  `scripts/`, `.github/`, and `internal/citest/` also always request full mode.
- Changed cgo/generated/embed/generate inputs, unknown ownership, nested module
  inputs outside the root package census, or unsupported build constraints.
- Missing comparison revisions, missing Git objects, failed/incomplete Go
  metadata, unsupported Go flags/workspaces/experiments, or differing normal and
  race-tagged package censuses.

`scripts/ci-select` (using the shared `internal/citest` package) selects changes
relative to the first parent of the verified PR merge checkout. It verifies the
`refs/pull/<number>/merge` event identity, the exact checkout revision, two merge
parents, the second parent's match to the event's PR head, and ancestry from the
event's base to the tested first parent. An older event base therefore does not
include unrelated changes subsequently merged to `main`. Missing, malformed or
inconsistent event/merge metadata requests the full suite.

Dependency discovery and manifest validation stay on the actual tested merge:
new imports or test dependents introduced on `main` still participate in
selection. The manifest records that tested merge as its head and the verified
first parent as its comparison base. NUL-delimited Git status records preserve
unusual path names and rename pairs. It obtains `go list -json` metadata without `-test` for
Linux amd64, macOS arm64 and Windows amd64, all with cgo enabled. Normal and race
metadata are conservatively unioned. Traversal includes dependency nodes reached
through test imports, including local replacement modules that connect two root
packages. Discovery uses `-mod=readonly` and disposable copies of the module and
checksum files, preserving relative replacements without modifying module inputs.
It does not compile the dependency grammars for discovery.

The planner publishes its full/affected reason, selected packages and job counts
in the job summary and uploads the manifest. Native jobs verify manifest
integrity/completeness, exact checkout revision, toolchain, native
GOOS/GOARCH/CGO context and package census before filtering. Planning uncertainty
produces an explicit full plan; an unexpected planner failure blocks CI. A
legitimate zero-package plan is explicit and still gets builds, lint and runtime
smoke checks. It cannot substitute for a failed plan.

## What stays enabled

The dynamic matrix omits test partitions with no selected packages. Selected
packages retain all their tests, examples and fuzz seeds, the existing census,
batching, race/panic/crash handling, rerun rules and failure logs. Linux retains
race detection and coverage. macOS/Windows retain native execution; whenever the
indexer is selected, the macOS rest job still runs its watcher tests under race
detection even when there are no other rest packages.

A small Linux race smoke uses the existing production wiring tests
`TestDaemon_EndToEnd_ToolHandlerCanReturnItsDispatchSlot` and
`TestComposeWiresTheLeaseManagerIntoTheRESTSurface` to supplement imports with
runtime handler/transport coverage. All-OS default builds remain enabled (Linux
in the existing `build-onnx` job), along with static/tagged builds, complete lint,
module tidiness, CGO flag checks, benchmarks and the separate security workflows.

Only full-suite Linux coverage goes to Codecov. Affected-run coverage is kept in
explicitly named `affected-coverage-*` artifacts and is never presented as
whole-repository coverage. The stable **CI result** job checks the planner,
planned test execution, smoke and unchanged build/lint gates, and rejects failed,
cancelled, or unexpectedly skipped work. It does not replace separate security
checks or change administrative rulesets.

## Validation and expected scope

The gate and its workflow-policy tests live in the standalone Go tool
`scripts/ci-result` (`go test ./scripts/ci-result`). Tests exercise the actual CLI
input/output path, valid and rejected receipts, malformed input, and workflow
wiring with LF/CRLF checkouts. The CI test-selection/partitioning path requires
Go and Git; unrelated repository Python tools are unchanged.

The policy, selector and runner are Go code in `internal/citest`, with small
commands under `scripts/ci-select` and `scripts/ci-test`. Their Go tests create
isolated Git/Go modules and exercise reverse imports,
test-only/race/platform imports, local replacement bridges, additions,
deletes/renames, resource and metadata fallbacks, explicit overrides, and stale
or malformed manifests. A fixture also runs the real partition runner in full
and affected modes and compares the executed tests. Additional fixtures verify
first-failure evidence, retry scope, cancellation and owned-child cleanup.
Run these tests with `go test ./internal/citest ./scripts/ci-result`.

Representative package/job selections on the implementation tree (203 root Go
packages per target) are shown below. These counts describe planned work, not
measured speedups; setup and compilation costs remain.

| Changed input | Packages per OS | Test jobs / full 12 |
| --- | ---: | ---: |
| `cmd/gortex/affected_test.go` | 1 | 3 |
| `internal/mcp/outline_retention_test.go` | 10 | 6 |
| `internal/lint/lint.go` | 11 | 6 |
| `internal/graph/store_sqlite/wal_shrink.go` | 96 | 12 |
| CI, dependency, or other non-Go input | full | 12 |

A broad change may still need every partition while reducing the packages in
rest. If misses are observed, use `CI_FULL_TESTS=true` and retain the selection
reports to guide a later revision.
