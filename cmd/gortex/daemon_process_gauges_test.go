package main

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The daemon's footprint is mostly memory the Go runtime cannot see: SQLite's
// page caches and parser allocations live outside the Go heap, and mapped
// store files are file pages. Both on-demand surfaces — the daemon_health
// snapshot and `daemon status` — must carry the operating system's view and
// SQLite's page-cache figure beside the Go runtime's, and health must say how
// many checkout build loops are running.
func TestDaemonReportsMemoryOutsideTheGoHeap(t *testing.T) {
	c := probeController(t, "repos:\n  - path: /work/alpha\n")
	health := buildDaemonHealthSnapshot(time.Now(), c, nil, nil)
	st, err := c.Status(context.Background())
	require.NoError(t, err)

	pageCache, ok := health["sqlite_page_cache_bytes"].(uint64)
	require.True(t, ok, "health has no SQLite page-cache gauge: %#v", health["sqlite_page_cache_bytes"])
	peak, ok := health["sqlite_page_cache_peak_bytes"].(uint64)
	require.True(t, ok, "health has no SQLite page-cache peak: %#v", health["sqlite_page_cache_peak_bytes"])
	assert.GreaterOrEqual(t, peak, pageCache)
	assert.GreaterOrEqual(t, st.Runtime.SQLitePageCachePeak, st.Runtime.SQLitePageCache)
	coordinators, ok := health["coordinators"].(int)
	require.True(t, ok, "health has no coordinator count: %#v", health["coordinators"])
	assert.Zero(t, coordinators, "a controller without a checkout lifecycle runs no build loops")

	switch runtime.GOOS {
	case "linux":
		for _, key := range []string{"rss_bytes", "rss_peak_bytes", "pss_bytes"} {
			value, _ := health[key].(uint64)
			assert.Positive(t, value, "health %s", key)
		}
		assert.Positive(t, st.Runtime.RSS)
		assert.Positive(t, st.Runtime.PSS)
	case "darwin":
		for _, key := range []string{"rss_bytes", "rss_peak_bytes", "footprint_bytes", "footprint_peak_bytes"} {
			value, _ := health[key].(uint64)
			assert.Positive(t, value, "health %s", key)
		}
		assert.Positive(t, st.Runtime.RSS)
		assert.Positive(t, st.Runtime.Footprint)
		assert.GreaterOrEqual(t, st.Runtime.FootprintPeak, st.Runtime.Footprint)
	}
}

// The runtime block of a status answer is one moment of the process. Go's
// figures come from ReadMemStats before the mutex; the operating system's and
// SQLite's are read beside them rather than after the view census and the
// mutex-guarded aggregate, which can wait seconds behind a track. Goroutines
// started while the aggregate is assembled must therefore not be counted.
func TestStatusSamplesTheRuntimeBlockBeforeTheAggregate(t *testing.T) {
	c := probeController(t, "repos:\n  - path: /work/alpha\n")
	release := make(chan struct{})
	defer close(release)
	const late = 1000
	c.toolSurface = func() (string, string, int) { // runs inside the aggregate, under c.mu
		for range late {
			go func() { <-release }()
		}
		return "core", "defer", 0
	}

	before := runtime.NumGoroutine()
	st, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "core", st.ToolPreset, "the aggregate was not assembled")
	assert.Less(t, st.Runtime.NumGoroutine, before+late/2,
		"the runtime block was sampled after the aggregate, not beside ReadMemStats")
}
