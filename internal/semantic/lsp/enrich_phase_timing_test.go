package lsp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
)

// The completion log must break the enrich wall time out per pass. Speed
// forensics on a production C# monorepo hit a wall without this: 241k
// requests took the same 26 minutes at maxParallel 6 and 12, so the pole was
// NOT slot starvation — but the single aggregate duration cannot say which
// pass owns the minutes. Every phase boundary already exists in
// EnrichRepoContext; this pins that their wall times ride the log the
// forensics scripts scrape.
func TestLSP_Enrich_CompletionLogCarriesPhaseTimings(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot := t.TempDir()
	g := graph.New()
	server := newFakeLSPServer()
	defConfirmFixture(t, repoRoot, g, server)

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	core, logs := observer.New(zap.InfoLevel)
	p.logger = zap.New(core)

	require.NoError(t, runEnrich(t, p, g, repoRoot, 5*time.Second))

	entries := logs.FilterMessage("LSP enrich: hover phase complete").All()
	require.Len(t, entries, 1, "completion log must be emitted exactly once")
	fields := entries[0].ContextMap()
	for _, k := range []string{
		"phase_setup_ms", "phase_impls_ms", "phase_confirm_ms",
		"phase_definitions_ms", "phase_refs_add_ms", "phase_sweep_ms",
	} {
		v, ok := fields[k]
		require.Truef(t, ok, "completion log must carry %s", k)
		ms, isInt := v.(int64)
		require.Truef(t, isInt, "%s must be an int64 millisecond count, got %T", k, v)
		assert.GreaterOrEqual(t, ms, int64(0), k)
	}
}
