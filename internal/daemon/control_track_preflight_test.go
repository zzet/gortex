package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
)

type preflightOnlyController struct {
	fakeController
	preflightCalls int
	preflightErr   error
}

func (c *preflightOnlyController) PreflightTrack(ctx context.Context, _ TrackParams) error {
	c.preflightCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.preflightErr
}

func TestControlTrackPreflightNeverMutates(t *testing.T) {
	params, err := json.Marshal(TrackParams{Path: "candidate", Name: "src"})
	require.NoError(t, err)
	for _, collision := range []bool{false, true} {
		controller := &preflightOnlyController{}
		if collision {
			controller.preflightErr = &indexer.PrefixCollisionError{Prefix: "src", ExistingPath: "owner", Path: "candidate"}
		}
		server := New("", "test", zap.NewNop())
		server.Controller = controller
		response := server.handleControl(t.Context(), nil, ControlRequest{Kind: ControlTrackPreflight, Params: params})
		require.Equal(t, !collision, response.OK)
		if collision {
			require.Equal(t, ErrPrefixCollision, response.ErrorCode)
		}
		require.Equal(t, 1, controller.preflightCalls)
		require.Empty(t, controller.trackCalls)
	}
	// Old controllers cannot accidentally reinterpret preflight as a track.
	old := &fakeController{}
	server := New("", "test", zap.NewNop())
	server.Controller = old
	response := server.handleControl(t.Context(), nil, ControlRequest{Kind: ControlTrackPreflight, Params: params})
	require.False(t, response.OK)
	require.Empty(t, old.trackCalls)
}

func TestControlTrackPreflightCanceledNeverMutates(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	controller := &preflightOnlyController{}
	server := New("", "test", zap.NewNop())
	server.Controller = controller
	response := server.handleControl(ctx, nil, ControlRequest{Kind: ControlTrackPreflight})
	require.False(t, response.OK)
	require.Empty(t, controller.trackCalls)
}
