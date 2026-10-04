package graphview

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type contractAttachmentReadFunc func(context.Context, graph.ContractAttachmentKey) (*graph.ContractAttachment, error)

func (f contractAttachmentReadFunc) GetContractAttachmentContext(ctx context.Context, key graph.ContractAttachmentKey) (*graph.ContractAttachment, error) {
	return f(ctx, key)
}

func TestContractAnalysisCancellationAndFailedLeaseRecheckReleasePayload(t *testing.T) {
	s := openTestStore(t)
	handle, source, _ := isolatedContractPayload(t, s, "/selected", true)
	core := graph.New()
	core.AddNode(source)
	m := newTestMaterializer(s)
	input := ContractAnalysisInput{RepoPrefix: "repo", InputVersion: "v", InputFingerprint: "fp"}
	calls := 0
	failure := errors.New("attachment recheck failed")
	m.contractAttachmentReader = contractAttachmentReadFunc(func(context.Context, graph.ContractAttachmentKey) (*graph.ContractAttachment, error) {
		calls++
		if calls == 2 {
			return nil, failure
		}
		return &graph.ContractAttachment{RepoPrefix: "repo", PayloadGeneration: handle.ViewGeneration(), InputVersion: "v", InputFingerprint: "fp"}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := m.OpenContractAnalysis(ctx, core, input)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, v)
	require.Zero(t, calls)
	v, err = m.OpenContractAnalysis(context.Background(), core, input)
	require.ErrorIs(t, err, failure)
	require.Nil(t, v)
	require.False(t, m.Leases.InUse(handle.ViewGeneration()))
}

func TestContractAnalysisCheckedShapeRefusesCorrectionAfterSelection(t *testing.T) {
	s := openTestStore(t)
	handle, source, shape := isolatedContractPayload(t, s, "/selected", true)
	core := graph.New()
	core.AddNode(source)
	m := newTestMaterializer(s)
	m.contractAttachmentReader = contractAttachmentReadFunc(func(context.Context, graph.ContractAttachmentKey) (*graph.ContractAttachment, error) {
		return &graph.ContractAttachment{RepoPrefix: "repo", PayloadGeneration: handle.ViewGeneration(), InputVersion: "v", InputFingerprint: "fp"}, nil
	})
	v, err := m.OpenContractAnalysis(context.Background(), core, ContractAnalysisInput{RepoPrefix: "repo", InputVersion: "v", InputFingerprint: "fp"})
	require.NoError(t, err)
	defer v.Close()
	// An administrative correction cannot be mistaken for an empty shape.
	correction, err := s.BeginDerivedCorrection(context.Background(), store_sqlite.DerivedCorrectionRequest{GenerationID: handle.ViewGeneration(), Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	require.NoError(t, err)
	require.NoError(t, correction.ReplaceSourceEdges(context.Background(), nil, nil, []*graph.Node{{ID: shape.ID, Kind: shape.Kind, FilePath: shape.FilePath, RepoPrefix: "repo", Meta: map[string]any{"shape": &contracts.Shape{Kind: "string"}}}}))
	got, err := v.ShapeNode(context.Background(), shape.ID)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Nil(t, got)
	_, err = correction.Finish(context.Background())
	require.NoError(t, err)
}

func isolatedContractPayload(t *testing.T, s *store_sqlite.Store, route string, complete bool) (*store_sqlite.Store, *graph.Node, *graph.Node) {
	t.Helper()
	id, handle := beginTestGeneration(t, s, "isolated-contract-analysis")
	source := &graph.Node{ID: "repo/handler.go::serve", Kind: graph.KindFunction, FilePath: "repo/handler.go", RepoPrefix: "repo", Meta: map[string]any{"body": "selected-core"}}
	target := &graph.Node{ID: "http::GET::route", Name: "http::GET::route", Kind: graph.KindContract, FilePath: source.FilePath, RepoPrefix: "repo", Language: "contract",
		Meta: map[string]any{"type": "http", "role": "provider", "symbol_id": source.ID, "contract_owner_record": true, "contract_meta": map[string]any{"method": "GET", "path": route}}}
	shape := &graph.Node{ID: "repo/types.go::Response", Kind: graph.KindType, FilePath: "repo/types.go", RepoPrefix: "repo", Meta: map[string]any{"shape": &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: "selected", Type: "string"}}}}}
	edge := &graph.Edge{From: source.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: source.FilePath, Meta: map[string]any{
		"contract_owner_repo_prefix": "repo", "contract_owner_type": "http", "contract_owner_symbol_id": source.ID, "contract_owner_meta": target.Meta["contract_meta"],
	}}
	handle.AddBatch([]*graph.Node{target, shape}, []*graph.Edge{edge})
	state := store_sqlite.ProducerStateIncomplete
	if complete {
		state = store_sqlite.ProducerStateComplete
	}
	require.NoError(t, handle.SetProducerState(store_sqlite.ProducerCompleteness{Producer: string(CapContracts), State: state}))
	publishTestGeneration(t, s, id)
	return handle, source, shape
}

func TestContractAnalysisKeepsSelectedCoreAndIsolatesRegistryAndShapes(t *testing.T) {
	s := openTestStore(t)
	handle, source, shape := isolatedContractPayload(t, s, "/selected", true)
	core := graph.New()
	core.AddNode(source)
	core.AddNode(&graph.Node{ID: shape.ID, Kind: graph.KindType, FilePath: shape.FilePath, RepoPrefix: "repo", Meta: map[string]any{"core-declaration": "new", "shape": &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: "old-core-shape"}}}}})
	core.AddNode(&graph.Node{ID: "http::GET::primary-only", Kind: graph.KindContract, RepoPrefix: "repo", Meta: map[string]any{"type": "http", "role": "provider", "contract_meta": map[string]any{"path": "/primary"}}})
	m := newTestMaterializer(s)
	input := ContractAnalysisInput{RepoPrefix: "repo", CheckoutID: "linked", InputVersion: "contract-v1", InputFingerprint: "selected-inputs"}
	attachment := &graph.ContractAttachment{RepoPrefix: input.RepoPrefix, CheckoutID: input.CheckoutID, PayloadGeneration: handle.ViewGeneration(), InputVersion: input.InputVersion, InputFingerprint: input.InputFingerprint}
	m.contractAttachmentReader = contractAttachmentReadFunc(func(_ context.Context, key graph.ContractAttachmentKey) (*graph.ContractAttachment, error) {
		require.Equal(t, input.InputVersion, key.InputVersion)
		require.Equal(t, input.InputFingerprint, key.InputFingerprint)
		return attachment, nil
	})
	v, err := m.OpenContractAnalysis(context.Background(), core, input)
	require.NoError(t, err)
	defer v.Close()
	require.Same(t, core, v.Core)
	require.True(t, m.Leases.InUse(handle.ViewGeneration()))
	registry, err := contracts.LoadRegistryFromGraphChecked(context.Background(), v.RegistryReader, contracts.RegistryLoadOptions{RepoPrefix: "repo"})
	require.NoError(t, err)
	require.Len(t, registry.All(), 1)
	require.Equal(t, "/selected", registry.All()[0].Meta["path"])
	require.Equal(t, "selected-core", core.GetNode(source.ID).Meta["body"])
	schema, err := v.ShapeNode(context.Background(), shape.ID)
	require.NoError(t, err)
	require.Equal(t, shape.Meta["shape"], schema.Meta["shape"])
	require.Equal(t, "new", core.GetNode(shape.ID).Meta["core-declaration"])
	require.Equal(t, "old-core-shape", core.GetNode(shape.ID).Meta["shape"].(*contracts.Shape).Fields[0].Name)
	v.Close()
	require.False(t, m.Leases.InUse(handle.ViewGeneration()))
}

func TestContractAnalysisLeaseRechecksExactAttachmentAndReleasesOldPayload(t *testing.T) {
	s := openTestStore(t)
	old, source, _ := isolatedContractPayload(t, s, "/old", true)
	current, _, _ := isolatedContractPayload(t, s, "/current", true)
	core := graph.New()
	core.AddNode(source)
	m := newTestMaterializer(s)
	input := ContractAnalysisInput{RepoPrefix: "repo", InputVersion: "v", InputFingerprint: "fp"}
	calls := 0
	m.contractAttachmentReader = contractAttachmentReadFunc(func(_ context.Context, _ graph.ContractAttachmentKey) (*graph.ContractAttachment, error) {
		calls++
		generation := current.ViewGeneration()
		if calls == 1 {
			generation = old.ViewGeneration()
		}
		return &graph.ContractAttachment{RepoPrefix: "repo", PayloadGeneration: generation, InputVersion: "v", InputFingerprint: "fp"}, nil
	})
	v, err := m.OpenContractAnalysis(context.Background(), core, input)
	require.NoError(t, err)
	defer v.Close()
	require.Equal(t, 4, calls)
	require.Equal(t, current.ViewGeneration(), v.Attachment.PayloadGeneration)
	require.False(t, m.Leases.InUse(old.ViewGeneration()))
	require.True(t, m.Leases.InUse(current.ViewGeneration()))
}

func TestContractAnalysisRefusesDifferentInputsOrUncompletedPayload(t *testing.T) {
	for _, scenario := range []string{"historical-inputs", "incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			s := openTestStore(t)
			handle, source, _ := isolatedContractPayload(t, s, "/latest", scenario != "incomplete")
			core := graph.New()
			core.AddNode(source)
			m := newTestMaterializer(s)
			m.contractAttachmentReader = contractAttachmentReadFunc(func(_ context.Context, _ graph.ContractAttachmentKey) (*graph.ContractAttachment, error) {
				return &graph.ContractAttachment{RepoPrefix: "repo", PayloadGeneration: handle.ViewGeneration(), InputVersion: "v", InputFingerprint: "latest"}, nil
			})
			fingerprint := "latest"
			if scenario == "historical-inputs" {
				fingerprint = "historical"
			}
			v, err := m.OpenContractAnalysis(context.Background(), core, ContractAnalysisInput{RepoPrefix: "repo", InputVersion: "v", InputFingerprint: fingerprint})
			require.Error(t, err)
			require.Nil(t, v)
			require.False(t, m.Leases.InUse(handle.ViewGeneration()))
		})
	}
}
