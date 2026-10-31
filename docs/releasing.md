# Release-candidate checks

## Offline-untrack escape hatch

After the shutdown dependency #1022 lands, run the native offline-untrack gate
on both the reference Mac and reference Linux host. Use the release's pinned
toolchain/build flags to produce the binary first, then run:

```sh
GORTEX_E2E=1 GORTEX_E2E_BINARY=/absolute/path/to/pinned/gortex \
  go test -v -run '^TestOfflineUntrackEscapeHatch$' -count=20 -timeout=45m ./cmd/gortex/
```

Save the verbose test output for each platform, including every cold-stop and
offline-untrack duration and normal child-exit result. Require
20 consecutive passes per platform, every active-cold stop ≤5 seconds, zero
SIGKILLs, offline absolute-path untrack ≤1 second, a released store lock, and
the remaining repository ready with zero removed-prefix rows after restart.
A skipped, timed-out, setup-failed or unexecuted gate is not a pass. These
native 20-run gates remain required before claiming #1029 acceptance.
Use the same recovery steps in the [v0.64.12 upgrade note](upgrade-v0.64.12.md).

## Native worktree edits

For every RC tag, run both native acceptance flows before claiming native
Windows worktree edits are supported. These are manual, explicit private-daemon
tests; a green ordinary Windows build or untagged CI run is not this gate.

```sh
RC_TAG=v0.64.12-rc.1 # use the actual candidate tag
gh workflow run bench-arm.yml --ref "$RC_TAG" -f suite=acceptance-native-windows
gh workflow run bench-arm.yml --ref "$RC_TAG" -f suite=acceptance-native
```

The Windows job builds the tagged native binary with the safe C/C++ flags and
runs `TestIssue767WorktreeReadinessIntegration` ten times. Each repetition runs
the same-drive case and attempts the cross-drive case using a fresh writable
directory on another fixed local drive. If none is available, the output must
contain the cross-drive skip and its reason; never report that variant as run.
The existing Ubuntu flow runs the extended test alongside its other drivers.

Review the always-uploaded `acceptance-native-windows-*` evidence, including
build metadata/hash, test output, private daemon/shutdown logs, edit responses
and before/preview/after source snapshots. Confirm:

- 10/10 same-drive repetitions stopped and restarted without a forced kill.
- Every after-restart dry run and real edit was admitted within 3 seconds of
  starting `gortex call edit`, without `view_read_only` or lane-busy refusals.
- Dry runs left bytes unchanged; the second real marker was on disk and
  searchable through the exact worktree view, absent from the primary.
- Cross-drive results or a reasoned skip are visible, and Ubuntu acceptance
  passed. Mutation failures were retained without automatic retry.

Post both workflow run links and an explicit outcome on [#1036](https://github.com/zzet/gortex/issues/1036)
for that RC: **did not reproduce**, **reproduced**, or **incomplete**. Build,
setup or censored failures are incomplete evidence, not a successful support
gate; keep their artifacts. Branch runs help validate the harness but do not
replace the run pinned to the actual RC tag.

If the smoke reproduces [#777](https://github.com/zzet/gortex/issues/777), the
milestone's release notes must say **"Native Windows edits are unsupported in
this release."** Open and link the Windows path-identity hardening follow-up,
assign it to the next patch milestone, and record the trigger on #1036. Do not
silently retry edits, relax the admission deadline, or label WSL2 results as
native Windows evidence.
