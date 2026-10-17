//go:build windows

package gitstate

import (
	"fmt"
	"io/fs"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsFileIDInfo matches FILE_ID_INFO. Its full 128-bit identifier is
// required on ReFS, where BY_HANDLE_FILE_INFORMATION's index is not unique.
type windowsFileIDInfo struct {
	volumeSerial uint64
	fileID       [16]byte
}

// pathIdentity reads the volume and file object from a handle opened without
// following the leaf reparse point, matching SamplePathEvidence's Lstat.
func pathIdentity(path string, _ fs.FileInfo) (volumeKind, volumeToken, identity string) {
	name, err := windows.UTF16PtrFromString(windowsIdentityPath(path))
	if err != nil {
		return VolumeKindUnsupported, "", ""
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return VolumeKindUnsupported, "", ""
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var info windowsFileIDInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return VolumeKindUnsupported, "", ""
	}
	if info.fileID == [16]byte{} {
		return VolumeKindUnsupported, "", ""
	}
	volumeToken = fmt.Sprintf("%d", info.volumeSerial)
	identity = fmt.Sprintf("%s:%s:%x", VolumeKindWindowsFileID, volumeToken, info.fileID)
	return VolumeKindWindowsFileID, volumeToken, identity
}

// SamplePathEvidence supplies a cleaned absolute path. Unlike os.Lstat,
// CreateFile does not add an extended prefix when long paths need one.
func windowsIdentityPath(path string) string {
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) ||
		strings.HasPrefix(path, `\??\`) || strings.HasPrefix(path, `\\.\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}
