package platform

import (
	"runtime"
	"testing"
)

// TestReadProcessMemoryReportsThisProcess: the peak resident set is known on
// every Unix system and at least what the Go runtime holds resident now; on
// macOS with cgo the physical footprint is too, and its lifetime peak is at
// least the current figure. Allocating and touching 64 MiB raises the peaks.
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
}
