package graphview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractServedValidationKeepsPayloadRevisionAndLeaseFence(t *testing.T) {
	for _, change := range []string{"revision", "payload_lease", "source_lease"} {
		t.Run(change, func(t *testing.T) {
			s := openTestStore(t)
			payload, _, shape := isolatedContractPayload(t, s, "/served", true)
			m := newTestMaterializer(s)
			state := graph.ContractInputState{RepoPrefix: "repo", InputVersion: "v", InputFingerprint: "fp", Accepted: true}
			require.NoError(t, s.BeginContractInputMutationContext(t.Context(), nil, state, nil))
			require.NoError(t, s.AcceptContractInputMutationContext(t.Context(), state))
			inputs, err := m.CaptureContractInputs(t.Context(), nil, "repo", "")
			require.NoError(t, err)
			sources, err := m.AcquireContractServedSources(t.Context(), inputs)
			require.NoError(t, err)
			defer sources.Close()
			m.contractAttachmentReader = contractAttachmentReadFunc(func(context.Context, graph.ContractAttachmentKey) (*graph.ContractAttachment, error) {
				return &graph.ContractAttachment{RepoPrefix: "repo", PayloadGeneration: payload.ViewGeneration(), InputVersion: "v", InputFingerprint: "fp"}, nil
			})
			key := graph.ContractAttachmentKey{RepoPrefix: "repo", InputVersion: "v", InputFingerprint: "fp"}
			served, err := m.OpenServedContractAnalysis(t.Context(), key, payload.ViewGeneration(), inputs.Witnesses, sources)
			require.NoError(t, err)
			defer served.Close()
			require.NoError(t, served.ValidateServed(t.Context()))
			switch change {
			case "revision":
				correction, err := s.BeginDerivedCorrection(t.Context(), store_sqlite.DerivedCorrectionRequest{GenerationID: payload.ViewGeneration(), Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
				require.NoError(t, err)
				require.NoError(t, correction.ReplaceSourceEdges(t.Context(), nil, nil, []*graph.Node{{ID: shape.ID, Kind: shape.Kind, FilePath: shape.FilePath, RepoPrefix: "repo", Meta: map[string]any{"shape": &contracts.Shape{Kind: "string"}}}}))
				require.ErrorIs(t, served.ValidateServed(t.Context()), graph.ErrContractProjectionStale)
				_, err = correction.Finish(t.Context())
				require.NoError(t, err)
			case "payload_lease":
				served.lease.Release()
				require.ErrorIs(t, served.ValidateServed(t.Context()), graph.ErrContractProjectionStale)
			case "source_lease":
				served.servedSources.Close()
				require.ErrorIs(t, served.ValidateServed(t.Context()), graph.ErrContractProjectionStale)
			}
		})
	}
}

func TestContractServedSourcesRejectForeignInputAuthority(t *testing.T) {
	s := openTestStore(t)
	m := newTestMaterializer(s)
	_, err := m.AcquireContractServedSources(t.Context(), &SelectedContractInputs{State: graph.ContractInputState{Accepted: true}})
	require.ErrorIs(t, err, graph.ErrContractInputVector)
}
