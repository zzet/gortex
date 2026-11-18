//go:build performance

package indexer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This is a literal idle wall-clock gate, kept separate from deterministic
// timer-argument controls and run alone on the serial measurement host.
func TestToolAdmissionStartsPatchWithinTenMilliseconds(t *testing.T) {
	dir, w := toolAdmissionFixture(t)
	path := filepath.Join(dir, "main.go")
	entered := make(chan time.Time, 1)
	w.pointMutationPatch = func(string, ChangeKind, uint64) error { entered <- time.Now(); return nil }
	const samples = 15
	var worst time.Duration
	for range samples {
		started := time.Now()
		ticket, err := w.EnqueueToolMutation(t.Context(), path)
		require.NoError(t, err)
		var actual time.Time
		select {
		case actual = <-entered:
		case <-time.After(time.Second):
			t.Fatal("tool patch did not start")
		}
		delay := actual.Sub(started)
		worst = max(worst, delay)
		require.LessOrEqual(t, delay, 10*time.Millisecond, "literal admission-to-patch-start bound")
		require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	}
	t.Logf("idle tool admission to stub patch: n=%d worst=%s bound=10ms", samples, worst)
}
