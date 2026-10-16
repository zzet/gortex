package daemon

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"

	"github.com/zzet/gortex/internal/platform"
)

// stateDir returns the directory the daemon keeps its runtime state in
// (socket, PID file, logs) and whether it could be resolved.
//
// An absolute $XDG_CACHE_HOME is honoured on every platform. When it is
// unset the location stays at the historical default so an existing
// daemon state directory is not orphaned:
//
//   - Windows: %USERPROFILE%\.gortex\cache (via os.UserCacheDir).
//   - macOS / Linux: $HOME/.gortex/cache.
//
// The boolean is false when the home / cache directory can't be
// resolved at all, in which case callers fall back to the temp dir.
func stateDir() (string, bool) {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("XDG_CACHE_HOME"); v == "" || !filepath.IsAbs(v) {
			if _, err := os.UserCacheDir(); err != nil {
				return "", false
			}
		}
		return platform.OSCacheDir(), true
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v == "" || !filepath.IsAbs(v) {
		if _, err := os.UserHomeDir(); err != nil {
			return "", false
		}
	}
	return platform.CacheDir(), true
}

// SocketPath returns the socket path the daemon listens on. The socket
// is an AF_UNIX socket on every supported OS — Windows has supported
// AF_UNIX since Windows 10 1803, so the same transport works there.
//
// Order of preference:
//  1. $GORTEX_DAEMON_SOCKET — explicit override (tests, custom deployments).
//  2. $XDG_RUNTIME_DIR/gortex.sock — Linux standard for user runtime files.
//     This path is cleaned automatically on logout and has sensible perms.
//  3. The per-user state dir — $HOME/.gortex/cache on macOS/Linux,
//     %USERPROFILE%\.gortex\cache on Windows.
//
// AF_UNIX socket paths have a length limit (~104 bytes on macOS, 108 on Linux
// and Windows). An auto-computed path that would exceed it — a deeply-nested
// home directory, a long $XDG_RUNTIME_DIR — is replaced by a short, stable
// temp-dir fallback (clampSocketPath) so the listener binds instead of failing.
// An explicit $GORTEX_DAEMON_SOCKET override is honoured verbatim: the user
// chose that path and gets a loud failure if it's too long, never a silent
// redirect.
func SocketPath() string {
	if override := os.Getenv("GORTEX_DAEMON_SOCKET"); override != "" {
		return override
	}
	return clampSocketPath(autoSocketPath())
}

// autoSocketPath computes the daemon's default socket path from the runtime /
// state directories, before any length clamping.
func autoSocketPath() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && runtime.GOOS == "linux" {
		return filepath.Join(rt, "gortex.sock")
	}
	if dir, ok := stateDir(); ok {
		return filepath.Join(dir, "daemon.sock")
	}
	// Fall back to the temp dir as a last resort; the daemon must start
	// somewhere.
	return filepath.Join(os.TempDir(), "gortex.sock")
}

// socketAddrMax is the AF_UNIX sun_path limit for the current OS: 104 bytes on
// macOS/BSD, 108 on Linux and Windows. A path whose length reaches this fails
// the bind, so we clamp strictly below it.
func socketAddrMax() int {
	if runtime.GOOS == "darwin" {
		return 104
	}
	return 108
}

// clampSocketPath returns p unchanged when it is short enough to bind, else a
// short temp-dir fallback derived from a stable hash of p — so two daemons that
// would have used different over-long paths still get distinct sockets.
func clampSocketPath(p string) string {
	if len(p) < socketAddrMax() {
		return p
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(p))
	return filepath.Join(os.TempDir(), fmt.Sprintf("gx-%08x.sock", h.Sum32()))
}

// PIDFilePath returns the path of the daemon PID file. The daemon writes
// this on startup and removes it on graceful shutdown. Staleness detection
// (for crashed daemons that never removed their PID) is a process-liveness
// probe — see platform.ProcessAlive.
func PIDFilePath() string {
	if override := os.Getenv("GORTEX_DAEMON_PIDFILE"); override != "" {
		return override
	}
	if dir, ok := stateDir(); ok {
		return filepath.Join(dir, "daemon.pid")
	}
	return filepath.Join(os.TempDir(), "gortex-daemon.pid")
}

// LogFilePath returns the daemon's rotating JSON log path.
// Interactive foreground mode uses stderr instead.
func LogFilePath() string {
	if override := os.Getenv("GORTEX_DAEMON_LOGFILE"); override != "" {
		return override
	}
	if dir, ok := stateDir(); ok {
		return filepath.Join(dir, "daemon.log")
	}
	return filepath.Join(os.TempDir(), "gortex-daemon.log")
}

// StderrLogFilePath holds crashes and other output outside the daemon logger.
// Keeping inherited descriptors separate lets the daemon rotate LogFilePath.
func StderrLogFilePath() string {
	return StderrLogPath(LogFilePath())
}

// StderrLogPath derives the raw stderr file beside a daemon's JSON log.
func StderrLogPath(logPath string) string {
	ext := filepath.Ext(logPath)
	return logPath[:len(logPath)-len(ext)] + ".stderr" + ext
}

// StateDir returns the directory the daemon keeps its runtime state in —
// the socket, PID file, logs, and any auxiliary journals a subsystem needs
// to survive a restart. It resolves the same way SocketPath / PIDFilePath
// do, and falls back to the temp dir when neither the cache nor the home
// directory can be resolved, so callers always get a usable directory.
//
// The directory is not created; use EnsureParentDir on the file you are
// about to write, or os.MkdirAll on a subdirectory of your own.
func StateDir() string {
	if dir, ok := stateDir(); ok {
		return dir
	}
	return os.TempDir()
}

// EnsureParentDir creates the parent directory of path with permissions
// 0o700 (user only). Daemon state files live under the user's cache dir
// and should not be world-readable. The mode is advisory on Windows,
// where filesystem ACLs already scope %USERPROFILE% to the user.
func EnsureParentDir(path string) error {
	dir := filepath.Dir(path)
	return os.MkdirAll(dir, 0o700)
}
