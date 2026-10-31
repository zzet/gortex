# Upgrading to v0.64.12

## Recovering from an unwanted warmup

To stop a runaway warmup: `gortex daemon stop`, then offline `gortex untrack`:

```sh
gortex daemon stop
gortex untrack /absolute/path/to/unwanted-repository
gortex daemon start
```

When an installed service manager owns the daemon, stop uses that manager.
The stop intent prevents an editor's MCP proxy from immediately restarting it.
Use an absolute repository path while offline, rather than a bare prefix, and
confirm that untrack succeeds with `(config only)` before starting again.
The next start purges that repository's orphaned rows instead of warming it.
Check `gortex daemon status`: the removed repository should be absent and the
remaining repositories should become ready.

See the [onboarding troubleshooting guide](onboarding.md#recovering-from-a-runaway-warmup).

## Contract analysis in linked worktrees waits for idle time

Contract analysis of linked git worktrees now runs only while the daemon is
idle: a worktree's job starts once the daemon has seen no tool call and no edit
for 5 s, and a fill that is running when work comes back stops at its next
checkpoint and resumes later. The primary checkout is not affected, and a
request that waits for fresh contract results still starts its worktree's
analysis at once.

While a worktree's analysis waits, an ordinary read there answers from the
worktree's last published contract analysis, with `contract_analysis.state`
`building` and reason `contract_analysis_pending`, or reports that no
attachment exists yet. It does not block.

To restore the earlier behavior, set the policy to `eager` in the daemon's own
config file (not a repository's `.gortex.yaml`):

```yaml
contract_analysis:
  linked_policy: eager
```

or start the daemon with `GORTEX_CONTRACT_LINKED_POLICY=eager`. `throttled`
runs one linked job at a time instead. See
[Contract analysis in linked worktrees](multi-repo.md#contract-analysis-in-linked-worktrees).
