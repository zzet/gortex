package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeStateStartupRoundTripAndFreshness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.state.json")
	t.Setenv("GORTEX_DAEMON_STATEFILE", path)
	started := time.Now().Add(-time.Second).UnixMilli()
	if err := WriteRuntimeState(RuntimeState{StartupPhase: StartupMigrating, StartupStartedAt: started, MigrationVersion: 17, MigrationName: "generation_indexes"}); err != nil {
		t.Fatal(err)
	}
	st, ok := ReadRuntimeState()
	if !ok {
		t.Fatal("runtime state was not readable")
	}
	if st.PID != os.Getpid() || st.MigrationVersion != 17 || st.MigrationName != "generation_indexes" {
		t.Fatalf("unexpected runtime state: %+v", st)
	}
	if !st.StartupProgressFresh(time.Now(), 10*time.Second) {
		t.Fatal("new heartbeat should be fresh")
	}
	st.StartupUpdatedAt = time.Now().Add(-11 * time.Second).UnixMilli()
	if st.StartupProgressFresh(time.Now(), 10*time.Second) {
		t.Fatal("stale heartbeat must not extend startup wait")
	}
	st.StartupUpdatedAt = 0
	if st.StartupProgressFresh(time.Now(), 10*time.Second) {
		t.Fatal("legacy state must not extend startup wait")
	}
}

func TestWriteRuntimeStatePublishesWholeJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.state.json")
	t.Setenv("GORTEX_DAEMON_STATEFILE", path)
	for i := 0; i < 100; i++ {
		if err := WriteRuntimeState(RuntimeState{StartupPhase: StartupOpeningStore, MigrationVersion: i}); err != nil {
			t.Fatal(err)
		}
		if _, ok := ReadRuntimeState(); !ok {
			t.Fatalf("publication %d was partial or unreadable", i)
		}
	}
}
