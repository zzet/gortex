package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// sideInputFiles is a committed checkout whose working-tree edit is read
// beside files it does not parse: the per-directory ignore files the walk
// gate reads at every directory above the edit, the module manifest and the
// lockfiles the per-save module relink reads at the root (the edited Go file
// imports a module, so the relink runs), and files nothing should read. The
// edited script imports an npm package: its resolution reads the npm
// manifests through the build's proof and leaves the proof complete.
var sideInputFiles = map[string]string{
	"go.mod":            "module example.com/fixture\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n",
	"go.sum":            "example.com/dep v1.0.0 h1:AAAA\n",
	".gortexignore":     "# nothing is ignored\n",
	"package.json":      "{\"name\":\"fixture\",\"dependencies\":{\"left-pad\":\"^1.0.0\"}}\n",
	"package-lock.json": "{\"name\":\"fixture\",\"lockfileVersion\":3,\"packages\":{\"node_modules/left-pad\":{\"version\":\"1.3.0\"}}}\n",
	"pkg/a.go":          "package pkg\n\nimport \"example.com/dep/lib\"\n\nfunc A() int { return lib.B() }\n",
	"pkg/b.go":          "package pkg\n\nfunc B() int { return 1 }\n",
	"other/c.go":        "package other\n\nfunc C() {}\n",
	"docs/notes.md":     "notes\n",
	"web/app.js":        "import pad from \"left-pad\";\n\nexport const padded = pad(\"a\", 2);\n",
}

// sideInputEdit and sideInputScriptEdit are the working-tree edits of pkg/a.go
// and web/app.js.
const (
	sideInputEdit       = "package pkg\n\nimport \"example.com/dep/lib\"\n\nfunc A() int { return lib.B() + 1 }\n"
	sideInputScriptEdit = "import pad from \"left-pad\";\n\nexport const padded = pad(\"b\", 3);\n"
)

func sideInputCheckout(t *testing.T, store *store_sqlite.Store) string {
	t.Helper()
	return sideInputCheckoutWith(t, store, nil)
}

// sideInputCheckoutWith is sideInputCheckout with extra files committed
// beside sideInputFiles.
func sideInputCheckoutWith(t *testing.T, store *store_sqlite.Store, extra map[string]string) string {
	t.Helper()
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "checkout")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	for name, body := range sideInputFiles {
		if err := os.MkdirAll(filepath.Join(repoDir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		builderWriteFile(t, repoDir, name, body)
	}
	for name, body := range extra {
		if err := os.MkdirAll(filepath.Join(repoDir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		builderWriteFile(t, repoDir, name, body)
	}
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	builderIndex(t, store, repoDir)
	builderWriteFile(t, repoDir, "pkg/a.go", sideInputEdit)
	builderWriteFile(t, repoDir, "web/app.js", sideInputScriptEdit)
	time.Sleep(2 * time.Second / 20) // past gitstate's change-stamp margin
	return repoDir
}

// sampledBuild is one working-tree build of a checkout against a sample
// taken before it, on the daemon's builder shape (the contract-core
// runtime), and what its prepublish fence was handed.
type sampledBuild struct {
	generation int64
	err        error
	set        buildReadSet
	complete   bool
	logs       *observer.ObservedLogs
}

func buildAgainstSample(t *testing.T, store *store_sqlite.Store, repoDir string, sampler *gitstate.DirtySampler, before gitstate.DirtySnapshot) sampledBuild {
	t.Helper()
	var (
		mu  sync.Mutex
		out sampledBuild
	)
	seen := func(set buildReadSet, complete bool) {
		mu.Lock()
		out.set, out.complete = set, complete
		mu.Unlock()
	}
	prepublishReadSetSeen.Store(&seen)
	defer prepublishReadSetSeen.Store(nil)
	core, logs := observer.New(zap.InfoLevel)
	builder := builderNewBuilder(store)
	builder.Logger = zap.New(core)
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
	})
	mu.Lock()
	defer mu.Unlock()
	out.generation, out.err, out.logs = generation, err, logs
	return out
}

// readSetHolds reports whether the fence's read set holds rel, as a file or
// under a directory it reads whole.
func readSetHolds(set buildReadSet, rel string) bool {
	for _, f := range set.files {
		if f == rel {
			return true
		}
	}
	for _, d := range set.dirs {
		if path.Dir(rel) == d {
			return true
		}
	}
	return false
}

// Every working-copy file a sample-pinned build reads beside the files it
// parses is in the read set its fence confirms: a per-directory ignore file
// the walk gate reads (or finds absent) at a directory above the edit, and a
// root manifest or lockfile the per-save module relink reads (or finds
// absent). Each case moves one such input after the build's sample and
// before the build reads it. The build parsed the sample's bytes for the
// edited file, so only the moved input can refuse it: the fence's read set
// names it, a full sample sees the move, and the build is torn rather than
// published under a sample its payload does not describe.
func TestASampledBuildFencesTheFilesItReadsBesideItsParse(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
	}{
		{"root ignore file edited", ".gortexignore", "pkg/a.go\n"},
		{"ignore file created above the edit", "pkg/.ignore", "a.go\n"},
		{"relinked lockfile edited", "package-lock.json", "{\"name\":\"fixture\",\"lockfileVersion\":3,\"packages\":{}}\n"},
		{"relinked lockfile created", "yarn.lock", "left-pad@^1.0.0:\n  version \"1.3.0\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "base")
			repoDir := sideInputCheckout(t, store)
			sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
			if err != nil {
				t.Fatal(err)
			}
			before, err := sampler.Sample(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			capability, err := sampler.ConfirmReadSet(context.Background(), before, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			builderWriteFile(t, repoDir, tc.path, tc.body)
			got := buildAgainstSample(t, store, repoDir, sampler, before)
			if !errors.Is(got.err, ErrDirtySnapshotChanged) {
				t.Fatalf("a build that read %s after it moved = generation %d, err %v; want torn", tc.path, got.generation, got.err)
			}
			if !capability.Confirmed {
				return // without change stamps only the full sample decides
			}
			if !got.complete || !readSetHolds(got.set, tc.path) {
				t.Fatalf("the fence's read set (complete=%v) does not hold %s: files %v dirs %v", got.complete, tc.path, got.set.files, got.set.dirs)
			}
			refused := false
			for _, entry := range got.logs.FilterMessage("indexer: working-tree inputs need a full sample").All() {
				if reason, _ := entry.ContextMap()["reason"].(string); strings.Contains(reason, tc.path) || strings.Contains(reason, path.Dir(tc.path)) {
					refused = true
				}
			}
			if !refused {
				t.Fatalf("the read set did not refuse the build at %s", tc.path)
			}
		})
	}
}

// The guard on the read set itself: whatever a sample-pinned build reads, it
// reads through its proof. Two identical checkouts are built from their own
// samples. In the second, every working-copy file outside the read set the
// first build's fence confirmed is rewritten after the sample — an ignore
// file to ignore everything, a manifest to another module, a source file to
// other declarations — and the build runs against the unmoved sample. Its
// fence confirms by the same read set, so the build publishes; it must
// publish exactly the first build's payload. A reader that took any of the
// rewritten files would have built another state and published it under a
// sample it does not describe.
func TestASampledBuildPublishesNothingItReadOutsideItsReadSet(t *testing.T) {
	// build builds a fresh checkout against its own sample; with avoid set,
	// it first rewrites (or, with remove, deletes) every file outside that
	// read set.
	build := func(t *testing.T, name string, avoid *buildReadSet, remove bool) (*store_sqlite.Store, sampledBuild, []string) {
		store := builderOpenStore(t, name)
		repoDir := sideInputCheckout(t, store)
		sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
		if err != nil {
			t.Fatal(err)
		}
		before, err := sampler.Sample(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if capability, err := sampler.ConfirmReadSet(context.Background(), before, nil, nil); err != nil || !capability.Confirmed {
			t.Skipf("the checkout's filesystem gives no change stamps: %+v %v", capability, err)
		}
		var rewritten []string
		if avoid != nil {
			rewritten = rewriteOutsideReadSet(t, repoDir, *avoid, remove)
		}
		return store, buildAgainstSample(t, store, repoDir, sampler, before), rewritten
	}
	firstStore, first, _ := build(t, "first", nil, false)
	if first.err != nil || first.generation <= 0 {
		t.Fatalf("the reference build failed: generation %d, %v", first.generation, first.err)
	}
	if !first.complete {
		t.Fatal("the reference build's read set is not a complete proof: the guard would test nothing")
	}
	want := generationPayload(firstStore, first.generation)
	if len(want) == 0 {
		t.Fatal("the reference build published no rows")
	}
	// In place, a reader that took the bytes of a file outside the read set
	// shows; removed, so does one that only checked whether it exists.
	for _, mode := range []struct {
		name   string
		remove bool
	}{{"rewritten", false}, {"removed", true}} {
		t.Run(mode.name, func(t *testing.T) {
			secondStore, second, rewritten := build(t, "second", &first.set, mode.remove)
			if len(rewritten) == 0 {
				t.Fatal("nothing outside the read set was rewritten: the guard would test nothing")
			}
			t.Logf("read set files %v dirs %v; %s outside it: %v", first.set.files, first.set.dirs, mode.name, rewritten)
			if second.err != nil || second.generation <= 0 {
				var reasons []string
				for _, entry := range second.logs.All() {
					if reason, ok := entry.ContextMap()["reason"].(string); ok {
						reasons = append(reasons, entry.Message+": "+reason)
					}
				}
				t.Fatalf("the fence refused a build whose read set held none of the %s files (%v): generation %d, %v; its read set files %v dirs %v; %v",
					mode.name, rewritten, second.generation, second.err, second.set.files, second.set.dirs, reasons)
			}
			got := generationPayload(secondStore, second.generation)
			if missing, extra := payloadDiff(want, got); len(missing) > 0 || len(extra) > 0 {
				t.Fatalf("a build published rows it read from files outside its read set (%s %v):\nmissing %v\nextra %v", mode.name, rewritten, missing, extra)
			}
		})
	}
}

// rewriteOutsideReadSet rewrites, in place, every regular file of the
// checkout outside set (and outside .git), each to content that changes what
// a reader of it would build, or with remove deletes each (outside the
// directories the read set names files in), and returns them.
func rewriteOutsideReadSet(t *testing.T, repoDir string, set buildReadSet, remove bool) []string {
	t.Helper()
	var rewritten []string
	err := filepath.WalkDir(repoDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || readSetHolds(set, rel) {
			return nil
		}
		if remove {
			if readSetListsDir(set, path.Dir(rel)) {
				// A removal there moves the directory's stamp, which is all
				// that proves the absent files the read set names in it: the
				// fence must take a full sample, and a correct one tears.
				return nil
			}
			if err := os.Remove(p); err != nil {
				return err
			}
			rewritten = append(rewritten, rel)
			return nil
		}
		body := "mutated\n"
		switch name := path.Base(rel); {
		case strings.HasSuffix(name, ".go"):
			body = fmt.Sprintf("package mutated\n\nimport \"example.com/mutated/%s\"\n\nfunc Mutated() { %s.M() }\n", path.Dir(rel), path.Base(path.Dir(rel)))
		case name == "go.mod":
			body = "module example.com/mutated\n\ngo 1.22\n\nrequire example.com/other v2.0.0\n"
		case strings.HasSuffix(name, ".json"):
			body = "{\"name\":\"mutated\",\"dependencies\":{\"right-pad\":\"^2.0.0\"},\"packages\":{\"node_modules/right-pad\":{\"version\":\"2.0.0\"}}}\n"
		case name == ".gortexignore" || name == ".ignore" || name == ".rgignore":
			body = "*\n"
		}
		file, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return err
		}
		if _, err := file.WriteString(body); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		rewritten = append(rewritten, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(rewritten)
	return rewritten
}

// readSetListsDir reports whether set names a file in dir.
func readSetListsDir(set buildReadSet, dir string) bool {
	for _, f := range set.files {
		if path.Dir(f) == dir {
			return true
		}
	}
	return false
}

// generationPayload is a generation's own rows: its node ids and its edges as
// from -kind-> to.
func generationPayload(store *store_sqlite.Store, generation int64) map[string]bool {
	view := store.AtGeneration(generation)
	out := map[string]bool{}
	for _, node := range view.AllNodes() {
		out["node "+node.ID] = true
	}
	for _, edge := range view.AllEdges() {
		out["edge "+edge.From+" -"+string(edge.Kind)+"-> "+edge.To] = true
	}
	return out
}

func payloadDiff(want, got map[string]bool) (missing, extra []string) {
	for row := range want {
		if !got[row] {
			missing = append(missing, row)
		}
	}
	for row := range got {
		if !want[row] {
			extra = append(extra, row)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

// A build that proves its reads reads manifests through provenManifestTree,
// and every answer lands in its read set: a file read by its bytes (or its
// absence), a probe by the directory that decides it, a glob by every
// directory it lists on the way down, the top-level listing by the root and
// each directory it reported. A whole-tree config scan, which no read set
// holds, makes the proof unbounded.
func TestTheManifestTreeOfAProvenBuildRecordsItsReads(t *testing.T) {
	root := builderTempDir(t, "checkout")
	for name, body := range map[string]string{
		"package.json":                       "{}\n",
		"build-a/compile_commands.json":      "[]\n",
		"build-b/sub/compile_commands.json":  "[]\n",
		"include/h.h":                        "\n",
		"src/main.c":                         "\n",
		"notes/nothing-reads-this-directory": "\n",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		builderWriteFile(t, root, name, body)
	}
	proof := newBuildContentProof(root, gitstate.DirtySnapshot{})
	tree := provenManifestTree{diskManifestTree: newDiskManifestTree(root), proof: proof}
	if _, ok := tree.readFile("package.json"); !ok {
		t.Fatal("read package.json")
	}
	if _, ok := tree.readFile("pnpm-workspace.yaml"); ok {
		t.Fatal("read an absent manifest")
	}
	if _, ok := tree.readFile("build-a/compile_commands.json"); !ok {
		t.Fatal("read build-a/compile_commands.json")
	}
	if !tree.isDir("include") || tree.isFile("src/missing.c") {
		t.Fatal("probes answered differently from the working copy")
	}
	if got := tree.matchFiles("build*/compile_commands.json"); len(got) != 1 || got[0] != "build-a/compile_commands.json" {
		t.Fatalf("glob = %v", got)
	}
	if got := tree.topLevelDirs(".h"); !got["include"] {
		t.Fatalf("top-level dirs = %v", got)
	}
	set, _ := buildReadSetFrom(proof.readSet(context.Background(), nil, nil))
	for _, rel := range []string{"package.json", "pnpm-workspace.yaml"} {
		if !slices.Contains(set.files, rel) {
			t.Errorf("read set files %v miss the manifest read %s", set.files, rel)
		}
	}
	proof.mu.Lock()
	read, absent := proof.parsed["package.json"], proof.parsed["pnpm-workspace.yaml"]
	proof.mu.Unlock()
	if read.sha256 != gitstate.BlobSHA256([]byte("{}\n")) || absent.sha256 != absentContent {
		t.Errorf("recorded package.json %+v, pnpm-workspace.yaml %+v; want package.json's bytes and pnpm-workspace.yaml absent", read, absent)
	}
	// An absence is no content to confirm by: the fence holds it by its
	// stamps, and readMoved by the working copy. Nor is a manifest claimed by
	// name (package.json): readers beyond the proof take it too, so its
	// stamp proves it.
	if sums := proof.proven(); sums["build-a/compile_commands.json"] != gitstate.BlobSHA256([]byte("[]\n")) || len(sums) != 1 {
		t.Errorf("proven content = %v, want build-a/compile_commands.json's bytes alone", sums)
	}
	for _, dir := range []string{".", "src", "build-a", "build-b", "include", "notes"} {
		if !slices.Contains(set.dirs, dir) {
			t.Errorf("read set dirs %v miss %s", set.dirs, dir)
		}
	}
	if proof.isUnbounded() {
		t.Fatal("recorded reads made the proof unbounded")
	}
	// Every answer is re-asked at the fence: the same answers hold, and an
	// answer the working copy gives otherwise now is a read it refutes.
	if moved, err := proof.unrecordedMoved(context.Background(), nil, gitstate.DirtySnapshot{}); err != nil || moved != "" {
		t.Fatalf("nothing moved, yet unrecordedMoved = %q, %v", moved, err)
	}
	builderWriteFile(t, root, "src/missing.c", "\n")
	if moved, err := proof.unrecordedMoved(context.Background(), nil, gitstate.DirtySnapshot{}); err != nil || moved != "is src/missing.c a file" {
		t.Fatalf("a probe answered otherwise now = %q, %v; want it refuted", moved, err)
	}
	if err := os.Remove(filepath.Join(root, "src", "missing.c")); err != nil {
		t.Fatal(err)
	}
	tsAliasCacheMu.Lock()
	delete(tsAliasCache, root)
	tsAliasCacheMu.Unlock()
	tsAliasCollectionForTree(tree)
	if !proof.isUnbounded() || proof.proven() != nil {
		t.Fatal("a whole-tree config scan left the proof bounded")
	}
}

// A build whose tsconfig / jsconfig alias scopes the process-wide cache
// answers reads no config itself, so its proof stays bounded; but its payload
// is built on what those configs said, so each config a cached scope came
// from joins the read set, held to its change stamp: one moved and restored
// during the build refuses it.
func TestACachedAliasScopeHoldsItsConfigsToTheirStamps(t *testing.T) {
	builderIsolateGit(t)
	root := builderTempDir(t, "checkout")
	builderGit(t, root, "init", "--initial-branch=main")
	for name, body := range map[string]string{
		"tsconfig.json":     "{\"compilerOptions\":{\"baseUrl\":\".\",\"paths\":{\"@lib/*\":[\"lib/*\"]}}}\n",
		"web/jsconfig.json": "{\"compilerOptions\":{\"baseUrl\":\".\",\"paths\":{\"@ui/*\":[\"ui/*\"]}}}\n",
		"lib/x.ts":          "export const x = 1;\n",
		"web/ui/y.js":       "export const y = 2;\n",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		builderWriteFile(t, root, name, body)
	}
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "A")
	time.Sleep(2 * time.Second / 20) // past gitstate's change-stamp margin
	sampler, before := sampleOf(t, root)
	if !sampleStampsTrusted(t, sampler, before) {
		t.Skip("the checkout's filesystem gives no change stamps")
	}
	t.Cleanup(func() {
		tsAliasCacheMu.Lock()
		delete(tsAliasCache, root)
		tsAliasCacheMu.Unlock()
	})
	if loadTSAliasCollection(root) == nil { // an earlier build's scan
		t.Fatal("the fixture's configs gave no alias scopes")
	}
	proof := newBuildContentProof(root, before)
	tree := provenManifestTree{diskManifestTree: newDiskManifestTree(root), proof: proof}
	if tsAliasCollectionForTree(tree) == nil {
		t.Fatal("the cache answered no alias scopes")
	}
	if proof.isUnbounded() {
		t.Fatal("a cached alias collection made the proof unbounded")
	}
	set, _ := buildReadSetFrom(proof.readSet(context.Background(), nil, nil))
	for _, rel := range []string{"tsconfig.json", "web/jsconfig.json"} {
		proof.mu.Lock()
		held, read := proof.parsed[rel]
		proof.mu.Unlock()
		if !read || held.sha256 != "" || !slices.Contains(set.files, rel) {
			t.Errorf("config %s behind a cached scope: recorded %v (%+v), in read set %v; want it held as a read of unknown bytes", rel, read, held, slices.Contains(set.files, rel))
		}
	}
	if moved, err := proof.unrecordedMoved(context.Background(), sampler, before); err != nil || moved != "" {
		t.Fatalf("nothing moved, yet unrecordedMoved = %q, %v", moved, err)
	}
	config := filepath.Join(root, "web", "jsconfig.json")
	held, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, held, 0o644); err != nil {
		t.Fatal(err)
	}
	if moved, err := proof.unrecordedMoved(context.Background(), sampler, before); err != nil || moved != "web/jsconfig.json" {
		t.Fatalf("a config behind a cached scope moved and restored during the build = %q, %v; want web/jsconfig.json held to its stamp", moved, err)
	}
}

// A delta that deletes a file plans its dependents by extracting the changed
// files again (editDeltaDependentsWalk), after the per-file pass parsed them.
// That second read is recorded too: a file saved between the parse and the
// walk was read with two different contents, so the payload is no single
// state's, and the fence tears the build although the parse alone read the
// sample's bytes.
func TestTheDeletionWalkRecordsTheFilesItReadsAgain(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := sideInputCheckout(t, store)
	if err := os.Remove(filepath.Join(repoDir, "other", "c.go")); err != nil {
		t.Fatal(err)
	}
	// An added file the pass parses after pkg/a.go.
	builderWriteFile(t, repoDir, "pkg/z.go", "package pkg\n\nfunc Z() int { return A() }\n")
	time.Sleep(2 * time.Second / 20)
	sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capability, err := sampler.ConfirmReadSet(context.Background(), before, nil, nil); err != nil || !capability.Confirmed {
		t.Skipf("the checkout's filesystem gives no change stamps: %+v %v", capability, err)
	}
	registry := builderRegistry()
	goExtractor, ok := registry.GetByLanguage("go")
	if !ok {
		t.Fatal("no Go extractor registered")
	}
	// The pass's extraction of pkg/z.go, after it parsed pkg/a.go, saves
	// pkg/a.go: the pass parsed the sample's bytes, the walk after it
	// reads the save.
	saver := &savingExtractor{Extractor: goExtractor, trigger: "z.go", path: filepath.Join(repoDir, "pkg", "a.go"), line: "\nfunc Later() {}\n"}
	saver.armed.Store(true)
	registry.Register(saver)
	refuted := readSetConfirmations.refuted.Load()
	builder := builderNewBuilder(store)
	builder.Registry = registry
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	generationID, _, err := builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		Sampler:      sampler,
		before:       &before,
	})
	if _, saveErr := saver.saved(); saver.armed.Load() || saveErr != nil {
		t.Fatalf("the save never landed (err %v)", saveErr)
	}
	if report := LastEditDeltaReport(); report == nil || !report.DependentsWalked {
		t.Fatal("the delta did not plan its dependents by the walk: the test does not exercise it")
	}
	if !errors.Is(err, ErrDirtySnapshotChanged) || readSetConfirmations.refuted.Load() == refuted {
		t.Fatalf("a build whose walk read a save its parse did not = generation %d, %v; want torn by its own reads", generationID, err)
	}
}

// A file saved while the pass extracts it leaves a stale speculative parse,
// and the pass reads it again (takePreparedRefresh, the legacy fallback's
// indexFile) and builds the saved bytes. That read is recorded like the
// parse: when the save is undone before the fence — the working copy is the
// sample's again, so stamps and a re-hash would confirm it — the build is
// still torn by its own reads, not published under a sample its payload does
// not describe.
func TestAFileReadAgainAfterItsParseWentStaleIsRecorded(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := sideInputCheckout(t, store)
	sampler, err := gitstate.NewDirtySampler(repoDir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capability, err := sampler.ConfirmReadSet(context.Background(), before, nil, nil); err != nil || !capability.Confirmed {
		t.Skipf("the checkout's filesystem gives no change stamps: %+v %v", capability, err)
	}
	registry := builderRegistry()
	goExtractor, ok := registry.GetByLanguage("go")
	if !ok {
		t.Fatal("no Go extractor registered")
	}
	saver := &savingExtractor{Extractor: goExtractor, trigger: "a.go", path: filepath.Join(repoDir, "pkg", "a.go"), line: "\nfunc Later() {}\n"}
	saver.armed.Store(true)
	registry.Register(saver)
	refuted := readSetConfirmations.refuted.Load()
	builder := builderNewBuilder(store)
	builder.Registry = registry
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	generationID, _, err := builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		Sampler:      sampler,
		before:       &before,
		buildBarrier: func() {
			// The undo: the working copy is the sample's again.
			builderWriteFile(t, repoDir, "pkg/a.go", sideInputEdit)
		},
	})
	if _, saveErr := saver.saved(); saver.armed.Load() || saveErr != nil {
		t.Fatalf("the save never landed (err %v)", saveErr)
	}
	if !errors.Is(err, ErrDirtySnapshotChanged) || readSetConfirmations.refuted.Load() == refuted {
		published := false
		if row, found, _ := store.Catalog().GetViewGeneration(context.Background(), generationID); found && row.PublishedAt != 0 {
			published = true
		}
		t.Fatalf("a build that read a file again after its save = generation %d (published %v), %v; want torn by its own reads", generationID, published, err)
	}
}
