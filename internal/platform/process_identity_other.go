//go:build !darwin && !linux && !windows

package platform

import "errors"

func processBirth(_ int) ([2]uint64, error) {
	return [2]uint64{}, errors.New("native process birth identity unavailable on this platform")
}

func processBirthAndRunning(_ int) ([2]uint64, bool, error) {
	return [2]uint64{}, false, errors.New("native process execution state unavailable on this platform")
}
