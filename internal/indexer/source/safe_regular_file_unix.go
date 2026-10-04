//go:build unix

package source

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const regularFilesystemReadSupported = true

func openRootRegularFile(root *os.Root, name string) (*os.File, error) {
	// os.Root resolves symlinks itself, including a final symlink even when
	// O_NOFOLLOW is requested. Let it confine only the parent, then enforce
	// the final-component rule in one kernel openat against that descriptor.
	parent, err := root.OpenFile(filepath.Dir(name), os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
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
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(name), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("%s: %w", name, errors.Join(ErrNotRegularFile, err))
	}
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%s: could not own opened descriptor", name)
	}
	return file, nil
}
