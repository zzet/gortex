//go:build unix || windows

package platform

import "testing"

func TestProcessCPUTimeCurrentProcessMonotonic(t *testing.T) {
	before, err := ProcessCPUTime()
	if err != nil {
		t.Fatalf("first current-process sample: %v", err)
	}
	after, err := ProcessCPUTime()
	if err != nil {
		t.Fatalf("second current-process sample: %v", err)
	}
	if before < 0 || after < before {
		t.Fatalf("invalid CPU progression: %s -> %s", before, after)
	}
}
