package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// stubDirCoverage fixes the daemon's scope answer without a socket.
func stubDirCoverage(t *testing.T, result daemon.DirCoverageResult) {
	t.Helper()
	stubDirCoverageBy(t, func(string) (daemon.DirCoverageResult, bool) { return result, true })
}

// stubDirCoverageBy answers per absolute scope, so a test can give one
// directory a different verdict from another.
func stubDirCoverageBy(t *testing.T, fn func(scope string) (daemon.DirCoverageResult, bool)) {
	t.Helper()
	old := dirCoverageFn
	dirCoverageFn = func(path string, _ time.Duration) (daemon.DirCoverageResult, bool) {
		return fn(path)
	}
	t.Cleanup(func() { dirCoverageFn = old })
}

// --- read doors: Read, and Bash cat/head/tail ---

// Nothing for a symbol lookup to return, so the door stays quiet. There is no
// local path-component shortcut here: every path goes to the daemon.
func TestEnrichRead_VendoredNeverIndexable_Silent(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true})
	for _, path := range []string{
		"node_modules/dpack/lib/Block.js",
		"/home/u/proj/node_modules/dpack/lib/Block.js",
		"vendor/github.com/x/y/z.go",
	} {
		res := enrichRead(map[string]any{"file_path": path}, "/repo")
		if res.deny || res.context != "" {
			t.Errorf("vendored read %q must be silent, got deny=%v ctx=%q", path, res.deny, res.context)
		}
	}
}

// A repo can re-include a vendored tree (`!node_modules/`). The verdict comes
// from the daemon, not the path shape, so this denies despite looking vendored.
func TestEnrichRead_ReincludedVendored_Denies(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{Indexed: true, Count: 9, Tracked: true, ProbeOK: true})
	res := enrichRead(map[string]any{"file_path": "node_modules/dpack/lib/Block.js"}, "/repo")
	if !res.deny {
		t.Fatalf("re-included and indexed vendored read must deny: %#v", res)
	}
	if !strings.Contains(res.reason, "9 symbols indexed") {
		t.Fatalf("deny reason should carry the symbol count: %q", res.reason)
	}
}

// The non-vendored half: a bundle the repo's own exclude globs drop.
func TestEnrichRead_NeverIndexableByDaemon_Silent(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true})
	res := enrichRead(map[string]any{"file_path": "web/app.min.js"}, "/repo")
	if res.deny || res.context != "" {
		t.Fatalf("excluded-by-design read must be silent: deny=%v ctx=%q", res.deny, res.context)
	}
}

// The daemon looked and placed the path outside every tracked checkout, so no
// graph will ever hold it. ProbeOK is set because that IS the daemon's answer:
// an untracked path is the one abstention-shaped verdict it can give.
func TestEnrichRead_Untracked_Silent(t *testing.T) {
	withDaemonReachable(t, true)
	for _, st := range []fileIndexStatus{
		{ProbeOK: true}, // answered: nothing tracks it
		{},              // no answer, and nothing tracks it either
	} {
		stubFileIndexScope(t, st)
		res := enrichRead(map[string]any{"file_path": "/elsewhere/pkg/a.go"}, "/repo")
		if res.deny || res.context != "" {
			t.Fatalf("untracked read (%+v) must be silent: deny=%v ctx=%q", st, res.deny, res.context)
		}
	}
}

// A failed probe is not evidence. The daemon is up, so the tools the advisory
// names work; silence here turns one timed-out probe into a full bypass.
func TestEnrichRead_ProbeUnreachedButDaemonUp_Advises(t *testing.T) {
	withDaemonReachable(t, true)
	stubFileIndexScope(t, fileIndexStatus{Unreached: true})
	res := enrichRead(map[string]any{"file_path": "pkg/a.go"}, "/repo")
	if res.deny {
		t.Fatalf("a failed probe must never deny: %#v", res)
	}
	if res.context == "" {
		t.Fatal("a failed probe against a live daemon should keep the advisory, got silence")
	}
}

// The same holds for an answer that reached the hook but settled nothing: a
// tracked path whose serving view could not be read.
func TestEnrichRead_TrackedButNoVerdict_Advises(t *testing.T) {
	withDaemonReachable(t, true)
	stubFileIndexScope(t, fileIndexStatus{Tracked: true}) // ProbeOK false
	res := enrichRead(map[string]any{"file_path": "pkg/a.go"}, "/repo")
	if res.deny || res.context == "" {
		t.Fatalf("a tracked path with no verdict should keep the advisory: deny=%v ctx=%q", res.deny, res.context)
	}
}

// The one probe failure that warrants silence: the named tools are down too.
func TestEnrichRead_ProbeFailedDaemonDown_Silent(t *testing.T) {
	withDaemonReachable(t, false)
	stubFileIndexScope(t, fileIndexStatus{Unreached: true})
	res := enrichRead(map[string]any{"file_path": "pkg/a.go"}, "/repo")
	if res.deny || res.context != "" {
		t.Fatalf("read with the daemon down must be silent: deny=%v ctx=%q", res.deny, res.context)
	}
}

// The one state that earns an advisory outright.
func TestEnrichRead_IndexableNotYetIndexed_Advises(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{Tracked: true, ProbeOK: true})
	res := enrichRead(map[string]any{"file_path": "pkg/new.go"}, "/repo")
	if res.deny || res.context == "" {
		t.Fatalf("indexable-not-yet-indexed read should advise: deny=%v ctx=%q", res.deny, res.context)
	}
}

// The token-saving tier is untouched.
func TestEnrichRead_Indexed_Denies(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{Indexed: true, Count: 5, Tracked: true, ProbeOK: true})
	res := enrichRead(map[string]any{"file_path": "pkg/a.go"}, "/repo")
	if !res.deny {
		t.Fatalf("indexed read must deny: %#v", res)
	}
	if !strings.Contains(res.reason, "5 symbols indexed") {
		t.Fatalf("deny reason should carry the symbol count: %q", res.reason)
	}
}

// Nothing for a SYMBOL lookup to return, so no nudge toward one. The search
// doors reach the opposite verdict; see TestHookSearchScope_Symbolless*.
func TestEnrichRead_IndexedButSymbolless_Silent(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{Symbolless: true, Tracked: true, ProbeOK: true})
	res := enrichRead(map[string]any{"file_path": "pkg/doc.go"}, "/repo")
	if res.deny || res.context != "" {
		t.Fatalf("symbol-free indexed read must be silent: deny=%v ctx=%q", res.deny, res.context)
	}
}

// Bash is where an agent goes the moment Read denies, so it must match —
// silences included. `cat` of a vendored file used to still print the nudge.
func TestEnrichBash_ReadNeverIndexable_Silent(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true})
	for _, cmd := range []string{
		"cat node_modules/dpack/lib/Block.js",
		"head -50 vendor/github.com/x/y/z.go",
	} {
		res := enrichBash(map[string]any{"command": cmd}, "/repo")
		if res.deny || res.context != "" {
			t.Errorf("`%s` must be silent, got deny=%v ctx=%q", cmd, res.deny, res.context)
		}
	}
}

// Neither door may become the quiet one an agent learns to prefer.
func TestEnrichBash_ProbeFailureParityWithRead(t *testing.T) {
	for _, tc := range []struct {
		name        string
		st          fileIndexStatus
		reachable   bool
		wantContext bool
	}{
		{"untracked path", fileIndexStatus{}, true, false},
		{"probe unreached, daemon up", fileIndexStatus{Unreached: true}, true, true},
		{"probe unreached, daemon down", fileIndexStatus{Unreached: true}, false, false},
		{"tracked, no verdict, daemon up", fileIndexStatus{Tracked: true}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDaemonReachable(t, tc.reachable)
			stubFileIndexScope(t, tc.st)
			read := enrichRead(map[string]any{"file_path": "pkg/a.go"}, "/repo")
			bash := enrichBash(map[string]any{"command": "cat pkg/a.go"}, "/repo")
			if (read.context != "") != tc.wantContext {
				t.Errorf("Read advisory = %v, want %v (ctx=%q)", read.context != "", tc.wantContext, read.context)
			}
			if (bash.context != "") != tc.wantContext {
				t.Errorf("Bash advisory = %v, want %v (ctx=%q)", bash.context != "", tc.wantContext, bash.context)
			}
			if read.deny || bash.deny {
				t.Errorf("neither door may deny without an indexed verdict: read=%v bash=%v", read.deny, bash.deny)
			}
		})
	}
}

// The one Bash read state that still advises.
func TestEnrichBash_ReadIndexableNotYetIndexed_Advises(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{Tracked: true, ProbeOK: true})
	res := enrichBash(map[string]any{"command": "cat pkg/new.go"}, "/repo")
	if res.deny || res.context == "" {
		t.Fatalf("indexable-not-yet-indexed Bash read should advise: deny=%v ctx=%q", res.deny, res.context)
	}
}

// --- search doors: single-file scopes ---

// Mirrors the Read path.
func TestHookSearchScope_VendoredSingleFile_NonSource(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true})
	v := hookSearchScope("/repo", map[string]any{"path": "node_modules/foo/bar.js"})
	if v != searchScopeNonSource {
		t.Fatalf("vendored single-file scope should be NonSource, got %v", v)
	}
}

// Re-include parity with Read.
func TestHookSearchScope_ReincludedVendoredSingleFile_Indexed(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{Indexed: true, Count: 3, Tracked: true, ProbeOK: true})
	v := hookSearchScope("/repo", map[string]any{"path": "node_modules/foo/bar.js"})
	if v != searchScopeIndexed {
		t.Fatalf("re-included indexed single-file scope should be Indexed, got %v", v)
	}
}

// The non-vendored half.
func TestHookSearchScope_NeverIndexableSingleFile_NonSource(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true})
	v := hookSearchScope("/repo", map[string]any{"path": "web/app.min.js"})
	if v != searchScopeNonSource {
		t.Fatalf("unindexable single-file scope should be NonSource, got %v", v)
	}
}

// Keeps the historical posture rather than going silent: the file probe and the
// pattern probe run on different transports, so one failing is not proof.
func TestHookSearchScope_SingleFileProbeUnavailable_Unproven(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{Unreached: true})
	v := hookSearchScope("/repo", map[string]any{"path": "pkg/a.go"})
	if v != searchScopeUnproven {
		t.Fatalf("no-verdict single-file scope should stay Unproven, got %v", v)
	}
}

// A size- or gate-skipped file earns a synthetic node, so it comes back Held
// (hence Symbolless) AND unindexable at once. Its bytes were never read, so
// text search has nothing of it and the deny would point nowhere.
func TestHookSearchScope_SkippedFileIsNonSourceDespiteItsNode(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{
		Symbolless:     true,
		NeverIndexable: true,
		Tracked:        true,
		ProbeOK:        true,
	})
	if v := hookSearchScope("/repo", map[string]any{"path": "internal/generated/huge.go"}); v != searchScopeNonSource {
		t.Fatalf("a file the walk rejected is NonSource even though the graph names it, got %v", v)
	}
}

// Search and read doors deliberately disagree here: Read offers symbol lookups
// (nothing to return); Grep/Glob offer text/files/outline, which all have rows
// for any file the graph holds.
func TestHookSearchScope_SymbollessSingleFile_Indexed(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{Symbolless: true, Tracked: true, ProbeOK: true})
	if v := hookSearchScope("/repo", map[string]any{"path": "pkg/doc.go"}); v != searchScopeIndexed {
		t.Fatalf("symbol-free single-file scope should be Indexed for the search doors, got %v", v)
	}
}

// …and the deny must name a tool that actually has rows for the file.
func TestGrepAndGlob_SymbollessSingleFile_DenyNamesTextSearch(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{Symbolless: true, Tracked: true, ProbeOK: true})

	grep := enrichGrep(map[string]any{"pattern": "TODO", "path": "pkg/doc.go"}, 0, "/repo")
	if !grep.deny {
		t.Fatalf("Grep of an indexed symbol-free file should deny: %#v", grep)
	}
	if !strings.Contains(grep.reason, `search(operation:"text"`) {
		t.Errorf("the deny must redirect to text search, which has rows for the file: %q", grep.reason)
	}

	glob := enrichGlob(map[string]any{"pattern": "**/*.go", "path": "pkg/doc.go"}, "/repo")
	if !glob.deny {
		t.Fatalf("Glob scoped to an indexed symbol-free file should deny: %#v", glob)
	}
	if !strings.Contains(glob.reason, `search(operation:"files"`) {
		t.Errorf("the deny must redirect to file search, which has rows for the file: %q", glob.reason)
	}
}

// --- search doors: directory scopes ---

// Must not fall through to the pattern probe, which ignores `path` and would
// deny because some unrelated file elsewhere defines a matching symbol.
func TestHookSearchScope_ProvenEmptyDirectory_NonSource(t *testing.T) {
	stubScopeTracked(t, false, true)
	dir := t.TempDir()
	if v := hookSearchScope(dir, map[string]any{"path": "node_modules/dpack"}); v != searchScopeNonSource {
		t.Fatalf("proven-empty directory scope should be NonSource, got %v", v)
	}
}

// Grep and Glob must agree with Read on that directory.
func TestGrepAndGlob_ProvenEmptyDirectory_Silent(t *testing.T) {
	stubScopeTracked(t, false, true)
	dir := t.TempDir()
	if res := enrichGrep(map[string]any{"pattern": "Block", "path": "node_modules/dpack"}, 0, dir); res.deny || res.context != "" {
		t.Errorf("Grep in a proven-empty directory must be silent: deny=%v ctx=%q", res.deny, res.context)
	}
	if res := enrichGlob(map[string]any{"pattern": "**/*.js", "path": "node_modules/dpack"}, dir); res.deny || res.context != "" {
		t.Errorf("Glob in a proven-empty directory must be silent: deny=%v ctx=%q", res.deny, res.context)
	}
}

// Unprovable scope keeps the historical posture — the pattern probe still runs.
func TestHookSearchScope_UnprovableDirectory_Unproven(t *testing.T) {
	stubScopeTracked(t, false, false)
	dir := t.TempDir()
	if v := hookSearchScope(dir, map[string]any{"path": "node_modules/dpack"}); v != searchScopeUnproven {
		t.Fatalf("unprovable directory scope should stay Unproven, got %v", v)
	}
}

// A directory proven to hold source still denies.
func TestHookSearchScope_TrackedDirectory_Indexed(t *testing.T) {
	stubScopeTracked(t, true, true)
	dir := t.TempDir()
	if v := hookSearchScope(dir, map[string]any{"path": filepath.Base(dir)}); v != searchScopeIndexed {
		t.Fatalf("tracked directory scope should be Indexed, got %v", v)
	}
}

// --- what a scope answer may prove ---

// The graph holding nothing under a scope is not proof the scope holds no
// source: that is equally true of a tree excluded by design and of one the
// walk has not reached yet. Only a completed admission walk separates them.
func TestScopeTrackedFromCoverage(t *testing.T) {
	for _, tc := range []struct {
		name          string
		result        daemon.DirCoverageResult
		wantHasSource bool
		wantProbeOK   bool
	}{{
		name:          "scope holds indexed source",
		result:        daemon.DirCoverageResult{Answered: true, Tracked: true, HasSource: true},
		wantHasSource: true,
		wantProbeOK:   true,
	}, {
		name:        "graph empty and the walk claims nothing ⇒ proven source-free",
		result:      daemon.DirCoverageResult{Answered: true, Tracked: true, Walked: true},
		wantProbeOK: true,
	}, {
		name:   "graph empty but the walk claims a file ⇒ mid-walk, NOT proof",
		result: daemon.DirCoverageResult{Answered: true, Tracked: true, Walked: true, Indexable: true},
	}, {
		name:   "the walk did not finish ⇒ NOT proof",
		result: daemon.DirCoverageResult{Answered: true, Tracked: true},
	}, {
		name:   "an unfinished walk that found nothing yet is still not proof",
		result: daemon.DirCoverageResult{Answered: true, Tracked: true, Indexable: false},
	}, {
		name:   "the daemon could not answer",
		result: daemon.DirCoverageResult{},
	}, {
		name:        "a scope outside every corpus is an answer",
		result:      daemon.DirCoverageResult{Answered: true, Walked: true},
		wantProbeOK: true,
	}, {
		// An older daemon omits every flag this verb added, which must read
		// as an abstention rather than as an empty scope.
		name:   "a daemon that predates the verb's flags",
		result: daemon.DirCoverageResult{Answered: true, Tracked: true, HasSource: false},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			gotHas, gotOK := scopeTrackedFromCoverage(tc.result)
			if gotHas != tc.wantHasSource || gotOK != tc.wantProbeOK {
				t.Errorf("scopeTrackedFromCoverage(%+v) = (%v, %v), want (%v, %v)",
					tc.result, gotHas, gotOK, tc.wantHasSource, tc.wantProbeOK)
			}
		})
	}
}

// The verdict must survive the transport, not just the decision function.

// A repo mid-walk answers "nothing indexed here" for a package full of source.
func TestScopeTrackedViaDaemon_WarmingRepoIsNotProof(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	writeScopeFile(t, dir, "internal/parser.go", "package p\n")
	stubDirCoverage(t, daemon.DirCoverageResult{
		Answered: true, Tracked: true, Walked: true, Indexable: true,
	})

	hasSource, probeOK := scopeTrackedViaDaemon(dir, "internal")
	if hasSource || probeOK {
		t.Fatalf("source the graph has not reached yet leaves the scope unproven, got (%v, %v)", hasSource, probeOK)
	}
}

// The complement: a tree whose files the walk will never claim is proven empty.
func TestScopeTrackedViaDaemon_ProvenEmptySubdirectory(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	writeScopeFile(t, dir, "node_modules/dpack/index.js", "module.exports = {}\n")
	stubDirCoverage(t, daemon.DirCoverageResult{Answered: true, Tracked: true, Walked: true})

	hasSource, probeOK := scopeTrackedViaDaemon(dir, "node_modules/dpack")
	if hasSource || !probeOK {
		t.Fatalf("an excluded-by-design subdirectory should be proven empty, got (%v, %v)", hasSource, probeOK)
	}
}

// The scope the daemon is asked about is the one the caller named, resolved
// against cwd — the daemon does the rest, because only it can tell a worktree
// from an ordinary tracked root.
func TestScopeTrackedViaDaemon_AsksAboutTheScopeItself(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	writeScopeFile(t, dir, "internal/parser.go", "package p\n")
	asked := ""
	stubDirCoverageBy(t, func(scope string) (daemon.DirCoverageResult, bool) {
		asked = scope
		return daemon.DirCoverageResult{Answered: true, Tracked: true, HasSource: true}, true
	})

	if hasSource, probeOK := scopeTrackedViaDaemon(dir, "internal"); !hasSource || !probeOK {
		t.Fatalf("a scope the graph holds source under denies, got (%v, %v)", hasSource, probeOK)
	}
	if want := filepath.Join(dir, "internal"); asked != want {
		t.Errorf("the daemon was asked about %q, want %q", asked, want)
	}
}

// A scope that is not a directory never reaches the daemon.
func TestScopeTrackedViaDaemon_MissingScopeIsUnproven(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	stubDirCoverageBy(t, func(scope string) (daemon.DirCoverageResult, bool) {
		t.Errorf("an unwalkable scope was put to the daemon: %q", scope)
		return daemon.DirCoverageResult{}, false
	})

	if hasSource, probeOK := scopeTrackedViaDaemon(dir, "gone"); hasSource || probeOK {
		t.Fatalf("a scope that does not exist proves nothing, got (%v, %v)", hasSource, probeOK)
	}
}

// A transport failure is not a verdict.
func TestScopeTrackedViaDaemon_UnreachedIsNotProvenEmpty(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	writeScopeFile(t, dir, "infra/main.tf", "resource \"x\" \"y\" {}\n")
	stubDirCoverageBy(t, func(string) (daemon.DirCoverageResult, bool) {
		return daemon.DirCoverageResult{}, false
	})

	hasSource, probeOK := scopeTrackedViaDaemon(dir, "infra")
	if hasSource || probeOK {
		t.Fatalf("a scope the daemon never answered for is unproven, got (%v, %v)", hasSource, probeOK)
	}
}

// writeScopeFile creates root/rel and its parents.
func writeScopeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- the coverage answer's flags, as the hook reads them ---

// The daemon's abstention must never arrive as a negative verdict, and an
// older daemon that omits `answered` is one long abstention.
func TestNoGraphAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		st        fileIndexStatus
		reachable bool
		want      bool
	}{
		{"indexed", fileIndexStatus{Indexed: true, Count: 3, ProbeOK: true, Tracked: true}, true, false},
		{"indexed by a daemon with no tracked flag", fileIndexStatus{Indexed: true, Count: 3, ProbeOK: true}, true, false},
		{"never indexable", fileIndexStatus{NeverIndexable: true, ProbeOK: true, Tracked: true}, true, true},
		{"symbolless", fileIndexStatus{Symbolless: true, ProbeOK: true, Tracked: true}, true, true},
		{"tracked, indexable, not held", fileIndexStatus{ProbeOK: true, Tracked: true}, true, false},
		{"answered: nothing tracks it", fileIndexStatus{ProbeOK: true}, true, true},
		{"no answer, nothing tracks it", fileIndexStatus{}, true, true},
		{"unreached, daemon up", fileIndexStatus{Unreached: true}, true, false},
		{"unreached, daemon down", fileIndexStatus{Unreached: true}, false, true},
		{"tracked, no verdict, daemon up", fileIndexStatus{Tracked: true}, true, false},
		{"an older daemon's silence", fileIndexStatus{Tracked: true}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDaemonReachable(t, tc.reachable)
			if got := tc.st.noGraphAnswer(); got != tc.want {
				t.Errorf("noGraphAnswer(%+v) = %v, want %v", tc.st, got, tc.want)
			}
		})
	}
}
