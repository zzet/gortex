package indexer

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
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

// repoPatchDoor submits an edit of one file to its repository's lane and
// reports the patch's outcome once it finishes.
type repoPatchDoor func(t *testing.T, prefix, path string) <-chan error

// reconcileDoor is the coalescing reconcile a watcher tick or a recovery
// reaches.
func reconcileDoor(mi *MultiIndexer) repoPatchDoor {
	return func(_ *testing.T, prefix, path string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := mi.IncrementalReindexRepo(prefix, []string{path})
			done <- err
		}()
		return done
	}
}

// watcherDoor is a tool write's point patch through its repository's watcher,
// the route an edit takes in the daemon.
func watcherDoor(t *testing.T, mi *MultiIndexer, prefixes ...string) repoPatchDoor {
	t.Helper()
	configs := make(map[string]config.WatchConfig, len(prefixes))
	for _, prefix := range prefixes {
		configs[prefix] = config.WatchConfig{Enabled: true, DebounceMs: 20}
	}
	mw, err := NewMultiWatcher(mi, configs, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, mw.Start())
	t.Cleanup(func() { _ = mw.Stop() })
	return func(t *testing.T, prefix, path string) <-chan error {
		ticket, err := mw.EnqueueToolMutation(t.Context(), path)
		require.NoError(t, err)
		require.NotNil(t, ticket, "the watcher of %s refused the tool write", prefix)
		done := make(chan error, 1)
		go func() {
			result := <-ticket.Done
			if result.Err == nil && !result.Reindexed {
				result.Err = fmt.Errorf("the patch of %s did not reindex", prefix)
			}
			done <- result.Err
		}()
		return done
	}
}

// holdRepoPatch submits an edit of one file in prefix through door and parks
// the patch inside its repository lane and topology mutation, at tail:
// "resolve" before the resolver binds the frontier, "derived" before the
// derived passes. finish lets the patch complete and returns its error.
func holdRepoPatch(t *testing.T, mi *MultiIndexer, door repoPatchDoor, tail, prefix, path, content string) (finish func() error) {
	t.Helper()
	idx := mi.GetIndexer(prefix)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	park := func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	switch tail {
	case "resolve":
		idx.incrementalResolveFilesHook = func(frontier []string) {
			park()
			mi.runMasterResolveFilesWithEvidence(frontier, false, idx.resolver)
		}
	case "derived":
		idx.incrementalCatchupHook = func(kind string, _ []string) {
			if kind == "derived" {
				park()
			}
		}
	default:
		t.Fatalf("unknown tail %q", tail)
	}
	writeFile(t, path, content)
	done := door(t, prefix, path)
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("patch of %s finished without reaching its %s tail: %v", prefix, tail, err)
	case <-time.After(30 * time.Second):
		t.Fatalf("patch of %s never reached its %s tail", prefix, tail)
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

// A patch parked in one repository's resolver or derived tail holds that
// repository's lane and topology gate, and the lane's status names it. Patches
// of the five other repositories in the same store, submitted together, must
// still take their own lanes and gates and publish their relations, whichever
// door the edits come through.
func TestTopologyGatePerRepoAllowsParallelPatches(t *testing.T) {
	holders := map[string]OutputMutationEntry{
		"reconcile": OutputEntryRepositoryReconcileLane,
		"watcher":   OutputEntryWatcherPatchGraph,
	}
	others := []string{"lib", "beta", "gamma", "delta", "epsilon"}
	for _, door := range []string{"reconcile", "watcher"} {
		for _, tail := range []string{"resolve", "derived"} {
			t.Run(door+"/"+tail, func(t *testing.T) {
				mi, g, roots := repoTopologyFixture(t, others[1:]...)
				awaitRepoCoupling(t, g)
				submit := reconcileDoor(mi)
				if door == "watcher" {
					submit = watcherDoor(t, mi, append([]string{"app"}, others...)...)
				}
				finishApp := holdRepoPatch(t, mi, submit, tail, "app", filepath.Join(roots["app"], "main.go"),
					"package app\nfunc Run() {}\nfunc Start() { Run() }\nfunc Extra() { Run() }\n")
				parked := time.Now()

				appLane, ok := mi.RepositoryLane("app")
				require.True(t, ok)
				require.NotNil(t, appLane.Holder, "the parked patch is not reported as the lane's tenant")
				require.Equal(t, string(holders[door]), appLane.Holder.Kind)
				require.NotNil(t, appLane.Topology)
				require.Equal(t, "ready", appLane.Topology.Coupling)
				require.Equal(t, []reach.RepositoryTopologyGate{{Repo: "app", TopologyGate: reach.TopologyGate{Writers: 1}}},
					appLane.Topology.Repositories, "the parked patch holds only the topology gate of app")
				require.Equal(t, []reach.TopologyWriter{{Repo: "app", State: "holding"}}, untimedWriters(appLane.Topology.Writers),
					"the parked patch is not named as the one topology writer")

				held, free := mi.TryRepositoryMutationLanes([]string{"app"})
				if free {
					held()
				}
				require.False(t, free, "control: the parked patch must hold the lane of app")
				for _, repo := range others {
					lane, ok := mi.RepositoryLane(repo)
					require.True(t, ok)
					require.Nil(t, lane.Holder, "the lane of %s reports a tenant beside a parked patch of app", repo)
					release, free := mi.TryRepositoryMutationLanes([]string{repo})
					require.True(t, free, "the lane of %s is held beside a parked patch of app", repo)
					release()
					gate, ok := admittedWithin(5*time.Second, func() func(bool, []string) {
						return reach.BeginRepoTopologyMutation(g, repo)
					})
					require.True(t, ok, "the topology gate of %s waited for a parked patch of app", repo)
					gate(false, nil)
				}

				done := make(map[string]<-chan error, len(others))
				for _, repo := range others {
					path := filepath.Join(roots[repo], "lib.go")
					pkg := repo
					if repo == "lib" {
						pkg = "library"
					}
					writeFile(t, path, "package "+pkg+"\nfunc Hello() {}\nfunc Local() { Hello() }\nfunc More() { Hello() }\n")
					done[repo] = submit(t, repo, path)
				}
				bound := time.After(10 * time.Second)
				for _, repo := range others {
					select {
					case err := <-done[repo]:
						require.NoError(t, err, "the patch of %s", repo)
					case <-bound:
						t.Fatalf("a patch of %s waited behind a parked patch of app", repo)
					}
					require.True(t, hasInEdge(g, repo+"/lib.go::Hello", repo+"/lib.go::More", graph.EdgeCalls),
						"the patch of %s did not publish its new caller", repo)
				}

				releasing := time.Now()
				require.NoError(t, finishApp())
				require.True(t, hasInEdge(g, "app/main.go::Run", "app/main.go::Extra", graph.EdgeCalls),
					"the released patch of app did not publish its new caller")
				parkedMS := float64(releasing.Sub(parked).Microseconds()) / 1000
				require.Eventually(t, func() bool {
					appLane, _ := mi.RepositoryLane("app")
					for _, kind := range appLane.ByKind {
						if kind.Kind == string(holders[door]) && kind.Longest.HeldForMS >= parkedMS {
							return true
						}
					}
					return false
				}, 5*time.Second, time.Millisecond, "the parked patch's tenure of at least %.1f ms was not recorded", parkedMS)
				require.Empty(t, reach.Topology(g).Writers, "finished patches are still listed as topology writers")
			})
		}
	}
}

// untimedWriters is writers without their times, which it requires to be set.
func untimedWriters(writers []reach.TopologyWriter) []reach.TopologyWriter {
	out := make([]reach.TopologyWriter, 0, len(writers))
	for _, writer := range writers {
		if writer.SinceUnixMS <= 0 || writer.ForMS < 0 {
			return nil
		}
		writer.SinceUnixMS, writer.ForMS = 0, 0
		out = append(out, writer)
	}
	return out
}

// admittedWithin reports whether begin returns within d.
func admittedWithin[F any](d time.Duration, begin func() F) (F, bool) {
	done := make(chan F, 1)
	go func() { done <- begin() }()
	select {
	case finish := <-done:
		return finish, true
	case <-time.After(d):
		var none F
		return none, false
	}
}

func hasInEdge(g graph.Store, to, from string, kind graph.EdgeKind) bool {
	for _, e := range g.GetInEdges(to) {
		if e.From == from && e.Kind == kind {
			return true
		}
	}
	return false
}

func TestLookupNotStarvedByOtherRepoWriters(t *testing.T) {
	mi, g, roots := repoTopologyFixture(t)
	awaitRepoCoupling(t, g)
	finishApp := holdRepoPatch(t, mi, reconcileDoor(mi), "derived", "app", filepath.Join(roots["app"], "main.go"),
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
	app.incrementalResolveFilesHook = func(frontier []string) []string {
		stats := mi.runMasterResolveFilesObserved(frontier, false, app.resolver, nil)
		g.AddBatch(nil, []*graph.Edge{{
			From: "app/main.go::Extra", To: "lib/lib.go::Hello", Kind: graph.EdgeReferences,
			FilePath: "app/main.go", Line: 4,
		}})
		resolved = true
		if stats == nil {
			return nil
		}
		return stats.DroppedFiles
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

// Patches of independent repositories run their resolver and derived tails
// concurrently. Exercise their shared state together with impact lookups.
func TestConcurrentRepoPatchTails(t *testing.T) {
	for _, coupled := range []bool{false, true} {
		t.Run(fmt.Sprintf("coupled=%v", coupled), func(t *testing.T) {
			mi, g, roots := repoTopologyFixture(t)
			awaitRepoCoupling(t, g)
			var wg sync.WaitGroup
			patch := func(prefix, file string, body func(i int) string) {
				defer wg.Done()
				for i := range 12 {
					writeFile(t, file, body(i))
					if _, err := mi.IncrementalReindexRepo(prefix, []string{file}); err != nil {
						t.Error(err)
						return
					}
				}
			}
			wg.Add(3)
			go patch("lib", filepath.Join(roots["lib"], "lib.go"), func(i int) string {
				var b strings.Builder
				b.WriteString("package library\nfunc Hello() {}\nfunc Local() { Hello() }\n")
				for j := range i % 4 {
					fmt.Fprintf(&b, "func Extra%d() { Local() }\n", j)
				}
				return b.String()
			})
			go patch("app", filepath.Join(roots["app"], "main.go"), func(i int) string {
				if coupled && i%2 == 1 {
					return fmt.Sprintf("package app\nimport library \"example.org/library\"\nfunc Run() {}\nfunc Start() { Run(); library.Hello() }\nfunc V%d() {}\n", i)
				}
				return fmt.Sprintf("package app\nfunc Run() {}\nfunc Start() { Run() }\nfunc V%d() { Start() }\n", i)
			})
			go func() {
				defer wg.Done()
				for range 60 {
					reach.LookupContext(t.Context(), g, "lib/lib.go::Hello")
					reach.LookupContext(t.Context(), g, "app/main.go::Run")
				}
			}()
			wg.Wait()
			d1, _, _, hit, truncated := reach.LookupContext(t.Context(), g, "lib/lib.go::Local")
			if !hit || truncated {
				t.Fatalf("final lookup hit=%v truncated=%v", hit, truncated)
			}
			if got := reachIDs(d1); len(got) != 3 {
				t.Fatalf("callers of Local after the last patch = %v, want Extra0..Extra2", got)
			}
		})
	}
}
