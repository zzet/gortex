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
