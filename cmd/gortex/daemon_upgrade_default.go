//go:build !acceptance

package main

import "github.com/zzet/gortex/internal/serverstack"

func configureDaemonUpgradeCheckpoint(_ *serverstack.SharedServerConfig, _ func(string, int)) {}
