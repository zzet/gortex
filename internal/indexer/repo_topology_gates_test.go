package indexer

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/reach"
)

// awaitRepoCoupling waits until the store's repository coupling is read, so a
// patch of lib or app no longer holds the whole graph.
func awaitRepoCoupling(t *testing.T, g graph.Store) {
	t.Helper()
	require.Eventually(t, func() bool {
		finishLib := reach.BeginRepoTopologyMutation(g, "lib")
		defer finishLib(false, nil)
		admitted := make(chan struct{})
		go func() {
			reach.BeginRepoTopologyMutation(g, "app")(false, nil)
			close(admitted)
		}()
		select {
		case <-admitted:
			return true
		case <-time.After(10 * time.Millisecond):
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "the repository coupling was never read")
}

// holdRepoPatch starts a patch of one file in prefix and parks it inside its
// topology mutation, after the parse and resolver tails and before the
// derived passes. finish lets the patch complete and returns its error.
func holdRepoPatch(t *testing.T, mi *MultiIndexer, prefix, path, content string) (finish func() error) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	mi.GetIndexer(prefix).incrementalCatchupHook = func(kind string, _ []string) {
		if kind == "derived" {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	writeFile(t, path, content)
	done := make(chan error, 1)
	go func() {
		_, err := mi.IncrementalReindexRepo(prefix, []string{path})
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("patch of %s finished without reaching its derived tail: %v", prefix, err)
	case <-time.After(30 * time.Second):
		t.Fatalf("patch of %s never reached its derived tail", prefix)
	}
	var (
		released sync.Once
		err      error
	)
	finish = func() error {
		released.Do(func() {
			close(release)
			err = <-done
		})
		return err
	}
	t.Cleanup(func() { _ = finish() })
	return finish
}

func TestTopologyGatePerRepoAllowsParallelPatches(t *testing.T) {
	mi, g, roots := repoTopologyFixture(t)
	awaitRepoCoupling(t, g)
	finishApp := holdRepoPatch(t, mi, "app", filepath.Join(roots["app"], "main.go"),
		"package app\nfunc Run() {}\nfunc Start() { Run() }\nfunc Extra() { Run() }\n")

	lib := filepath.Join(roots["lib"], "lib.go")
	writeFile(t, lib, "package library\nfunc Hello() {}\nfunc Local() { Hello() }\nfunc More() { Local() }\n")
	done := make(chan error, 1)
	go func() {
		_, err := mi.IncrementalReindexRepo("lib", []string{lib})
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a patch of lib waited behind an in-flight patch of app")
	}
	require.NoError(t, finishApp())
}

func TestLookupNotStarvedByOtherRepoWriters(t *testing.T) {
	mi, g, roots := repoTopologyFixture(t)
	awaitRepoCoupling(t, g)
	finishApp := holdRepoPatch(t, mi, "app", filepath.Join(roots["app"], "main.go"),
		"package app\nfunc Run() {}\nfunc Start() { Run() }\nfunc Extra() { Run() }\n")

	started := time.Now()
	d1, _, _, hit, truncated := reach.LookupContext(t.Context(), g, "lib/lib.go::Hello")
	elapsed := time.Since(started)
	require.True(t, hit)
	require.False(t, truncated, "impact on lib degraded to a lower bound after %s behind a patch of app", elapsed)
	require.Less(t, elapsed, time.Second)
	require.Equal(t, []string{"lib/lib.go::Local"}, reachIDs(d1))
	require.NoError(t, finishApp())
}

func TestForeignRepoEditKeepsReachRecordsHot(t *testing.T) {
	mi, g, roots := repoTopologyFixture(t)
	awaitRepoCoupling(t, g)
	reach.BuildIndex(g)

	main := filepath.Join(roots["app"], "main.go")
	writeFile(t, main, "package app\nfunc Run() {}\nfunc Start() { Run() }\nfunc Extra() { Run() }\n")
	_, err := mi.IncrementalReindexRepo("app", []string{main})
	require.NoError(t, err)

	_, _, _, hit, _ := reach.LookupCached(g, "app/main.go::Run")
	require.False(t, hit, "control: the edited repository's records must be retired")
	d1, _, _, hit, _ := reach.LookupCached(g, "lib/lib.go::Hello")
	require.True(t, hit, "an edit in app retired the records of lib")
	require.Equal(t, []string{"lib/lib.go::Local"}, reachIDs(d1))
}

// A patch whose resolver pass binds a reference, not a call, from app into
// lib joins the two repositories all the same: the records of lib must not be
// served after it. The resolver hook binds the edge the way a name-based
// resolver pass does, outside the cross-repository candidate pass.
func TestRepoPatchBindingAReferenceRetiresTheOtherRepo(t *testing.T) {
	mi, g, roots := repoTopologyFixture(t)
	awaitRepoCoupling(t, g)
	reach.BuildIndex(g)
	_, _, _, hit, _ := reach.LookupCached(g, "lib/lib.go::Hello")
	require.True(t, hit, "fixture must start from a published record")

	app := mi.GetIndexer("app")
	resolved := false
	app.incrementalResolveFilesHook = func(frontier []string) {
		mi.runMasterResolveFilesWithEvidence(frontier, false, app.resolver)
		g.AddBatch(nil, []*graph.Edge{{
			From: "app/main.go::Extra", To: "lib/lib.go::Hello", Kind: graph.EdgeReferences,
			FilePath: "app/main.go", Line: 4,
		}})
		resolved = true
	}
	main := filepath.Join(roots["app"], "main.go")
	writeFile(t, main, "package app\nfunc Run() {}\nfunc Start() { Run() }\nfunc Extra() { Run() }\n")
	_, err := mi.IncrementalReindexRepo("app", []string{main})
	require.NoError(t, err)
	require.True(t, resolved, "the patch must run its resolver pass")
	var bound bool
	for _, e := range g.GetInEdges("lib/lib.go::Hello") {
		bound = bound || e.From == "app/main.go::Extra" && e.Kind == graph.EdgeReferences
	}
	require.True(t, bound, "the reference must survive the patch's later passes")

	_, _, _, hit, _ = reach.LookupCached(g, "lib/lib.go::Hello")
	require.False(t, hit, "a reference bound from app into lib left the records of lib current")
}
