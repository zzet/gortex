package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

func TestDaemonMigrationPhaseCoversRemainingOpen(t *testing.T) {
	r := newDaemonStartupReporterWithWriter(zap.NewNop(), func(daemon.RuntimeState) error { return nil })
	defer r.Stop()
	state := func() daemon.RuntimeState { r.mu.Lock(); defer r.mu.Unlock(); return r.state }
	require.Equal(t, daemon.StartupOpeningStore, state().StartupPhase)
	r.StoreOpened()
	require.Equal(t, daemon.StartupOpeningStore, state().StartupPhase, "ordinary opens do not invent a migration")
	r.ObserveMigration(store_sqlite.MigrationProgress{Phase: store_sqlite.MigrationStarted, Version: 44, Name: "actual last step"})
	r.ObserveMigration(store_sqlite.MigrationProgress{Phase: store_sqlite.MigrationFinished, Version: 44, Name: "actual last step"})
	require.Equal(t, daemon.StartupMigrating, state().StartupPhase, "step completion is before transaction commit, index recreation and schema stamping")
	require.Equal(t, 44, state().MigrationVersion)
	r.StoreOpened()
	require.Equal(t, daemon.StartupOpeningStore, state().StartupPhase)
	require.Zero(t, state().MigrationVersion)
	require.Empty(t, state().MigrationName)
	r.Serving("owned.sqlite")
	require.Equal(t, daemon.StartupServing, state().StartupPhase)
	require.Zero(t, state().MigrationVersion)
	require.Empty(t, state().MigrationName)
	r.Fail(errors.New("owned startup failure"))
	require.Equal(t, daemon.StartupFailed, state().StartupPhase)
}
