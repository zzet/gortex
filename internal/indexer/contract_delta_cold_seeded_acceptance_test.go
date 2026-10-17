//go:build acceptance

package indexer

import (
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The complete edit cross product stays outside ordinary CI. Each cell owns
// its core and independently cold oracle; no cached expected output is reused.
func TestContractDeltaColdPayloadAcceptanceMatrix(t *testing.T) {
	for _, row := range contractColdMatrixFixtures() {
		for _, path := range slices.Sorted(maps.Keys(row.files)) {
			for _, operation := range []string{"comment", "new_function", "value_or_owner", "remove", "add"} {
				t.Run(fmt.Sprintf("%s/%s/%s", row.name, path, operation), func(t *testing.T) {
					runContractColdMatrixCell(t, row, path, operation)
				})
			}
		}
	}
}

// This is a reproducible stateful campaign over the actual retained actor P,
// primary incremental door and production worker/materializer. It is not the
// ordinary graph edit_delta_property test, a benchmark or a latency assertion.
// Every final accepted state gets an independent store/core and first cold job.
func TestContractDeltaColdPayloadSeeded200(t *testing.T) {
	const seed int64 = 0x806c4
	rng := rand.New(rand.NewSource(seed))
	row := contractColdMatrixFixtures()[0]
	row.files = maps.Clone(row.files)
	row.files["config.go"] = contractColdMatrixFixtures()[1].files["config.go"]
	files := maps.Clone(row.files)
	f := newContractJobFixtureWithRegistry(t, files, contractColdMatrixRegistry())
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	contractColdMatrixPremise(t, f, row)
	previousCold := contractColdMatrixCold(t, files)
	require.Equal(t, previousCold, contractColdMatrixRead(t, f))
	deltaJobs, noOpStates := 0, 0
	paths := []string{"server.go", "client.go", "config.go"}
	operations := []string{"comment", "new_function", "value_or_owner", "remove_or_add", "restore"}
	counts := make(map[string]int)
	cutpoints := make(map[string]int)
	for step := 0; step < 200; step++ {
		// The seed chooses files/values; cycling the shuffled deck ensures
		// every mutation family is exercised, rather than hoping it is drawn.
		if step%len(operations) == 0 {
			rng.Shuffle(len(operations), func(i, j int) { operations[i], operations[j] = operations[j], operations[i] })
		}
		operation := operations[step%len(operations)]
		path := paths[rng.Intn(len(paths))]
		placement := "none"
		if step%10 == 0 || step%10 == 5 {
			path, operation = "server.go", "value_or_owner"
			placement = "after_extract"
			if step%10 == 5 {
				placement = "before_fence"
			}
		}
		t.Run(fmt.Sprintf("step_%03d_%s_%s_%s", step, path, operation, placement), func(t *testing.T) {
			before := contractColdMatrixAttachment(t, f)
			base := row.files[path]
			if operation == "remove_or_add" {
				if _, exists := files[path]; exists {
					f.remove(path)
					delete(files, path)
					counts["remove"]++
				} else {
					f.save(path, base)
					files[path] = base
					counts["add"]++
				}
			} else {
				text := base
				switch operation {
				case "comment":
					text += fmt.Sprintf("\n// seeded_%03d_%d\n", step, rng.Intn(10000))
				case "new_function":
					text += fmt.Sprintf("\nfunc MatrixSeeded%d() int { return %d }\n", step, rng.Intn(10000))
				case "value_or_owner":
					if path == "config.go" {
						text = strings.ReplaceAll(base, "MATRIX_DATABASE_URL", fmt.Sprintf("MATRIX_DATABASE_URL_%03d", step))
					} else {
						text = strings.ReplaceAll(base, "/v1/items", fmt.Sprintf("/v1/items-%03d", step))
					}
				}
				if text == files[path] {
					text += fmt.Sprintf("\n// distinct_seeded_save_%03d\n", step)
				}
				require.NotEqual(t, files[path], text, "every logical step changes accepted bytes or census")
				if _, exists := files[path]; !exists {
					counts["add"]++
				}
				f.save(path, text)
				files[path] = text
				counts[operation]++
			}
			var hooks *contractConvergenceHooks
			fired := false
			if placement != "none" {
				move := func() {
					if fired {
						return
					}
					fired = true
					// Exercise a real source-only accepted edit at the cutpoint.
					// Rewinding the input-state row after a boundary-changing save
					// invents an impossible journal/key pair and strands its work.
					before, found, err := f.store.ContractInputStateContext(t.Context(), "fixture", "")
					require.NoError(t, err)
					require.True(t, found)
					prior, known, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/server.go")
					require.NoError(t, err)
					require.True(t, known)
					epoch := f.epoch.Load()
					text := files["server.go"] + fmt.Sprintf("\n// accepted_interleaving_%03d\n", step)
					f.save("server.go", text)
					after, found, err := f.store.ContractInputStateContext(t.Context(), "fixture", "")
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, before, after, "the real edit must retain the exact accepted namespace identity")
					current, known, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/server.go")
					require.NoError(t, err)
					require.True(t, known)
					require.NotEqual(t, prior.SourceFingerprint, current.SourceFingerprint)
					require.Greater(t, f.epoch.Load(), epoch, "the concurrent accepted source must actually publish")
					files["server.go"] = text
					cutpoints[placement]++
				}
				hooks = &contractConvergenceHooks{}
				if placement == "after_extract" {
					hooks.afterExtract = func(path string) {
						if path == "fixture/server.go" {
							move()
						}
					}
				} else {
					hooks.beforeFence = func(int) { move() }
				}
			}
			var ran bool
			previousCold, ran = runContractColdMatrixAcceptedState(t, f, &cache, before, previousCold, files, hooks)
			if ran {
				deltaJobs++
			} else {
				noOpStates++
			}
			require.Equal(t, deltaJobs, cache.deltas, "the same retained actor advances only for actual key-moving jobs")
			if placement != "none" {
				require.True(t, ran, "the interleaving's route edit must create a real delta job")
				require.True(t, fired, "the named interleaving must actually execute")
			}
			// Cold fixtures created with this subtest are closed before the
			// next step; only the one incremental actor/cache spans steps.
		})
		if t.Failed() {
			return // preserve the first reproducible seed/step, not cascades
		}
	}
	for _, operation := range []string{"comment", "new_function", "value_or_owner", "remove", "add", "restore"} {
		require.Positive(t, counts[operation], operation)
	}
	require.Equal(t, 20, cutpoints["after_extract"])
	require.Equal(t, 20, cutpoints["before_fence"])
	require.Equal(t, 200, deltaJobs+noOpStates)
	require.Positive(t, deltaJobs)
	require.Positive(t, noOpStates, "the campaign must exercise certified source-only identity carry")
	t.Logf("seed=%d completed=200 delta_jobs=%d no_op_states=%d mutations=%v deterministic_cutpoints=%v", seed, deltaJobs, noOpStates, counts, cutpoints)
}
