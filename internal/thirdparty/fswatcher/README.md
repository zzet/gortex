# Vendored: github.com/sgtdi/fswatcher

This directory is a vendored copy of
[`github.com/sgtdi/fswatcher`](https://github.com/sgtdi/fswatcher)
**v1.3.0**, licensed under MIT (see `LICENSE`). Gortex imports this package
through its in-tree path so the patched lifecycle is exercised by the normal
repository test suite.

## Why it is vendored

The upstream `EventAggregator` can accept an event after `close` has marked
the aggregator closed and snapshotted an empty event map. The run goroutine
then repeatedly observes the overdue event while `flushDue` refuses to remove
it because the aggregator is closed. The watcher appears stopped to its
caller, but the orphaned goroutine spins indefinitely.

As of upstream v1.3.0, no released version contains the required close/add
serialization and join semantics.

## Modifications by the Gortex project

- Serialize final event admission and close with the aggregator mutex.
- Recheck the closed state after acquiring that mutex.
- Join the run goroutine before `close` returns.
- Make concurrent `close` callers wait for the same completed shutdown.
- Add deterministic regression tests for the in-flight-add interleaving.
- Remove one unused logging helper and annotate cross-platform helpers so the
  vendored package satisfies Gortex's host-platform lint configuration.

All other source files are reproduced verbatim from v1.3.0.
