//go:build darwin || linux

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestDaemonStopTargetOwnsFreeStoreAndRuntimeBeforeZombieIsReaped(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	require.NoError(t, target.storeErr)
	require.NotNil(t, target.store)
	// A zombie leader is a separate observation from final shared-file-table
	// release on Linux. Await the actual captured lock independently, still
	// without Wait; never infer store-free from the process state alone.
	probeReleased := func() (free bool, err error) {
		file, err := os.OpenFile(target.store.path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return false, err
		}
		defer func() { err = errors.Join(err, file.Close()) }()
		info, err := file.Stat()
		if err != nil {
			return false, err
		}
		if !os.SameFile(target.store.info, info) {
			return false, errors.New("independent fixture lock differs from captured store lock")
		}
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		err = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		return err == nil, err
	}
	free, err := probeReleased()
	require.NoError(t, err)
	require.False(t, free, "independent probe must observe the live helper holding its lock")
	require.NoError(t, exit())
	// Independent native witness, before cmd.Wait: do not let the identity
	// implementation under test define its own zombie oracle.
	require.Eventually(t, func() bool {
		state, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(target.process.PID())).Output()
		return err == nil && strings.HasPrefix(strings.TrimSpace(string(state)), "Z")
	}, 5*time.Second, 10*time.Millisecond)
	var observedFree bool
	var probeErr error
	require.Eventually(t, func() bool {
		observedFree, probeErr = probeReleased()
		return observedFree || probeErr != nil // Unknown/errors fail, never become free.
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, probeErr)
	require.True(t, observedFree)
	free, err = target.storeFree()
	require.NoError(t, err)
	require.True(t, free)
	var out bytes.Buffer
	result, err := waitForDaemonTarget(&out, target, 0, false)
	require.NoError(t, err)
	require.False(t, result.forced)
	require.Contains(t, out.String(), "not yet been reaped by its parent")
	require.NotContains(t, out.String(), "timed out")
	gone, err := target.runtimeGone()
	require.NoError(t, err)
	require.True(t, gone)
	same, err := target.process.Matches()
	require.NoError(t, err)
	require.True(t, same, "fixture was reaped before the store-free/runtime proof")
	running, err := target.process.Running()
	require.NoError(t, err)
	require.False(t, running)
	require.NoError(t, wait()) // Only now release this exact child PID.
}
