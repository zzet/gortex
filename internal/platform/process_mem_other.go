//go:build !linux && !(darwin && cgo)

package platform

func readCurrentMemory(*ProcessMemory) {}
