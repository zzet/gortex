package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOwnedLoopbackProfileCaptureAndFailures(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/debug/pprof/profile" || r.URL.Query().Get("seconds") != "1" {
					t.Errorf("unexpected profile request: %s", r.URL)
				}
				w.WriteHeader(code)
				_, _ = w.Write([]byte("retained-profile-fixture"))
			}))
			defer server.Close()
			status, _ := json.Marshal(map[string]any{"pprof_addr": strings.TrimPrefix(server.URL, "http://")})
			path := filepath.Join(t.TempDir(), "profile.pprof")
			err := captureCPUProfile(context.Background(), status, time.Second, path)
			if code == http.StatusOK {
				data, readErr := os.ReadFile(path)
				if err != nil || readErr != nil || string(data) != "retained-profile-fixture" {
					t.Fatalf("profile not retained: %s %v %v", data, err, readErr)
				}
				if err := captureCPUProfile(context.Background(), status, time.Second, path); err == nil {
					t.Fatal("existing profile overwritten")
				}
			} else if err == nil {
				t.Fatal("HTTP failure became successful profile")
			}
		})
	}
	for _, status := range []string{`{}`, `{"pprof_addr":"192.0.2.1:1234"}`, `{"pprof_addr":"localhost:1234"}`} {
		if err := captureCPUProfile(context.Background(), []byte(status), time.Second, filepath.Join(t.TempDir(), "no.pprof")); err == nil {
			t.Fatalf("accepted non-owned endpoint: %s", status)
		}
	}
}
