package serverstack

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// clearSnapshotTriggerEnv isolates the process environment for the trigger
// knobs; an empty value means "use the default" to durationEnv/int64Env.
func clearSnapshotTriggerEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"GORTEX_SNAPSHOT_INTERVAL",
		"GORTEX_SNAPSHOT_JOURNAL_BYTES",
		"GORTEX_SNAPSHOT_TICK",
	} {
		t.Setenv(name, "")
	}
}

// TestSnapshotTriggerDefaults pins the designed defaults
// (odd/tasks/23-snapshot-triggers.md): the interval trigger is OFF, the
// journal threshold is 512 MiB and the janitor checks every 30s. It also pins
// the regression behind review zzet/gortex#901 minor: openMemoryBackend used
// to seed `interval: 30 * time.Second` before loadFromEnv, which overwrote it
// — a literal that looked enabled while the trigger stayed off.
func TestSnapshotTriggerDefaults(t *testing.T) {
	clearSnapshotTriggerEnv(t)

	var cfg snapshotTriggerConfig
	cfg.loadFromEnv(nil)

	if cfg.interval != 0 {
		t.Errorf("interval default = %v, want 0 (off): the literal that used to read 30s was dead code", cfg.interval)
	}
	if want := int64(512 << 20); cfg.journalBytes != want {
		t.Errorf("journalBytes default = %d, want %d", cfg.journalBytes, want)
	}
	if want := 30 * time.Second; cfg.tick != want {
		t.Errorf("tick default = %v, want %v", cfg.tick, want)
	}

	// A zero interval must never fire, however long the daemon idles...
	if reason := cfg.triggerReason(time.Hour, 0); reason != "" {
		t.Errorf("triggerReason(1h, no journal) = %q, want \"\" (interval off)", reason)
	}
	// ...while the journal threshold still does.
	if reason := cfg.triggerReason(0, 512<<20); reason != "journal_size" {
		t.Errorf("triggerReason(0, threshold) = %q, want journal_size", reason)
	}
}

// TestSnapshotTriggerEnvOverrides asserts each knob is taken from its
// environment variable, including an explicit 0 disabling the trigger.
func TestSnapshotTriggerEnvOverrides(t *testing.T) {
	clearSnapshotTriggerEnv(t)
	t.Setenv("GORTEX_SNAPSHOT_INTERVAL", "45s")
	t.Setenv("GORTEX_SNAPSHOT_JOURNAL_BYTES", "1024")
	t.Setenv("GORTEX_SNAPSHOT_TICK", "5s")

	var cfg snapshotTriggerConfig
	cfg.loadFromEnv(nil)

	if cfg.interval != 45*time.Second {
		t.Errorf("interval = %v, want 45s", cfg.interval)
	}
	if cfg.journalBytes != 1024 {
		t.Errorf("journalBytes = %d, want 1024", cfg.journalBytes)
	}
	if cfg.tick != 5*time.Second {
		t.Errorf("tick = %v, want 5s", cfg.tick)
	}
	if reason := cfg.triggerReason(45*time.Second, 0); reason != "interval" {
		t.Errorf("triggerReason(45s) = %q, want interval", reason)
	}

	t.Setenv("GORTEX_SNAPSHOT_INTERVAL", "0s")
	cfg.loadFromEnv(nil)
	if cfg.interval != 0 {
		t.Errorf("explicit 0s interval = %v, want 0 (off)", cfg.interval)
	}
}

// TestSnapshotTriggerInvalidEnvFallsBack asserts a malformed knob is ignored
// (with a warning) instead of silently enabling a zero or absurd trigger.
func TestSnapshotTriggerInvalidEnvFallsBack(t *testing.T) {
	clearSnapshotTriggerEnv(t)
	t.Setenv("GORTEX_SNAPSHOT_INTERVAL", "not-a-duration")

	var cfg snapshotTriggerConfig
	cfg.loadFromEnv(zap.NewNop())

	if cfg.interval != 0 {
		t.Errorf("invalid interval = %v, want the 0 default", cfg.interval)
	}
}
