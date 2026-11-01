package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

func TestStartupHandoffRequiresAnObservedMatchingOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("GORTEX_DAEMON_STATEFILE", path)
	now := time.Now()
	write := func(phase daemon.StartupPhase, updated time.Time) {
		writeHandoffStateFixture(t, phase, updated)
	}
	waiter := daemonStartupHeartbeatWaiter()
	write(daemon.StartupServing, now)
	require.False(t, waiter(now), "a daemon already serving is not evidence of this wait's startup")
	write(daemon.StartupMigrating, now)
	require.True(t, waiter(now))
	write(daemon.StartupServing, now)
	require.True(t, waiter(now.Add(time.Millisecond)), "same owner can finish its non-atomic handoff")
	require.False(t, daemonStartupHeartbeatWaiter()(now.Add(time.Millisecond)), "observation is local to one wait")
	var state daemon.RuntimeState
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &state))
	state.ProcessBirthToken = "different-process-incarnation"
	body, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, body, 0o600))
	require.False(t, waiter(now.Add(time.Millisecond)), "a reused PID cannot inherit the handoff lease")
}

func TestStartupHandoffDoesNotRenewFromPeriodicServingHeartbeats(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "state.json"))
	now := time.Now()
	waiter := daemonStartupHeartbeatWaiter()
	writeHandoffStateFixture(t, daemon.StartupOpeningStore, now)
	require.True(t, waiter(now))
	later := now.Add(spawnProgressFreshness + time.Millisecond)
	writeHandoffStateFixture(t, daemon.StartupServing, later)
	require.False(t, waiter(later), "fresh periodic Serving writes cannot extend the fixed handoff lease")
}

func TestStartupHandoffRejectsStaleFailedAndFutureRecords(t *testing.T) {
	for _, name := range []string{"stale", "failed", "future"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "state.json"))
			now := time.Now()
			waiter := daemonStartupHeartbeatWaiter()
			writeHandoffStateFixture(t, daemon.StartupMigrating, now)
			require.True(t, waiter(now))
			state := daemon.RuntimeState{StartupPhase: daemon.StartupServing, StartupUpdatedAt: now.UnixMilli()}
			switch name {
			case "stale":
				state.StartupUpdatedAt = now.Add(-2 * spawnProgressFreshness).UnixMilli()
			case "failed":
				state.StartupPhase = daemon.StartupFailed
			case "future":
				state.StartupUpdatedAt = now.Add(2 * spawnProgressFreshness).UnixMilli()
			}
			writeHandoffStateFixture(t, state.StartupPhase, time.UnixMilli(state.StartupUpdatedAt))
			require.False(t, waiter(now.Add(time.Millisecond)))
		})
	}
}

// The real publisher deliberately stamps real time. Alter only this owned
// synthetic record after publication to exercise stale/future clock inputs.
func writeHandoffStateFixture(t *testing.T, phase daemon.StartupPhase, updated time.Time) {
	t.Helper()
	require.NoError(t, daemon.WriteRuntimeState(daemon.RuntimeState{StartupPhase: phase}))
	path := daemon.RuntimeStatePath()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var state daemon.RuntimeState
	require.NoError(t, json.Unmarshal(body, &state))
	state.StartupUpdatedAt = updated.UnixMilli()
	body, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, body, 0o600))
}

func TestStartupHandoffRejectsUnchangedStaleOwnerToken(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "state.json"))
	now := time.Now()
	writeHandoffStateFixture(t, daemon.StartupMigrating, now)
	body, err := os.ReadFile(daemon.RuntimeStatePath())
	require.NoError(t, err)
	var state daemon.RuntimeState
	require.NoError(t, json.Unmarshal(body, &state))
	// Model an old incarnation's unchanged, well-formed token at a PID that
	// currently names a live different incarnation. The real native identity
	// disagrees, although both reads of this owned record agree with each other.
	parts := strings.Split(state.ProcessBirthToken, ":")
	require.Len(t, parts, 5)
	first := "f"
	if strings.HasPrefix(parts[2], first) {
		first = "e"
	}
	parts[2] = first + parts[2][1:]
	state.ProcessBirthToken = strings.Join(parts, ":")
	recordedToken := state.ProcessBirthToken
	body, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(daemon.RuntimeStatePath(), body, 0o600))
	waiter := daemonStartupHeartbeatWaiter()
	require.True(t, waiter(now), "legacy pre-socket freshness behavior stays unchanged")
	state.StartupPhase = daemon.StartupServing
	body, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(daemon.RuntimeStatePath(), body, 0o600))
	require.Equal(t, recordedToken, state.ProcessBirthToken, "the stale owner token never changes")
	require.False(t, waiter(now.Add(time.Millisecond)), "file-to-file equality cannot authorize a native-incarnation handoff")
}

func TestStartupHandoffLegacyPresocketDoesNotGrantServingLease(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "state.json"))
	now := time.Now()
	writeHandoffStateFixture(t, daemon.StartupOpeningStore, now)
	body, err := os.ReadFile(daemon.RuntimeStatePath())
	require.NoError(t, err)
	var state daemon.RuntimeState
	require.NoError(t, json.Unmarshal(body, &state))
	state.ProcessBirthToken = ""
	body, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(daemon.RuntimeStatePath(), body, 0o600))
	waiter := daemonStartupHeartbeatWaiter()
	require.True(t, waiter(now))
	state.StartupPhase = daemon.StartupServing
	body, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(daemon.RuntimeStatePath(), body, 0o600))
	require.False(t, waiter(now.Add(time.Millisecond)), "a legacy record cannot establish the new incarnation-bound Serving lease")
}

func TestStartupHandoffRechecksNativeProofWhenConsumed(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "state.json"))
	now := time.Now()
	proofAvailable := true
	waiter := startupHeartbeatWaiterWithOwnerProof(func(state daemon.RuntimeState) bool { return proofAvailable && startupRuntimeOwnerMatches(state) })
	writeHandoffStateFixture(t, daemon.StartupMigrating, now)
	require.True(t, waiter(now))
	before, ok := daemon.ReadRuntimeState()
	require.True(t, ok)
	// Native metadata may become inaccessible or identify another incarnation
	// after grant. Keeping the record unchanged cannot substitute for re-proof.
	proofAvailable = false
	writeHandoffStateFixture(t, daemon.StartupServing, now)
	after, ok := daemon.ReadRuntimeState()
	require.True(t, ok)
	require.Equal(t, before.ProcessBirthToken, after.ProcessBirthToken)
	require.False(t, waiter(now.Add(time.Millisecond)))
}
