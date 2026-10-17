package goanalysis

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// The first touch of a package must not wait for the whole module's
// warm-up: the checkout's dirty packages are listed first, one at a time;
// another module's long compiler load neither cancels nor holds back a
// checkout's warm-up; and a package retained without export data (listed
// while it did not compile) gets its export data back right after a pass
// that compiles, instead of being checked from source on every pass.

// gitFixture makes root a git working tree with everything committed, with
// an isolated git configuration.
func gitFixture(t *testing.T, root string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(cfg, []byte("[user]\n\tname = t\n\temail = t@example.invalid\n[commit]\n\tgpgsign = false\n"), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-qm", "fixture"}} {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}

// TestWarmupListsDirtyPackagesFirstOneAtATime: helper has a dirty file, so
// the warm-up's first listing is helper alone (not the largest packages'
// batch), merged before anything else is listed.
func TestWarmupListsDirtyPackagesFirstOneAtATime(t *testing.T) {
	root := typecheckCacheFixture(t)
	gitFixture(t, root)
	writeFile(t, root, "helper/helper.go", readFixture(t, root, "helper/helper.go")+"\n// dirty\n")

	p := newTestProvider(t)
	p.warm.quiet = 10 * time.Millisecond
	t.Cleanup(func() { _ = p.Close() })
	queue, targeted, err := p.planWarmup(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, "example.com/scope/helper", queue[0], "the dirty package leads: %v", queue)
	require.Equal(t, 1, targeted)

	var (
		mu      sync.Mutex
		batches [][]string
	)
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if warmupListingFlags(cfg) {
			mu.Lock()
			batches = append(batches, append([]string(nil), patterns...))
			mu.Unlock()
		}
		return packages.Load(cfg, patterns...)
	}
	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	status := waitWarm(t, p, root)
	require.Equal(t, warmupWarm, status.State)
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, batches)
	require.Equal(t, []string{"example.com/scope/helper"}, batches[0], "the first listing is the dirty package alone: %v", batches)
}

// TestWarmupIsNotPreemptedByAnotherModulesLoad: a long compiler load over
// one module directory (a primary's whole-module load) neither holds back
// nor cancels another directory's warm-up; a load over the warm-up's own
// directory still preempts it.
func TestWarmupIsNotPreemptedByAnotherModulesLoad(t *testing.T) {
	root := typecheckCacheFixture(t)
	primary := typecheckCacheFixture(t)
	p := newTestProvider(t)
	p.warm.quiet = 10 * time.Millisecond
	p.warm.batchSize = 1
	t.Cleanup(func() { _ = p.Close() })
	listing := make(chan *packages.Config, 16)
	release := make(chan struct{})
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if warmupListingFlags(cfg) {
			listing <- cfg
			select {
			case <-release:
			case <-cfg.Context.Done():
				return nil, cfg.Context.Err()
			}
		}
		return packages.Load(cfg, patterns...)
	}

	endPrimary := p.beginCompilerLoad(primary) // the primary's long load runs
	defer endPrimary()
	require.Equal(t, warmupOutcomeStarted, p.WarmCheckoutCompiler(root, cachedScope))
	var cfg *packages.Config
	select {
	case cfg = <-listing:
	case <-time.After(30 * time.Second):
		t.Fatal("the warm-up waited for another module's load")
	}
	endOther := p.beginCompilerLoad(primary) // another load there begins
	endOther()
	require.NoError(t, cfg.Context.Err(), "another module's load does not cancel the warm-up's listing")
	require.Equal(t, 0, warmupStatus(p, root).Preemptions)

	endOwn := p.beginCompilerLoad(root)
	require.Error(t, cfg.Context.Err(), "a load over the warm-up's own module preempts it")
	require.Equal(t, 1, warmupStatus(p, root).Preemptions)
	endOwn()
	close(release)
	status := waitWarm(t, p, root)
	require.Equal(t, warmupWarm, status.State)
}

// relistChainFixture: top imports mid imports leaf; leafBroken renames
// build's declaration and leaves its use (an edit in flight).
func relistChainFixture(t *testing.T) (root, leaf, leafBroken string) {
	t.Helper()
	root = resolvedTempDir(t)
	writeGoMod(t, root, "example.com/relist")
	leaf = "package leaf\n\n// A is New's result.\ntype A struct{ N int }\n\n// New builds an A.\nfunc New() A { return build() }\n\nfunc build() A { return A{N: 1} }\n"
	writeFile(t, root, "leaf/leaf.go", leaf)
	writeFile(t, root, "mid/mid.go", "package mid\n\nimport \"example.com/relist/leaf\"\n\n// Value's type is leaf.New's result.\nvar Value = leaf.New()\n")
	writeFile(t, root, "top/top.go", "package top\n\nimport \"example.com/relist/mid\"\n\n// Use binds mid.Value.\nfunc Use() int {\n\tv := mid.Value\n\treturn v.N\n}\n")
	return root, leaf, strings.Replace(leaf, "func build() A", "func buildRen1() A", 1)
}

var relistTopHandle = retentionHandle("top/top.go", "Use", 6, 9)

// TestExportRelistAfterCompilingPassEndsSourceChecks: top's closure is
// first listed while leaf does not compile (leaf and mid come back without
// export data). After the edit is undone the pass checks leaf and mid from
// source and, as it compiled, schedules one relist; from then on the pass
// is a plain hit over retained export data. A pass over a tree that does
// not compile schedules nothing, and the same files are never relisted
// twice.
func TestExportRelistAfterCompilingPassEndsSourceChecks(t *testing.T) {
	root, leaf, broken := relistChainFixture(t)
	loader := &countingLoader{}
	p := newTestProvider(t)
	p.packagesLoad = loader.load
	p.warm.quiet = 10 * time.Millisecond
	t.Cleanup(func() { _ = p.Close() })
	lists := func() int {
		loader.mu.Lock()
		defer loader.mu.Unlock()
		return loader.meta
	}

	// This test specifically exercises content changes under retained metadata.
	// recordManifest conservatively relists files stamped at/after listing
	// start, so establish a stable pre-listing fixture instead of depending
	// on the filesystem clock advancing between WriteFile and packages.Load.
	fixtureStamp := time.Now().Add(-time.Minute)
	writeLeaf := func(content string) {
		t.Helper()
		writeFile(t, root, "leaf/leaf.go", content)
		require.NoError(t, os.Chtimes(filepath.Join(root, "leaf/leaf.go"), fixtureStamp, fixtureStamp))
		fixtureStamp = fixtureStamp.Add(time.Second)
	}
	for _, rel := range []string{"mid/mid.go", "top/top.go"} {
		require.NoError(t, os.Chtimes(filepath.Join(root, rel), fixtureStamp, fixtureStamp))
	}
	writeLeaf(broken)
	c := retentionPass(t, p, root, relistTopHandle, false, "top while leaf is broken")
	require.Equal(t, 0, c.RelistScheduled, "a tree that does not compile is never relisted")
	st := retainedState(t, p, root)
	func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		require.Empty(t, st.meta["example.com/relist/leaf"].ExportFile, "fixture: leaf listed without export data")
		for _, path := range []string{"example.com/relist/leaf", "example.com/relist/mid", "example.com/relist/top"} {
			manifest := st.manifests[path]
			require.NotNil(t, manifest, "retained fixture manifest for %s", path)
			require.False(t, manifest.stale, "fixture requires stable retained metadata: path=%s listing_start_ns=%d files=%+v", path, st.listedAt[path].UnixNano(), manifest.files)
		}
	}()

	// Still broken, now in a declaration: checked from source with hard
	// errors, so nothing is relisted. (A dependency's function bodies are
	// stripped when it is checked from source, so a body-only error there
	// is not seen; the relist then finds it, and the signature bound keeps
	// it to one listing per file state.)
	writeLeaf(strings.Replace(broken, "func New() A", "func New() Missing", 1))
	c = retentionPass(t, p, root, relistTopHandle, false, "leaf still broken")
	require.Equal(t, 2, c.SourceDependencies, "fixture: leaf and mid are checked from source: counters=%+v", *c)
	require.Equal(t, 0, c.RelistScheduled, "a pass with hard errors never relists: counters=%+v", *c)

	writeLeaf(leaf)
	c = retentionPass(t, p, root, relistTopHandle, true, "edit undone")
	require.Equal(t, 2, c.SourceDependencies, "leaf and mid are checked from source: counters=%+v", *c)
	require.Equal(t, 1, c.RelistScheduled, "the pass compiled: one relist: counters=%+v", *c)
	p.waitExportRelists()
	st.mu.Lock()
	require.NotEmpty(t, st.meta["example.com/relist/leaf"].ExportFile, "the relist brought leaf's export data back")
	require.NotEmpty(t, st.meta["example.com/relist/mid"].ExportFile)
	st.mu.Unlock()

	before := lists()
	c = retentionPass(t, p, root, relistTopHandle, true, "after the relist")
	require.Equal(t, 0, c.SourceDependencies, "nothing is checked from source any more")
	require.Equal(t, 1, c.ClosureHits)
	require.Equal(t, 0, c.RelistScheduled)
	require.Equal(t, before, lists(), "no listing on the pass")
}

// TestExportRelistIsBoundedPerFiles: a relist that cannot bring export data
// back (the package still does not build as listed) is not scheduled again
// until the package's files change.
func TestExportRelistIsBoundedPerFiles(t *testing.T) {
	root, _, _ := relistChainFixture(t)
	p := newTestProvider(t)
	p.warm.quiet = 10 * time.Millisecond
	t.Cleanup(func() { _ = p.Close() })
	retentionPass(t, p, root, relistTopHandle, true, "cold")
	st := retainedState(t, p, root)
	st.mu.Lock()
	leafMeta := st.meta["example.com/relist/leaf"]
	roots := []*packages.Package{st.meta["example.com/relist/top"]}
	leafMeta.ExportFile = "" // as if listed while broken
	require.True(t, p.scheduleExportRelist(st, roots, []string{"example.com/relist/leaf"}))
	require.False(t, p.scheduleExportRelist(st, roots, []string{"example.com/relist/leaf"}), "one relist at a time")
	st.mu.Unlock()
	p.waitExportRelists()
	st.mu.Lock()
	require.NotEmpty(t, st.meta["example.com/relist/leaf"].ExportFile)
	st.meta["example.com/relist/leaf"].ExportFile = ""
	require.False(t, p.scheduleExportRelist(st, roots, []string{"example.com/relist/leaf"}), "same files: never twice")
	st.mu.Unlock()
	writeFile(t, root, "leaf/leaf.go", readFixture(t, root, "leaf/leaf.go")+"\n// changed\n")
	st.mu.Lock()
	require.True(t, p.scheduleExportRelist(st, roots, []string{"example.com/relist/leaf"}), "changed files: again")
	st.mu.Unlock()
	p.waitExportRelists()
}
