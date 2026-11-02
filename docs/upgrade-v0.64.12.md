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

## Tracked repositories survive inventory failures

Family inventory failures no longer expire an explicitly tracked repository out
of configuration. Temporary outages retain its identity and recover its watcher
and offline edits when the checkout returns. Cleanup authorization is stored as
`Cause` in the existing journal; this safety change does not add a schema step.

An older build ignores `Cause` when reading that journal and does not enforce
this cleanup policy. Finish pending cleanup with the newer build before a
downgrade. See [tracking and temporary outages](multi-repo.md#tracking-and-temporary-outages).

## The store schema upgrade

The first daemon start after upgrading from v0.64.10 (store schema v21) or
v0.64.11 (v40) upgrades the graph store in place before the daemon opens its
socket. While it runs, `gortex daemon start` shows `migrating schema vN
(name)` and the daemon answers nothing — `tools/list` included — until the
upgrade has finished. Nothing is reindexed.

How long it takes grows with the store. Measured locally on the 1.5 GB store
v0.64.10 writes for this repository (190k nodes, 1.16M edges), the upgrade
from v21 took about 13 seconds, grew a 0.6 GB write-ahead log and added 66 MB
to the store; from v0.64.11's v40 it took under 3 seconds. Taking the backup
(below) adds the time to copy the store once. Numbers for large
multi-repository stores have not been measured yet.

### Free disk space

Before the first step, the daemon estimates how much disk the upgrade needs
(the write-ahead log it grows plus the main file's growth, plus 64 MiB) and
compares it with the free space on the store's volume. When there is not
enough, it refuses before touching the store and exits, and
`gortex daemon start` prints:

```text
daemon exited during startup: not enough free disk space to upgrade the
sqlite store schema: upgrading <store> from v21 to v<N> needs <bytes> bytes
(… MiB) free, <bytes> bytes (… MiB) available; the store was not modified —
free disk space on that volume or move the store to a larger one, then start
again (--backend-path starts the daemon on another volume) (…)
```

For the 1.5 GB store above the check asks for about 1.6 GB of free space.

Free the space and start again, or move the stopped store (`store.sqlite`
plus any `store.sqlite-wal`) to a larger volume and start the daemon with
`--backend-path <new path>`. A service manager that restarts the daemon in a
loop keeps hitting the same refusal until space is freed.

### The pre-upgrade backup

When the volume has room for the upgrade and a copy of the store, the daemon
first copies the store to `store.sqlite.v<old version>.backup` next to it
(`~/.gortex/store/store.sqlite.v21.backup` for a default v0.64.10 install).
The backup is one self-contained SQLite file at the old schema version. When
there is room for the upgrade but not for the copy, the upgrade runs without
a backup and the daemon log says why (`upgrading the store schema without a
backup`). A backup that already exists for that version is kept, so a restart
after an interrupted upgrade never replaces it.

The daemon deletes backups older than 14 days when it starts. To remove them
sooner:

```sh
gortex store cleanup-backup            # or: --backend-path <store>
```

### Rolling back to v0.64.10

v0.64.10 cannot read an upgraded store: started on one, it deletes the store
and reindexes every tracked repository from source. It leaves the backup file
alone. (v0.64.11 refuses an upgraded store instead, with `stored v<N>,
supported v40`.) To go back to the pre-upgrade store without that reindex,
restore the backup before starting the old release:

```sh
gortex daemon stop
cd ~/.gortex/store            # or the directory of your --backend-path
rm -f store.sqlite store.sqlite-wal store.sqlite-shm
mv store.sqlite.v21.backup store.sqlite
# install v0.64.10 again, then:
gortex daemon start
```

Without a backup, the way back is that full reindex.

### An interrupted upgrade

A daemon killed or crashing during the upgrade leaves the store at its old
version; the next start runs the upgrade again from the beginning and
completes it. The schema version is written only after every step and index
rebuild has finished.
