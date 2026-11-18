//go:build !unix

package resolver

func processMajorFaults() int64 { return 0 }
