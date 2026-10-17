package indexer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
)

type metadataLimitDependencyBackend struct {
	*contractCoreBackendProbe
	store *store_sqlite.Store
	keys  []string
}

func (b *metadataLimitDependencyBackend) BoundaryDependents(ctx context.Context, keys []string) ([]contractCoreDependent, error) {
	b.keys = append([]string(nil), keys...)
	rows, err := b.store.ContractBoundaryReceiptsForLookupKeysContext(ctx, keys, 4096)
	if err != nil {
		return nil, err
	}
	var out []contractCoreDependent
	for _, row := range rows {
		out = append(out, contractCoreDependent{RepoPrefix: row.RepoPrefix, CheckoutID: row.CheckoutID, FilePath: row.FilePath,
			ReceiptVersion: row.Version, ReceiptFingerprint: row.Fingerprint, SourceFingerprint: row.SourceFingerprint, Accepted: row.Accepted, Scope: row.Scope})
	}
	return out, nil
}

func TestContractJournalFullProducerFrontierKeepsKnownDependencyMembership(t *testing.T) {
	var source strings.Builder
	source.WriteString("package fixture\n")
	for i := 0; i < 1024; i++ {
		fmt.Fprintf(&source, "func Pending%d() int { return %d }\n", i, i)
	}
	const path = "fixture/pending.go"
	src := []byte(source.String())
	parsed, err := languages.NewGoExtractor().Extract(path, src)
	require.NoError(t, err)
	if parsed.Tree != nil {
		defer parsed.Tree.Release()
	}
	idx := newTestIndexer(graph.New())
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	store := newFTSStore(t)
	backend := &metadataLimitDependencyBackend{contractCoreBackendProbe: &contractCoreBackendProbe{known: true}, store: store}
	journal, err := newContractCoreInputJournal(t.Context(), backend)
	require.NoError(t, err)
	require.NoError(t, journal.prepare(t.Context(), idx, path, "go", src, parsed, false))
	change := journal.changes[path]
	require.Len(t, change.Delta.ChangedProducedKeys, 4096, "the retained 1024-function fixture's exact ordinary frontier")
	require.False(t, change.Delta.Scope.Unknown)
	consumer := graph.ContractBoundaryReceipt{RepoPrefix: "other", CheckoutID: "other-actor", FilePath: "other/consumer.go", Version: "boundary-fixture", Fingerprint: "consumer", SourceFingerprint: "consumer-source", LookupKeys: []string{change.Delta.ChangedProducedKeys[0]}}
	opaque := consumer
	opaque.FilePath, opaque.Fingerprint = "other/opaque.go", "opaque"
	opaque.LookupKeys = []string{graph.ContractBoundaryUnknownMembershipKey}
	opaque.Scope = graph.ContractWorkScope{Unknown: true, Causes: []string{"opaque_boundary"}}
	require.NoError(t, store.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{consumer, opaque}))
	require.NoError(t, store.AcceptContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{consumer, opaque}))
	require.NoError(t, journal.begin([]string{path}))
	require.Len(t, backend.keys, 4097, "one fixed metadata key is added to the unchanged 4096 ordinary-key bound")
	require.Contains(t, backend.keys, graph.ContractBoundaryUnknownMembershipKey)
	require.Len(t, backend.begun, 1)
	begun := backend.begun[0][0]
	require.NotContains(t, begun.Delta.Scope.Causes, "dependency_membership_unknown", "a query-boundary mismatch cannot fabricate new unknown namespace debt")
	require.False(t, begun.Delta.Scope.Unknown)
	require.Len(t, begun.Dependents, 2)
	for _, dependent := range begun.Dependents {
		require.Equal(t, "other", dependent.RepoPrefix)
		require.Equal(t, "other-actor", dependent.CheckoutID)
		require.True(t, dependent.Accepted)
		if dependent.FilePath == opaque.FilePath {
			require.Equal(t, opaque.Scope, dependent.Scope)
		}
	}
}
