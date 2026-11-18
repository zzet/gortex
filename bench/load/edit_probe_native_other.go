//go:build !linux && !darwin

package main

import "os"

// Other hosts report no inode; os.SameFile still decides replacement.
func editProbeInode(os.FileInfo) *uint64 { return nil }
