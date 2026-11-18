//go:build windows

package source

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const regularFilesystemReadSupported = true

func openRootRegularFile(root *os.Root, name string) (*os.File, error) {
	// Root confines every parent component. A permitted parent link may resolve
	// inside the root, as it does for other FilesystemSource reads. This
	// synchronous directory open can await a Windows directory oplock; the
	// caller's context cannot interrupt the OS open.
	parent, err := root.OpenFile(filepath.Dir(name), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: parent is not a directory: %w", name, ErrNotRegularFile)
	}

	// Open only the final component relative to the confined parent handle.
	// FILE_OPEN_REPARSE_POINT prevents a final link from being followed, and
	// the handle attribute check below rejects every kind of reparse point.
	objectName, err := windows.NewNTUnicodeString(filepath.Base(name))
	if err != nil {
		return nil, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_READ|windows.SYNCHRONIZE,
		&attrs, &windows.IO_STATUS_BLOCK{}, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, windows.FILE_SYNCHRONOUS_IO_NONALERT|
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|
			windows.FILE_COMPLETE_IF_OPLOCKED, 0, 0)
	if err != nil {
		if handle != 0 && handle != windows.Handle(syscall.InvalidHandle) {
			_ = windows.CloseHandle(handle)
		}
		if status, ok := err.(windows.NTStatus); ok {
			// A successful create can report an oplock break still in progress.
			// Never read that handle while the previous owner has not acked it.
			if status == windows.STATUS_OPLOCK_BREAK_IN_PROGRESS {
				return nil, fmt.Errorf("%s: oplock break in progress: %w", name, ErrRegularReadUnsupported)
			}
			err = status.Errno()
		}
		return nil, &os.PathError{Op: "NtCreateFile", Path: name, Err: err}
	}

	var fileInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &fileInfo); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, &os.PathError{Op: "GetFileInformationByHandle", Path: name, Err: err}
	}
	if fileInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("%s: reparse point: %w", name, ErrNotRegularFile)
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("%s: could not own opened descriptor", name)
	}
	return file, nil
}
