package hooks

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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

// No tracked repo owns it, so no graph will ever hold it.
func TestEnrichRead_Untracked_Silent(t *testing.T) {
	withDaemonReachable(t, true)
	stubFileIndexScope(t, fileIndexStatus{}) // not tracked, no verdict
	res := enrichRead(map[string]any{"file_path": "/elsewhere/pkg/a.go"}, "/repo")
	if res.deny || res.context != "" {
		t.Fatalf("untracked read must be silent: deny=%v ctx=%q", res.deny, res.context)
	}
}

// A failed probe is not evidence. The daemon is up, so the tools the advisory
// names work; silence here turned one timed-out probe into a full bypass.
func TestEnrichRead_ProbeFailedButDaemonUp_Advises(t *testing.T) {
	withDaemonReachable(t, true)
	stubFileIndexScope(t, fileIndexStatus{Tracked: true}) // ProbeOK false
	res := enrichRead(map[string]any{"file_path": "pkg/a.go"}, "/repo")
	if res.deny {
		t.Fatalf("a failed probe must never deny: %#v", res)
	}
	if res.context == "" {
		t.Fatal("a failed probe against a live daemon should keep the advisory, got silence")
	}
}

// The one probe failure that warrants silence: the named tools are down too.
func TestEnrichRead_ProbeFailedDaemonDown_Silent(t *testing.T) {
	withDaemonReachable(t, false)
	stubFileIndexScope(t, fileIndexStatus{Tracked: true}) // ProbeOK false
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
		{"tracked, probe failed, daemon up", fileIndexStatus{Tracked: true}, true, true},
		{"tracked, probe failed, daemon down", fileIndexStatus{Tracked: true}, false, false},
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
	stubFileIndexScope(t, fileIndexStatus{}) // ProbeOK false
	v := hookSearchScope("/repo", map[string]any{"path": "pkg/a.go"})
	if v != searchScopeUnproven {
		t.Fatalf("no-verdict single-file scope should stay Unproven, got %v", v)
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
		witness       fileIndexStatus
		witnessKind   scopeWitness
		wantHasSource bool
		wantProbeOK   bool
	}{{
		name:          "scope holds indexed source",
		rel:           "internal",
		answers:       map[string][2]bool{"internal": {hasSource, answered}},
		wantHasSource: true,
		wantProbeOK:   true,
	}, {
		name:          "scope empty and its source is excluded by design ⇒ proven empty",
		rel:           "node_modules/dpack",
		answers:       map[string][2]bool{"node_modules/dpack": {empty, answered}},
		witness:       fileIndexStatus{NeverIndexable: true, Tracked: true, ProbeOK: true},
		witnessKind:   witnessFound,
		wantHasSource: false,
		wantProbeOK:   true,
	}, {
		name:          "scope holds no source at all ⇒ proven empty",
		rel:           "docs",
		answers:       map[string][2]bool{"docs": {empty, answered}},
		witnessKind:   witnessNone,
		wantHasSource: false,
		wantProbeOK:   true,
	}, {
		name:          "scope's source is indexable and simply absent ⇒ warming, NOT proof",
		rel:           "internal/parser",
		answers:       map[string][2]bool{"internal/parser": {empty, answered}},
		witness:       fileIndexStatus{Tracked: true, ProbeOK: true},
		witnessKind:   witnessFound,
		wantHasSource: false,
		wantProbeOK:   false,
	}, {
		name:          "witness probe failed ⇒ not proof",
		rel:           "internal",
		answers:       map[string][2]bool{"internal": {empty, answered}},
		witness:       fileIndexStatus{Tracked: true},
		witnessKind:   witnessFound,
		wantHasSource: false,
		wantProbeOK:   false,
	}, {
		name:          "witness walk could not finish ⇒ not proof",
		rel:           "internal",
		answers:       map[string][2]bool{"internal": {empty, answered}},
		witnessKind:   witnessUnknown,
		wantHasSource: false,
		wantProbeOK:   false,
	}, {
		name:          "witness contradicts find_files ⇒ enforce",
		rel:           "internal",
		answers:       map[string][2]bool{"internal": {empty, answered}},
		witness:       fileIndexStatus{Indexed: true, Count: 3, Tracked: true, ProbeOK: true},
		witnessKind:   witnessFound,
		wantHasSource: true,
		wantProbeOK:   true,
	}, {
		name:          "an empty repo root is corroborated like any other scope",
		rel:           "",
		answers:       map[string][2]bool{"": {empty, answered}},
		witness:       fileIndexStatus{Tracked: true, ProbeOK: true},
		witnessKind:   witnessFound,
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
			witness := func() (fileIndexStatus, scopeWitness) {
				witnessCalls++
				return tc.witness, tc.witnessKind
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

// Nothing the indexer could ever claim lives here, so no file probe is owed.
func TestScopeTrackedViaDaemon_NoSourceInScope(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	writeScopeFile(t, dir, "docs/guide.md", "# hi\n")
	withRepoRoot(t, dir)
	stubFindFilesProbe(t, map[string][2]bool{"docs": {false, true}})
	stubFileIndexScope(t, fileIndexStatus{}) // must never be consulted

	hasSource, probeOK := scopeTrackedViaDaemon(dir, "docs")
	if hasSource || !probeOK {
		t.Fatalf("a docs-only scope should be proven non-source, got (%v, %v)", hasSource, probeOK)
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

// The walk picks the first source-looking file and ignores everything else.
func TestScopeWitnessViaWalk(t *testing.T) {
	t.Run("finds source past non-source entries", func(t *testing.T) {
		dir := t.TempDir()
		writeScopeFile(t, dir, "README.md", "# hi\n")
		writeScopeFile(t, dir, "assets/logo.png", "\x89PNG\n")
		writeScopeFile(t, dir, "pkg/a.go", "package a\n")
		withRepoRoot(t, dir)
		stubFileIndexScope(t, fileIndexStatus{Indexed: true, Count: 2, Tracked: true, ProbeOK: true})

		st, w := scopeWitnessViaWalk(dir, dir)
		if w != witnessFound {
			t.Fatalf("a tree holding a .go file has a witness, got %v", w)
		}
		if !st.Indexed {
			t.Fatalf("the witness carries the per-file verdict, got %#v", st)
		}
	})

	t.Run("no source at all", func(t *testing.T) {
		dir := t.TempDir()
		writeScopeFile(t, dir, "docs/a.md", "# a\n")
		writeScopeFile(t, dir, "docs/b.txt", "b\n")

		if _, w := scopeWitnessViaWalk(dir, dir); w != witnessNone {
			t.Fatalf("a docs-only tree has no witness, got %v", w)
		}
	})

	t.Run("missing directory is unknown, not empty", func(t *testing.T) {
		if _, w := scopeWitnessViaWalk(t.TempDir(), filepath.Join(t.TempDir(), "gone")); w != witnessUnknown {
			t.Fatalf("an unwalkable scope proves nothing, got %v", w)
		}
	})
}

// --- wire parsing ---

func TestParseFileSummaryScope(t *testing.T) {
	guidance := func(body string) []byte {
		return []byte(`{"result":{"content":[{"text":` + strconv.Quote(body) + `}]}}`)
	}
	for _, tc := range []struct {
		name string
		resp []byte
		want fileIndexStatus
	}{{
		name: "indexed summary",
		resp: guidance(`{"total_nodes":7}`),
		want: fileIndexStatus{Indexed: true, Count: 7, ProbeOK: true},
	}, {
		name: "excluded by rule",
		resp: guidance(`{"recoverable":true,"condition":"file_not_indexed","data":{"path":"node_modules/x.js","excluded":true,"unindexable":true}}`),
		want: fileIndexStatus{NeverIndexable: true, ProbeOK: true},
	}, {
		// Unclaimed language / size cap / content gate: no exclude rule matched,
		// but the graph will never hold it — hence the union.
		name: "unindexable but not rule-excluded",
		resp: guidance(`{"recoverable":true,"condition":"file_not_indexed","data":{"path":"db/0001.sql","excluded":false,"unindexable":true}}`),
		want: fileIndexStatus{NeverIndexable: true, ProbeOK: true},
	}, {
		name: "indexed but defines no symbols",
		resp: guidance(`{"recoverable":true,"condition":"file_not_indexed","data":{"path":"pkg/doc.go","indexed":true}}`),
		want: fileIndexStatus{Symbolless: true, ProbeOK: true},
	}, {
		name: "tracked, indexable, not indexed yet",
		resp: guidance(`{"recoverable":true,"condition":"file_not_indexed","data":{"path":"pkg/new.go","excluded":false}}`),
		want: fileIndexStatus{ProbeOK: true},
	}, {
		// `data` is free-form server-side, so an `excluded` key on another
		// payload must not silence enforcement for indexed source.
		name: "excluded flag on a foreign condition is ignored",
		resp: guidance(`{"recoverable":true,"condition":"repo_not_tracked","data":{"path":"pkg/a.go","excluded":true}}`),
		want: fileIndexStatus{ProbeOK: true},
	}, {
		name: "no condition field at all",
		resp: guidance(`{"recoverable":true,"data":{"path":"pkg/a.go","excluded":true}}`),
		want: fileIndexStatus{ProbeOK: true},
	}, {
		// get_file_summary turns recovered panics into isError: no verdict.
		name: "error frame is no verdict",
		resp: []byte(`{"result":{"isError":true,"content":[{"text":"get_file_summary internal error: boom"}]}}`),
		want: fileIndexStatus{},
	}, {
		name: "unparseable body is no verdict",
		resp: guidance("not json"),
		want: fileIndexStatus{},
	}, {
		name: "unparseable frame is no verdict",
		resp: []byte("not json"),
		want: fileIndexStatus{},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseFileSummaryScope(tc.resp); got != tc.want {
				t.Errorf("parseFileSummaryScope() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A well-formed empty list is (false, true) — "asked, and it was empty". The
// split keeps a transport failure distinguishable from an empty directory.
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

// --- ownership: "nothing tracks it" vs "we could not ask" ---

// withTrackedRegistryKnown fixes whether the registry was readable.
func withTrackedRegistryKnown(t *testing.T, known bool) {
	t.Helper()
	old := trackedRegistryKnownFn
	trackedRegistryKnownFn = func() bool { return known }
	t.Cleanup(func() { trackedRegistryKnownFn = old })
}

// withNoTrackedRepos empties the registry, so every path resolves untracked.
func withNoTrackedRepos(t *testing.T) {
	t.Helper()
	old := hookTrackedReposFn
	hookTrackedReposFn = func() []daemon.TrackedRepoStatus { return nil }
	t.Cleanup(func() { hookTrackedReposFn = old })
}

// A non-empty registry is believable without a daemon round-trip.
func TestTrackedRegistryKnown_NonEmptyRegistry(t *testing.T) {
	withRepoRoot(t, "/repo")
	if !trackedRegistryKnown() {
		t.Fatal("a non-empty tracked-repo registry is self-evidently readable")
	}
}

// cachedTrackedRepos returns the same nil slice for "the daemon reports zero
// repos" and for "the status RPC failed", and only the first is a verdict.
func TestFileIndexScopeViaDaemon_RegistryUnreadable_IsNotUntracked(t *testing.T) {
	withDaemonReachable(t, true)
	withNoTrackedRepos(t)
	withTrackedRegistryKnown(t, false)

	st := fileIndexScopeViaDaemon("/repo", "pkg/a.go")
	if st.Tracked {
		t.Fatalf("an unreadable registry cannot establish ownership: %#v", st)
	}
	if !st.OwnershipUnknown {
		t.Fatalf("an unreadable registry must mark ownership unknown: %#v", st)
	}
	if st.noGraphAnswer() {
		t.Fatal("ownership we could not establish must keep the advisory, not silence it")
	}
}

// The complement: a readable registry that simply does not contain the path is
// a real verdict, and silence is the right answer to it.
func TestFileIndexScopeViaDaemon_ReadableRegistryUntracked_IsSilent(t *testing.T) {
	withDaemonReachable(t, true)
	withRepoRoot(t, "/repo")
	withTrackedRegistryKnown(t, true)

	st := fileIndexScopeViaDaemon("/repo", "/elsewhere/pkg/a.go")
	if st.Tracked || st.OwnershipUnknown {
		t.Fatalf("a readable registry gives a real untracked verdict: %#v", st)
	}
	if !st.noGraphAnswer() {
		t.Fatal("a path no tracked repo owns has no graph answer to redirect to")
	}
}

// End to end through the read door, on the real fileIndexScopeFn.
func TestEnrichRead_RegistryUnreadable_Advises(t *testing.T) {
	withDaemonReachable(t, true)
	withNoTrackedRepos(t)
	withTrackedRegistryKnown(t, false)

	res := enrichRead(map[string]any{"file_path": "pkg/a.go"}, "/repo")
	if res.deny {
		t.Fatalf("an unreadable registry must never deny: %#v", res)
	}
	if res.context == "" {
		t.Fatal("an unreadable registry against a live daemon keeps the advisory, got silence")
	}
}

// --- out of the active scope is not "symbol-free" ---

// The wire flag must not also read as Symbolless — that state denies on the
// search door.
func TestParseFileSummaryScope_OutOfScopeIsNotSymbolless(t *testing.T) {
	body := `{"condition":"file_not_indexed","data":{"indexed":true,"out_of_scope":true}}`
	st := parseFileSummaryScope([]byte(`{"result":{"content":[{"text":` + strconv.Quote(body) + `}]}}`))
	if !st.OutOfScope {
		t.Fatalf("out_of_scope must ride through: %#v", st)
	}
	if st.Symbolless {
		t.Fatal("out-of-scope must not read as Symbolless — Symbolless denies on the search door")
	}
}

// Both doors have to agree about the same file: the tools a deny redirects to
// apply the same repo filter, so denying an out-of-scope file is a dead end.
func TestOutOfScope_ReadAndSearchDoorsAgree(t *testing.T) {
	withDaemonReachable(t, true)
	dir := t.TempDir()
	writeScopeFile(t, dir, "a.go", "package a\n")
	file := filepath.Join(dir, "a.go")

	t.Run("out of scope: both doors stay out of the way", func(t *testing.T) {
		stubFileIndexScope(t, fileIndexStatus{OutOfScope: true, Tracked: true, ProbeOK: true})
		if res := enrichRead(map[string]any{"file_path": file}, dir); res.deny || res.context != "" {
			t.Fatalf("out-of-scope read must be silent: deny=%v ctx=%q", res.deny, res.context)
		}
		if v := hookSearchScope(dir, map[string]any{"path": file}); v != searchScopeNonSource {
			t.Fatalf("an out-of-scope file must not deny on the search door, got %v", v)
		}
		if res := enrichGrep(map[string]any{"pattern": "func Foo", "path": file}, 0, dir); res.deny {
			t.Fatalf("Grep must not be denied toward tools that cannot answer: %#v", res)
		}
	})

	t.Run("genuinely symbol-free: the search door still denies", func(t *testing.T) {
		stubFileIndexScope(t, fileIndexStatus{Symbolless: true, Tracked: true, ProbeOK: true})
		if res := enrichRead(map[string]any{"file_path": file}, dir); res.deny || res.context != "" {
			t.Fatalf("symbol-free read must be silent: deny=%v ctx=%q", res.deny, res.context)
		}
		if v := hookSearchScope(dir, map[string]any{"path": file}); v != searchScopeIndexed {
			t.Fatalf("the locators DO have rows for a symbol-free indexed file, got %v", v)
		}
	})
}
