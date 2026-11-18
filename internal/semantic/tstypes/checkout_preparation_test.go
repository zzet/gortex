package tstypes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/semantic"
)

func TestCheckoutPreparationOnlyApprovesSuppressedFallback(t *testing.T) {
	spec := GoSpec()
	spec.Suppressed = func() bool { return true }
	p := NewProvider(spec, zap.NewNop())
	require.True(t, p.ConcurrentCheckoutPreparation(context.Background(), "", "", semantic.CheckoutCompilerScope{}, nil))
	spec.Suppressed = func() bool { return false }
	require.False(t, p.ConcurrentCheckoutPreparation(context.Background(), "", "", semantic.CheckoutCompilerScope{}, nil))
	spec.Suppressed = nil
	require.False(t, p.ConcurrentCheckoutPreparation(context.Background(), "", "", semantic.CheckoutCompilerScope{}, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	spec.Suppressed = func() bool { return true }
	require.False(t, p.ConcurrentCheckoutPreparation(ctx, "", "", semantic.CheckoutCompilerScope{}, nil))
}
