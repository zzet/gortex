package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// RuntimeState is what a running daemon records about the choices it resolved
// at startup, for the out-of-band CLI commands that have to read the same
// files it does. The PID file says only that a daemon exists; a command like
// `gortex repos` reads the daemon's store directly and needs to know WHICH
// store, which is otherwise knowable only from the argv of a process it cannot
// see. Everything here is a resolved absolute value, never a flag as typed.
// StartupPhase is the daemon's pre-socket lifecycle state. The real socket is
// still the authoritative ready signal; these phases only explain why a live
// child has not opened it yet.
type StartupPhase string

const (
	StartupOpeningStore StartupPhase = "opening_store"
	StartupMigrating    StartupPhase = "migrating"
	StartupServing      StartupPhase = "serving"
	StartupFailed       StartupPhase = "failed"
)

type RuntimeState struct {
	// PID is the daemon process that wrote this file. Readers use it to tell
	// a live record from one a killed daemon left behind.
	PID int `json:"pid"`
	// BackendPath is the resolved graph store file the daemon opened —
	// --backend-path expanded to an absolute path, or the platform default
	// when the flag was not given.
	BackendPath string `json:"backend_path"`
	// Startup fields are optional for backward compatibility with runtime
	// records written by older binaries.
	StartupPhase     StartupPhase `json:"startup_phase,omitempty"`
	StartupStartedAt int64        `json:"startup_started_at,omitempty"`
	StartupUpdatedAt int64        `json:"startup_updated_at,omitempty"`
	MigrationVersion int          `json:"migration_version,omitempty"`
	MigrationName    string       `json:"migration_name,omitempty"`
	StartupError     string       `json:"startup_error,omitempty"`
}

// StartupProgressFresh reports whether st is a live pre-socket progress record
// updated recently enough for a supervising CLI to keep waiting.
func (st RuntimeState) StartupProgressFresh(now time.Time, maxAge time.Duration) bool {
	if st.StartupPhase != StartupOpeningStore && st.StartupPhase != StartupMigrating {
		return false
	}
	if st.StartupUpdatedAt <= 0 || maxAge <= 0 {
		return false
	}
	updated := time.UnixMilli(st.StartupUpdatedAt)
	return !updated.After(now.Add(maxAge)) && now.Sub(updated) <= maxAge
}

// RuntimeStatePath returns the file the daemon records its resolved runtime
// state in. It sits beside the PID file so the two share a lifetime, and is
// overridable for tests and custom deployments.
func RuntimeStatePath() string {
	if override := os.Getenv("GORTEX_DAEMON_STATEFILE"); override != "" {
		return override
	}
	if dir, ok := stateDir(); ok {
		return filepath.Join(dir, "daemon.state.json")
	}
	return filepath.Join(os.TempDir(), "gortex-daemon.state.json")
}

// WriteRuntimeState records st for the lifetime of this daemon. The PID is
// stamped from the calling process, so a caller cannot publish a record that
// claims to belong to some other daemon.
func WriteRuntimeState(st RuntimeState) error {
	st.PID = os.Getpid()
	st.StartupUpdatedAt = time.Now().UnixMilli()
	path := RuntimeStatePath()
	if err := EnsureParentDir(path); err != nil {
		return err
	}
	blob, err := json.Marshal(st)
	if err != nil {
		return err
	}
	// Heartbeats and migration callbacks can update this file while the
	// detached parent reads it. Publish by same-directory atomic replacement so
	// a reader never observes a truncated JSON document.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon-state-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(blob); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// RemoveRuntimeState deletes the record. Called on the shutdown path alongside
// the PID file; an already-absent file is not an error.
func RemoveRuntimeState() {
	_ = os.Remove(RuntimeStatePath())
}

// ReadRuntimeState returns the running daemon's recorded state. It reports
// false when there is no record, when the record cannot be parsed, or when the
// process that wrote it is gone — a record a killed daemon left behind names a
// store nothing is using, and routing a reader there is worse than falling
// back to the default.
func ReadRuntimeState() (RuntimeState, bool) {
	var st RuntimeState
	blob, err := os.ReadFile(RuntimeStatePath())
	if err != nil {
		return RuntimeState{}, false
	}
	if err := json.Unmarshal(blob, &st); err != nil {
		return RuntimeState{}, false
	}
	if st.PID <= 0 || !platform.ProcessAlive(st.PID) {
		return RuntimeState{}, false
	}
	return st, true
}
