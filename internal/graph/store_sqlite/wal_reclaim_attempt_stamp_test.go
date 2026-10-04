package store_sqlite

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Every reclaim attempt that ran gets a line with its stamp, whatever its
// outcome: one stopped by an edit cycle after it had started copying reports
// its time, CPU and the wal-index before and after (attempted ahead of
// backfilled when the pass was cut short). Refusals that did nothing stay in
// the per-minute summary, which reports the log's real size.
func TestWALReclaimLogsEveryAttemptThatRan(t *testing.T) {
	var out bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(previous) })

	started := time.Date(2026, 9, 27, 11, 19, 20, 0, time.UTC)
	var skips walReclaimSkipLog
	now := started.Add(time.Hour)

	abandoned := walReclaimResult{
		outcome: walReclaimSkipped, reason: "build_lane_busy", began: true,
		bytesBefore: 2 << 30, started: started, elapsed: 21 * time.Second, processCPU: 13 * time.Second,
		laneAtStart: false, laneAtEnd: true,
		framesBefore: walIndexSnapshot{MxFrame: 560000, NBackfill: 120}, framesAfter: walIndexSnapshot{MxFrame: 561000, NBackfill: 120, NBackfillAttempted: 560000},
		framesKnown: true,
	}
	logWALReclaimOutcome(abandoned, 0, &skips, now)
	line := out.String()
	require.Contains(t, line, `wal reclaim abandoned reason="build_lane_busy" wal_bytes=2147483648`)
	require.Contains(t, line, "started=11:19:20.000 elapsed=21s process_cpu=13s lane_busy_start=false lane_busy_end=true")
	require.Contains(t, line, "wal_frames=560000 backfilled_before=120 backfilled_after=120 backfill_attempted_after=560000")
	require.Equal(t, 0, skips.count, "an attempt that ran is not folded into the refusal summary")

	out.Reset()
	refused := walReclaimResult{outcome: walReclaimSkipped, reason: "build_lane_busy", bytesBefore: 3 << 30, started: started}
	logWALReclaimOutcome(refused, 0, &skips, now)
	require.Equal(t, 0, skips.count, "the summary was due, so it was written and reset")
	require.True(t, strings.Contains(out.String(), "wal reclaim skipped n=1") && strings.Contains(out.String(), "wal_bytes=3221225472"),
		"the refusal summary reports the log's size: %q", out.String())
	require.NotContains(t, out.String(), "abandoned")
}

// A refusal measures the log itself, so the summary never reads 0 for a log
// that is gigabytes long.
func TestWALReclaimRefusalReportsTheLogSize(t *testing.T) {
	store := openPayloadStore(t)
	store.SetBuildLaneBusy(func() bool { return true })
	walPath := store.dbPath + "-wal"
	_, err := store.writerDB.Exec(`CREATE TABLE attempt_stamp_probe (v BLOB)`)
	require.NoError(t, err)
	_, err = store.writerDB.Exec(`INSERT INTO attempt_stamp_probe VALUES (randomblob(65536))`)
	require.NoError(t, err)
	cfg := walReclaimConfig{thresholdBytes: 1}
	res := store.reclaimWALOnce(cfg, store.writerDB, walPath)
	require.Equal(t, walReclaimSkipped, res.outcome)
	require.Equal(t, "build_lane_busy", res.reason)
	require.False(t, res.began)
	require.Equal(t, walFileSize(walPath), res.bytesBefore)
	require.Positive(t, res.bytesBefore)
	require.True(t, res.framesKnown, "every attempt carries the wal-index before and after")
	require.Positive(t, res.framesBefore.MxFrame)
	require.Contains(t, res.stampSuffix(), "wal_frames=")
}
