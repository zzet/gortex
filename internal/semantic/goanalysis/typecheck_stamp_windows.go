//go:build windows

package goanalysis

import "golang.org/x/sys/windows"

func statStamp(path string) (fileStamp, bool) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fileStamp{}, false
	}
	// Read identity and metadata from one handle. Sharing deletion allows
	// editors to replace the file while a compiler pass checks its stamp.
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return fileStamp{}, false
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return fileStamp{}, false
	}
	return fileStamp{
		size:   int64(uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)),
		mtime:  info.LastWriteTime.Nanoseconds(),
		ino:    uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		volume: info.VolumeSerialNumber,
	}, true
}
