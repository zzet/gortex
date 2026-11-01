package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

type identityPendingController struct{ fakeController }

func (*identityPendingController) Track(context.Context, TrackParams) (json.RawMessage, error) {
	return nil, indexer.ErrRepoIdentityPending
}

func TestControlTrackIdentityPendingIsRetryable(t *testing.T) {
	s := &Server{Controller: &identityPendingController{}}
	params, err := json.Marshal(TrackParams{Path: "unresolved-root"})
	require.NoError(t, err)
	response := s.handleControl(t.Context(), nil, ControlRequest{Kind: ControlTrack, Params: params})
	require.False(t, response.OK)
	require.Equal(t, ErrRepoLoading, response.ErrorCode)
	require.True(t, response.Retryable)
}
