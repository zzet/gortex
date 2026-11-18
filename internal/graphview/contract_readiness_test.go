package graphview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func contractWorkGeneration(t *testing.T, store *store_sqlite.Store, rows ...graph.ContractWork) *store_sqlite.Store {
	t.Helper()
	id, handle := beginTestGeneration(t, store, "contract-work")
	// The test passes physical selected handles directly; token composition
	// must not depend on the graph/file masks or on catalog ancestry guessing.
	require.NoError(t, handle.SetContractWork(context.Background(), rows))
	publishTestGeneration(t, store, id)
	return handle
}

func testPendingContractWork(token, path string) graph.ContractWork {
	return graph.ContractWork{Token: token, OriginGeneration: 1, CheckoutID: "original-checkout", RepoPrefix: "repo", FilePath: path,
		InputVersion: "accepted-version", InputFingerprint: "accepted-inputs", State: graph.ContractWorkPending,
		Scope: graph.ContractWorkScope{Groups: []graph.ContractWorkGroup{{ContractID: "http::GET /route"}}}}
}

func TestContractReadinessScopedDebtAndExactAcknowledgments(t *testing.T) {
	s := openTestStore(t)
	require.NoError(t, s.SetContractState(graph.ContractState{RepoPrefix: "repo", IndexedSHA: "baseline", CompletedAt: 1}))
	deleted := testPendingContractWork("deleted-owner", "/repo/removed.go")
	deleted.Scope.Deleted = true
	newer := testPendingContractWork("newer-inputs", deleted.FilePath)
	newer.InputVersion = "newer-version"
	lower := contractWorkGeneration(t, s, deleted, newer)
	complete := deleted
	complete.State = graph.ContractWorkComplete
	upper := contractWorkGeneration(t, s, complete)
	selection := ContractSelection{RepoPrefixes: []string{"repo"}, FilePaths: []string{deleted.FilePath}}
	r, err := readContractReadiness(context.Background(), s, []*store_sqlite.Store{lower, upper}, "repo", selection)
	require.NoError(t, err)
	require.Equal(t, ContractReadiness{State: StateIncomplete, Pending: 1}, r, "old deletion acknowledgment cannot clear newer accepted work")
	selection.FilePaths = []string{"/repo/unrelated.go"}
	r, err = readContractReadiness(context.Background(), s, []*store_sqlite.Store{lower, upper}, "repo", selection)
	require.NoError(t, err)
	require.Equal(t, StateComplete, r.State, "unrelated scoped contract data remains usable")
	selection.FilePaths = nil
	selection.Groups = newer.Scope.Groups
	r, err = readContractReadiness(context.Background(), s, []*store_sqlite.Store{lower, upper}, "repo", selection)
	require.NoError(t, err)
	require.Equal(t, StateIncomplete, r.State, "matching companions retain old/new group debt")
}

func TestContractReadinessRejectsMismatchedCrossLayerAcknowledgment(t *testing.T) {
	for _, kind := range []string{"fingerprint", "scope", "actor", "origin", "regression"} {
		t.Run(kind, func(t *testing.T) {
			s := openTestStore(t)
			pending := testPendingContractWork("same-token", "/repo/owner.go")
			if kind == "regression" {
				pending.State = graph.ContractWorkComplete
			}
			lower := contractWorkGeneration(t, s, pending)
			ack := pending
			ack.State = graph.ContractWorkComplete
			switch kind {
			case "fingerprint":
				ack.InputFingerprint = "different"
			case "scope":
				ack.Scope.Deleted = true
			case "actor":
				ack.CheckoutID = "other-checkout"
			case "origin":
				ack.OriginGeneration++
			case "regression":
				ack.State = graph.ContractWorkPending
			}
			upper := contractWorkGeneration(t, s, ack)
			r, err := readContractReadiness(context.Background(), nil, []*store_sqlite.Store{lower, upper}, "repo", ContractSelection{RepoPrefixes: []string{"repo"}})
			require.Error(t, err)
			require.Equal(t, ContractReadiness{}, r)
		})
	}
}

func TestContractReadinessBaselineAndInheritedCorpusActorScope(t *testing.T) {
	s := openTestStore(t)
	selection := ContractSelection{RepoPrefixes: []string{"repo"}}
	r, err := newTestMaterializer(s).ContractReadiness(context.Background(), selection)
	require.NoError(t, err)
	require.Equal(t, StateUnavailable, r.State, "legacy silence is not a certified empty registry")
	require.NoError(t, s.SetContractState(graph.ContractState{RepoPrefix: "repo", IndexedSHA: "empty", CompletedAt: 1, ContractCount: 0}))
	foreign := testPendingContractWork("linked-checkout", "/repo/owner.go")
	require.NoError(t, s.SetContractWork(context.Background(), []graph.ContractWork{foreign}))
	r, err = readContractReadiness(context.Background(), s, nil, "repo", selection)
	require.NoError(t, err)
	require.Equal(t, StateComplete, r.State, "inherited corpus cannot read another linked checkout's tokens")
	primary := foreign
	primary.Token, primary.CheckoutID = "primary-inputs", ""
	require.NoError(t, s.SetContractWork(context.Background(), []graph.ContractWork{primary}))
	r, err = readContractReadiness(context.Background(), s, nil, "repo", selection)
	require.NoError(t, err)
	require.Equal(t, ContractReadiness{State: StateIncomplete, Pending: 1}, r)
}

func TestContractReadinessCancellationAndClosedStoreReturnNoStatus(t *testing.T) {
	s := openTestStore(t)
	selection := ContractSelection{RepoPrefixes: []string{"repo"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := readContractReadiness(ctx, s, nil, "repo", selection)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, ContractReadiness{}, r)
	require.NoError(t, s.Close())
	r, err = readContractReadiness(context.Background(), s, nil, "repo", selection)
	require.Error(t, err)
	require.Equal(t, ContractReadiness{}, r)
}

func TestContractReadinessUnknownAndSharedInputsStayConservative(t *testing.T) {
	selection := ContractSelection{RepoPrefixes: []string{"repo"}, FilePaths: []string{"/repo/unrelated.go"}}
	row := testPendingContractWork("shared", "/repo/constants.go")
	row.Scope.Causes = []string{"shared_constant_changed"}
	require.True(t, contractWorkRelevant(row, selection), "file-only selection cannot exclude dependency work")
	selection.Groups = []graph.ContractWorkGroup{{ContractID: "http::GET /different"}}
	require.False(t, contractWorkRelevant(row, selection), "complete known cohort excludes an unrelated explicit cohort")
	row.Scope.Unknown = true
	require.True(t, contractWorkRelevant(row, selection))
	row.Scope.Unknown = false
	row.Scope.Causes = []string{"unknown-new-producer-cause"}
	require.True(t, contractWorkRelevant(row, selection))
}
