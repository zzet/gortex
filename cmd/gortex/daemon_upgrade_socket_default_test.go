//go:build !acceptance && (linux || darwin)

package main

import (
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
	"testing"
)

func migrationSocketAcceptanceBuild(_ *testing.T, _ string, _ *zap.Logger, _ ...store_sqlite.MigrationObserver) (*daemonState, error, bool) {
	return nil, nil, false
}
