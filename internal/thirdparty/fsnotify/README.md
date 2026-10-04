# Vendored: github.com/fsnotify/fsnotify

This directory is a vendored copy of
[`github.com/fsnotify/fsnotify`](https://github.com/fsnotify/fsnotify)
**v1.10.1**, licensed under the BSD 3-Clause license (see `LICENSE`). Gortex
imports this package through its in-tree path so the patched Windows backend
is exercised by the normal repository test suite.

## Why it is vendored

On Windows, tearing down a watch (removing it, or deleting the watched
directory) can leave completion packets on the I/O completion port that still
point at the watch. Upstream v1.10.1 then lets the watch become unreachable
while the kernel still holds its `OVERLAPPED` and buffer, so the reader can
recover a pointer into freed memory and the kernel can write into it. The
Go runtime then aborts with `found pointer to free object` or
`found bad pointer in Go heap`. The same stale packets also re-arm the dead
watch and close its handle a second time
([fsnotify#768](https://github.com/fsnotify/fsnotify/issues/768)).

The git watcher removes watches on ref changes, so this crashed the Gortex
daemon and the Windows test suite.

As of upstream v1.10.1, no released version keeps torn-down watches alive.

## Modifications by the Gortex project

- `backend_windows.go`: every `ReadDirectoryChangesW` call gets its own op,
  with its own `OVERLAPPED` and buffer, kept in a Go-owned set until its
  completion packet is dequeued. Packets for torn-down watches are dropped.
  Packets for superseded reads are parsed but do not re-arm. Teardown runs
  once per watch, so its handle is closed once. `Close` waits for outstanding
  packets before closing the port. Submitted upstream as
  [fsnotify#782](https://github.com/fsnotify/fsnotify/pull/782).
- `backend_windows.go`: `sendError` puts `Close`'s handshake token back the way
  `sendEvent` does, from
  [fsnotify#769](https://github.com/fsnotify/fsnotify/pull/769).
- `backend_windows_test.go`: a Windows regression test that churns a directory
  while adding and removing its watch, and while deleting watched directories.
- Import paths rewritten to the in-tree location. Upstream tests, the `cmd/`
  tool and `internal/ztest` are not vendored.

All other source files are reproduced verbatim from v1.10.1.
