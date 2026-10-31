//go:build !linux && !darwin

package main

func volumeCapacity(string) Metric {
	return unavailable("bytes", "filesystem capacity collector unavailable")
}
