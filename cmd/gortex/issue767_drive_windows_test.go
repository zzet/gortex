//go:build acceptance && windows

package main

import "golang.org/x/sys/windows"

func issue767FixedDrive(root string) bool {
	path, err := windows.UTF16PtrFromString(root)
	return err == nil && windows.GetDriveType(path) == windows.DRIVE_FIXED
}
