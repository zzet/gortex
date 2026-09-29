package indexer

import (
	"runtime/metrics"

	"go.uber.org/zap/zapcore"

	"github.com/zzet/gortex/internal/platform"
)

// processMemorySample is the process's memory at one instant: the Go
// runtime's own accounting (runtime/metrics, no stop-the-world) and the
// operating system's (peak resident set, and the physical footprint on
// macOS). A build's memory-envelope release logs the sample taken at its
// admission against one taken at its release, so the build that raised the
// process's peak is attributable from the log alone.
type processMemorySample struct {
	goTotal      uint64
	heapObjects  uint64
	heapReleased uint64
	os           platform.ProcessMemory
}

var processMemoryMetrics = []string{
	"/memory/classes/total:bytes",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/released:bytes",
}

func sampleProcessMemory() processMemorySample {
	samples := make([]metrics.Sample, len(processMemoryMetrics))
	for i, name := range processMemoryMetrics {
		samples[i].Name = name
	}
	metrics.Read(samples)
	value := func(i int) uint64 {
		if samples[i].Value.Kind() != metrics.KindUint64 {
			return 0
		}
		return samples[i].Value.Uint64()
	}
	return processMemorySample{
		goTotal:      value(0),
		heapObjects:  value(1),
		heapReleased: value(2),
		os:           platform.ReadProcessMemory(),
	}
}

// processMemorySpan is one build's memory window: from its admission sample
// to its release sample.
type processMemorySpan struct {
	before, after processMemorySample
}

func (s processMemorySample) until(after processMemorySample) processMemorySpan {
	return processMemorySpan{before: s, after: after}
}

func raisedBy(before, after uint64) uint64 {
	if after > before {
		return after - before
	}
	return 0
}

// MarshalLogObject writes the release-time figures and how much this build
// raised each lifetime peak (0 when an earlier build holds the peak).
func (s processMemorySpan) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddUint64("go_total_bytes", s.after.goTotal)
	enc.AddUint64("heap_objects_bytes", s.after.heapObjects)
	enc.AddUint64("heap_released_bytes", s.after.heapReleased)
	enc.AddUint64("peak_rss_bytes", s.after.os.PeakRSSBytes)
	enc.AddUint64("peak_rss_raised_bytes", raisedBy(s.before.os.PeakRSSBytes, s.after.os.PeakRSSBytes))
	if s.after.os.PeakFootprintBytes != 0 {
		enc.AddUint64("footprint_bytes", s.after.os.FootprintBytes)
		enc.AddUint64("peak_footprint_bytes", s.after.os.PeakFootprintBytes)
		enc.AddUint64("peak_footprint_raised_bytes",
			raisedBy(s.before.os.PeakFootprintBytes, s.after.os.PeakFootprintBytes))
	}
	return nil
}
