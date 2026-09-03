package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

func TestDaemonStartupProgressRequiresFreshMatchingChild(t *testing.T) {
	now := time.Now()
	fresh := daemon.RuntimeState{PID: 42, StartupPhase: daemon.StartupMigrating, StartupUpdatedAt: now.UnixMilli(), MigrationVersion: 18, MigrationName: "mask_tables"}
	if label, ok := daemonStartupProgress(fresh, true, 42, now, 10*time.Second); !ok || label != "migrating schema v18 (mask_tables)" {
		t.Fatalf("fresh matching heartbeat: %q %v", label, ok)
	}
	if _, ok := daemonStartupProgress(fresh, true, 43, now, 10*time.Second); ok {
		t.Fatal("another PID must not extend wait")
	}
	fresh.StartupUpdatedAt = now.Add(-11 * time.Second).UnixMilli()
	if _, ok := daemonStartupProgress(fresh, true, 42, now, 10*time.Second); ok {
		t.Fatal("stale heartbeat must not extend wait")
	}
	fresh.StartupUpdatedAt = 0
	if _, ok := daemonStartupProgress(fresh, true, 42, now, 10*time.Second); ok {
		t.Fatal("legacy state must not extend wait")
	}
	if _, ok := daemonStartupProgress(fresh, false, 42, now, 10*time.Second); ok {
		t.Fatal("missing state must not extend wait")
	}
}

func TestDaemonStartupReporterMigrationFailureIsSanitized(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "daemon.state.json"))
	r := newDaemonStartupReporter(zap.NewNop())
	defer r.Stop()
	r.ObserveMigration(store_sqlite.MigrationProgress{Version: 17, Name: "generation_indexes", Phase: store_sqlite.MigrationStarted})
	st, ok := daemon.ReadRuntimeState()
	if !ok || st.StartupPhase != daemon.StartupMigrating || st.MigrationVersion != 17 {
		t.Fatalf("migration state: %+v, ok=%v", st, ok)
	}
	secret := "postgres://user:secret@example/private"
	r.ObserveMigration(store_sqlite.MigrationProgress{Version: 17, Name: "generation_indexes", Phase: store_sqlite.MigrationFailed, Error: errors.New(secret)})
	st, ok = daemon.ReadRuntimeState()
	if !ok || st.StartupPhase != daemon.StartupFailed {
		t.Fatalf("failed state: %+v, ok=%v", st, ok)
	}
	if st.StartupError == "" || strings.Contains(st.StartupError, "secret") || strings.Contains(st.StartupError, "private") {
		t.Fatalf("raw sensitive error leaked: %q", st.StartupError)
	}
}
