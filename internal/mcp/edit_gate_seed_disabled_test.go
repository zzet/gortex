//go:build !gortex_edit_gate_seed

package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEditGateProductionBuildCannotEnableACKSeed(t *testing.T) {
	t.Setenv("GORTEX_EDIT_GATE_SEED", "ack-wait")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, editGateACKSeed(ctx, nil), "production build has no timer, hook or environment activation")
}
