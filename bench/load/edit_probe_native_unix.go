//go:build linux || darwin

package main

import (
	"os"
	"syscall"
)

func editProbeInode(info os.FileInfo) *uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	inode := stat.Ino
	return &inode
}
