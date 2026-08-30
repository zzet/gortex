package main

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestBeforeTrackDeadlineBoundsDaemonReadiness(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	started := time.Now()
	_, timedOut := beforeTrackDeadline(time.Now().Add(20*time.Millisecond), func() daemonDecision {
		<-release
		return daemonReady
	})
	if !timedOut {
		t.Fatal("blocking daemon readiness must consume the shared deadline")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("deadline returned after %s, want under 500ms", elapsed)
	}
}

func TestNotifyDaemonTrackUsesRemainingDeadline(t *testing.T) {
	socket := filepath.Join("/tmp", filepath.Base(filepath.Dir(t.TempDir()))+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		_ = listener.Close()
	})

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		if _, readErr := reader.ReadBytes('\n'); readErr != nil {
			return
		}
		if writeErr := daemon.WriteJSONLine(conn, daemon.HandshakeAck{OK: true}); writeErr != nil {
			return
		}
		if _, readErr := reader.ReadBytes('\n'); readErr != nil {
			return
		}
		<-release // Deliberately never answer ControlTrack inside its budget.
	}()

	started := time.Now()
	err = notifyDaemonTrack(t.TempDir(), time.Now().Add(40*time.Millisecond))
	if !errors.Is(err, daemon.ErrDaemonUnresponsive) {
		t.Fatalf("notifyDaemonTrack error = %v, want ErrDaemonUnresponsive", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("ControlTrack returned after %s, want under 500ms", elapsed)
	}
}

func TestRunTrackWaitTimeoutIncludesControlTrackAndKeepsConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "nonexistent.sock"))
	repo := t.TempDir()

	origWait, origTimeout := trackWait, trackWaitTimeout
	origName, origAsWorktree := trackName, trackAsWorktree
	origEnsure, origNotify := trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn
	release := make(chan struct{})
	notifyStarted := make(chan time.Time, 1)
	trackWait = true
	trackWaitTimeout = 25 * time.Millisecond
	trackName = ""
	trackAsWorktree = false
	trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonReady }
	trackNotifyDaemonTrackFn = func(string, time.Time) error {
		notifyStarted <- time.Now()
		<-release
		return nil
	}
	t.Cleanup(func() {
		close(release)
		trackWait, trackWaitTimeout = origWait, origTimeout
		trackName, trackAsWorktree = origName, origAsWorktree
		trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn = origEnsure, origNotify
	})

	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	err := runTrack(cmd, []string{repo})
	if err == nil {
		t.Fatal("expected the blocking ControlTrack stage to time out")
	}
	if !strings.Contains(err.Error(), "timed out after 25ms") || !strings.Contains(err.Error(), "tracked in config") {
		t.Fatalf("timeout error must report the total budget and durable config: %v", err)
	}
	stageStarted := <-notifyStarted
	if elapsed := time.Since(stageStarted); elapsed > 500*time.Millisecond {
		t.Fatalf("ControlTrack stage returned after %s, want under 500ms", elapsed)
	}

	gc, loadErr := config.LoadGlobal()
	if loadErr != nil {
		t.Fatalf("load global config: %v", loadErr)
	}
	wantRepo := pathkey.CanonicalExistingRoot(repo)
	for _, entry := range gc.Repos {
		if pathkey.CanonicalExistingRoot(entry.Path) == wantRepo {
			return
		}
	}
	t.Fatalf("timed-out --wait must leave %s tracked in config; repos=%v", repo, gc.Repos)
}

func TestWaitForRepoIndexedBoundsBlockingStatusRPC(t *testing.T) {
	abs := t.TempDir()
	origFn := trackStatusFn
	release := make(chan struct{})
	trackStatusFn = func() (daemon.StatusResponse, error) {
		<-release
		return daemon.StatusResponse{}, nil
	}
	t.Cleanup(func() {
		close(release)
		trackStatusFn = origFn
	})

	started := time.Now()
	err := waitForRepoIndexed(&bytes.Buffer{}, abs, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected blocking status RPC to time out")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("status RPC returned after %s, want under 500ms", elapsed)
	}
}
