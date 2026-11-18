package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
)

// A PID file alone is not permission to signal: bind it to the socket's kernel
// peer, its process birth, and the same resolved runtime/store for this handoff.
// The spawn lock is held by the caller throughout capture, stop and lock wait.
type daemonHandoffPID struct {
	identity daemonIdentity
	state    daemon.RuntimeState
	birth    string
	socket   os.FileInfo
}

func captureDaemonHandoffPID(expected daemonIdentity) (*daemonHandoffPID, error) {
	if expected.peerPID <= 0 || expected.peerPID == os.Getpid() {
		return nil, fmt.Errorf("cannot verify a distinct daemon socket peer; use the matching binary to restart the daemon")
	}
	state, ok := daemon.ReadRuntimeState()
	pid, pidOK := daemon.RunningPID()
	if !ok || !pidOK || pid != expected.peerPID || state.PID != pid || !filepath.IsAbs(state.BackendPath) {
		return nil, fmt.Errorf("daemon PID/runtime/store does not match its socket peer; use the matching binary to restart the daemon")
	}
	for _, path := range []string{state.BackendPath, state.BackendPath + ".lock"} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("daemon runtime store/lock is not an existing regular file: %s", path)
		}
	}
	birth, err := handoffProcessBirth(pid)
	if err != nil {
		return nil, fmt.Errorf("capture daemon process identity: %w", err)
	}
	socket, err := os.Stat(daemon.SocketPath())
	if err != nil {
		return nil, err
	}
	g := &daemonHandoffPID{identity: expected, state: state, birth: birth, socket: socket}
	if err := g.validate(true); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *daemonHandoffPID) validate(requireSocket bool) error {
	pid := g.state.PID
	birth, err := handoffProcessBirth(pid)
	if err != nil || birth != g.birth || !platform.ProcessAlive(pid) {
		return fmt.Errorf("daemon process identity changed before signaling")
	}
	state, ok := daemon.ReadRuntimeState()
	currentPID, pidOK := daemon.RunningPID()
	if !ok || !pidOK || currentPID != pid || state.PID != pid || state.BackendPath != g.state.BackendPath {
		return fmt.Errorf("daemon PID/runtime/store identity changed before signaling")
	}
	if requireSocket {
		current := runningDaemonInfo()
		if current.peerPID != pid || current.version != g.identity.version || current.protocol != g.identity.protocol || current.protocolKnown != g.identity.protocolKnown || current.instance != g.identity.instance || current.mismatch() != g.identity.mismatch() || !current.olderThan(binaryVersion()) {
			if current.mismatch() {
				return fmt.Errorf("daemon socket identity changed before signaling: %w", current.err)
			}
			return fmt.Errorf("daemon socket identity changed before signaling")
		}
		socket, err := os.Stat(daemon.SocketPath())
		if err != nil || !os.SameFile(socket, g.socket) {
			return fmt.Errorf("daemon socket was replaced before signaling")
		}
		// The fresh handshake may have raced process exit/PID reuse.
		currentBirth, err := handoffProcessBirth(pid)
		if err != nil || currentBirth != g.birth {
			return fmt.Errorf("daemon process identity changed during handshake")
		}
	}
	return nil
}

func (g *daemonHandoffPID) alive(pid int) bool {
	if pid != g.state.PID || !platform.ProcessAlive(pid) {
		return false
	}
	birth, err := handoffProcessBirth(pid)
	// Unknown is not proof of exit. Keep waiting, then refuse to force-kill
	// unless the exact captured process can be positively identified again.
	return err != nil || birth == g.birth
}

func (g *daemonHandoffPID) forceKill(pid int) error {
	if pid != g.state.PID {
		return fmt.Errorf("daemon PID changed during close")
	}
	if err := g.validate(false); err != nil {
		return err
	}
	return platform.KillProcess(pid)
}

func (g *daemonHandoffPID) cleanup() {
	// A reused PID or newly written runtime/socket belongs to someone else.
	if platform.ProcessAlive(g.state.PID) {
		return
	}
	stateBytes, err := os.ReadFile(daemon.RuntimeStatePath())
	if err != nil && !os.IsNotExist(err) {
		return
	}
	if err == nil {
		// ReadRuntimeState intentionally rejects exited PIDs. The captured
		// runtime's stable fields are checked here without requiring liveness.
		var state daemon.RuntimeState
		if json.Unmarshal(stateBytes, &state) != nil || state.PID != g.state.PID || state.BackendPath != g.state.BackendPath {
			return
		}
	}
	pidBytes, err := os.ReadFile(daemon.PIDFilePath())
	if err != nil && !os.IsNotExist(err) {
		return
	}
	if err == nil && strings.TrimSpace(string(pidBytes)) != strconv.Itoa(g.state.PID) {
		return
	}
	if socket, err := os.Stat(daemon.SocketPath()); err == nil {
		if !os.SameFile(socket, g.socket) {
			return
		}
	} else if !os.IsNotExist(err) {
		return
	}
	_ = os.Remove(daemon.PIDFilePath())
	_ = os.Remove(daemon.SocketPath())
}

func stopDaemonForHandoff(expected daemonIdentity) error {
	if runtime.GOOS == "windows" {
		if expected.mismatch() {
			return fmt.Errorf("graceful unsupervised protocol handoff is unavailable on Windows; use the matching binary to restart the running daemon")
		}
		// A compatible Windows daemon can close via its supported control RPC.
		return stopRunningDaemon(io.Discard)
	}
	g, err := captureDaemonHandoffPID(expected)
	if err != nil {
		return err
	}
	stopProgress := installDaemonCloseProgressForStore(g.state.BackendPath)
	defer stopProgress()
	// Revalidate immediately before the signal, including a fresh handshake;
	// never signal a stale/reused PID merely because its file remains.
	if err := g.validate(true); err != nil {
		return err
	}
	if err := platform.TerminateProcess(g.state.PID); err != nil {
		return err
	}
	_, err = waitForDaemonExitChecked(os.Stderr, g.state.PID, daemonStopExitGrace(false, daemonStopPreflight{}), true, g.alive, g.forceKill)
	if err != nil {
		return err
	}
	g.cleanup()
	// Process exit is necessary, and the actual old store lock must also be
	// free. Do not open SQLite, run migrations or assume a timeout means free.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock := flock.New(g.state.BackendPath + ".lock")
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("wait for old daemon store lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("old daemon store lock is still held")
	}
	if err := lock.Unlock(); err != nil {
		return fmt.Errorf("release store-lock probe: %w", err)
	}
	return nil
}
