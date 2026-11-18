package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSocketPathClampFallback proves the AF_UNIX length clamp: a short path
// passes through, an over-long one is replaced by a short, stable, distinct
// temp-dir socket.
func TestSocketPathClampFallback(t *testing.T) {
	short := filepath.Join(os.TempDir(), "gortex.sock")
	if got := clampSocketPath(short); got != short {
		t.Errorf("a short path must pass through unchanged; got %q", got)
	}

	long := "/" + strings.Repeat("a", 200) + "/daemon.sock"
	got := clampSocketPath(long)
	if len(got) >= socketAddrMax() {
		t.Errorf("clamped path still too long (%d ≥ %d): %q", len(got), socketAddrMax(), got)
	}
	if !strings.HasSuffix(got, ".sock") {
		t.Errorf("the fallback must be a .sock path; got %q", got)
	}
	if clampSocketPath(long) != got {
		t.Error("the clamp must be deterministic (same input → same socket)")
	}
	long2 := "/" + strings.Repeat("b", 200) + "/daemon.sock"
	if clampSocketPath(long2) == got {
		t.Error("different over-long paths must map to distinct sockets")
	}
}

// TestIdleTimeoutFromEnvParse covers the opt-in idle-timeout parsing: only a
// valid positive duration enables auto-exit; everything else stays disabled.
func TestIdleTimeoutFromEnvParse(t *testing.T) {
	if parseIdleTimeout("") != 0 {
		t.Error("empty must disable the idle timeout")
	}
	if parseIdleTimeout("garbage") != 0 {
		t.Error("an unparseable value must disable the idle timeout")
	}
	if parseIdleTimeout("-5m") != 0 {
		t.Error("a non-positive duration must disable the idle timeout")
	}
	if got := parseIdleTimeout("30m"); got != 30*time.Minute {
		t.Errorf("parseIdleTimeout(30m) = %v, want 30m", got)
	}
}

func TestStderrLogFilePath(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct{ log, want string }{
		{"daemon.log", "daemon.stderr.log"},
		{"gortex-daemon.log", "gortex-daemon.stderr.log"},
		{"custom.log", "custom.stderr.log"},
		{"custom", "custom.stderr"},
	} {
		t.Run(tt.log, func(t *testing.T) {
			logPath := filepath.Join(dir, tt.log)
			t.Setenv("GORTEX_DAEMON_LOGFILE", logPath)
			want := filepath.Join(dir, tt.want)
			if got := StderrLogFilePath(); got != want {
				t.Fatalf("StderrLogFilePath() = %q, want %q", got, want)
			}
		})
	}
}
