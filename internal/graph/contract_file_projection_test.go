package graph

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractFileProjectionPreservesRecordedOwnershipAndSiblings(t *testing.T) {
	g := New()
	a := &Node{ID: "a.go::handler", Kind: KindFunction, FilePath: "a.go", RepoPrefix: "a"}
	b := &Node{ID: "b.go::handler", Kind: KindFunction, FilePath: "b.go", RepoPrefix: "b"}
	c := &Node{ID: "contract", Kind: KindContract, FilePath: "canonical.go", Meta: map[string]any{"contract_meta": "canonical"}}
	legacy := &Node{ID: "legacy", Kind: KindContract, FilePath: "recorded.go", Meta: map[string]any{"contract_meta": "legacy"}}
	file := &Node{ID: "recorded.go", Kind: KindFile, FilePath: "recorded.go", Meta: map[string]any{"contract_dependency_stamp": "stamp"}}
	owners := []*Edge{
		{From: a.ID, To: c.ID, Kind: EdgeProvides, FilePath: "recorded.go", Meta: map[string]any{"contract_meta": map[string]any{"file": "recorded.go"}}},
		{From: b.ID, To: c.ID, Kind: EdgeConsumes, FilePath: "sibling.go"},
		{From: a.ID, To: legacy.ID, Kind: EdgeHandlesRoute, FilePath: "foreign.go"},
	}
	g.AddBatch([]*Node{a, b, c, legacy, file}, owners)
	p, err := g.LoadContractFileProjectionContext(context.Background(), "a", []string{"recorded.go", "recorded.go"})
	require.NoError(t, err)
	require.Len(t, p.FileNodes["recorded.go"], 2)
	require.Equal(t, []*Node{legacy}, p.ScalarNodes)
	require.Equal(t, c, p.Targets[c.ID])
	require.Len(t, p.OwnerRows, 3)
	for _, n := range p.FileNodes["recorded.go"] {
		if n.Kind == KindFile {
			require.Equal(t, "stamp", n.Meta["contract_dependency_stamp"])
		}
	}
	scopes := map[string]string{}
	for _, row := range p.OwnerRows {
		scopes[row.Edge.From+string(row.Edge.Kind)] = row.RepoPrefix
	}
	require.Equal(t, "b", scopes[b.ID+string(EdgeConsumes)])
	p, err = g.LoadContractFileProjectionContext(context.Background(), "a", []string{"a.go"})
	require.NoError(t, err)
	require.Len(t, p.OffFileOwnerRows, 2)
	require.Empty(t, p.Targets)
}

type countedContractLayer struct {
	*OverlayLayer
	fileCalls, idCalls int
}

func (l *countedContractLayer) LayerContractFileProjectionContext(ctx context.Context, repo string, paths []string) (ContractFileProjection, error) {
	l.fileCalls++
	return l.OverlayLayer.LayerContractFileProjectionContext(ctx, repo, paths)
}
func (l *countedContractLayer) LayerContractIDProjectionContext(ctx context.Context, ids []string) (ContractFileProjection, error) {
	l.idCalls++
	return l.OverlayLayer.LayerContractIDProjectionContext(ctx, ids)
}
func TestContractFileProjectionHydratesGroupsOnceAcrossDeepStack(t *testing.T) {
	base := New()
	base.AddBatch([]*Node{{ID: "a.go::handler", Kind: KindFunction, FilePath: "a.go"}, {ID: "contract", Kind: KindContract}}, []*Edge{{From: "a.go::handler", To: "contract", Kind: EdgeProvides, FilePath: "a.go"}})
	var r Reader = base
	var layers []*countedContractLayer
	for i := 0; i < 8; i++ {
		l := &countedContractLayer{OverlayLayer: NewOverlayLayer()}
		layers = append(layers, l)
		r = NewOverlaidViewWithLayer(r, l)
	}
	p, err := r.(ContractFileProjectionReader).LoadContractFileProjectionContext(context.Background(), "", []string{"a.go"})
	require.NoError(t, err)
	require.Len(t, p.OwnerRows, 1)
	for i, l := range layers {
		require.Equal(t, 1, l.fileCalls, fmt.Sprint(i))
		require.Equal(t, 3, l.idCalls, fmt.Sprint(i))
	}
}

type failedContractLayer struct {
	*OverlayLayer
	failure error
}

func (l *failedContractLayer) LayerContractIDProjectionContext(context.Context, []string) (ContractFileProjection, error) {
	return ContractFileProjection{OwnerRows: []RepoEdgeRow{{Edge: &Edge{From: "partial"}}}}, l.failure
}
func TestContractFileProjectionFailsClosedDuringEndpointFiltering(t *testing.T) {
	base := New()
	source := &Node{ID: "a.go::handler", Kind: KindFunction, FilePath: "a.go"}
	base.AddBatch([]*Node{source, {ID: "contract", Kind: KindContract}}, []*Edge{{From: source.ID, To: "contract", Kind: EdgeProvides, FilePath: "recorded.go"}})
	sentinel := errors.New("checked endpoint read failed")
	layer := &failedContractLayer{OverlayLayer: NewOverlayLayer(), failure: sentinel}
	layer.MarkFile("a.go", false)
	layer.AddNode("a.go", source)
	p, err := NewOverlaidViewWithLayer(base, layer).LoadContractFileProjectionContext(context.Background(), "", []string{"recorded.go"})
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, ContractFileProjection{}, p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err = base.LoadContractFileProjectionContext(ctx, "", []string{"a.go"})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, ContractFileProjection{}, p)
}

func TestContractFileProjectionFileAndNodeTombstones(t *testing.T) {
	base := New()
	source := &Node{ID: "a.go::handler", Kind: KindFunction, Name: "handler", FilePath: "a.go"}
	base.AddBatch([]*Node{source, {ID: "contract", Kind: KindContract, FilePath: "canonical.go"}}, []*Edge{{From: source.ID, To: "contract", Kind: EdgeProvides, FilePath: "recorded.go"}})
	for _, fileDelete := range []bool{false, true} {
		layer := NewOverlayLayer()
		if fileDelete {
			layer.MarkFile("a.go", true)
		} else {
			layer.MarkRemoved(source.Name, source.ID)
		}
		p, err := NewOverlaidView(base, layer).LoadContractFileProjectionContext(context.Background(), "", []string{"recorded.go"})
		require.NoError(t, err)
		require.Empty(t, p.OwnerRows)
	}
}

func TestContractFileProjectionRefusesOrphanOwnershipInsteadOfRevivingScalar(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{{ID: "legacy", Kind: KindContract, FilePath: "legacy.go"}}, []*Edge{{From: "missing-source", To: "legacy", Kind: EdgeProvides, FilePath: "other.go"}})
	p, err := g.LoadContractFileProjectionContext(context.Background(), "", []string{"legacy.go"})
	require.ErrorIs(t, err, ErrContractProjectionIncomplete)
	require.Equal(t, ContractFileProjection{}, p)
}
