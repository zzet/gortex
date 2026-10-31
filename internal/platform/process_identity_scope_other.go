//go:build !linux

package platform

func processBootScope() (string, error) { return "epoch", nil }
