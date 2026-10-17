package indexer

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Install with both the work coverage and fair fill sources. Each fixture owes
// cold work for the entire census; alternating slices models another actor
// occupying the slot between captures. The bounded loop asserts logical
// progress rather than a host-dependent completion deadline.
func TestContractFillColdWorkMakesBoundedProgressAcrossActors(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}
	for i := 0; i < 33; i++ {
		files[fmt.Sprintf("helper%02d.go", i)] = fmt.Sprintf("package fixture\nfunc Helper%d() int { return %d }\n", i, i)
	}
	actors := []*contractJobFixture{newContractJobFixture(t, files), newContractJobFixture(t, files)}
	caches := make([]*contractDeltaCache, len(actors))
	complete := make([]bool, len(actors))
	seen := []map[string]bool{{}, {}}
	workIdentities := make([][]string, len(actors))
	for turn := 0; turn < 10; turn++ {
		actor := turn % len(actors)
		if complete[actor] {
			continue
		}
		before := len(seen[actor])
		newlyExtracted := map[string]bool{}
		hooks := &contractConvergenceHooks{beforeExtract: func(path string) {
			newlyExtracted[path] = true
		}}
		f := actors[actor]
		f.layers++
		report, err := runFixtureContractJob(t, f.options, contractFixtureJob{
			layer: f.layers, cache: &caches[actor], hooks: hooks,
			otherActorWaiting: func() bool { return !complete[1-actor] },
			adjust: func(snapshot *ContractFollowupSnapshot) {
				require.Len(t, snapshot.Files, len(files), "each recapture includes the full cold census")
				var identities []string
				namespaceDebt := false
				for _, row := range snapshot.Work {
					identities = append(identities, contractWorkIdentity(row))
					if row.RepoPrefix == "fixture" && row.FilePath == "fixture/" && row.State == graph.ContractWorkPending && row.Scope.Unknown {
						namespaceDebt = true
					}
				}
				require.True(t, namespaceDebt, "cold work retains pending unknown namespace debt: %#v", snapshot.Work)
				require.True(t, newContractWorkCoverage(snapshot.Work).broad, "namespace debt covers the full census")
				if workIdentities[actor] == nil {
					workIdentities[actor] = identities
				} else {
					require.ElementsMatch(t, workIdentities[actor], identities, "recaptures retain the exact full work obligations")
				}
			},
		})
		for path := range newlyExtracted {
			seen[actor][path] = true
		}
		if err != nil {
			require.ErrorAs(t, err, new(*ContractFillContinued))
			require.Greater(t, len(seen[actor]), before, "each continued slice must advance beyond the already extracted prefix")
			require.False(t, report.Published)
		} else {
			require.True(t, report.Published)
			complete[actor] = true
		}
	}
	require.Equal(t, []bool{true, true}, complete, "both cold-work actors complete in bounded alternating slices")
	for actor, f := range actors {
		require.Len(t, seen[actor], len(files), "completed cold fill extracted the entire census")
		for name := range files {
			require.Contains(t, seen[actor], "fixture/"+name)
		}
		require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	}
}

func TestContractFillContinuationMembershipChangesAndEvictionFinish(t *testing.T) {
	for _, change := range []string{"add_remove", "same_path_new_work", "evict", "cancel", "key_move"} {
		t.Run(change, func(t *testing.T) {
			files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}
			for i := 0; i < 33; i++ {
				files[fmt.Sprintf("helper%02d.go", i)] = fmt.Sprintf("package fixture\nfunc Helper%d() int { return %d }\n", i, i)
			}
			f := newContractJobFixture(t, files)
			var cache *contractDeltaCache
			extracted := map[string]int{}
			hooks := &contractConvergenceHooks{beforeExtract: func(path string) { extracted[path]++ }}
			run := func(yield func(context.Context) error) (ContractFollowupReport, error) {
				f.layers++
				return runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: &cache, hooks: hooks, yield: yield, otherActorWaiting: func() bool { return true }})
			}
			_, err := run(nil)
			require.ErrorAs(t, err, new(*ContractFillContinued))
			require.NotNil(t, cache.continuation)
			switch change {
			case "add_remove":
				f.remove("helper00.go")
				delete(files, "helper00.go")
				files["new.go"] = "package fixture\nfunc Added() {}\n"
				f.save("new.go", files["new.go"])
			case "same_path_new_work":
				advanceContractCoverageInputs(t, f.store, "fixture", "fixture/helper00.go", graph.ContractWorkScope{})
			case "evict":
				cache = nil
			case "cancel", "key_move":
				failure := context.Canceled
				if change == "key_move" {
					failure = errContractJobKeyMoved
				}
				_, err = run(func(context.Context) error { return failure })
				require.ErrorIs(t, err, failure)
				if cache != nil {
					require.Nil(t, cache.continuation, "failed jobs cannot mint a continuation anchor")
				}
			}
			complete := false
			for segment := 0; segment < 5; segment++ {
				report, runErr := run(nil)
				if runErr == nil {
					require.True(t, report.Published)
					complete = true
					break
				}
				require.ErrorAs(t, runErr, new(*ContractFillContinued))
			}
			require.True(t, complete, "a finite change or cache loss must not restart the same prefix forever")
			if change == "same_path_new_work" {
				require.Equal(t, 2, extracted["fixture/helper00.go"], "new same-fingerprint debt forces an actual new own extraction")
			}
			require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
		})
	}
}
