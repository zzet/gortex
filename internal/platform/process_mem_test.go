package platform

import (
	"runtime"
	"testing"
)

// TestReadProcessMemoryReportsThisProcess: the peak resident set is known on
// every Unix system and at least what the Go runtime holds resident now; on
// macOS with cgo the physical footprint is too, and its lifetime peak is at
// least the current figure. Allocating and touching 64 MiB raises the peaks
// and puts at least that much in the current resident (and, on Linux,
// proportional) set.
func TestReadProcessMemoryReportsThisProcess(t *testing.T) {
	before := ReadProcessMemory()
	buf := make([]byte, 64<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = byte(i)
	}
	after := ReadProcessMemory()
	runtime.KeepAlive(buf)
	switch runtime.GOOS {
	case "windows", "plan9", "js", "wasip1":
		if after.PeakRSSBytes != 0 {
			t.Fatalf("peak RSS %d reported where no reader exists", after.PeakRSSBytes)
		}
		return
	}
	if after.PeakRSSBytes < 64<<20 {
		t.Fatalf("peak RSS %d is below the 64 MiB this test touched", after.PeakRSSBytes)
	}
	if after.PeakRSSBytes < before.PeakRSSBytes {
		t.Fatalf("peak RSS went down: %d -> %d", before.PeakRSSBytes, after.PeakRSSBytes)
	}
	if runtime.GOOS == "darwin" && cgoEnabled {
		if after.FootprintBytes < 64<<20 || after.PeakFootprintBytes < after.FootprintBytes {
			t.Fatalf("footprint %d, peak %d", after.FootprintBytes, after.PeakFootprintBytes)
		}
		if after.PeakFootprintBytes < before.PeakFootprintBytes {
			t.Fatalf("peak footprint went down: %d -> %d", before.PeakFootprintBytes, after.PeakFootprintBytes)
		}
	}
	if (runtime.GOOS == "linux" || runtime.GOOS == "darwin" && cgoEnabled) && after.RSSBytes < 64<<20 {
		t.Fatalf("resident set %d is below the 64 MiB this test touched", after.RSSBytes)
	}
	if runtime.GOOS == "linux" && (after.PSSBytes < 64<<20 || after.PSSBytes > after.RSSBytes) {
		t.Fatalf("proportional set %d, resident set %d", after.PSSBytes, after.RSSBytes)
	}
}

func TestParseSmapsRollup(t *testing.T) {
	var got ProcessMemory
	parseSmapsRollup([]byte(`55d4c0a00000-7ffd5e5fe000 ---p 00000000 00:00 0    [rollup]
Rss:             2097152 kB
Pss:             1048576 kB
Pss_Dirty:        524288 kB
Pss_Anon:         786432 kB
Pss_File:         262144 kB
Pss_Shmem:             0 kB
Anonymous:        786432 kB
`), &got)
	if want := (ProcessMemory{RSSBytes: 2 << 30, PSSBytes: 1 << 30, PSSFileBytes: 256 << 20}); got != want {
		t.Fatalf("parsed %+v, want %+v", got, want)
	}
	// A kernel that predates the Pss_File split still reports the totals.
	got = ProcessMemory{}
	parseSmapsRollup([]byte("Rss: 4 kB\nPss: 2 kB\nnot a counter\n"), &got)
	if want := (ProcessMemory{RSSBytes: 4096, PSSBytes: 2048}); got != want {
		t.Fatalf("parsed %+v from an older kernel's rollup, want %+v", got, want)
	}
}
