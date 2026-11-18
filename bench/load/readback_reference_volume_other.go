//go:build !linux && !darwin

package main

import "errors"

func readbackReferenceVolumeIdentity(string) (string, error) {
	return "", errors.New("owning-volume identity is unavailable on this platform")
}
