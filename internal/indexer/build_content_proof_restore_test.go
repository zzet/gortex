package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// A sample-pinned build's reads of a path its sample does not decide (a clean
// file, a path the sample never saw) are proven by the working copy the
// fence's full sample finds, and its reads of a path the sample does decide
// (present with its bytes, or absent) by the sample. Each case below moves a
// path after the sample so that a reader of the build sees another state —
// other bytes, a file that the sample does not hold, a file gone — and then,
// in most cases, puts the sample's state back before the fence. The working
// copy then fingerprints as the sample again, so a fence that trusted the
// fingerprints alone would publish a payload of a state that never existed,
// under that sample, and no later cycle would ever rebuild it. Every case
// must tear the build instead.

// restoreBuild is one sample-pinned build of repoDir against before, on the
// daemon's builder shape (the contract-core runtime). saver, when set, is
// armed on the Go extractor; barrier runs at the prepublish fence.
func restoreBuild(t *testing.T, store *store_sqlite.Store, repoDir string, sampler *gitstate.DirtySampler, before gitstate.DirtySnapshot, saver *savingExtractor, barrier func()) (int64, error) {
	t.Helper()
	builder := builderNewBuilder(store)
	if saver != nil {
		registry := builderRegistry()
		goExtractor, ok := registry.GetByLanguage("go")
		if !ok {
			t.Fatal("no go extractor")
		}
		saver.Extractor = goExtractor
		saver.armed.Store(true)
		registry.Register(saver)
		builder.Registry = registry
	}
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	generation, _, err := builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		Sampler:      sampler,
		before:       &before,
		buildBarrier: barrier,
	})
	if saver != nil && saver.armed.Load() {
		t.Fatal("the save never landed: the case tests nothing")
	}
	return generation, err
}

// sampleOf takes repoDir's sample through a sampler of its own.
func sampleOf(t *testing.T, repoDir string) (*gitstate.DirtySampler, gitstate.DirtySnapshot) {
	t.Helper()
	sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sampler, before
}

// wantTorn fails unless the build was refused.
func wantTorn(t *testing.T, what string, generation int64, err error) {
	t.Helper()
	if !errors.Is(err, ErrDirtySnapshotChanged) {
		t.Fatalf("%s: generation %d, err %v; want the build torn", what, generation, err)
	}
}

// A clean file (the root go.mod, which the per-save module relink reads) is
// saved with other bytes after the sample, read by the build in that state,
// and restored to its committed bytes before the fence.
func TestACleanFileReadInAnotherStateAndRestoredTearsTheBuild(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := sideInputCheckout(t, store)
	goMod := filepath.Join(repoDir, "go.mod")
	original, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatal(err)
	}
	sampler, before := sampleOf(t, repoDir)
	saver := &savingExtractor{trigger: "a.go", path: goMod, line: "// moved\n"}
	saver.onSave = func() {
		_ = os.WriteFile(goMod, []byte("module example.com/fixture\n\ngo 1.22\n\nrequire example.com/other v2.0.0\n"), 0o644)
	}
	generation, err := restoreBuild(t, store, repoDir, sampler, before, saver, func() {
		if err := os.WriteFile(goMod, original, 0o644); err != nil {
			t.Error(err)
		}
	})
	wantTorn(t, "a build that read go.mod in another state, restored before the fence", generation, err)
}

// A file the sample holds deleted is recreated after the sample with other
// declarations, parsed by the build, and deleted again before the fence —
// alone, so the working copy is the sample's again, or while a file the
// build never read is saved, so the fence's full sample differs and only
// what the build read decides.
func TestAFileTheSampleHoldsDeletedReadAndDeletedAgainTearsTheBuild(t *testing.T) {
	for _, tc := range []struct {
		name      string
		alsoSaved bool
	}{{"alone", false}, {"while an unread file is saved", true}} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "base")
			repoDir := sideInputCheckout(t, store)
			gone := filepath.Join(repoDir, "other", "c.go")
			original, err := os.ReadFile(gone)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(gone); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second / 20) // past gitstate's change-stamp margin
			sampler, before := sampleOf(t, repoDir)
			if err := os.WriteFile(gone, append(original, []byte("\nfunc Phantom() {}\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
			generation, err := restoreBuild(t, store, repoDir, sampler, before, nil, func() {
				_ = os.Remove(gone)
				if tc.alsoSaved {
					if err := appendToFile(filepath.Join(repoDir, "docs", "notes.md"), "more\n"); err != nil {
						t.Error(err)
					}
				}
			})
			wantTorn(t, "a build that parsed a file its sample holds deleted", generation, err)
		})
	}
}

// tsAliasCheckout is a committed checkout whose script imports through a
// multi-target tsconfig alias (@x/* → a/*, b/*) that resolves to b/foo.ts,
// the only target present, with a Go edit and a script edit in the working
// tree. The alias collection is loaded (and cached) before the sample, so the
// build resolves the import through the cached collection's existence probes.
func tsAliasCheckout(t *testing.T, store *store_sqlite.Store) string {
	t.Helper()
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "checkout")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	for name, body := range map[string]string{
		"go.mod":         "module example.com/fixture\n\ngo 1.22\n",
		"tsconfig.json":  "{\"compilerOptions\":{\"baseUrl\":\".\",\"paths\":{\"@x/*\":[\"a/*\",\"b/*\"]}}}\n",
		"b/foo.ts":       "export function foo(): number { return 1 }\n",
		"web/app.ts":     "import { foo } from \"@x/foo\";\n\nexport const v = foo();\n",
		"pkg/a.go":       "package pkg\n\nfunc A() int { return 1 }\n",
		"a/.keep":        "\n",
		"docs/notes.txt": "x\n",
	} {
		if err := os.MkdirAll(filepath.Join(repoDir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		builderWriteFile(t, repoDir, name, body)
	}
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	builderIndex(t, store, repoDir)
	if loadTSAliasCollection(repoDir) == nil {
		t.Fatal("no alias scope loaded: the case tests nothing")
	}
	builderWriteFile(t, repoDir, "pkg/a.go", "package pkg\n\nfunc A() int { return 2 }\n")
	builderWriteFile(t, repoDir, "web/app.ts", "import { foo } from \"@x/foo\";\n\nexport const v = foo() + 1;\n")
	time.Sleep(2 * time.Second / 20) // past gitstate's change-stamp margin
	return repoDir
}

// A target of a multi-target alias is created after the sample, so the
// build's resolution probes find it and resolve the import there; the sample
// does not hold it. Whether it is removed again before the fence or kept, the
// build must not publish under the sample.
func TestAnAliasTargetCreatedAfterTheSampleTearsTheBuild(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove bool
	}{{"removed before the fence", true}, {"kept", false}} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "base")
			repoDir := tsAliasCheckout(t, store)
			created := filepath.Join(repoDir, "a", "foo.ts")
			sampler, before := sampleOf(t, repoDir)
			// The save creates the target (and appends to it): nothing else
			// moves, so once it is removed the working copy is the sample's.
			saver := &savingExtractor{trigger: "a.go", path: created}
			saver.onSave = func() {
				_ = os.WriteFile(created, []byte("export function foo(): number { return 2 }\n"), 0o644)
			}
			generation, err := restoreBuild(t, store, repoDir, sampler, before, saver, func() {
				if tc.remove {
					_ = os.Remove(created)
				}
			})
			wantTorn(t, "a build whose alias resolution found a target the sample does not hold", generation, err)
		})
	}
}

// A dependent the build re-derives (pkg2/x.go calls into a file the working
// tree deletes, or whose callee it renames) is removed after the sample, so
// the build finds it gone (the deleted file's importer scan, the dependents
// walk) and drops or skips its rows. Whether it is restored before the fence
// or stays removed, the build must not publish under a sample that holds it.
func TestADependentGoneAtItsReadTearsTheBuild(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rename  bool
		restore bool
	}{
		{"importer of a deleted file, restored before the fence", false, true},
		{"importer of a deleted file, still removed", false, false},
		{"caller of a renamed function, restored before the fence", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "base")
			builderIsolateGit(t)
			repoDir := builderTempDir(t, "checkout")
			builderGit(t, repoDir, "init", "--initial-branch=main")
			for name, body := range map[string]string{
				"go.mod":     "module example.com/fixture\n\ngo 1.22\n",
				"other/c.go": "package other\n\nfunc C() int { return 1 }\n",
				"pkg2/x.go":  "package pkg2\n\nimport \"example.com/fixture/other\"\n\nfunc X() int { return other.C() }\n",
				"pkg/a.go":   "package pkg\n\nfunc A() int { return 1 }\n",
			} {
				if err := os.MkdirAll(filepath.Join(repoDir, filepath.Dir(name)), 0o755); err != nil {
					t.Fatal(err)
				}
				builderWriteFile(t, repoDir, name, body)
			}
			builderGit(t, repoDir, "add", "-A")
			builderGit(t, repoDir, "commit", "-m", "A")
			builderIndex(t, store, repoDir)
			if tc.rename {
				builderWriteFile(t, repoDir, "other/c.go", "package other\n\nfunc C2() int { return 1 }\n")
			} else if err := os.Remove(filepath.Join(repoDir, "other", "c.go")); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second / 20) // past gitstate's change-stamp margin
			sampler, before := sampleOf(t, repoDir)
			x := filepath.Join(repoDir, "pkg2", "x.go")
			xBytes, err := os.ReadFile(x)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(x); err != nil {
				t.Fatal(err)
			}
			generation, err := restoreBuild(t, store, repoDir, sampler, before, nil, func() {
				if tc.restore {
					_ = os.WriteFile(x, xBytes, 0o644)
				}
			})
			wantTorn(t, "a build that found a dependent its sample holds gone", generation, err)
		})
	}
}

// A file the sample holds edited is removed after the sample and before the
// plan checks the change set against the disk, which publishes it as a
// deletion; it is restored with the sample's bytes before the fence.
func TestAChangeDemotedToADeletionAndRestoredTearsTheBuild(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := sideInputCheckout(t, store)
	edited := filepath.Join(repoDir, "pkg", "a.go")
	sampler, before := sampleOf(t, repoDir)
	if err := os.Remove(edited); err != nil {
		t.Fatal(err)
	}
	generation, err := restoreBuild(t, store, repoDir, sampler, before, nil, func() {
		if err := os.WriteFile(edited, []byte(sideInputEdit), 0o644); err != nil {
			t.Error(err)
		}
	})
	wantTorn(t, "a build that published an edited file of its sample as deleted", generation, err)
}

// The scoped walk of a build that proves its reads records an explicit path
// it finds gone as a read of no file: the path's rows leave the payload on
// that finding, so the fence must be able to refute it (a sample that holds
// the file contradicts it, and a working copy that holds it again at the
// fence refutes it).
func TestTheScopedWalkRecordsAnExplicitPathItFindsGone(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "a.go"), "package p\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package p\n\nfunc B() { A() }\n")
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	if _, err := idx.Index(dir); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(dir, "b.go")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	proof := newBuildContentProof(dir, gitstate.DirtySnapshot{})
	idx.contentProof = proof
	defer func() { idx.contentProof = nil }()
	if _, err := idx.IncrementalReindexPaths(dir, []string{gone}); err != nil {
		t.Fatal(err)
	}
	proof.mu.Lock()
	got, recorded := proof.parsed["b.go"]
	// The deletion detection checks the path again before it evicts it: a
	// path found present again there keeps its rows, so that answer is
	// recorded too.
	probed, checked := proof.probed["b.go"]
	proof.mu.Unlock()
	if !recorded || got.sha256 != absentContent {
		t.Fatalf("the walk's finding that b.go is gone = %+v (recorded %v), want a read of no file", got, recorded)
	}
	if !checked || probed.found {
		t.Fatalf("the deletion detection's check of b.go = %+v (recorded %v), want a probe that found no file", probed, checked)
	}
}

// A prefix read of a file (the language sniff of an unknown extension) is a
// read of unknown bytes: the file is held to its change stamp at the fence,
// even when a whole-file read of it records the sample's bytes.
func TestASniffPrefixReadLeavesTheFileUnproven(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "tool")
	writeFile(t, script, "#!/bin/sh\necho hi\n")
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	proof := newBuildContentProof(dir, gitstate.DirtySnapshot{})
	idx.contentProof = proof
	if prefix := idx.readSniffPrefix(script); len(prefix) == 0 {
		t.Fatal("no prefix read")
	}
	proof.recordRead(script, []byte("#!/bin/sh\necho hi\n"), nil)
	proof.mu.Lock()
	got, recorded := proof.parsed["tool"]
	proof.mu.Unlock()
	if !recorded || got.sha256 != "" || got.conflict {
		t.Fatalf("a sniffed then read file = %+v (recorded %v), want read with unknown bytes", got, recorded)
	}
	if _, proven := proof.proven()["tool"]; proven {
		t.Fatal("a sniffed file is proven by its bytes")
	}
}

// An ignore file the walk gate reads is moved after the sample so that it
// excludes the edited file, and put back before the fence: the build drops
// the edit's rows on a state that never existed, the working copy is the
// sample's again at the fence, and the only trace left is the ignore file's
// change stamp (or its directory's). The build must be torn.
func TestAnIgnoreFileMovedAndRestoredTearsTheBuild(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		created          bool
	}{
		{"root ignore file rewritten to exclude the edit", ".gortexignore", "pkg/a.go\n", false},
		{"ignore file created beside the edit", "pkg/.ignore", "a.go\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "base")
			repoDir := sideInputCheckout(t, store)
			ignore := filepath.Join(repoDir, filepath.FromSlash(tc.path))
			original, err := os.ReadFile(ignore)
			if err != nil && !tc.created {
				t.Fatal(err)
			}
			sampler, before := sampleOf(t, repoDir)
			if err := os.WriteFile(ignore, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			generation, err := restoreBuild(t, store, repoDir, sampler, before, nil, func() {
				if tc.created {
					_ = os.Remove(ignore)
				} else if err := os.WriteFile(ignore, original, 0o644); err != nil {
					t.Error(err)
				}
			})
			if !sampleStampsTrusted(t, sampler, before) {
				return // without change stamps only the full sample decides
			}
			wantTorn(t, "a build that read "+tc.path+" in another state, restored before the fence", generation, err)
		})
	}
}

// The ownership pass reads CODEOWNERS from the working copy on every delta
// build: the first of .github/CODEOWNERS, CODEOWNERS and docs/CODEOWNERS that
// reads. A committed CODEOWNERS rewritten after the sample to give the edited
// file another owner, and restored before the fence, built a state that never
// existed — the working copy is the sample's again at the fence, and the
// rewrite leaves its directory's stamp alone — so the build must be torn. Left
// alone, the build publishes the owner the committed file names.
func TestACodeownersMovedAndRestoredTearsTheBuild(t *testing.T) {
	const (
		committed = "* @fixture/base\n"
		moved     = "* @fixture/base\npkg/a.go @fixture/other\n"
	)
	for _, tc := range []struct {
		name, path string
		move       bool
	}{
		{"untouched", "CODEOWNERS", false},
		{"rewritten at .github/CODEOWNERS", ".github/CODEOWNERS", true},
		{"rewritten at CODEOWNERS", "CODEOWNERS", true},
		{"rewritten at docs/CODEOWNERS", "docs/CODEOWNERS", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "base")
			repoDir := sideInputCheckoutWith(t, store, map[string]string{tc.path: committed})
			owners := filepath.Join(repoDir, filepath.FromSlash(tc.path))
			sampler, before := sampleOf(t, repoDir)
			if tc.move {
				if err := os.WriteFile(owners, []byte(moved), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			generation, err := restoreBuild(t, store, repoDir, sampler, before, nil, func() {
				if !tc.move {
					return
				}
				if err := os.WriteFile(owners, []byte(committed), 0o644); err != nil {
					t.Error(err)
				}
			})
			if after, sampleErr := sampler.Sample(context.Background()); sampleErr != nil || after.Fingerprint != before.Fingerprint {
				t.Fatalf("the restore did not bring the checkout back to its sample (%v)", sampleErr)
			}
			if tc.move {
				wantTorn(t, "a build that read "+tc.path+" in another state, restored before the fence", generation, err)
				return
			}
			if err != nil || generation <= 0 {
				t.Fatalf("a build of an untouched CODEOWNERS = generation %d, err %v; want it published", generation, err)
			}
			owned := false
			for row := range generationPayload(store, generation) {
				if strings.Contains(row, "fixture/base") && strings.Contains(row, "-"+string(graph.EdgeOwns)+"->") && strings.HasSuffix(row, "pkg/a.go") {
					owned = true
				}
			}
			if !owned {
				t.Fatal("the published build carries no owner for pkg/a.go: the ownership pass read nothing")
			}
		})
	}
}

// sampleStampsTrusted reports whether the checkout's filesystem gives the
// change stamps the fence proves unrecorded reads by.
func sampleStampsTrusted(t *testing.T, sampler *gitstate.DirtySampler, before gitstate.DirtySnapshot) bool {
	t.Helper()
	_, proven, err := sampler.ConfirmReadSetSettled(context.Background(), before, nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return proven
}

// The guard on the read set's completeness, whichever reader a build reaches:
// every path of the checkout is moved in turn after the build's sample — a
// file rewritten to content that changes what a reader of it builds, a file
// removed, an ignore file that ignores everything or a module manifest
// created in a directory — and put back before the fence, so the working copy
// is the sample's again and fingerprints as it. A build that read the moved
// state through any reader, recorded or not, built a state that never
// existed: it must be torn (or fail), or publish exactly what a build of the
// unmoved checkout publishes. A reader that slips past the proof shows here
// as a published payload that differs.
func TestASampledBuildPublishesNoStateAMovedAndRestoredPathShowedIt(t *testing.T) {
	reference := builderOpenStore(t, "reference")
	referenceDir := sideInputCheckout(t, reference)
	sampler, before := sampleOf(t, referenceDir)
	if !sampleStampsTrusted(t, sampler, before) {
		t.Skip("the checkout's filesystem gives no change stamps")
	}
	generation, err := restoreBuild(t, reference, referenceDir, sampler, before, nil, nil)
	if err != nil || generation <= 0 {
		t.Fatalf("the reference build failed: generation %d, %v", generation, err)
	}
	want := generationPayload(reference, generation)
	if len(want) == 0 {
		t.Fatal("the reference build published no rows")
	}

	type move struct {
		name    string
		path    string
		apply   func(abs string) error
		restore func(abs string, original []byte, existed bool) error
	}
	putBack := func(abs string, original []byte, existed bool) error {
		if !existed {
			return os.Remove(abs)
		}
		return os.WriteFile(abs, original, 0o644)
	}
	var moves []move
	files := make([]string, 0, len(sideInputFiles))
	dirs := map[string]bool{".": true}
	for name := range sideInputFiles {
		files = append(files, name)
		dirs[filepath.ToSlash(filepath.Dir(name))] = true
	}
	sort.Strings(files)
	for _, name := range files {
		body := []byte(mutatedBody(name))
		moves = append(moves,
			move{"rewritten " + name, name, func(abs string) error { return os.WriteFile(abs, body, 0o644) }, putBack},
			move{"removed " + name, name, os.Remove, putBack})
	}
	dirList := make([]string, 0, len(dirs))
	for dir := range dirs {
		dirList = append(dirList, dir)
	}
	sort.Strings(dirList)
	for _, dir := range dirList {
		for _, created := range []struct{ name, body string }{
			{".ignore", "*\n"},
			{"go.mod", "module example.com/moved\n\ngo 1.22\n"},
		} {
			rel := path.Join(dir, created.name)
			if _, committed := sideInputFiles[rel]; committed {
				continue
			}
			body := []byte(created.body)
			moves = append(moves, move{"created " + rel, rel, func(abs string) error { return os.WriteFile(abs, body, 0o644) }, putBack})
		}
	}
	// The named root files every build reads beside the walk's: a CODEOWNERS
	// at each location the ownership pass tries, giving every file an owner.
	for _, rel := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		if _, committed := sideInputFiles[rel]; committed {
			continue
		}
		moves = append(moves, move{"created " + rel, rel,
			func(abs string) error {
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					return err
				}
				return os.WriteFile(abs, []byte("* @moved/owners\n"), 0o644)
			},
			func(abs string, original []byte, existed bool) error {
				if err := putBack(abs, original, existed); err != nil {
					return err
				}
				// The directory the move made, if any: an empty one is
				// nothing to the sample, but the checkout had none.
				if dir := filepath.Dir(abs); filepath.Base(dir) == ".github" {
					return os.Remove(dir)
				}
				return nil
			}})
	}

	published, torn := 0, 0
	for _, mv := range moves {
		t.Run(mv.name, func(t *testing.T) {
			store := builderOpenStore(t, "moved")
			repoDir := sideInputCheckout(t, store)
			abs := filepath.Join(repoDir, filepath.FromSlash(mv.path))
			original, readErr := os.ReadFile(abs)
			existed := readErr == nil
			sampler, before := sampleOf(t, repoDir)
			if err := mv.apply(abs); err != nil {
				t.Fatal(err)
			}
			restored := false
			restore := func() {
				restored = true
				if err := mv.restore(abs, original, existed); err != nil {
					t.Error(err)
				}
			}
			generation, err := restoreBuild(t, store, repoDir, sampler, before, nil, restore)
			if !restored {
				// The build stopped before its fence (a read of the moved
				// state failed it): nothing was published.
				restore()
			}
			if after, sampleErr := sampler.Sample(context.Background()); sampleErr != nil || after.Fingerprint != before.Fingerprint {
				t.Fatalf("the restore did not bring the checkout back to its sample (%v)", sampleErr)
			}
			if err != nil {
				if row, found, _ := store.Catalog().GetViewGeneration(context.Background(), generation); generation > 0 && found && row.PublishedAt != 0 {
					t.Fatalf("a failed build published generation %d: %v", generation, err)
				}
				torn++
				t.Logf("not published: %v", err)
				return
			}
			published++
			t.Logf("published generation %d", generation)
			got := generationPayload(store, generation)
			if missing, extra := payloadDiff(want, got); len(missing) > 0 || len(extra) > 0 {
				t.Fatalf("a build published a state that never existed (%s, restored before the fence):\nmissing %v\nextra %v", mv.name, missing, extra)
			}
		})
	}
	if published == 0 || torn == 0 {
		t.Fatalf("%d builds published and %d were torn: the guard compares nothing or moves nothing the builds read", published, torn)
	}
}

// mutatedBody is content for a checkout file that changes what a reader of it
// would build.
func mutatedBody(name string) string {
	switch base := path.Base(name); {
	case strings.HasSuffix(base, ".go"):
		return fmt.Sprintf("package mutated\n\nimport \"example.com/mutated/%s\"\n\nfunc Mutated() { %s.M() }\n", path.Dir(name), path.Base(path.Dir(name)))
	case strings.HasSuffix(base, ".js"):
		return "import pad from \"right-pad\";\n\nexport const mutated = pad(\"m\", 9);\n"
	case base == "go.mod":
		return "module example.com/mutated\n\ngo 1.22\n\nrequire example.com/other v2.0.0\n"
	case base == "go.sum":
		return "example.com/other v2.0.0 h1:BBBB\n"
	case strings.HasSuffix(base, ".json"):
		return "{\"name\":\"mutated\",\"dependencies\":{\"right-pad\":\"^2.0.0\"},\"packages\":{\"node_modules/right-pad\":{\"version\":\"2.0.0\"}}}\n"
	case base == ".gortexignore" || base == ".ignore" || base == ".rgignore":
		return "*\n"
	}
	return "# mutated\n"
}

// A manifest the read set claims by name is read by readers beyond the proof
// too (the project-name detection reads the root go.mod), so one reader's
// recorded bytes do not stand for every read of it: a root go.mod a recorded
// reader took with the sample's bytes is still held to its change stamp, and
// its recorded bytes are no content proof.
func TestAManifestClaimedByNameIsProvenByItsStampAlone(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := sideInputCheckout(t, store)
	sampler, before := sampleOf(t, repoDir)
	if !sampleStampsTrusted(t, sampler, before) {
		t.Skip("the checkout's filesystem gives no change stamps")
	}
	goMod := filepath.Join(repoDir, "go.mod")
	held, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatal(err)
	}
	proof := newBuildContentProof(repoDir, before)
	proof.recordRead(goMod, held, nil)
	if _, proven := proof.proven()["go.mod"]; proven {
		t.Fatal("a manifest claimed by name is proven by one reader's bytes")
	}
	proof.readSet(context.Background(), []string{"pkg/a.go"}, nil)
	if moved, err := proof.unrecordedMoved(context.Background(), sampler, before); err != nil || moved != "" {
		t.Fatalf("nothing moved, yet unrecordedMoved = %q, %v", moved, err)
	}
	// Moved and restored while another reader took it: the same bytes now.
	if err := os.WriteFile(goMod, []byte("module example.com/moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goMod, held, 0o644); err != nil {
		t.Fatal(err)
	}
	if moved, err := proof.unrecordedMoved(context.Background(), sampler, before); err != nil || moved != "go.mod" {
		t.Fatalf("a root go.mod moved and restored during the build = %q, %v; want go.mod held to its stamp", moved, err)
	}
}

// The fence holds a manifest a reader recorded to its change stamp: a root
// go.mod moved and put back after the build read it (its bytes are the
// sample's again, so the recorded read still holds) may have been read in
// between by a reader that records nothing, which the fence cannot rule out.
// The build is torn rather than published.
func TestAManifestMovedAndRestoredAfterItsReadTearsTheBuild(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := sideInputCheckout(t, store)
	goMod := filepath.Join(repoDir, "go.mod")
	held, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatal(err)
	}
	sampler, before := sampleOf(t, repoDir)
	if !sampleStampsTrusted(t, sampler, before) {
		t.Skip("the checkout's filesystem gives no change stamps")
	}
	generation, err := restoreBuild(t, store, repoDir, sampler, before, nil, func() {
		if err := os.WriteFile(goMod, []byte("module example.com/moved\n"), 0o644); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(goMod, held, 0o644); err != nil {
			t.Error(err)
		}
	})
	wantTorn(t, "a build whose root go.mod moved and was restored after its read", generation, err)
}
