package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/zzet/gortex/internal/platform"
)

// An already-closed descriptor is completed, but a joined real close error must
// not disappear merely because another branch also wraps net.ErrClosed.
func runtimeAlreadyClosed(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		parts := joined.Unwrap()
		if len(parts) == 0 {
			return false
		}
		for _, part := range parts {
			if !runtimeAlreadyClosed(part) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return runtimeAlreadyClosed(wrapped.Unwrap())
	}
	return errors.Is(err, net.ErrClosed)
}

// ownedRuntimeFile is one startup-published file, not an arbitrary removal
// rule. Capture its native ID and immutable record bytes before cleanup.
type ownedRuntimeFile struct {
	path    string
	info    os.FileInfo
	content []byte
	record  bool
}

func captureOwnedRuntimeFile(path string, record bool) (owned ownedRuntimeFile, err error) {
	owned.path, owned.record = path, record
	if path == "" {
		return owned, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return owned, nil
	}
	if err != nil {
		return owned, err
	}
	if info.Mode()&os.ModeSymlink != 0 || record && !info.Mode().IsRegular() || !record && !info.Mode().IsRegular() && info.Mode()&os.ModeSocket == 0 {
		return owned, fmt.Errorf("unexpected owned runtime file type: %s", path)
	}
	if !os.SameFile(info, info) {
		return owned, fmt.Errorf("cannot capture runtime file identity: %s", path)
	}
	owned.info = info
	if record {
		file, err := openExistingRuntimeRecord(path)
		if err != nil {
			return owned, err
		}
		defer func() { err = errors.Join(err, file.Close()) }()
		opened, err := file.Stat()
		if err != nil {
			return owned, err
		}
		if !os.SameFile(info, opened) {
			return owned, fmt.Errorf("runtime record changed during capture: %s", path)
		}
		owned.info = opened
		const maxRuntimeBytes = 64 << 10
		owned.content, err = io.ReadAll(io.LimitReader(file, maxRuntimeBytes+1))
		if err != nil {
			return owned, err
		}
		if len(owned.content) > maxRuntimeBytes {
			return owned, fmt.Errorf("runtime record exceeds %d bytes: %s", maxRuntimeBytes, path)
		}
	}
	return owned, nil
}

func (f ownedRuntimeFile) remove() error {
	if f.path == "" {
		return nil
	}
	current, err := captureOwnedRuntimeFile(f.path, f.record)
	if err != nil {
		return err
	}
	if current.info == nil {
		return nil
	}
	if f.info == nil || !os.SameFile(f.info, current.info) || !bytes.Equal(f.content, current.content) {
		return fmt.Errorf("refusing to remove changed runtime file: %s", f.path)
	}
	return os.Remove(f.path)
}

func removeStaleDaemonEndpoint(path string) error {
	file, err := captureOwnedRuntimeFile(path, false)
	if err != nil {
		return err
	}
	return file.remove()
}

// Listen has not published a serving runtime yet. Close every acquired
// descriptor and remove only its captured startup files; preserve the failed
// startup-state record for diagnostics and retain all actual cleanup errors.
func (s *Server) failListen(cause error) error {
	errs := []error{cause}
	for _, listener := range []net.Listener{s.httpListener, s.listener} {
		if listener != nil {
			if err := listener.Close(); err != nil && !runtimeAlreadyClosed(err) {
				errs = append(errs, err)
			}
		}
	}
	errs = append(errs, s.ownedSocket.remove(), s.ownedPID.remove())
	return errors.Join(errs...)
}

func (s *Server) captureStoppedRuntimeState() (ownedRuntimeFile, error) {
	file, err := captureOwnedRuntimeFile(s.statePath, true)
	if err != nil || file.info == nil {
		return file, err
	}
	var state RuntimeState
	if err := json.Unmarshal(file.content, &state); err != nil {
		return ownedRuntimeFile{}, fmt.Errorf("read owned daemon runtime record: %w", err)
	}
	identity, err := platform.CaptureProcessIdentity(os.Getpid())
	if err != nil {
		return ownedRuntimeFile{}, err
	}
	token, err := identity.Token()
	if err != nil {
		return ownedRuntimeFile{}, err
	}
	if state.PID != os.Getpid() || state.ProcessBirthToken != token {
		return ownedRuntimeFile{}, errors.New("refusing to remove foreign or unverified daemon runtime record")
	}
	return file, nil
}

func (s *Server) removeRuntimeFiles() error {
	state, stateErr := s.captureStoppedRuntimeState()
	var errs []error
	errs = append(errs, stateErr, s.ownedSocket.remove(), s.ownedPID.remove())
	if stateErr == nil {
		errs = append(errs, state.remove())
	}
	// Actual absence is part of the completion receipt, not an assumption
	// made from a successful Remove return or from Controller.OK.
	for _, path := range []string{s.ownedSocket.path, s.pidPath, s.statePath} {
		if path == "" {
			continue
		}
		if _, err := os.Lstat(path); err == nil {
			errs = append(errs, fmt.Errorf("daemon runtime file remains after cleanup: %s", path))
		} else if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
