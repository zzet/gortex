# Fork notes — why this fork exists and how to run it

Fork of `zzet/gortex` (`origin` = `MuiGoku123432/gortex`, `upstream` = `zzet/gortex`).

**Purpose: mainframe support.** Gortex ships COBOL and JCL extractors
(`internal/parser/languages/cobol.go`, `jcl.go`) that are structurally sound but
incomplete for a real IDMS/CICS estate. This fork closes those gaps.

The consumer, the corpus and the measured baseline live in
**`~/repos/mine/PythonApps/cobol-kg/`** — read its `PLAN.md` and `BASELINE.md`
before changing anything under `internal/parser/languages/`.

---

## Running this fork WITHOUT disturbing the official install

The official Homebrew `gortex` keeps tracking every other repo. Both daemons run
at once, on separate trees, verified 2026-08-27.

    export XDG_DATA_HOME=$HOME/.gortex-fork/data
    export XDG_CACHE_HOME=$HOME/.gortex-fork/cache
    export XDG_CONFIG_HOME=$HOME/.gortex-fork/config
    ./bin/gortex daemon start --detach

`internal/platform/xdg.go` resolves every per-user path through
`unifiedDir(envVar, homeSub)`: an **absolute** `XDG_*_HOME` wins and the category
lands at `<XDG_*_HOME>/gortex`; otherwise everything collapses into `~/.gortex`.
So those three variables relocate the store (`DataDir()/store`), the socket and
logs (`CacheDir()`), config, memories, the sidecar DB and telemetry
(`DataDir()/telemetry`) in one move. **A relative path is silently ignored** —
the XDG spec mandates it — so they must be absolute.

`GORTEX_DAEMON_SOCKET` overrides just the socket if that is all you need.

### Verified, not assumed

Three fork-only commands (`daemon status`, `repos`, a `call get_file_summary`)
against a tracked 5,169-file corpus left `~/.gortex` **byte-identical** at depth
2. The fork's own store, memories and sidecar appeared under
`~/.gortex-fork/data/gortex/`. The official daemon (pid 3193, v0.60.0, 8d
uptime) stayed `ready` throughout.

One trap when re-testing: invoking the **official** `gortex` CLI touches
`~/.gortex/telemetry`, so a mixed test shows a change that is not the fork's.
Keep the two binaries out of each other's test windows.

## Do not put `bin/gortex` on PATH

It is 318 MB and three minor versions ahead of the Homebrew install. Invoke it
as `./bin/gortex` so there is never a question about which binary answered.

---

## Staying current with upstream

`cobol.go` and `jcl.go` were **byte-identical across the 1,304 upstream commits
from v0.61.2 to v0.63.8** — nobody else is working on them, so rebases are cheap.
`extractor_plugin.go` is the exception: it grew 264 → 656 lines and gained
`ExtractBounded(ctx, path, src, parser.ExtractionLimits)`.

    git fetch upstream --tags
    git rebase upstream/main        # local commits are docs-only; expect no conflict
    go build ./... && go test ./internal/parser/...

**Rebase, don't merge**, so the two upstream-bound fixes below stay clean PRs.
`git rev-list --left-right --count main...upstream/main` is only as truthful as
your last fetch — it read "0 behind" while 1,304 commits behind.

## The work, in order (from `cobol-kg/BASELINE.md`)

| | change | upstream? |
|---|---|---|
| 0 | `cobolDivRe`: `DIVISION\.` → `DIVISION\b` | **yes** |
| 1 | `COPY IDMS` multi-word form, `COPY REPLACING`, dynamic `CALL` | **yes** |
| 2 | DATA DIVISION extractor — levels, PIC, REDEFINES, OCCURS | fork-local |
| 3 | `$CBAP` macro preprocessor (56 macros) | fork-local, site-specific |
| 4 | JCL `SET` / `INCLUDE` / `JCLLIB ORDER` symbolic resolution | **yes** |
| 5 | `.ctl` / `.bms` / `.ezt` extractors | later |

**Item 0 is a one-character fix affecting 441 of 506 DCC programs.** It survived
this long because all three fixtures in `cobol_test.go` use the bare
`PROCEDURE DIVISION.`; real code writes `PROCEDURE DIVISION USING LK-PARM.`,
which the regex misses, so `inProcedure` never flips and the whole procedure
division is skipped — no paragraphs, no PERFORM, no GO TO. Add a `USING` fixture
with the fix.

## Extension collisions decide fork-vs-plugin

`RegisterExtractorPlugins` **silently skips** a plugin whose extension a built-in
already claims. `.cbl` `.cpy` `.jcl` `.asm` `.txt` are claimed, so items 0-4 can
only be done here. `.ctl` `.prc` `.bms` `.ezt` `.dat` are free, so item 5 could be
Python subprocess plugins — but `.prc` is JCL syntax and should just be mapped to
`.jcl` in the corpus builder instead of getting its own extractor.
