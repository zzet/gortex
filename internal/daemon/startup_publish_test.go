package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestStartupMetadataControlDoesNotRequireController(t *testing.T) {
	s := New("unused", "test", zap.NewNop())
	s.EnableStartupMetadata()
	reply := s.handleControl(context.Background(), nil, ControlRequest{Kind: ControlSearchSymbols})
	require.Equal(t, ErrRepoLoading, reply.ErrorCode)
	require.True(t, reply.Retryable)
	require.True(t, s.handleControl(context.Background(), nil, ControlRequest{Kind: ControlShutdown}).OK)
	require.NoError(t, s.Shutdown())
}

func TestStartupRuntimePublicationIsFencedBeforeOwnerJoin(t *testing.T) {
	s := New("unused", "test", zap.NewNop())
	s.EnableStartupMetadata()
	entered, release := make(chan struct{}), make(chan struct{})
	s.CancelShutdownOwners = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() { done <- s.Shutdown() }()
	<-entered
	require.Error(t, s.PublishStartupRuntime(nil, nil, nil, nil, nil, ""), "close admission precedes a potentially slow owner join")
	close(release)
	require.NoError(t, <-done)
	require.True(t, s.StartupClosed())
}
