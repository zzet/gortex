package serverstack

import (
	"os"
	"strconv"
	"time"

	"go.uber.org/zap"
)

// Periodic snapshot triggers + the epoch sidecar for the memory backend
// (issue davalillo/gortex#23). The epoch is the snapshot generation: the
// sidecar carries it beside the snapshot, journal_open header lines carry it
// inside the journal, and replay pairs the two (see graph.MemoryStore).

// memoryEpochPath sits beside the snapshot: <snapshot>.epoch.
func memoryEpochPath(snapshotPath string) string {
	return snapshotPath + ".epoch"
}

// readJournalEpoch returns the snapshot generation the sidecar records, 0 on
// a missing or unreadable sidecar (a fresh store, or one whose sidecar write
// never landed — replay treats a too-old journal as the safe direction).
func readJournalEpoch(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(string(data), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// writeJournalEpoch publishes the generation atomically (tmp + rename) so a
// crash leaves either the old or the new value, never a torn one.
func writeJournalEpoch(path string, epoch uint64) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(epoch, 10)), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// snapshotTriggerConfig is the periodic-dump trigger set. All knobs come
// from the environment; zero values disable the corresponding trigger.
type snapshotTriggerConfig struct {
	// interval: dump at least every N (GORTEX_SNAPSHOT_INTERVAL). Bounds the
	// crash loss window. 0 = off.
	interval time.Duration
	// journalBytes: dump once the journal byte counter crosses this
	// (GORTEX_SNAPSHOT_JOURNAL_BYTES). Bounds crash replay cost. 0 = off.
	journalBytes int64
	// tick: how often the janitor checks (GORTEX_SNAPSHOT_TICK).
	tick time.Duration
}

func (c *snapshotTriggerConfig) loadFromEnv(logger *zap.Logger) {
	c.interval = durationEnv("GORTEX_SNAPSHOT_INTERVAL", 0, logger)
	c.journalBytes = int64Env("GORTEX_SNAPSHOT_JOURNAL_BYTES", 512<<20, logger)
	c.tick = durationEnv("GORTEX_SNAPSHOT_TICK", 30*time.Second, logger)
	if c.tick <= 0 {
		c.tick = 30 * time.Second
	}
}

// triggerReason names the trigger that fired, "" for none. Interval wins
// over journal size so a long-idle daemon still bounds its loss window.
func (c *snapshotTriggerConfig) triggerReason(sinceLastDump time.Duration, journalBytes int64) string {
	if c.interval > 0 && sinceLastDump >= c.interval {
		return "interval"
	}
	if c.journalBytes > 0 && journalBytes >= c.journalBytes {
		return "journal_size"
	}
	return ""
}

func durationEnv(name string, def time.Duration, logger *zap.Logger) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v < 0 {
		if logger != nil {
			logger.Warn("memory backend: invalid snapshot trigger env, using default",
				zap.String("env", name), zap.String("value", raw), zap.Duration("default", def))
		}
		return def
	}
	return v
}

func int64Env(name string, def int64, logger *zap.Logger) int64 {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		if logger != nil {
			logger.Warn("memory backend: invalid snapshot trigger env, using default",
				zap.String("env", name), zap.String("value", raw), zap.Int64("default", def))
		}
		return def
	}
	return v
}
