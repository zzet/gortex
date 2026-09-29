package indexer

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

// TestProcessMemorySpanAttributesAPeakToTheBuild: the memory-envelope release
// line carries the release-time figures and how much the build raised the
// process's lifetime peaks; a build that touched fresh memory beyond the old
// peak reports a raise, one that did not reports none.
func TestProcessMemorySpanAttributesAPeakToTheBuild(t *testing.T) {
	encode := func(span processMemorySpan) map[string]any {
		enc := zapcore.NewMapObjectEncoder()
		require.NoError(t, span.MarshalLogObject(enc))
		return enc.Fields
	}

	quiet := sampleProcessMemory()
	fields := encode(quiet.until(sampleProcessMemory()))
	require.Positive(t, fields["go_total_bytes"])
	require.Contains(t, fields, "heap_objects_bytes")
	require.Contains(t, fields, "heap_released_bytes")
	if runtime.GOOS == "windows" {
		return
	}
	require.Positive(t, fields["peak_rss_bytes"])

	before := sampleProcessMemory()
	grow := int(before.os.PeakRSSBytes) + 96<<20
	buf := make([]byte, grow)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	fields = encode(before.until(sampleProcessMemory()))
	runtime.KeepAlive(buf)
	require.Positive(t, fields["peak_rss_raised_bytes"], "a build that set a new peak must say so: %v", fields)
	if runtime.GOOS == "darwin" && fields["peak_footprint_bytes"] != nil {
		require.Positive(t, fields["peak_footprint_raised_bytes"], "fields %v", fields)
	}

	raisedBefore := processMemorySample{os: before.os}
	raisedBefore.os.PeakRSSBytes = ^uint64(0) >> 1
	raisedBefore.os.PeakFootprintBytes = ^uint64(0) >> 1
	fields = encode(raisedBefore.until(sampleProcessMemory()))
	require.EqualValues(t, 0, fields["peak_rss_raised_bytes"], "an earlier build's peak is not this build's raise")
}
