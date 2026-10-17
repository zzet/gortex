package graph

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
)

type checkedContractWrapper struct {
	*Graph
	failure error
}

func (w checkedContractWrapper) Unwrap() Reader { return w.Graph }
func (w checkedContractWrapper) LayerContractRepoProjectionContext(context.Context, string) (ContractFileProjection, error) {
	return ContractFileProjection{}, w.failure
}
func (w checkedContractWrapper) LayerContractFileProjectionContext(context.Context, string, []string) (ContractFileProjection, error) {
	return ContractFileProjection{}, w.failure
}
func (w checkedContractWrapper) LayerContractIDProjectionContext(context.Context, []string) (ContractFileProjection, error) {
	return ContractFileProjection{}, w.failure
}
func TestCheckedContractProjectionDoesNotUnwrapCheckedRevisionFence(t *testing.T) {
	sentinel := errors.New("selected layer revision rejected")
	reader := checkedContractWrapper{Graph: New(), failure: sentinel}
	p, err := CompleteContractRepoProjection(context.Background(), reader, "repo")
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, ContractFileProjection{}, p)
	p, err = CompleteContractFileProjection(context.Background(), reader, "repo", []string{"repo/file.go"})
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, ContractFileProjection{}, p)
	p, err = CompleteContractIDProjection(context.Background(), reader, []string{"contract"})
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, ContractFileProjection{}, p)
}

type malformedContractRepoReader struct{ *Graph }

func (r malformedContractRepoReader) LayerContractRepoProjectionContext(context.Context, string) (ContractFileProjection, error) {
	return ContractFileProjection{OwnerRows: []RepoEdgeRow{{}}}, nil
}
func TestCheckedContractRepoProjectionRejectsMalformedOwnership(t *testing.T) {
	base := malformedContractRepoReader{New()}
	p, err := NewOverlaidView(base, NewOverlayLayer()).LoadContractRepoProjectionContext(context.Background(), "repo")
	require.ErrorIs(t, err, ErrContractProjectionIncomplete)
	require.Equal(t, ContractFileProjection{}, p)
}
