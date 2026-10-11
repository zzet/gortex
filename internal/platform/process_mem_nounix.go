//go:build !unix

package platform

func peakRSSBytes() uint64 { return 0 }
