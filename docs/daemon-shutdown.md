# Daemon shutdown

## Final process cleanup

The foreground daemon keeps an exit watchdog through Cobra and terminal cleanup.
It starts only after both the actual shared-stack close and the actual owned
runtime/descriptor cleanup succeed. Contract-drain diagnostic warnings alone do
not negate a successful shared close; actual close errors prevent the watchdog
from arming. Producer joins still precede backend close.

If outer cleanup remains stuck, a best-effort goroutine snapshot starts after
two seconds and an independent exit deadline fires after three seconds. The
deadline does not wait for logging, filesystem sync or stderr. Its diagnostic
log handle is opened before the graph and retained until process exit, so it
survives the command logger's close; rotation may rename that pinned log inode.
A missing completion marker means the dump is incomplete. Normal completion
disarms without joining a potentially blocked diagnostic writer. These bounds
start after successful store/runtime cleanup; they do not bound earlier owner
joins, SQLite BEGIN, filesystem sync or legacy algorithm work.

## SQLite checkpoint policy

Closing the store skips its final WAL `TRUNCATE` checkpoint by default. Committed WAL frames remain available for recovery on the next open. Every graph-store connection disables SQLite's implicit last-connection checkpoint, so closing the connection cannot secretly repeat that copy.

The close still joins its store owners, seals deferred bulk indexes, restores and releases the pinned bulk connection, and closes both pools. This policy does not change normal passive checkpoints, WAL reclaim, an explicit `CheckpointWAL`, or explicit bulk-finalization operations. It also does not change SQLite's existing `NORMAL`/bulk `OFF` synchronous settings or promise protection beyond those settings against power loss.

Set these values in the **daemon's environment when starting it**. Setting them only on a later `gortex daemon stop` command cannot change the running daemon's policy.

| Environment | Final checkpoint |
| --- | --- |
| Neither setting present | Skip |
| `GORTEX_SQLITE_CLOSE_CHECKPOINT=full` | Full explicit drain |
| `GORTEX_SQLITE_CLOSE_CHECKPOINT=skip` | Skip, including when a legacy cap is present |
| Only `GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S=N`, `N > 0` | Full drain with the existing cap of `min(N, estimate)` |
| Only `GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S=0` | Full drain without an imposed deadline, preserving this explicit legacy setting |

Positive legacy caps retain their 24-hour maximum. A `full` policy also honors a valid legacy cap. Negative or malformed legacy values produce a warning and do not enable a drain. An invalid policy produces a warning and falls back to a valid legacy opt-in, or to skip.

For example, an operator who wants the historical full drain can start the daemon with:

```sh
GORTEX_SQLITE_CLOSE_CHECKPOINT=full gortex daemon start --detach
```

The policy is captured once for each owning close. The storage status includes `close_checkpoint_policy` (`skip` or `full`); absence identifies an older or unknown backend. A skipped checkpoint has a zero close-checkpoint estimate while WAL byte and pending-frame telemetry still describe the real backlog. Skip does not create a completed checkpoint-progress report or teach a synthetic drain rate.

A positive deadline can interrupt SQLite admission or SQL work; it cannot guarantee interruption of a filesystem sync already in progress. These settings alone do not establish an overall daemon-stop latency bound. Native stop, immediate-reopen, and warm graph-call acceptance remain separate measurements.

## Stop ownership and older daemons

`GORTEX_STOP_DUMP=1 gortex daemon stop` opts into **diagnostic termination** on
Unix only, when the ordinary shutdown/TERM grace and any advancing-close wait
have been exhausted. The CLI rechecks the captured incarnation, runtime and
existing store lock, sends SIGQUIT, then permits one second for completion.
Go's default SIGQUIT handler writes a goroutine dump to the daemon's stderr and
terminates the process; this is neither a nonfatal snapshot nor a promise that
the dump completed. If the daemon still owns its store, the CLI repeats all
ownership/store-free checks before the existing KILL fallback. Output records
QUIT-only versus QUIT followed by KILL; signal errors remain errors. A free,
unknown or changed target is never authorized by an earlier poll. Windows
reports SIGQUIT unavailable and retains its existing force-stop path without a
substitute diagnostic signal. Only the exact value `1` opts in; default stop
behavior is unchanged.

The CLI captures the daemon's declared store, existing lock identity, runtime files and native process incarnation before requesting shutdown. Success requires the captured lock to be free and the old runtime files to be gone. A still-visible process may be exiting or awaiting parent reaping; a free store is not a reason to kill it. Missing or replaced lock files remain unknown, and failed signals or cleanup are returned as errors.

New runtime records include an optional native process-birth token. Linux tokens include the boot ID because process start ticks are boot-relative. A present mismatched token is refused. Older JSON remains readable, and normal control-socket shutdown remains available. An older daemon's successful matching PID probe can authorize a fallback signal after endpoint and native-birth rechecks. Without a matching token or successful matching probe, the CLI refuses a PID-only TERM/KILL fallback with an unknown-owner error: a stale PID may already belong to an unrelated process. This deliberately limits forced shutdown of unreachable older daemons; it does not silently claim a successful stop. An already-reaped recorded daemon can be cleaned only with positive native absence and the captured runtime/store proofs, without any signal or guessed default store.
