//go:build windows

package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/zzet/gortex/internal/daemon"
	"golang.org/x/sys/windows"
)

var daemonLogGetStdHandle = windows.GetStdHandle
var daemonLogSetStdHandle = windows.SetStdHandle

// Distinct os.File wrappers can own the same inherited HANDLE. Keep the unused
// wrappers alive so their finalizers cannot close a later reuse of that HANDLE.
var daemonLogRetainedFiles []*os.File

func openDaemonExitLogFile(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// The retained diagnostic handle must not block normal log rotation.
	// Match Go's O_APPEND access (all generic-write rights except WRITE_DATA),
	// adding FILE_SHARE_DELETE so a rotated inode remains usable until exit.
	access := uint32(windows.FILE_GENERIC_WRITE &^ windows.FILE_WRITE_DATA)
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	h, err := windows.CreateFile(name, access, share, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

func prepareDaemonLogOutput(path string, stderr, stdout **os.File) error {
	if !daemonServiceLog {
		return nil
	}
	crashPath := daemon.StderrLogPath(path)
	if err := daemon.EnsureParentDir(crashPath); err != nil {
		return fmt.Errorf("create daemon log directory: %w", err)
	}
	output, err := openDaemonStderrLog(path, crashPath)
	if err != nil {
		return err
	}
	defer output.Close()
	return replaceDaemonLogFDPair(stderr, stdout, output)
}

func replaceDaemonLogFDPair(stderr, stdout **os.File, source *os.File) error {
	if err := replaceDaemonLogFD(stderr, source, stdout); err != nil {
		return fmt.Errorf("redirect stderr/stdout: %w", err)
	}
	return nil
}

func replaceDaemonLogFD(target **os.File, source *os.File, others ...**os.File) error {
	var handle windows.Handle
	process := windows.CurrentProcess()
	if err := windows.DuplicateHandle(process, windows.Handle(source.Fd()), process, &handle, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return fmt.Errorf("duplicate stderr log handle: %w", err)
	}
	replacement := os.NewFile(uintptr(handle), source.Name())
	targets := append([]**os.File{target}, others...)
	originals := make([]*os.File, 0, len(targets))
	type standardHandle struct {
		kind   uint32
		handle windows.Handle
	}
	var stdHandles []standardHandle
	for _, slot := range targets {
		originals = append(originals, *slot)
		var kind uint32
		switch slot {
		case &os.Stderr:
			kind = windows.STD_ERROR_HANDLE
		case &os.Stdout:
			kind = windows.STD_OUTPUT_HANDLE
		default:
			continue
		}
		// Preserve InvalidHandle even on error so rollback restores an absent handle.
		old, err := daemonLogGetStdHandle(kind)
		if err != nil && old != windows.InvalidHandle {
			_ = replacement.Close()
			return fmt.Errorf("get standard log handle: %w", err)
		}
		stdHandles = append(stdHandles, standardHandle{kind, old})
	}
	for i, std := range stdHandles {
		if err := daemonLogSetStdHandle(std.kind, handle); err != nil {
			redirectErr := fmt.Errorf("set standard log handle: %w", err)
			restored := true
			for _, original := range stdHandles[:i] {
				if err := daemonLogSetStdHandle(original.kind, original.handle); err != nil {
					redirectErr = errors.Join(redirectErr, fmt.Errorf("restore standard log handle: %w", err))
					restored = false
				}
			}
			if restored {
				_ = replacement.Close()
			} else {
				// A native standard handle may still refer to the replacement.
				daemonLogRetainedFiles = append(daemonLogRetainedFiles, replacement)
			}
			return redirectErr
		}
	}
	for _, slot := range targets {
		*slot = replacement
		if slot == &os.Stderr {
			log.SetOutput(os.Stderr)
		}
	}
	// Collect HANDLEs before closing any wrapper: Close invalidates its Fd().
	byHandle := make(map[uintptr]*os.File, len(originals))
	var toClose []*os.File
	for _, original := range originals {
		if original == nil {
			continue
		}
		fd := original.Fd()
		if first, exists := byHandle[fd]; exists {
			if first != original {
				daemonLogRetainedFiles = append(daemonLogRetainedFiles, original)
			}
			continue
		}
		byHandle[fd] = original
		toClose = append(toClose, original)
	}
	var closeErr error
	for _, original := range toClose {
		err := original.Close()
		if err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			closeErr = errors.Join(closeErr, err)
		}
	}
	return closeErr
}
