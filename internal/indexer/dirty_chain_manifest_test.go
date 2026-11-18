package indexer

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// manifestSpec spells one dirty entry compactly: "present:<hash>",
// "present:<hash>:<mode>", "absent", "opaque:<hash>" or "head_equal".
type manifestSpec map[string]string

func (spec manifestSpec) entries() []store_sqlite.InputManifestEntry {
	var out []store_sqlite.InputManifestEntry
	for p, s := range spec {
		parts := strings.Split(s, ":")
		e := store_sqlite.InputManifestEntry{FilePath: p, Admission: store_sqlite.InputManifestNotApplicable}
		switch parts[0] {
		case "present":
			e.State = store_sqlite.InputManifestPresent
			e.Admission = store_sqlite.InputManifestAdmitted
			e.ContentSHA256 = parts[1]
			e.Mode = "100644"
			if len(parts) > 2 {
				e.Mode = parts[2]
			}
		case "absent":
			e.State = store_sqlite.InputManifestAbsent
		case "opaque":
			e.State = store_sqlite.InputManifestOpaque
			e.ContentSHA256 = parts[1]
		case "head_equal":
			e.State = store_sqlite.InputManifestHeadEqual
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b store_sqlite.InputManifestEntry) int { return strings.Compare(a.FilePath, b.FilePath) })
	return out
}

func (spec manifestSpec) resolved(policy string) resolvedManifest {
	return resolvedFromFull(store_sqlite.InputManifestMeta{PolicyDigest: policy}, spec.entries())
}

func headHoldsAllBut(missing ...string) func(string) bool {
	return func(p string) bool { return !slices.Contains(missing, p) }
}

type planDeltaCase struct {
	name       string
	parent     manifestSpec
	next       manifestSpec
	headLacks  []string
	nextPolicy string
	want       []LayerPathChange
	reason     string
}

func runPlanDeltaCases(t *testing.T, cases []planDeltaCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := tc.nextPolicy
			if policy == "" {
				policy = "p1"
			}
			got, reason := planDelta(tc.parent.resolved("p1"), tc.next.resolved(policy), headHoldsAllBut(tc.headLacks...))
			if reason != tc.reason {
				t.Fatalf("reason=%q want %q (changes %+v)", reason, tc.reason, got)
			}
			if tc.reason != "" {
				if got != nil {
					t.Fatalf("fallback returned changes: %+v", got)
				}
				if !slices.Contains(dirtyChainFallbackReasons, reason) {
					t.Fatalf("reason %q is not a declared fallback code", reason)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("changes=%+v want %+v", got, tc.want)
			}
		})
	}
}

// Two unrelated dirty files stay dirty across every case, so the delta is
// smaller than the dirty set whenever only one path moves.
var planDeltaStable = manifestSpec{"pkg/a/a.go": "present:a1", "pkg/b/b.go": "present:b1"}

func withStable(extra manifestSpec) manifestSpec {
	out := manifestSpec{}
	for k, v := range planDeltaStable {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestPlanDeltaEditsAddsDeletesAndRenames(t *testing.T) {
	runPlanDeltaCases(t, []planDeltaCase{
		{
			name:   "body edit of a dirty file",
			parent: withStable(manifestSpec{"pkg/c/c.go": "present:c1"}),
			next:   withStable(manifestSpec{"pkg/c/c.go": "present:c2"}),
			want:   []LayerPathChange{{Path: "pkg/c/c.go", Kind: LayerPathModified}},
		},
		{
			name:   "comment edit of a committed file",
			parent: withStable(nil),
			next:   withStable(manifestSpec{"pkg/c/c.go": "present:c-comment"}),
			want:   []LayerPathChange{{Path: "pkg/c/c.go", Kind: LayerPathModified}},
		},
		{
			name:      "add of a new file",
			parent:    withStable(nil),
			next:      withStable(manifestSpec{"pkg/new/new.go": "present:n1"}),
			headLacks: []string{"pkg/new/new.go"},
			want:      []LayerPathChange{{Path: "pkg/new/new.go", Kind: LayerPathAdded}},
		},
		{
			name:   "delete of a committed file",
			parent: withStable(nil),
			next:   withStable(manifestSpec{"pkg/c/c.go": "absent"}),
			want:   []LayerPathChange{{Path: "pkg/c/c.go", Kind: LayerPathDeleted}},
		},
		{
			name:   "delete of a dirty-modified file",
			parent: withStable(manifestSpec{"pkg/c/c.go": "present:c2"}),
			next:   withStable(manifestSpec{"pkg/c/c.go": "absent"}),
			want:   []LayerPathChange{{Path: "pkg/c/c.go", Kind: LayerPathDeleted}},
		},
		{
			name:      "rename decomposes into delete and add",
			parent:    withStable(nil),
			next:      withStable(manifestSpec{"pkg/old.go": "absent", "pkg/renamed.go": "present:r1"}),
			headLacks: []string{"pkg/renamed.go"},
			want: []LayerPathChange{
				{Path: "pkg/old.go", Kind: LayerPathDeleted},
				{Path: "pkg/renamed.go", Kind: LayerPathAdded},
			},
		},
		{
			name:      "edit of a dirty-added file",
			parent:    withStable(manifestSpec{"pkg/new/new.go": "present:n1"}),
			next:      withStable(manifestSpec{"pkg/new/new.go": "present:n2"}),
			headLacks: []string{"pkg/new/new.go"},
			want:      []LayerPathChange{{Path: "pkg/new/new.go", Kind: LayerPathModified}},
		},
		{
			name:   "unchanged sample resolves to the parent",
			parent: withStable(manifestSpec{"pkg/c/c.go": "present:c1"}),
			next:   withStable(manifestSpec{"pkg/c/c.go": "present:c1"}),
			want:   nil,
		},
		{
			name:   "staged residue equal to HEAD is the committed state",
			parent: withStable(nil),
			next:   withStable(manifestSpec{"pkg/c/c.go": "head_equal"}),
			want:   nil,
		},
	})
}

func TestPlanDeltaUndoAndRestore(t *testing.T) {
	runPlanDeltaCases(t, []planDeltaCase{
		{
			// Falling through to the parent would expose its stale dirty
			// payload, so the committed bytes are re-emitted.
			name:   "partial undo of a modified file re-emits the committed file",
			parent: withStable(manifestSpec{"pkg/c/c.go": "present:c2"}),
			next:   withStable(nil),
			want:   []LayerPathChange{{Path: "pkg/c/c.go", Kind: LayerPathModified}},
		},
		{
			name:   "partial undo reported as HEAD-equal staged residue",
			parent: withStable(manifestSpec{"pkg/c/c.go": "present:c2"}),
			next:   withStable(manifestSpec{"pkg/c/c.go": "head_equal"}),
			want:   []LayerPathChange{{Path: "pkg/c/c.go", Kind: LayerPathModified}},
		},
		{
			name:      "undo of an added file deletes it",
			parent:    withStable(manifestSpec{"pkg/new/new.go": "present:n1"}),
			next:      withStable(nil),
			headLacks: []string{"pkg/new/new.go"},
			want:      []LayerPathChange{{Path: "pkg/new/new.go", Kind: LayerPathDeleted}},
		},
		{
			name:   "restore of a deleted file re-emits the committed file",
			parent: withStable(manifestSpec{"pkg/c/c.go": "absent"}),
			next:   withStable(nil),
			want:   []LayerPathChange{{Path: "pkg/c/c.go", Kind: LayerPathModified}},
		},
		{
			name:   "restore with an edit of a deleted file",
			parent: withStable(manifestSpec{"pkg/c/c.go": "absent"}),
			next:   withStable(manifestSpec{"pkg/c/c.go": "present:c3"}),
			want:   []LayerPathChange{{Path: "pkg/c/c.go", Kind: LayerPathAdded}},
		},
	})
}

func TestPlanDeltaFallbacks(t *testing.T) {
	runPlanDeltaCases(t, []planDeltaCase{
		{
			name:   "module manifest changed",
			parent: withStable(nil),
			next:   withStable(manifestSpec{"go.mod": "present:m2"}),
			reason: dirtyChainFallbackDependencyManifestChanged,
		},
		{
			name:   "nested package manifest changed",
			parent: withStable(manifestSpec{"web/package.json": "present:p1"}),
			next:   withStable(manifestSpec{"web/package.json": "present:p2"}),
			reason: dirtyChainFallbackDependencyManifestChanged,
		},
		{
			name:   "ignore file added",
			parent: withStable(nil),
			next:   withStable(manifestSpec{"pkg/.gortexignore": "present:i1"}),
			reason: dirtyChainFallbackDependencyManifestChanged,
		},
		{
			name:   "path alias config changed",
			parent: withStable(nil),
			next:   withStable(manifestSpec{"tsconfig.base.json": "present:t1"}),
			reason: dirtyChainFallbackDependencyManifestChanged,
		},
		{
			name:   "undo of a manifest edit",
			parent: withStable(manifestSpec{"go.sum": "present:s2"}),
			next:   withStable(nil),
			reason: dirtyChainFallbackDependencyManifestChanged,
		},
		{
			name:       "policy digest changed",
			parent:     withStable(manifestSpec{"pkg/c/c.go": "present:c1"}),
			next:       withStable(manifestSpec{"pkg/c/c.go": "present:c2"}),
			nextPolicy: "p2",
			reason:     dirtyChainFallbackPolicyChanged,
		},
		{
			name:   "symlink retargeted",
			parent: withStable(manifestSpec{"link": "present:l1:120000"}),
			next:   withStable(manifestSpec{"link": "present:l2:120000"}),
			reason: dirtyChainFallbackSymlinkOrSubmoduleChanged,
		},
		{
			name:   "file replaced by a symlink",
			parent: withStable(nil),
			next:   withStable(manifestSpec{"pkg/c/c.go": "present:l1:120000"}),
			reason: dirtyChainFallbackSymlinkOrSubmoduleChanged,
		},
		{
			name:   "opaque directory changed",
			parent: withStable(manifestSpec{"vendor/mod": "opaque:o1"}),
			next:   withStable(manifestSpec{"vendor/mod": "opaque:o2"}),
			reason: dirtyChainFallbackSymlinkOrSubmoduleChanged,
		},
		{
			name:   "a manifest change outranks a symlink change",
			parent: withStable(nil),
			next:   withStable(manifestSpec{"go.mod": "present:m2", "link": "present:l1:120000"}),
			reason: dirtyChainFallbackDependencyManifestChanged,
		},
		{
			name:   "clean checkout",
			parent: withStable(nil),
			next:   manifestSpec{"pkg/a/a.go": "head_equal"},
			reason: dirtyChainFallbackCleanCheckout,
		},
	})
}

// TestPlanDeltaSmallDeltasAlwaysChain pins the size rule: a delta is judged by
// its own size. One that touches at most dirtyChainSmallDelta paths chains
// however small the dirty set it lands in — the second edit of a one-file
// dirty set is one file over the previous generation — and only a larger
// delta that is no smaller than the direct build goes direct.
func TestPlanDeltaSmallDeltasAlwaysChain(t *testing.T) {
	runPlanDeltaCases(t, []planDeltaCase{
		{
			name:   "one file edited again",
			parent: manifestSpec{"a.go": "present:1"},
			next:   manifestSpec{"a.go": "present:2"},
			want:   []LayerPathChange{{Path: "a.go", Kind: LayerPathModified}},
		},
		{
			name:   "the first edit over a clean parent goes direct",
			parent: manifestSpec{},
			next:   manifestSpec{"a.go": "present:1"},
			reason: dirtyChainFallbackDeltaNotSmaller,
		},
		{
			name:   "a delta larger than the dirty set",
			parent: manifestSpec{"a.go": "present:1", "b.go": "present:2", "c.go": "present:3", "d.go": "present:4"},
			next:   manifestSpec{"a.go": "present:1b"},
			want: []LayerPathChange{
				{Path: "a.go", Kind: LayerPathModified}, {Path: "b.go", Kind: LayerPathModified},
				{Path: "c.go", Kind: LayerPathModified}, {Path: "d.go", Kind: LayerPathModified},
			},
		},
	})

	old := dirtyChainSmallDelta
	dirtyChainSmallDelta = 1
	t.Cleanup(func() { dirtyChainSmallDelta = old })
	runPlanDeltaCases(t, []planDeltaCase{
		{
			name:   "large delta larger than the snapshot",
			parent: manifestSpec{"a.go": "present:1", "b.go": "present:2", "c.go": "present:3", "d.go": "present:4"},
			next:   manifestSpec{"a.go": "present:1b"},
			reason: dirtyChainFallbackDeltaNotSmaller,
		},
		{
			name:   "large delta as large as the snapshot",
			parent: manifestSpec{"a.go": "present:1", "b.go": "present:1"},
			next:   manifestSpec{"a.go": "present:2", "b.go": "present:2"},
			reason: dirtyChainFallbackDeltaNotSmaller,
		},
		{
			name:   "small delta at the bound still chains",
			parent: manifestSpec{"a.go": "present:1"},
			next:   manifestSpec{"a.go": "present:2"},
			want:   []LayerPathChange{{Path: "a.go", Kind: LayerPathModified}},
		},
	})
}

func TestPlanDeltaUnchangedOpaqueEntryIsReused(t *testing.T) {
	runPlanDeltaCases(t, []planDeltaCase{{
		name:   "unchanged opaque entry and one edit",
		parent: withStable(manifestSpec{"vendor/mod": "opaque:o1", "pkg/c/c.go": "present:c1"}),
		next:   withStable(manifestSpec{"vendor/mod": "opaque:o1", "pkg/c/c.go": "present:c2"}),
		want:   []LayerPathChange{{Path: "pkg/c/c.go", Kind: LayerPathModified}},
	}})
}

func TestAdmittedManifestMirrorsSample(t *testing.T) {
	snap := gitstate.DirtySnapshot{Contents: []gitstate.DirtyContent{
		{Path: "b.go", State: gitstate.DirtyContentPresent, Mode: "100644", SHA256: "sb"},
		{Path: "a.go", State: gitstate.DirtyContentPresent, Mode: "100755", SHA256: "sa"},
		{Path: "gone.go", State: gitstate.DirtyContentAbsent},
		{Path: "residue.go", State: gitstate.DirtyContentPresent, Mode: "100644", SHA256: "sr", HeadEqual: true},
		{Path: "ghost.go", State: gitstate.DirtyContentAbsent, HeadEqual: true},
		{Path: "vendor/mod", State: gitstate.DirtyContentOpaque, SHA256: "so"},
		{Path: "big.bin", State: gitstate.DirtyContentPresent, Mode: "100644", SHA256: "sbig"},
	}}
	var asked []string
	admit := func(p string) store_sqlite.InputManifestAdmission {
		asked = append(asked, p)
		if p == "big.bin" {
			return store_sqlite.InputManifestOversized
		}
		return store_sqlite.InputManifestAdmitted
	}
	meta, entries := admittedManifest(snap, admit, "policy")
	if !meta.IsFull || meta.EntryCount != len(entries) || meta.PolicyDigest != "policy" || meta.ManifestVersion != store_sqlite.InputManifestVersion {
		t.Fatalf("meta=%+v", meta)
	}
	slices.Sort(asked)
	if want := []string{"a.go", "b.go", "big.bin", "residue.go"}; !slices.Equal(asked, want) {
		t.Fatalf("admission asked for %v want %v", asked, want)
	}
	want := []store_sqlite.InputManifestEntry{
		{FilePath: "a.go", State: store_sqlite.InputManifestPresent, Admission: store_sqlite.InputManifestAdmitted, Mode: "100755", ContentSHA256: "sa"},
		{FilePath: "b.go", State: store_sqlite.InputManifestPresent, Admission: store_sqlite.InputManifestAdmitted, Mode: "100644", ContentSHA256: "sb"},
		{FilePath: "big.bin", State: store_sqlite.InputManifestPresent, Admission: store_sqlite.InputManifestOversized, Mode: "100644", ContentSHA256: "sbig"},
		{FilePath: "ghost.go", State: store_sqlite.InputManifestHeadEqual, Admission: store_sqlite.InputManifestNotApplicable},
		{FilePath: "gone.go", State: store_sqlite.InputManifestAbsent, Admission: store_sqlite.InputManifestNotApplicable},
		{FilePath: "residue.go", State: store_sqlite.InputManifestHeadEqual, Admission: store_sqlite.InputManifestAdmitted, Mode: "100644", ContentSHA256: "sr"},
		{FilePath: "vendor/mod", State: store_sqlite.InputManifestOpaque, Admission: store_sqlite.InputManifestNotApplicable, ContentSHA256: "so"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries=%+v\nwant   %+v", entries, want)
	}
	if clean, cleanEntries := admittedManifest(gitstate.DirtySnapshot{}, admit, "policy"); len(cleanEntries) != 0 || clean.EntryCount != 0 || !clean.IsFull {
		t.Fatalf("clean manifest meta=%+v entries=%+v", clean, cleanEntries)
	}
}

func TestResolveChainLinksNewestWins(t *testing.T) {
	full := func(spec manifestSpec) chainManifestLink {
		e := spec.entries()
		return chainManifestLink{OK: true, Entries: e, Meta: store_sqlite.InputManifestMeta{ManifestVersion: store_sqlite.InputManifestVersion, IsFull: true, EntryCount: len(e), PolicyDigest: "p"}}
	}
	delta := func(spec manifestSpec) chainManifestLink {
		l := full(spec)
		l.Meta.IsFull = false
		return l
	}
	root := full(manifestSpec{"a.go": "present:1", "b.go": "present:1", "c.go": "absent"})
	d1 := delta(manifestSpec{"a.go": "present:2", "b.go": "head_equal"})
	d2 := delta(manifestSpec{"d.go": "present:1"})
	got, reason := resolveChainLinks(root, d1, d2)
	if reason != "" {
		t.Fatalf("reason=%q", reason)
	}
	want := manifestSpec{"a.go": "present:2", "c.go": "absent", "d.go": "present:1"}.resolved("p")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved=%+v want %+v", got, want)
	}

	for _, tc := range []struct {
		name   string
		links  []chainManifestLink
		reason string
	}{
		{"no links", nil, dirtyChainFallbackParentManifestMissing},
		{"hop without a manifest", []chainManifestLink{root, {}, d2}, dirtyChainFallbackParentManifestMissing},
		{"root is a delta", []chainManifestLink{d1, d2}, dirtyChainFallbackParentManifestMissing},
		{"unsupported version", []chainManifestLink{func() chainManifestLink { l := root; l.Meta.ManifestVersion = 99; return l }()}, dirtyChainFallbackParentManifestMissing},
		{"entry count disagrees", []chainManifestLink{func() chainManifestLink { l := root; l.Meta.EntryCount = 7; return l }()}, dirtyChainFallbackParentManifestMissing},
		{"policy differs along the chain", []chainManifestLink{root, func() chainManifestLink { l := d1; l.Meta.PolicyDigest = "q"; return l }()}, dirtyChainFallbackPolicyChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, reason := resolveChainLinks(tc.links...); reason != tc.reason {
				t.Fatalf("reason=%q want %q", reason, tc.reason)
			}
		})
	}

	// A full link mid-chain restarts the fold.
	compacted := full(manifestSpec{"z.go": "present:1"})
	if got, _ := resolveChainLinks(root, d1, compacted); !reflect.DeepEqual(got, manifestSpec{"z.go": "present:1"}.resolved("p")) {
		t.Fatalf("full link did not restart the fold: %+v", got)
	}
}

func TestManifestDeltaEntriesResolveToNext(t *testing.T) {
	parent := manifestSpec{"a.go": "present:1", "b.go": "present:1", "c.go": "absent", "keep.go": "present:k"}
	next := manifestSpec{"a.go": "present:2", "c.go": "absent", "d.go": "present:1", "keep.go": "present:k"}
	delta := manifestDeltaEntries(parent.resolved("p"), next.resolved("p"))
	wantDelta := manifestSpec{"a.go": "present:2", "b.go": "head_equal", "d.go": "present:1"}.entries()
	if !reflect.DeepEqual(delta, wantDelta) {
		t.Fatalf("delta=%+v want %+v", delta, wantDelta)
	}
	folded := resolveChainManifest(parent.entries(), delta)
	folded.policy = "p"
	if !reflect.DeepEqual(folded, next.resolved("p")) {
		t.Fatalf("parent+delta=%+v want %+v", folded, next.resolved("p"))
	}
}

func TestDependencyManifestPathClassification(t *testing.T) {
	for p, want := range map[string]bool{
		"go.mod": true, "sub/go.mod": true, "go.sum": true, "go.work": true,
		"package.json": true, "web/package-lock.json": true, "Cargo.toml": true,
		"tsconfig.json": true, "apps/x/tsconfig.app.json": true, "jsconfig.json": true,
		".gitignore": true, "pkg/.gortexignore": true, ".gortex.yaml": true,
		"pkg/a/a.go": false, "README.md": false, "config.json": false, "gomod.go": false,
	} {
		if got := dependencyManifestPath(p); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
}

func TestDirtyManifestPolicyDigestTracksPolicyIdentity(t *testing.T) {
	b := &SparseGenerationBuilder{}
	base := GenerationIdentity{ConfigHash: "c", ExtractorVersions: "e", ResolverVersion: "r", DependencyRevision: "d", TreeOID: "t"}
	digest := b.dirtyManifestPolicyDigest(base)
	if digest == "" || digest != b.dirtyManifestPolicyDigest(base) {
		t.Fatal("policy digest is not deterministic")
	}
	// The sample-side fields never enter the digest.
	sampled := base
	sampled.LowerViewFingerprint = "other"
	sampled.BaseGenerationID = 42
	sampled.ProvenanceCommitOID = "other"
	if b.dirtyManifestPolicyDigest(sampled) != digest {
		t.Fatal("sample identity moved the policy digest")
	}
	for name, mutate := range map[string]func(*GenerationIdentity){
		"config":     func(g *GenerationIdentity) { g.ConfigHash = "c2" },
		"extractors": func(g *GenerationIdentity) { g.ExtractorVersions = "e2" },
		"resolver":   func(g *GenerationIdentity) { g.ResolverVersion = "r2" },
		"deps":       func(g *GenerationIdentity) { g.DependencyRevision = "d2" },
		"head tree":  func(g *GenerationIdentity) { g.TreeOID = "t2" },
	} {
		changed := base
		mutate(&changed)
		if b.dirtyManifestPolicyDigest(changed) == digest {
			t.Errorf("%s did not move the policy digest", name)
		}
	}
}
