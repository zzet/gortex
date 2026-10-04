package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func contractBaselineAuthorityFixture(t *testing.T) (ContractFollowupCaptureOptions, *graphview.BasePin, *OutputGenerationAuthority) {
	t.Helper()
	ctx := context.Background()
	idx, store := newSQLiteIndexer(t)
	idx.SetRepoPrefix("fixture")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "routes.go"), "package fixture\nfunc register(router *Router) { router.GET(\"/before\", serve) }\nfunc serve() {}\n")
	backend, err := newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
	require.NoError(t, err)
	idx.contractCoreInputs, err = newContractCoreInputJournal(ctx, backend)
	require.NoError(t, err)
	_, err = idx.IndexCtx(ctx, root)
	require.NoError(t, err)
	prior, found, err := store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/routes.go")
	require.NoError(t, err)
	require.True(t, found)
	bad := *prior
	bad.Accepted = false
	bad.Version = "legacy-unknown"
	bad.Scope.Unknown = true
	require.NoError(t, store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{bad}))
	mi := NewMultiIndexer(store, idx.registry, search.NewNull(), nil, zap.NewNop())
	mi.repos["fixture"] = &RepoMetadata{RepoPrefix: "fixture", RootPath: root}
	mi.indexers["fixture"] = idx
	leases := graphview.NewLeaseManager()
	registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: "fixture", RootIdentity: root, Incarnation: t.Name()}, nil)
	require.NoError(t, err)
	_, err = leases.CaptureInitialRawRepositorySource(ctx, registration, "accepted-source")
	require.NoError(t, err)
	pin := leases.AcquireBaseCorpus("fixture")
	t.Cleanup(pin.Release)
	authority := NewOutputGenerationAuthority(leases)
	mi.SetOutputGenerationAuthority(authority)
	options := ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases}, MultiIndexer: mi, Registry: idx.registry, Config: idx.config, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }}
	return options, pin, authority
}

func TestContractBaselineAuthorityRefusesClosedAndChangedAcceptedSource(t *testing.T) {
	for _, mode := range []string{"closed", "source_changed"} {
		t.Run(mode, func(t *testing.T) {
			options, pin, authority := contractBaselineAuthorityFixture(t)
			require.NoError(t, pin.ValidateAcceptedCurrent())
			before, found, err := options.Store.ContractInputStateContext(t.Context(), "fixture", "")
			require.NoError(t, err)
			require.True(t, found)
			if mode == "closed" {
				authority.Close()
			} else {
				changed := false
				options.Yield = func(ctx context.Context) error {
					if !changed {
						changed = true
						lease, err := options.Materializer.Leases.AcquireBaseCorpusMutation(ctx, "fixture", options.MultiIndexer.repoRootPath("fixture"))
						if err != nil {
							return err
						}
						err = lease.Complete("newer-accepted-source")
						lease.Release()
						if err != nil {
							return err
						}
					}
					return ctx.Err()
				}
			}
			err = reconcilePrimaryContractBaseline(t.Context(), options, "fixture")
			if mode == "closed" {
				require.ErrorIs(t, err, ErrOutputMutationAuthorityClosed)
				require.Zero(t, authority.Stats().Issued)
				require.NoError(t, pin.ValidateAcceptedCurrent())
			} else {
				require.ErrorIs(t, err, graphview.ErrBaseCorpusChanged)
				require.ErrorIs(t, pin.ValidateAcceptedCurrent(), graphview.ErrBaseCorpusChanged)
				require.Equal(t, authority.Stats().Issued, authority.Stats().Settled)
				require.Zero(t, authority.Stats().LiveOwners)
			}
			baseline, err := options.Store.ContractBoundaryReceiptBaselineContext(t.Context(), "fixture", "")
			require.NoError(t, err)
			require.Nil(t, baseline, "refused reconstruction cannot certify a baseline")
			state, found, err := options.Store.ContractInputStateContext(t.Context(), "fixture", "")
			require.NoError(t, err)
			require.True(t, found)
			if mode == "closed" {
				require.Equal(t, before, state, "closed authority cannot mutate the input identity")
			} else {
				require.False(t, state.Accepted, "changed source cannot certify the pending input")
			}
			inputs, err := options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
			require.NoError(t, err)
			require.False(t, inputs.State.Accepted, "refused reconstruction is not contract-ready")
		})
	}
}

func TestContractBaselineOutputReceiptSettlesOnPanic(t *testing.T) {
	options, pin, authority := contractBaselineAuthorityFixture(t)
	require.PanicsWithValue(t, "baseline panic", func() {
		_ = options.MultiIndexer.withRepositoryMutationLanes(t.Context(), []string{"fixture"}, func() error {
			return options.MultiIndexer.withContractBaselineOutput(t.Context(), options.Store, "fixture", OutputEntryContractBaseline, func() error {
				require.NoError(t, pin.ValidateAcceptedCurrent())
				panic("baseline panic")
			})
		})
	})
	stats := authority.Stats()
	require.Equal(t, uint64(1), stats.Entries[OutputEntryContractBaseline])
	require.Equal(t, stats.Issued, stats.Settled)
	require.Zero(t, stats.LiveOwners)
	require.Zero(t, stats.Witnessed)
	require.NoError(t, pin.ValidateAcceptedCurrent())
}
