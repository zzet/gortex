package main

import (
	"testing"

	"github.com/zzet/gortex/internal/config"
)

func TestResolveDaemonHTTPAddr(t *testing.T) {
	gc := &config.GlobalConfig{Daemon: config.DaemonConfig{HTTPAddr: "127.0.0.1:7411"}}
	tests := []struct {
		name string
		flag string
		env  string
		gc   *config.GlobalConfig
		want string
	}{
		{"flag wins", "127.0.0.1:7413", "127.0.0.1:7412", gc, "127.0.0.1:7413"},
		{"env wins over config", "", "127.0.0.1:7412", gc, "127.0.0.1:7412"},
		{"config alone", "", "", gc, "127.0.0.1:7411"},
		{"flag with nil config", "127.0.0.1:7413", "", nil, "127.0.0.1:7413"},
		{"env with nil config", "", "127.0.0.1:7412", nil, "127.0.0.1:7412"},
		{"nil config", "", "", nil, ""},
		{"all empty", "", "", &config.GlobalConfig{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveDaemonHTTPAddr(tc.flag, tc.env, tc.gc); got != tc.want {
				t.Errorf("resolveDaemonHTTPAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolvedDaemonHTTPAddrRequiresToken(t *testing.T) {
	gc := &config.GlobalConfig{Daemon: config.DaemonConfig{HTTPAddr: "0.0.0.0:7411"}}
	for _, tc := range []struct {
		name string
		env  string
		gc   *config.GlobalConfig
	}{
		{"env", "0.0.0.0:7411", nil},
		{"config", "", gc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := resolveDaemonHTTPAddr("", tc.env, tc.gc)
			if err := httpTokenRequirementError(addr, ""); err == nil {
				t.Error("non-loopback resolved address without a token must be refused")
			}
			if err := httpTokenRequirementError(addr, "tok"); err != nil {
				t.Errorf("non-loopback resolved address with a token must be allowed; got %v", err)
			}
		})
	}
}
