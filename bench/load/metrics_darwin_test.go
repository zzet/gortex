//go:build darwin && cgo

package main

import (
	"math"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestDarwinCPUTimebaseAgainstGetrusage(t *testing.T) {
	sampler := newProcessSampler()
	before := sampler.sample(os.Getpid())["cpu_seconds"]
	var first, last syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &first); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(150 * time.Millisecond)
	var sink uint64
	for time.Now().Before(until) {
		for i := range 1000 {
			sink += uint64(i)
		}
	}
	runtime.KeepAlive(sink)
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &last); err != nil {
		t.Fatal(err)
	}
	after := sampler.sample(os.Getpid())["cpu_seconds"]
	seconds := func(value syscall.Rusage) float64 {
		return float64(value.Utime.Sec+value.Stime.Sec) + float64(value.Utime.Usec+value.Stime.Usec)/1e6
	}
	want := seconds(last) - seconds(first)
	delta := counterDelta(before, after)
	if delta.Value == nil || want <= 0 || math.Abs(*delta.Value-want) > math.Max(.02, want*.15) {
		t.Fatalf("Mach CPU vs getrusage: %+v want%gs", delta, want)
	}
}
