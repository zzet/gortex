//go:build acceptance && linux

package main

import (
	"os"
	"testing"
)

func TestReadbackCohortPeakRejectNonCgroupDirectory(t *testing.T) {
	// A real held directory on the ordinary test filesystem must not inherit the
	// cgroup mount's filesystem proof. No cgroup is opened, created or changed.
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := dir.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := cohortUnitIdentity(dir); err == nil {
		t.Fatal("ordinary held directory accepted as cgroup v2")
	}
}
