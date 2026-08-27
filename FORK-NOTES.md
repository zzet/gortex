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

### Two traps that cost real time on 2026-08-27

**`daemon stop` is not XDG-scoped when a service supervisor owns the daemon.**
It reported "stopped via service supervisor" and took down the *official*
Homebrew daemon, not the isolated one, despite the XDG variables being set.
Stop a specific daemon by pointing `GORTEX_DAEMON_SOCKET` at its socket, or
check `daemon status` afterwards — do not assume the env scoped it.

**This shell already exports `XDG_CONFIG_HOME=$HOME/.config`** (`.zshrc` line
23). So `gortex daemon start` from an interactive shell resolves
`ConfigDir()` to `~/.config/gortex`, which is empty, and the daemon comes up
with **zero tracked repos** while `~/.gortex/config.yaml` still holds all 47.
Nothing is lost, but it looks like total index loss. The launchd-started
daemon has no such variable, which is why it worked for eight days. Start the
official daemon with:

    env -u XDG_CONFIG_HOME gortex daemon start --detach

Since the wrapper sets all three variables explicitly, the fork is unaffected
either way.

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
| 0 | `cobolDivRe`: `DIVISION\.` → `DIVISION\b` — **DONE**, `11e79383` | **yes** |
| 1 | comment lines, `COPY IDMS` multi-word, dynamic `CALL` — **DONE**, `028ef49e` | **yes** |
| 2 | DATA DIVISION extractor — levels, PIC, REDEFINES, OCCURS | fork-local |
| 3 | `$CBAP` macro preprocessor (56 macros) | fork-local, site-specific |
| 4 | JCL `SET` / `INCLUDE` / `JCLLIB ORDER` symbolic resolution | **yes** |
| 5 | `.ctl` / `.bms` / `.ezt` extractors | later |

**Item 1 is done** (`028ef49e`). Scope changed on measurement: **`COPY REPLACING`
and `COPY x OF y` do not occur at all** in this estate, so neither was built.
What did occur:

* **comment lines were being scanned** — 2,242 of 3,768 `COPY` matches (59%) and
  117 of 469 `CALL` matches came from commented-out code and prose (`TO`,
  `FAILED`, `THIS`). Both regexes are unanchored and ran over raw source.
* `COPY IDMS [RECORD] <name>` — 590 statements collapsed onto one `IDMS` import.
* dynamic `CALL <identifier>` — **829 versus 352 quoted literals**, so most of
  the inter-program call graph was absent. Lands in a `dyncall` namespace with
  `OriginASTInferred`.

Edges 90,404 → 88,874, exactly `-2,242 -117 +829`. Three fixtures, each verified
to fail without its fix.

### Known, not yet fixed: anchored patterns under-match

The mirror of the comment bug. `PROGRAM-ID`, `DIVISION` and `SECTION` are
anchored `^\s*`, and run over raw source where columns 1-6 may hold a change
marker — `I0002  ENVIRONMENT DIVISION.` never matches. Stripping to the code
area *raises* the counts: **+223 divisions, +457 sections** on this corpus. It
also means some procedure divisions are still skipped for the item-0 reason via
a different route. Fixing it properly needs fixed-vs-free-format detection,
because `cobolStripLine` corrupts free-format source (it takes column 7 as the
indicator on any line of 7+ characters), so it is deliberately left out of the
item-1 change.

**Item 0 is done** (`11e79383`, branch `fix/cobol-procedure-division-using`).
Controlled measurement on the DCC corpus, same binary, regex the only change:
**nodes 14,780 → 35,670 (+141%), edges 23,449 → 90,404 (+286%)**. Two new
fixtures, both verified to fail without the fix. Ready to open upstream.

## Extension collisions decide fork-vs-plugin

`RegisterExtractorPlugins` **silently skips** a plugin whose extension a built-in
already claims. `.cbl` `.cpy` `.jcl` `.asm` `.txt` are claimed, so items 0-4 can
only be done here. `.ctl` `.prc` `.bms` `.ezt` `.dat` are free, so item 5 could be
Python subprocess plugins — but `.prc` is JCL syntax and should just be mapped to
`.jcl` in the corpus builder instead of getting its own extractor.
