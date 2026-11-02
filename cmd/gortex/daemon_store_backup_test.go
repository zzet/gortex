package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestDaemonStartupPrunesExpiredStoreBackups(t *testing.T) {
	startupPublicationEnv(t)
	store := daemonBackendPath
	now := time.Now()
	aged := func(path string, age time.Duration) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return path
	}
	expired := aged(store+".v21.backup", 15*24*time.Hour)
	recent := aged(store+".v40.backup", 5*24*time.Hour)
	partial := aged(store+".v43.backup.partial", time.Minute)
	unrelated := aged(store+".bak", 30*24*time.Hour)

	state, err := buildDaemonState(zap.NewNop())
	if err != nil {
		t.Fatalf("buildDaemonState: %v", err)
	}
	t.Cleanup(func() {
		if state.shared != nil {
			_ = state.shared.Close()
		}
	})

	for _, gone := range []string{expired, partial} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("daemon start left %s behind (stat err %v)", filepath.Base(gone), err)
		}
	}
	for _, kept := range []string{recent, unrelated} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("daemon start removed %s: %v", filepath.Base(kept), err)
		}
	}
}

func TestDaemonStartupReporterLogsBackupDecisions(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "daemon.state.json"))
	core, logs := observer.New(zapcore.InfoLevel)
	r := newDaemonStartupReporter(zap.New(core))
	defer r.Stop()

	r.ObserveMigration(store_sqlite.MigrationProgress{Version: 21, Phase: store_sqlite.MigrationBackupCreated, Detail: "/stores/store.sqlite.v21.backup"})
	r.ObserveMigration(store_sqlite.MigrationProgress{Version: 21, Phase: store_sqlite.MigrationBackupSkipped, Detail: "5 bytes available cover the upgrade"})

	created := logs.FilterField(zap.String("backup", "/stores/store.sqlite.v21.backup")).All()
	if len(created) != 1 || created[0].Level != zapcore.InfoLevel {
		t.Fatalf("created backup log: %+v", created)
	}
	skipped := logs.FilterField(zap.String("reason", "5 bytes available cover the upgrade")).All()
	if len(skipped) != 1 || skipped[0].Level != zapcore.WarnLevel {
		t.Fatalf("skipped backup log must say why: %+v", skipped)
	}
	st, ok := daemon.ReadRuntimeState()
	if !ok || st.StartupPhase != daemon.StartupOpeningStore || st.StartupError != "" {
		t.Fatalf("a backup decision is not a startup phase: %+v ok=%v", st, ok)
	}
}
