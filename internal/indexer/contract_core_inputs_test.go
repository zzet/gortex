package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

type contractCoreBackendProbe struct {
	prior                             *contractBoundaryReceipt
	known                             bool
	priorErr, beginErr, dependentsErr error
	dependents                        []contractCoreDependent
	begun                             [][]contractCoreInputChange
	accepted                          int
	receipts                          map[string]*contractBoundaryReceipt
	pendingReceipts                   map[string]*contractBoundaryReceipt
	onAccept                          func()
}

func (p *contractCoreBackendProbe) PriorBoundaryReceipt(_ context.Context, path string) (*contractBoundaryReceipt, bool, error) {
	if p.receipts != nil {
		return p.receipts[path], p.known, p.priorErr
	}
	return p.prior, p.known, p.priorErr
}
func (p *contractCoreBackendProbe) BoundaryDependents(context.Context, []string) ([]contractCoreDependent, error) {
	return p.dependents, p.dependentsErr
}
func (p *contractCoreBackendProbe) BeginBoundaryMutation(_ context.Context, changes []contractCoreInputChange) error {
	if p.beginErr != nil {
		return p.beginErr
	}
	p.begun = append(p.begun, changes)
	if p.pendingReceipts != nil {
		for _, change := range changes {
			p.pendingReceipts[change.FilePath] = change.Current
		}
	}
	return nil
}
func (p *contractCoreBackendProbe) AcceptBoundaryMutation(context.Context) error {
	p.accepted++
	if p.receipts != nil {
		for path, receipt := range p.pendingReceipts {
			p.receipts[path] = receipt
		}
	}
	if p.onAccept != nil {
		p.onAccept()
	}
	return nil
}

func TestContractCoreInputsUncertaintyDoesNotDeleteAcceptedSource(t *testing.T) {
	source := []byte("package fixture\nfunc Value() int { return 2 }\n")
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	prior := boundaryTestReceipt(t, receiptHandlerSource)
	backend := &contractCoreBackendProbe{prior: &prior, known: true}
	journal, err := newContractCoreInputJournal(context.Background(), backend)
	require.NoError(t, err)
	// A deliberately missing contract extraction result represents a failed
	// optional analysis, not a failed ordinary source extraction/application.
	require.NoError(t, journal.prepare(context.Background(), idx, "fixture/routes.go", "go", source, nil, false))
	require.NoError(t, journal.begin([]string{"fixture/routes.go"}))
	change := backend.begun[0][0]
	require.True(t, change.Delta.Scope.Unknown)
	require.Contains(t, change.Delta.Scope.Causes, "accepted_contract_extraction_incomplete")
	require.False(t, change.Deleted)
	require.False(t, change.Delta.Scope.Deleted)
	require.Equal(t, contractInputHash(source), change.SourceFingerprint)
	require.Empty(t, journal.changes, "bounded staged inputs drain after durable begin")
	require.NoError(t, journal.accept(nil))
	require.Equal(t, 1, backend.accepted, "acceptance acknowledges core application, not contract completion")
	require.True(t, backend.begun[0][0].Delta.Scope.Unknown, "contract readiness stays incomplete")
}

func TestContractCoreInputsCertifiedNewFileVersusUnknownLegacy(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	source := []byte("package fixture\nfunc Value() int { return 1 }\n")
	result, err := languages.NewGoExtractor().Extract("fixture/new.go", source)
	require.NoError(t, err)
	if result.Tree != nil {
		defer result.Tree.Release()
	}
	for _, known := range []bool{true, false} {
		backend := &contractCoreBackendProbe{known: known}
		journal, err := newContractCoreInputJournal(context.Background(), backend)
		require.NoError(t, err)
		require.NoError(t, journal.prepare(context.Background(), idx, "fixture/new.go", "go", source, result, false))
		require.NoError(t, journal.begin([]string{"fixture/new.go"}))
		require.Equal(t, !known, backend.begun[0][0].Delta.Scope.Unknown)
		if known {
			require.Empty(t, backend.begun[0][0].Delta.Scope.Causes)
		}
	}
}

func TestContractCoreInputsBeginFailureAndCoreFailureCannotAccept(t *testing.T) {
	refusal := errors.New("durable pending write failed")
	backend := &contractCoreBackendProbe{known: true, beginErr: refusal}
	journal, err := newContractCoreInputJournal(context.Background(), backend)
	require.NoError(t, err)
	journal.changes["deleted.go"] = contractCoreInputChange{FilePath: "deleted.go", Deleted: true}
	require.ErrorIs(t, journal.begin([]string{"deleted.go"}), refusal)
	require.Empty(t, journal.begun)
	require.Zero(t, backend.accepted)
	backend.beginErr = nil
	require.NoError(t, journal.begin([]string{"deleted.go"}))
	failedCore := errors.New("core source fence refused")
	require.ErrorIs(t, journal.accept(failedCore), failedCore)
	require.Zero(t, backend.accepted)
}

func TestContractCoreInputsRetainsDependentOwnerAndReplacesStagedReparse(t *testing.T) {
	dependent := contractCoreDependent{RepoPrefix: "consumer", CheckoutID: "linked", FilePath: "consumer/client.go", ReceiptVersion: "v", ReceiptFingerprint: "accepted", SourceFingerprint: "source", Accepted: true, Scope: graph.ContractWorkScope{LookupKeys: []string{"negative-name"}}}
	backend := &contractCoreBackendProbe{known: true, dependents: []contractCoreDependent{dependent}}
	journal, err := newContractCoreInputJournal(context.Background(), backend)
	require.NoError(t, err)
	journal.changes["provider.go"] = contractCoreInputChange{FilePath: "provider.go", Delta: contractBoundaryDelta{ChangedProducedKeys: []string{"negative-name"}}}
	require.NoError(t, journal.begin([]string{"provider.go"}))
	require.Equal(t, dependent, backend.begun[0][0].Dependents[0])
	require.Empty(t, backend.begun[0][0].Delta.Scope.LookupKeys, "another actor's scope is not merged into this source actor")
	journal.changes["provider.go"] = contractCoreInputChange{FilePath: "provider.go", Deleted: true, Delta: contractBoundaryDelta{Scope: graph.ContractWorkScope{Deleted: true}}}
	require.NoError(t, journal.begin([]string{"provider.go"}))
	require.Len(t, backend.begun, 2, "a later accepted reparse must not silently retain first staged inputs")
	require.True(t, backend.begun[1][0].Deleted)
}

func TestContractCoreInputsCancellationCannotAcknowledge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	backend := &contractCoreBackendProbe{known: true}
	journal, err := newContractCoreInputJournal(ctx, backend)
	require.NoError(t, err)
	journal.changes["file.go"] = contractCoreInputChange{FilePath: "file.go"}
	require.NoError(t, journal.begin([]string{"file.go"}))
	cancel()
	require.ErrorIs(t, journal.accept(nil), context.Canceled)
	require.Zero(t, backend.accepted)
}
