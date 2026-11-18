//go:build acceptance

package main

import "github.com/zzet/gortex/internal/serverstack"

func configureDaemonUpgradeCheckpoint(cfg *serverstack.SharedServerConfig, callback func(string, int)) {
	serverstack.SetAcceptanceUpgradeHook(cfg, callback)
}
