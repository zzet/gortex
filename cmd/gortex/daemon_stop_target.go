package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
)

type daemonStopFile struct {
	path    string
	info    os.FileInfo
	content []byte // PID/runtime records only; never read a socket as a file.
	regular bool
}

// Capture the native file identity now, including Windows' otherwise lazy
// path-only FileInfo ID. Never retain an open descriptor across the stop RPC.
func captureDaemonStopFile(path string, regular bool) (captured daemonStopFile, err error) {
	captured = daemonStopFile{path: path, regular: regular}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return captured, nil
	}
	if err != nil {
		return captured, err
	}
	if info.Mode()&os.ModeSymlink != 0 || regular && !info.Mode().IsRegular() {
		return captured, fmt.Errorf("unexpected daemon runtime file type: %s", path)
	}
	if !regular && info.Mode()&os.ModeSocket == 0 && !info.Mode().IsRegular() {
		return captured, fmt.Errorf("unexpected daemon endpoint file type: %s", path)
	}
	if !os.SameFile(info, info) {
		return captured, fmt.Errorf("cannot capture daemon runtime file identity: %s", path)
	}
	captured.info = info
	if regular {
		captured.content, err = readDaemonStopRecord(path)
		if err != nil {
			return captured, err
		}
	}
	_, err = captured.present()
	return captured, err
}

func readDaemonStopRecord(path string) (_ []byte, err error) {
	file, err := openExistingDaemonStoreLock(path) // existing/no-follow, never create
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	const maxRecordBytes = 64 << 10
	content, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxRecordBytes {
		return nil, fmt.Errorf("daemon runtime record exceeds %d bytes: %s", maxRecordBytes, path)
	}
	return content, nil
}

func (f daemonStopFile) present() (bool, error) {
	info, err := os.Lstat(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if f.info == nil || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(f.info, info) {
		return true, fmt.Errorf("daemon runtime file appeared or changed identity: %s", f.path)
	}
	if f.regular {
		content, err := readDaemonStopRecord(f.path)
		if err != nil {
			return true, err
		}
		if !bytes.Equal(content, f.content) {
			return true, fmt.Errorf("daemon runtime record changed content: %s", f.path)
		}
	}
	return true, nil
}

type daemonStopTarget struct {
	process             platform.ProcessIdentity
	absentPID           int // positive native absence observed before any RPC; never signal
	files               []daemonStopFile
	store               *daemonStoreLock
	storeErr            error
	signalOwnerVerified bool
	// Only an absent legacy runtime record or an explicitly empty backend
	// permits process-exit fallback. A failed/missing expected lock does not.
	noStore bool
}

func captureDaemonStopTarget(pid int) (*daemonStopTarget, error) {
	identity, err := platform.CaptureProcessIdentity(pid)
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return nil, err
	}
	target := &daemonStopTarget{process: identity}
	if errors.Is(err, os.ErrProcessDone) {
		target.absentPID = pid
	}
	for _, item := range []struct {
		path    string
		regular bool
	}{{daemon.PIDFilePath(), true}, {daemon.RuntimeStatePath(), true}, {daemon.SocketPath(), false}} {
		file, err := captureDaemonStopFile(item.path, item.regular)
		if err != nil {
			return nil, err
		}
		target.files = append(target.files, file)
	}
	pidRecord := target.files[0]
	if pidRecord.info == nil || strings.TrimSpace(string(pidRecord.content)) != strconv.Itoa(pid) {
		return nil, errors.New("daemon PID record does not match captured process")
	}
	state := target.files[1]
	if state.info == nil {
		target.noStore = true // Old daemon: no declared store; never guess a default.
	} else {
		var runtime daemon.RuntimeState
		if err := json.Unmarshal(state.content, &runtime); err != nil {
			return nil, fmt.Errorf("read captured daemon runtime: %w", err)
		}
		if runtime.PID != pid {
			return nil, errors.New("daemon runtime record does not match captured process")
		}
		if runtime.ProcessBirthToken != "" && target.absentPID == 0 {
			token, err := identity.Token()
			if err != nil {
				return nil, err
			}
			if runtime.ProcessBirthToken != token {
				return nil, fmt.Errorf("daemon runtime birth token does not match native process: %w", platform.ErrProcessIdentityChanged)
			}
			target.signalOwnerVerified = true
		}
		if runtime.BackendPath == "" {
			target.noStore = true
		} else {
			target.store, target.storeErr = captureDaemonStoreLock(runtime.BackendPath)
		}
	}
	if target.absentPID != 0 {
		if _, err := target.processRunning(); !errors.Is(err, os.ErrProcessDone) {
			return nil, errors.Join(errors.New("absent daemon PID changed during stop capture"), err)
		}
	} else if same, err := identity.Matches(); err != nil || !same {
		return nil, errors.Join(errors.New("daemon process changed during stop capture"), err)
	}
	return target, nil
}

func (t *daemonStopTarget) acceptProbeOwner(pid int) error {
	if pid <= 0 || pid != t.pid() {
		return errors.New("daemon probe PID does not match captured runtime owner")
	}
	if err := t.verifyBeforeControl(); err != nil {
		return err
	}
	t.signalOwnerVerified = true
	return nil
}

func (t *daemonStopTarget) requireSignalOwner() error {
	if t.absentPID != 0 || !t.signalOwnerVerified {
		return errors.New("cannot verify daemon incarnation for PID signal: legacy runtime has no matching birth token or successful matching daemon probe")
	}
	return nil
}

func (t *daemonStopTarget) pid() int {
	if t.absentPID != 0 {
		return t.absentPID
	}
	return t.process.PID()
}

func (t *daemonStopTarget) processRunning() (bool, error) {
	if t.absentPID != 0 {
		// Absence is not a reusable birth token. If this PID now exists,
		// refuse to infer ownership or signal its new incarnation.
		_, err := platform.CaptureProcessIdentity(t.absentPID)
		if errors.Is(err, os.ErrProcessDone) {
			return false, err
		}
		if err != nil {
			return false, err
		}
		return false, platform.ErrProcessIdentityChanged
	}
	return daemonCapturedProcessRunning(t.process)
}

func (t *daemonStopTarget) runtimeGone() (bool, error) {
	gone := true
	var errs []error
	for _, file := range t.files {
		present, err := file.present()
		gone = gone && !present && err == nil
		errs = append(errs, err)
	}
	return gone, errors.Join(errs...)
}

func (t *daemonStopTarget) verifyBeforeControl() error {
	if t.absentPID != 0 {
		return errors.New("refusing shutdown control for an already-exited captured PID")
	}
	if same, err := t.process.Matches(); err != nil || !same {
		return errors.Join(errors.New("daemon process changed before shutdown control"), err)
	}
	for _, file := range t.files {
		present, err := file.present()
		if err != nil {
			return err
		}
		if file.info != nil && !present {
			return fmt.Errorf("captured daemon runtime vanished before shutdown control: %s", file.path)
		}
	}
	return nil
}

func (t *daemonStopTarget) storeFree() (bool, error) {
	if t.storeErr != nil {
		return false, t.storeErr
	}
	if t.store != nil {
		return t.store.probe()
	}
	if !t.noStore {
		return false, errors.New("daemon store identity is unknown")
	}
	// Explicit older/no-store fallback needs actual native process exit.
	// Permission/query failures cannot masquerade as a free store.
	running, err := t.processRunning()
	if errors.Is(err, os.ErrProcessDone) {
		return true, nil
	}
	return !running && err == nil, err
}

func (t *daemonStopTarget) removeExitedRuntime() error {
	running, err := t.processRunning()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	if running {
		return errors.New("refusing runtime cleanup while captured daemon is still running")
	}
	var errs []error
	// Refuse the cleanup as a whole if any captured path now belongs to
	// something else. In particular, a rewritten PID record must not let us
	// remove a successor's socket merely because that pathname still exists.
	for _, file := range t.files {
		_, err := file.present()
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for _, file := range t.files {
		present, err := file.present()
		if err == nil && present {
			err = os.Remove(file.path)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("clean daemon runtime %s: %w", file.path, err))
		}
	}
	return errors.Join(errs...)
}
