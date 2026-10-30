package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

type daemonContractDrain interface {
	Close(context.Context) error
	waitClosed() error
}

// closeDaemonOwners keeps a diagnostic timeout separate from the ownership
// barrier. A slow contract producer must neither strand the shared stack by an
// early return nor outlive its backend. This join is not yet time bounded.
func closeDaemonOwners(ctx context.Context, contracts daemonContractDrain, closeShared func() error, logger *zap.Logger) error {
	started := time.Now()
	logger.Info("daemon: shutdown phase start", zap.String("phase", "contract_drain"))
	var warning error
	if err := contracts.Close(ctx); err != nil {
		warning = fmt.Errorf("contract drain diagnostic wait: %w", err)
		logger.Warn("daemon: contract drain still pending; waiting for ownership", zap.Error(err), zap.Duration("elapsed", time.Since(started)))
	}
	// Always join the SAME runtime-owned drain, even if the diagnostic wait
	// failed. Only this completion barrier permits backend teardown.
	drainErr := contracts.waitClosed()
	logger.Info("daemon: shutdown phase complete", zap.String("phase", "contract_drain"), zap.Duration("elapsed", time.Since(started)), zap.Error(drainErr))
	started = time.Now()
	logger.Info("daemon: shutdown phase start", zap.String("phase", "shared_close"))
	closeErr := closeShared()
	logger.Info("daemon: shutdown phase complete", zap.String("phase", "shared_close"), zap.Duration("elapsed", time.Since(started)), zap.Error(closeErr))
	if drainErr != nil {
		drainErr = fmt.Errorf("joined contract drain: %w", drainErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("shared close: %w", closeErr)
	}
	return errors.Join(warning, drainErr, closeErr)
}
