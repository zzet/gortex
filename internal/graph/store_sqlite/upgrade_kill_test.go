package store_sqlite

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Crash tests for the schema transition.
//
// A helper process (this test binary re-run on TestHelperUpgradeKillChild)
// opens a copy of an old store the way the daemon does and stops at one
// upgrade point; the parent SIGKILLs it there, then reopens the store and
// requires the result to equal an uninterrupted upgrade of the same store.

func TestUpgradeKillPointSeamsFireInOrder(t *testing.T) {
	path := seedOlderStore(t, 40, 50)
	var points []string
	s, err := Open(path, WithRebuild(), WithUpgradeBackup(), withDiskAvail(fixedDiskAvail(1<<50)),
		withUpgradeHook(func(point upgradePoint, version int) {
			points = append(points, fmt.Sprintf("%s@%d", point, version))
		}))
	require.NoError(t, err)
	require.NoError(t, s.Close())

	want := []string{fmt.Sprintf("%s@40", upgradeBackupCopied)}
	for v := 41; v <= currentSchemaVersion; v++ {
		want = append(want, fmt.Sprintf("%s@%d", upgradeInStep, v))
	}
	want = append(want,
		fmt.Sprintf("%s@%d", upgradeIndexRecreation, currentSchemaVersion),
		fmt.Sprintf("%s@%d", upgradeBeforeStamp, currentSchemaVersion))
	require.Equal(t, want, points)

	// A current store crosses no boundary at all.
	points = nil
	s, err = Open(path, withUpgradeHook(func(point upgradePoint, version int) {
		points = append(points, fmt.Sprintf("%s@%d", point, version))
	}))
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.Empty(t, points)
}

const (
	upgradeKillStoreEnv   = "GORTEX_TEST_UPGRADE_KILL_STORE"
	upgradeKillPointEnv   = "GORTEX_TEST_UPGRADE_KILL_POINT"
	upgradeKillVersionEnv = "GORTEX_TEST_UPGRADE_KILL_VERSION"
	// upgradeExternalStoreEnv runs the same matrix on a copy of a real store
	// (for example one an older release wrote for a large corpus). Local
	// validation only; CI uses the committed fixtures.
	upgradeExternalStoreEnv = "GORTEX_TEST_UPGRADE_STORE"
	upgradeKillReadyLine    = "UPGRADE-KILL-POINT"
)

// upgradeDaemonOptions are the options the daemon opens its store with,
// minus the volume probe: the crash tests are about recovery, not space.
func upgradeDaemonOptions(extra ...Option) []Option {
	return append([]Option{WithRebuild(), WithUpgradeBackup(), withDiskAvail(fixedDiskAvail(1 << 50))}, extra...)
}

// TestHelperUpgradeKillChild is the helper process: it upgrades the store
// named in the environment and parks at the requested point until killed.
func TestHelperUpgradeKillChild(t *testing.T) {
	store := os.Getenv(upgradeKillStoreEnv)
	if store == "" {
		t.Skip("helper process for TestUpgradeKillPointsRecoverEveryFixture")
	}
	point := upgradePoint(os.Getenv(upgradeKillPointEnv))
	version, err := strconv.Atoi(os.Getenv(upgradeKillVersionEnv))
	require.NoError(t, err)
	s, err := Open(store, upgradeDaemonOptions(withUpgradeHook(func(at upgradePoint, v int) {
		if at != point || v != version {
			return
		}
		fmt.Printf("%s %s %d\n", upgradeKillReadyLine, at, v)
		_ = os.Stdout.Sync()
		for {
			time.Sleep(time.Hour) // the parent kills the process here
		}
	}))...)
	if s != nil {
		_ = s.Close()
	}
	t.Fatalf("upgrade finished without reaching %s@%d (err=%v)", point, version, err)
}

// upgradeKillPoint is one place the matrix kills the helper.
type upgradeKillPoint struct {
	point   upgradePoint
	version int
	label   string
}

// upgradeKillPoints lists where an upgrade from stored is killed: after
// the backup copy, inside the first, middle and last pending step,
// during index recreation and just before the stamp.
func upgradeKillPoints(stored int) []upgradeKillPoint {
	pending := pendingBetween(stored, currentSchemaVersion, schemaMigrations)
	if len(pending) == 0 {
		return nil
	}
	points := []upgradeKillPoint{{upgradeBackupCopied, stored, "backup_copied"}}
	seen := map[int]bool{}
	for _, pick := range []struct {
		label string
		index int
	}{{"first_step", 0}, {"middle_step", len(pending) / 2}, {"last_step", len(pending) - 1}} {
		v := pending[pick.index].version
		if seen[v] {
			continue
		}
		seen[v] = true
		points = append(points, upgradeKillPoint{upgradeInStep, v, fmt.Sprintf("%s_v%d", pick.label, v)})
	}
	return append(points,
		upgradeKillPoint{upgradeIndexRecreation, currentSchemaVersion, "index_recreation"},
		upgradeKillPoint{upgradeBeforeStamp, currentSchemaVersion, "before_stamp"})
}

// upgradeOutcome is what an upgraded store must agree on with an
// uninterrupted upgrade of the same starting store.
type upgradeOutcome struct {
	version   int
	integrity string
	nodes     int
	edges     int
	digest    string
}

func readUpgradeOutcome(t testing.TB, path string) upgradeOutcome {
	t.Helper()
	db, err := openSQLiteStoreDB(sqliteDSN(path, "mode=ro"))
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	var out upgradeOutcome
	require.NoError(t, db.QueryRow("PRAGMA user_version").Scan(&out.version))
	require.NoError(t, db.QueryRow("PRAGMA integrity_check").Scan(&out.integrity))
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&out.nodes))
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM edges").Scan(&out.edges))
	h := sha256.New()
	for _, q := range []string{
		`SELECT id, view_gen, kind FROM nodes ORDER BY id, view_gen`,
		`SELECT from_id, to_id, kind, view_gen, file_path, line FROM edges ORDER BY from_id, to_id, kind, view_gen, file_path, line`,
	} {
		digestRows(t, db, q, h)
	}
	out.digest = fmt.Sprintf("%x", h.Sum(nil))
	return out
}

func digestRows(t testing.TB, db *sql.DB, query string, w io.Writer) {
	t.Helper()
	rows, err := db.Query(query)
	require.NoError(t, err)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	for rows.Next() {
		require.NoError(t, rows.Scan(ptrs...))
		fmt.Fprintln(w, values...)
	}
	require.NoError(t, rows.Err())
}

// copyUpgradeStore copies a store with its WAL into dir, as a stopped
// daemon's disk would hold it.
func copyUpgradeStore(t testing.TB, src, dir string) string {
	t.Helper()
	dst := filepath.Join(dir, "store.sqlite")
	for _, suffix := range []string{"", "-wal"} {
		in, err := os.Open(src + suffix)
		if suffix != "" && os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		out, err := os.Create(dst + suffix)
		require.NoError(t, err)
		_, err = io.Copy(out, in)
		require.NoError(t, err)
		require.NoError(t, out.Close())
		require.NoError(t, in.Close())
	}
	return dst
}

// upgradeKillWait bounds how long the helper may take to reach a point.
func upgradeKillWait() time.Duration {
	if raceDetectorOn {
		return 10 * time.Minute
	}
	return 3 * time.Minute
}

// killUpgradeAt runs the helper on store until it parks at kp, then SIGKILLs
// it (TerminateProcess on Windows).
func killUpgradeAt(t *testing.T, store string, kp upgradeKillPoint) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperUpgradeKillChild$")
	cmd.Env = append(os.Environ(),
		upgradeKillStoreEnv+"="+store,
		upgradeKillPointEnv+"="+string(kp.point),
		upgradeKillVersionEnv+"="+strconv.Itoa(kp.version))
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	transcript := make(chan string, 1)
	go func() {
		var seen strings.Builder
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			seen.WriteString(line + "\n")
			if strings.HasPrefix(line, upgradeKillReadyLine+" ") {
				ready <- line
			}
		}
		transcript <- seen.String()
		close(ready)
	}()
	select {
	case line, ok := <-ready:
		if !ok {
			_ = cmd.Wait()
			exited = true
			t.Fatalf("helper exited before %s:\n%s\nstderr:\n%s", kp.label, <-transcript, stderr.String())
		}
		require.Equal(t, fmt.Sprintf("%s %s %d", upgradeKillReadyLine, kp.point, kp.version), line)
	case <-time.After(upgradeKillWait()):
		t.Fatalf("helper did not reach %s within %s", kp.label, upgradeKillWait())
	}
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait() // a killed process reports its signal, not success
	exited = true
}

// upgradeKillSource is one starting store the matrix upgrades.
type upgradeKillSource struct {
	name    string
	path    string
	version int
}

// upgradeSources builds a store from every committed fixture, plus a copy of
// the real store named by upgradeExternalStoreEnv when it is set.
func upgradeSources(t *testing.T) []upgradeKillSource {
	t.Helper()
	scale := upgradeFixtureGortexScale
	if testing.Short() || raceDetectorOn {
		scale = upgradeFixtureScale{nodes: scale.nodes / 10, edges: scale.edges / 10}
	}
	var sources []upgradeKillSource
	for _, fixture := range loadUpgradeSchemaFixtures(t) {
		start := time.Now()
		path := buildUpgradeFixtureStore(t, fixture, scale, t.TempDir())
		t.Logf("fixture v%d: %d nodes, %d edges built in %s", fixture.version, scale.nodes, scale.edges, time.Since(start).Round(time.Millisecond))
		sources = append(sources, upgradeKillSource{name: fmt.Sprintf("fixture_v%d", fixture.version), path: path, version: fixture.version})
	}
	if external := os.Getenv(upgradeExternalStoreEnv); external != "" {
		path := copyUpgradeStore(t, external, t.TempDir())
		probe, err := probeStoreSchema(path)
		require.NoError(t, err)
		sources = append(sources, upgradeKillSource{name: fmt.Sprintf("external_v%d", probe.stored), path: path, version: probe.stored})
	}
	return sources
}

func TestUpgradeKillPointsRecoverEveryFixture(t *testing.T) {
	for _, source := range upgradeSources(t) {
		t.Run(source.name, func(t *testing.T) {
			before := readUpgradeOutcome(t, source.path)
			points := upgradeKillPoints(source.version)
			if len(points) == 0 {
				// The current schema: nothing to interrupt until the next step
				// is appended, but opening it must stay a no-op.
				store := copyUpgradeStore(t, source.path, t.TempDir())
				var events migrationEvents
				s, err := Open(store, upgradeDaemonOptions(WithMigrationObserver(events.observe))...)
				require.NoError(t, err)
				require.NoError(t, s.Close())
				require.Empty(t, events.phases())
				require.Equal(t, before, readUpgradeOutcome(t, store))
				return
			}

			baselineStore := copyUpgradeStore(t, source.path, t.TempDir())
			start := time.Now()
			s, err := Open(baselineStore, upgradeDaemonOptions()...)
			require.NoError(t, err)
			require.NoError(t, s.Close())
			t.Logf("uninterrupted upgrade v%d -> v%d: %s", source.version, currentSchemaVersion, time.Since(start).Round(time.Millisecond))
			want := readUpgradeOutcome(t, baselineStore)
			require.Equal(t, currentSchemaVersion, want.version)
			require.Equal(t, "ok", want.integrity)

			for _, kp := range points {
				t.Run(kp.label, func(t *testing.T) {
					store := copyUpgradeStore(t, source.path, t.TempDir())
					killUpgradeAt(t, store, kp)

					probe, err := probeStoreSchema(store)
					require.NoError(t, err)
					require.Equal(t, source.version, probe.stored, "a kill before the stamp leaves the old version")

					s, err := Open(store, upgradeDaemonOptions()...)
					require.NoError(t, err, "the restart completes the upgrade")
					require.NoError(t, s.Close())
					require.Equal(t, want, readUpgradeOutcome(t, store), "the restarted upgrade equals an uninterrupted one")

					// The backup a killed attempt finalized is only kept by the
					// restart; one killed before its rename is taken again, so
					// that is the point to prove the snapshot complete.
					backup := UpgradeBackupPath(store, source.version)
					require.FileExists(t, backup)
					if kp.point == upgradeBackupCopied {
						require.Equal(t, before, readUpgradeOutcome(t, backup), "the backup is the store as it was before the first attempt")
					}
					requireNoPartialBackup(t, store)
				})
			}
		})
	}
}
