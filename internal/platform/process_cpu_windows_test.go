//go:build windows

package platform

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestProcessCPUFiletimesAreDurations(t *testing.T) {
	cpu, err := processCPUFiletimes(windows.Filetime{LowDateTime: 15_000_000}, windows.Filetime{LowDateTime: 25_000_000})
	if err != nil || cpu != 4*time.Second {
		t.Fatalf("CPU durations: got %s, %v; want 4s", cpu, err)
	}
	cpu, err = processCPUFiletimes(windows.Filetime{}, windows.Filetime{})
	if err != nil || cpu != 0 {
		t.Fatalf("known zero CPU: got %s, %v", cpu, err)
	}
	_, err = processCPUFiletimes(windows.Filetime{HighDateTime: ^uint32(0)}, windows.Filetime{})
	if !errors.Is(err, errInvalidProcessCPUTime) {
		t.Fatalf("overflow must be unavailable: %v", err)
	}
}
