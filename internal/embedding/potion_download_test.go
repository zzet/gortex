package embedding

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
)

type potionDownloadTransport func(*http.Request) (*http.Response, error)

func (f potionDownloadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPotionDownloadPolicyForTestExecutables(t *testing.T) {
	// Exercise downloadPotion itself without ever reaching a network endpoint.
	// An allowed application reaches this sentinel transport; test executables
	// must stop earlier, even when the environment explicitly enables downloads.
	blockedNetwork := errors.New("download transport sentinel")
	originalArgs, originalTransport := os.Args, http.DefaultTransport
	t.Cleanup(func() {
		os.Args = originalArgs
		http.DefaultTransport = originalTransport
	})
	for _, tc := range []struct {
		name, executable, setting string
		wantRequest               bool
	}{
		{"unix_default", "/tmp/mcp.test", "", false},
		{"unix_enabled", "/tmp/mcp.test", "1", false},
		{"windows_default", `D:\scratch\mcp.test.exe`, "", false},
		{"windows_enabled", `D:\scratch\mcp.test.exe`, "1", false},
		{"application_default", "/usr/local/bin/gortex", "", true},
		{"application_enabled", `D:\bin\gortex.exe`, "1", true},
		{"application_other_value", "/usr/local/bin/gortex", "other", true},
		{"application_zero", "/usr/local/bin/gortex", "0", false},
		{"application_false", `D:\bin\gortex.exe`, " false ", false},
		{"application_off", "/usr/local/bin/gortex", "OFF", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_POTION_DOWNLOAD", tc.setting)
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			os.Args = []string{tc.executable}
			requests := 0
			http.DefaultTransport = potionDownloadTransport(func(*http.Request) (*http.Response, error) {
				requests++
				return nil, blockedNetwork
			})
			dir, err := downloadPotion()
			if dir != "" || err == nil {
				t.Fatalf("downloadPotion = (%q, %v), want no model and an error", dir, err)
			}
			if tc.wantRequest {
				if requests != 1 || !errors.Is(err, blockedNetwork) {
					t.Fatalf("application request count=%d error=%v", requests, err)
				}
			} else if requests != 0 || !strings.Contains(err.Error(), "model download disabled") {
				t.Fatalf("disabled request count=%d error=%v", requests, err)
			}
		})
	}
}
