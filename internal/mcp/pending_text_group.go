package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Only the serial pending-text scanner borrows this bounded ancestor stack.
// It retains descriptors and immutable identities, never a validity decision.
// Every file still checks each named component before/after its two full reads.
// Every result stays provisional until the complete group's close fence.
type pendingTextDirectory struct {
	name   string
	path   string
	handle *os.Root
	info   os.FileInfo
}

func (r *pendingTextRoot) refuseGroup(err error) error {
	if r.groupErr == nil {
		r.groupErr = err
	}
	return r.groupErr
}

// Pop deepest first while each parent remains open. Validate even after an
// earlier error/cancellation and close every descriptor before reporting it.
func (r *pendingTextRoot) closeDirectories(keep int) error {
	var errs []error
	for len(r.directories) > keep {
		i := len(r.directories) - 1
		entry := r.directories[i]
		parent := r.handle
		if i > 0 {
			parent = r.directories[i-1].handle
		}
		current, err := r.lstat(parent, entry.name)
		if err == nil && (current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(entry.info, current)) {
			err = errPhysicalFileMoved
		}
		if err != nil {
			errs = append(errs, r.refuseGroup(errPhysicalFileMoved))
		}
		if err := entry.handle.Close(); err != nil {
			errs = append(errs, r.refuseGroup(err))
		}
		r.directories[i] = pendingTextDirectory{}
		r.directories = r.directories[:i]
	}
	return errors.Join(errs...)
}

func (r *pendingTextRoot) walkGrouped(ctx context.Context, relative string, expected []pendingTextPathStamp) ([]pendingTextPathStamp, *os.Root, bool, error) {
	if r.groupErr != nil {
		return nil, nil, false, r.groupErr
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if expected != nil && len(expected) != len(parts) {
		return nil, nil, false, r.refuseGroup(errPhysicalFileMoved)
	}
	common := 0
	for common < len(parts)-1 && common < len(r.directories) && parts[common] == r.directories[common].name {
		common++
	}
	if err := r.closeDirectories(common); err != nil {
		return nil, nil, false, err
	}
	dir := r.handle
	path := ""
	var stamps []pendingTextPathStamp
	for i, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, r.refuseGroup(err)
		}
		path = filepath.Join(path, part)
		info, err := r.lstat(dir, part)
		if err != nil {
			if i < len(r.directories) {
				return nil, nil, false, r.refuseGroup(errPhysicalFileMoved)
			}
			return nil, nil, false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if expected != nil || i < len(r.directories) {
				return nil, nil, false, r.refuseGroup(errPhysicalFileMoved)
			}
			return nil, nil, true, nil // An initially stable alias keeps the legacy proof.
		}
		if !os.SameFile(info, info) {
			return nil, nil, false, r.refuseGroup(errPhysicalFileMoved)
		}
		if expected != nil && (expected[i].path != path || !os.SameFile(expected[i].info, info)) {
			return nil, nil, false, r.refuseGroup(errPhysicalFileMoved)
		}
		stamps = append(stamps, pendingTextPathStamp{path: path, info: info})
		if i == len(parts)-1 {
			return stamps, dir, false, nil
		}
		if !info.IsDir() {
			return nil, nil, false, r.refuseGroup(errPhysicalFileMoved)
		}
		if i < len(r.directories) {
			borrowed := r.directories[i]
			if borrowed.path != path || !os.SameFile(borrowed.info, info) {
				return nil, nil, false, r.refuseGroup(errPhysicalFileMoved)
			}
			dir = borrowed.handle
			continue
		}
		next, err := r.openRoot(dir, part)
		if err != nil {
			if next != nil {
				_ = next.Close()
			}
			return nil, nil, false, r.refuseGroup(err)
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = next.Close()
			if err == nil {
				err = errPhysicalFileMoved
			}
			return nil, nil, false, r.refuseGroup(err)
		}
		r.directories = append(r.directories, pendingTextDirectory{name: part, path: path, handle: next, info: info})
		dir = next
	}
	return nil, nil, false, r.refuseGroup(errPhysicalFileMoved)
}
