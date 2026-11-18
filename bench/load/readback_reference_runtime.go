package main

import (
	"errors"
	"os"
	"path/filepath"
)

// Capacity and volume identity are stable admission facts, unlike free bytes.
// Probe the nearest existing directory without allocating the fresh run path.
type readbackReferenceHostSnapshot struct {
	Host           Host   `json:"host"`
	ProbePath      string `json:"volume_probe_path"`
	VolumeIdentity string `json:"volume_identity"`
	Error          string `json:"error,omitempty"`
}

func inspectReadbackReferenceHost(profile, scratch string) (readbackReferenceHostSnapshot, error) {
	out := readbackReferenceHostSnapshot{}
	probe, err := filepath.Abs(scratch)
	if err == nil {
		for {
			info, statErr := os.Lstat(probe)
			if statErr == nil {
				if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					err = errors.New("volume probe must be an existing directory, not a symlink or file")
				}
				break
			}
			if !errors.Is(statErr, os.ErrNotExist) || filepath.Dir(probe) == probe {
				err = statErr
				break
			}
			probe = filepath.Dir(probe)
		}
	}
	out.ProbePath = probe
	if err == nil {
		out.VolumeIdentity, err = readbackReferenceVolumeIdentity(probe)
	}
	if err == nil {
		out.Host, _, err = inspectHost(profile, probe)
	}
	if err != nil {
		out.Error = err.Error()
	}
	return out, err
}

func readbackReferencePhaseOffered(schedule readbackReferenceSchedule, role string) int {
	if role == "protocol_preflight" {
		return 1 // Still expected if that diagnostic offer is censored.
	}
	return schedule.Cell.Offers // Never shrink acceptance for absent arrivals.
}

func readbackReferenceObservedOffers(rows []readbackReferenceRecord) int {
	observed := 0
	for _, row := range rows {
		if !row.ProbeOrigin.IsZero() {
			observed++
		}
	}
	return observed
}
