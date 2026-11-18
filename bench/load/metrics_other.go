//go:build (!linux && !darwin) || (darwin && !cgo)

package main

type unsupportedProcessSampler struct{}

func newProcessSampler() processSampler { return unsupportedProcessSampler{} }

func (unsupportedProcessSampler) sample(int) map[string]Metric {
	return absentProcessMetrics("process collector unavailable for this OS/build; not a reference run")
}
