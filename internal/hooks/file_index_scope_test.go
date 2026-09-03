package hooks

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// withRepoRoot makes root the one tracked repo, no daemon needed.
func withRepoRoot(t *testing.T, root string) {
	t.Helper()
	old := hookTrackedReposFn
	hookTrackedReposFn = func() []daemon.TrackedRepoStatus {
		return []daemon.TrackedRepoStatus{{Path: root}}
	}
	t.Cleanup(func() { hookTrackedReposFn = old })
}

// stubFindFilesProbe answers per repo-relative path ("" = whole repo); a path
// absent from the map answers (false, false) — "could not ask".
func stubFindFilesProbe(t *testing.T, answers map[string][2]bool) {
	t.Helper()
	old := findFilesProbeFn
	findFilesProbeFn = func(_, rel string) (bool, bool) {
		a, present := answers[rel]
		if !present {
			return false, false
		}
		return a[0], a[1]
	}
	t.Cleanup(func() { findFilesProbeFn = old })
}

// stubFileIndexScopeTimed stubs the file-verdict seam with the probe budget
// visible, for the tests that assert the witness shares one deadline.
func stubFileIndexScopeTimed(t *testing.T, fn func(cwd, filePath string, timeout time.Duration) fileIndexStatus) {
	t.Helper()
	old := fileIndexScopeFn
	fileIndexScopeFn = fn
	t.Cleanup(func() { fileIndexScopeFn = old })
}

// stubScopeWitness fixes the witness verdict without walking anything.
func stubScopeWitness(t *testing.T, w scopeWitness) {
	t.Helper()
	old := scopeWitnessFn
	scopeWitnessFn = func(string, string) scopeWitness { return w }
	t.Cleanup(func() { scopeWitnessFn = old })
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

// --- what an empty find_files answer may prove ---

// Zero hits is not proof of a vendored tree, and neither is "some file
// somewhere in this repo is indexed" — that holds while the walk has not
// reached this scope yet. Only the scope's own witness separates "excluded by
// design" from "not indexed yet".
func TestScopeTrackedFromProbes(t *testing.T) {
	const (
		hasSource = true
		empty     = false
		answered  = true
	)
	for _, tc := range []struct {
		name          string
		rel           string
		answers       map[string][2]bool
		witness       scopeWitness
		wantHasSource bool
		wantProbeOK   bool
	}{{
		name:          "scope holds indexed source",
		rel:           "internal",
		answers:       map[string][2]bool{"internal": {hasSource, answered}},
		wantHasSource: true,
		wantProbeOK:   true,
	}, {
		name:          "scope empty and every sampled file is unindexable ⇒ proven empty",
		rel:           "node_modules/dpack",
		answers:       map[string][2]bool{"node_modules/dpack": {empty, answered}},
		witness:       witnessNever,
		wantHasSource: false,
		wantProbeOK:   true,
	}, {
		name:          "witness settled nothing ⇒ NOT proof",
		rel:           "internal/parser",
		answers:       map[string][2]bool{"internal/parser": {empty, answered}},
		witness:       witnessUnknown,
		wantHasSource: false,
		wantProbeOK:   false,
	}, {
		name:          "witness contradicts find_files ⇒ enforce",
		rel:           "internal",
		answers:       map[string][2]bool{"internal": {empty, answered}},
		witness:       witnessSource,
		wantHasSource: true,
		wantProbeOK:   true,
	}, {
		name:          "an empty repo root is corroborated like any other scope",
		rel:           "",
		answers:       map[string][2]bool{"": {empty, answered}},
		witness:       witnessUnknown,
		wantHasSource: false,
		wantProbeOK:   false,
	}, {
		name:          "repo root holds source",
		rel:           "",
		answers:       map[string][2]bool{"": {hasSource, answered}},
		wantHasSource: true,
		wantProbeOK:   true,
	}, {
		name:          "scoped probe failed outright",
		rel:           "internal",
		answers:       map[string][2]bool{},
		wantHasSource: false,
		wantProbeOK:   false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			probe := func(at string) (bool, bool) {
				calls++
				a, present := tc.answers[at]
				if !present {
					return false, false
				}
				return a[0], a[1]
			}
			witnessCalls := 0
			witness := func() scopeWitness {
				witnessCalls++
				return tc.witness
			}
			gotHas, gotOK := scopeTrackedFromProbes(tc.rel, probe, witness)
			if gotHas != tc.wantHasSource || gotOK != tc.wantProbeOK {
				t.Errorf("scopeTrackedFromProbes(%q) = (%v, %v), want (%v, %v)",
					tc.rel, gotHas, gotOK, tc.wantHasSource, tc.wantProbeOK)
			}
			// One probe, never two — the witness replaced the repo-wide dial.
			if calls != 1 {
				t.Errorf("the scope is probed exactly once; calls = %d", calls)
			}
			if a, ok := tc.answers[tc.rel]; ok && a[0] && witnessCalls != 0 {
				t.Errorf("a scope that holds source must not pay the witness walk; calls = %d", witnessCalls)
			}
		})
	}
}

// The corroboration must survive the transport path, not just the decision fn:
// a repo mid-walk answers "no indexed files here" for a package full of source.
func TestScopeTrackedViaDaemon_WarmingRepoIsNotProof(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	writeScopeFile(t, dir, "internal/parser.go", "package p\n")
	withRepoRoot(t, dir)
	stubFindFilesProbe(t, map[string][2]bool{"internal": {false, true}})
	// Tracked and indexable — the walk simply has not reached it yet.
	stubFileIndexScope(t, fileIndexStatus{Tracked: true, ProbeOK: true})

	hasSource, probeOK := scopeTrackedViaDaemon(dir, "internal")
	if hasSource || probeOK {
		t.Fatalf("source the graph has not reached yet leaves the scope unproven, got (%v, %v)", hasSource, probeOK)
	}
}

// Indexed source elsewhere in the repo proves nothing about THIS scope.
func TestScopeTrackedViaDaemon_SourceElsewhereIsNotProof(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	writeScopeFile(t, dir, "internal/parser.go", "package p\n")
	withRepoRoot(t, dir)
	stubFindFilesProbe(t, map[string][2]bool{"internal": {false, true}, "": {true, true}})
	stubFileIndexScope(t, fileIndexStatus{Tracked: true, ProbeOK: true})

	hasSource, probeOK := scopeTrackedViaDaemon(dir, "internal")
	if hasSource || probeOK {
		t.Fatalf("indexed source elsewhere in the repo is not proof about this scope, got (%v, %v)", hasSource, probeOK)
	}
}

// The complement: a tree whose own files are excluded by design is proven empty.
func TestScopeTrackedViaDaemon_ProvenEmptySubdirectory(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	writeScopeFile(t, dir, "node_modules/dpack/index.js", "module.exports = {}\n")
	withRepoRoot(t, dir)
	stubFindFilesProbe(t, map[string][2]bool{"node_modules/dpack": {false, true}})
	stubFileIndexScope(t, fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true})

	hasSource, probeOK := scopeTrackedViaDaemon(dir, "node_modules/dpack")
	if hasSource || !probeOK {
		t.Fatalf("an excluded-by-design subdirectory should be proven empty, got (%v, %v)", hasSource, probeOK)
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

// The graph decides what a scope holds. looksLikeSourceFile only ranks the
// sample, so a scope whose files carry no recognised extension is put to the
// daemon rather than written off.
func TestScopeWitnessViaWalk(t *testing.T) {
	t.Run("a recognised source file represents the scope", func(t *testing.T) {
		dir := t.TempDir()
		writeScopeFile(t, dir, "README.md", "# hi\n")
		writeScopeFile(t, dir, "assets/logo.png", "\x89PNG\n")
		writeScopeFile(t, dir, "pkg/a.go", "package a\n")
		stubFileIndexScopeBy(t, func(_, path string) fileIndexStatus {
			if !strings.HasSuffix(path, ".go") {
				t.Errorf("the walk sampled %q over the .go file it should prefer", path)
			}
			return fileIndexStatus{Indexed: true, Count: 2, Tracked: true, ProbeOK: true}
		})

		if w := scopeWitnessViaWalk(dir, dir); w != witnessSource {
			t.Fatalf("a tree holding an indexed .go file has a source witness, got %v", w)
		}
	})

	t.Run("excluded by design", func(t *testing.T) {
		dir := t.TempDir()
		writeScopeFile(t, dir, "vendor/x/y.go", "package y\n")
		stubFileIndexScope(t, fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true})

		if w := scopeWitnessViaWalk(dir, dir); w != witnessNever {
			t.Fatalf("a tree the walk will never hold is proven source-free, got %v", w)
		}
	})

	t.Run("indexable but not held yet settles nothing", func(t *testing.T) {
		dir := t.TempDir()
		writeScopeFile(t, dir, "pkg/a.go", "package a\n")
		stubFileIndexScope(t, fileIndexStatus{Tracked: true, ProbeOK: true})

		if w := scopeWitnessViaWalk(dir, dir); w != witnessUnknown {
			t.Fatalf("a scope mid-walk is not a scope without source, got %v", w)
		}
	})

	t.Run("a failed witness probe settles nothing", func(t *testing.T) {
		dir := t.TempDir()
		writeScopeFile(t, dir, "pkg/a.go", "package a\n")
		stubFileIndexScope(t, fileIndexStatus{Unreached: true})

		if w := scopeWitnessViaWalk(dir, dir); w != witnessUnknown {
			t.Fatalf("a probe that never answered proves nothing, got %v", w)
		}
	})

	// The finding this rewrite exists for. Every one of these is a language
	// with a first-class extractor and none carries a recognised extension,
	// so an extension list would call the scope settled-non-source and switch
	// enforcement off for the repositories that need it most.
	t.Run("unrecognised source names go to the graph, not to a list", func(t *testing.T) {
		for _, name := range []string{
			"main.tf", "Chart.yaml", "playbook.yml", "App.csproj",
			"report.qmd", "init.luau", "README.md",
		} {
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				writeScopeFile(t, dir, name, "x\n")
				asked := ""
				stubFileIndexScopeBy(t, func(_, path string) fileIndexStatus {
					asked = filepath.Base(path)
					// The extractor claims it; it is simply not held yet.
					return fileIndexStatus{Tracked: true, ProbeOK: true}
				})

				if w := scopeWitnessViaWalk(dir, dir); w != witnessUnknown {
					t.Fatalf("%s must not read as settled-non-source, got %v", name, w)
				}
				if asked != name {
					t.Fatalf("the graph was asked about %q, want %q", asked, name)
				}
			})
		}
	})

	t.Run("unrecognised names the graph will never hold", func(t *testing.T) {
		dir := t.TempDir()
		writeScopeFile(t, dir, "docs/a.md", "# a\n")
		writeScopeFile(t, dir, "docs/b.txt", "b\n")
		stubFileIndexScope(t, fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true})

		if w := scopeWitnessViaWalk(dir, dir); w != witnessNever {
			t.Fatalf("a tree the daemon disowns is proven source-free, got %v", w)
		}
	})

	t.Run("an empty tree holds no source", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
			t.Fatal(err)
		}
		stubFileIndexScopeBy(t, func(_, path string) fileIndexStatus {
			t.Errorf("nothing to sample, yet the graph was asked about %q", path)
			return fileIndexStatus{}
		})

		if w := scopeWitnessViaWalk(dir, dir); w != witnessNever {
			t.Fatalf("a tree with no files at all holds no source, got %v", w)
		}
	})

	t.Run("missing directory is unknown, not empty", func(t *testing.T) {
		if w := scopeWitnessViaWalk(t.TempDir(), filepath.Join(t.TempDir(), "gone")); w != witnessUnknown {
			t.Fatalf("an unwalkable scope proves nothing, got %v", w)
		}
	})

	// WalkDir is lexical, so an unpruned .git swallows the entry budget before
	// any source file is reached and every warming-repo Grep pays for it.
	t.Run("dot-directories are pruned", func(t *testing.T) {
		dir := t.TempDir()
		for i := range witnessWalkEntries + 16 {
			writeScopeFile(t, dir, filepath.Join(".git", "objects", strconv.Itoa(i)), "x")
		}
		writeScopeFile(t, dir, "pkg/a.go", "package a\n")
		asked := ""
		stubFileIndexScopeBy(t, func(_, path string) fileIndexStatus {
			asked = path
			return fileIndexStatus{Indexed: true, Count: 1, Tracked: true, ProbeOK: true}
		})

		if w := scopeWitnessViaWalk(dir, dir); w != witnessSource {
			t.Fatalf("the .go file past .git must still be reached, got %v", w)
		}
		if filepath.Base(asked) != "a.go" {
			t.Fatalf("the graph was asked about %q, want the source file", asked)
		}
	})

	// …but pruning means an empty sample is no longer an empty scope.
	t.Run("a scope holding only dot-directories proves nothing", func(t *testing.T) {
		dir := t.TempDir()
		writeScopeFile(t, dir, filepath.Join(".hidden", "a.go"), "package a\n")

		if w := scopeWitnessViaWalk(dir, dir); w != witnessUnknown {
			t.Fatalf("a pruned scope with nothing left to sample proves nothing, got %v", w)
		}
	})
}

// The witness runs on the PreToolUse critical path, so the walk and every
// probe it raises share one deadline rather than each getting its own.
func TestScopeWitnessViaWalk_FitsOneBudget(t *testing.T) {
	t.Run("each probe sees the remaining budget", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"a.go", "b.go", "c.go"} {
			writeScopeFile(t, dir, name, "package p\n")
		}
		var budgets []time.Duration
		stubFileIndexScopeTimed(t, func(_, _ string, timeout time.Duration) fileIndexStatus {
			budgets = append(budgets, timeout)
			time.Sleep(2 * time.Millisecond)
			return fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true}
		})

		if w := scopeWitnessViaWalk(dir, dir); w != witnessNever {
			t.Fatalf("witness = %v, want witnessNever", w)
		}
		if len(budgets) != witnessSamples {
			t.Fatalf("the walk raised %d probes, want the %d-sample cap", len(budgets), witnessSamples)
		}
		if budgets[0] > witnessBudget {
			t.Errorf("the first probe got %s, more than the %s witness budget", budgets[0], witnessBudget)
		}
		for i := 1; i < len(budgets); i++ {
			if budgets[i] >= budgets[i-1] {
				t.Errorf("probe %d got %s, not less than the %s left before it — the budget is not shared",
					i, budgets[i], budgets[i-1])
			}
		}
	})

	t.Run("an exhausted budget settles nothing", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"a.go", "b.go"} {
			writeScopeFile(t, dir, name, "package p\n")
		}
		probes := 0
		stubFileIndexScopeTimed(t, func(_, _ string, _ time.Duration) fileIndexStatus {
			probes++
			time.Sleep(witnessBudget)
			return fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true}
		})

		start := time.Now()
		if w := scopeWitnessViaWalk(dir, dir); w != witnessUnknown {
			t.Fatalf("a witness that ran out of budget proves nothing, got %v", w)
		}
		if probes != 1 {
			t.Errorf("the walk raised %d probes past its deadline, want 1", probes)
		}
		// One overrunning probe, not one per sample.
		if elapsed := time.Since(start); elapsed > 2*witnessBudget {
			t.Errorf("the witness took %s, want it bounded near the %s budget", elapsed, witnessBudget)
		}
	})
}

// The whole point of the second return: a scope the daemon could not answer
// for must not read as a scope with nothing in it.
func TestScopeTrackedViaDaemon_WitnessUnknownIsNotProvenEmpty(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	writeScopeFile(t, dir, "infra/main.tf", "resource \"x\" \"y\" {}\n")
	withRepoRoot(t, dir)
	stubFindFilesProbe(t, map[string][2]bool{"infra": {false, true}})
	stubScopeWitness(t, witnessUnknown)

	hasSource, probeOK := scopeTrackedViaDaemon(dir, "infra")
	if hasSource || probeOK {
		t.Fatalf("an unsettled witness leaves the scope unproven, got (%v, %v)", hasSource, probeOK)
	}
}

// --- wire parsing ---

func TestParseFindFilesHasSource(t *testing.T) {
	body := func(s string) []byte {
		return []byte(`{"result":{"content":[{"text":` + strconv.Quote(s) + `}]}}`)
	}
	for _, tc := range []struct {
		name          string
		resp          []byte
		wantHasSource bool
		wantOK        bool
	}{
		{"one file", body(`{"count":1,"files":[{"path":"a.go"}]}`), true, true},
		{"well-formed empty list", body(`{"count":0,"files":[]}`), false, true},
		{"count without rows", body(`{"count":3,"files":[]}`), false, true},
		{"error frame", []byte(`{"result":{"isError":true,"content":[{"text":"boom"}]}}`), false, false},
		{"unparseable body", body("not json"), false, false},
		{"unparseable frame", []byte("not json"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotHas, gotOK := parseFindFilesHasSource(tc.resp)
			if gotHas != tc.wantHasSource || gotOK != tc.wantOK {
				t.Errorf("parseFindFilesHasSource() = (%v, %v), want (%v, %v)",
					gotHas, gotOK, tc.wantHasSource, tc.wantOK)
			}
		})
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
