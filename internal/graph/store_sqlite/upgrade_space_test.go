package store_sqlite

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// seedOlderStore writes a populated current-schema store and stamps it back
// to version, so the next Open runs every in-place step after it. Steps are
// idempotent, which is what makes the re-stamped store a faithful upgrade
// candidate.
func seedOlderStore(t *testing.T, version, nodes int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	require.NoError(t, err)
	batch := make([]*graph.Node, 0, nodes)
	edges := make([]*graph.Edge, 0, nodes)
	for i := range nodes {
		batch = append(batch, &graph.Node{
			ID: fmt.Sprintf("repo/pkg/file%d.go::Func%d", i%50, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("Func%d", i), FilePath: fmt.Sprintf("pkg/file%d.go", i%50), RepoPrefix: "repo",
		})
		if i > 0 {
			edges = append(edges, &graph.Edge{
				From: fmt.Sprintf("repo/pkg/file%d.go::Func%d", i%50, i), To: fmt.Sprintf("repo/pkg/file%d.go::Func%d", (i-1)%50, i-1),
				Kind: graph.EdgeCalls,
			})
		}
	}
	require.NoError(t, s.AddBatchChecked(batch, edges))
	require.NoError(t, s.Close())
	withRawDB(t, path, func(db *sql.DB) { require.NoError(t, setUserVersion(db, version)) })
	return path
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	require.NoError(t, err)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func rawUserVersion(t *testing.T, path string) int {
	t.Helper()
	var version int
	withRawDB(t, path, func(db *sql.DB) {
		var err error
		version, err = readUserVersion(db)
		require.NoError(t, err)
	})
	return version
}

func fixedDiskAvail(n uint64) func(string) (uint64, error) {
	return func(string) (uint64, error) { return n, nil }
}

func TestUpgradeFreeSpaceRefusalLeavesStoreByteIdentical(t *testing.T) {
	path := seedOlderStore(t, 40, 400)
	before := fileDigest(t, path)
	const available = 12345

	s, err := Open(path, WithRebuild(), withDiskAvail(fixedDiskAvail(available)))
	require.Nil(t, s)
	require.ErrorIs(t, err, ErrInsufficientUpgradeSpace)
	var short *InsufficientUpgradeSpaceError
	require.ErrorAs(t, err, &short)
	require.Equal(t, path, short.Path)
	require.Equal(t, 40, short.From)
	require.Equal(t, currentSchemaVersion, short.To)
	require.Equal(t, uint64(available), short.Available)
	require.Greater(t, short.Need, short.Available)

	msg := err.Error()
	require.Contains(t, msg, path, "the refusal names the store")
	require.Contains(t, msg, strconv.FormatUint(short.Need, 10)+" bytes", "the refusal names the bytes needed")
	require.Contains(t, msg, strconv.FormatUint(available, 10)+" bytes", "the refusal names the bytes available")

	require.Equal(t, before, fileDigest(t, path), "a refused upgrade must not touch the store file")
	require.Equal(t, 40, rawUserVersion(t, path))
}

func TestUpgradeFreeSpaceEstimateScalesWithPendingSteps(t *testing.T) {
	noop := func(*sql.Tx) error { return nil }
	additive := func(version int) schemaMigration {
		return schemaMigration{version: version, inPlace: noop, preserveAnalysis: true}
	}
	reshaping := schemaMigration{version: 26, inPlace: noop}
	stats := storeSpaceStats{pageSize: 4096, pageCount: 10_000}

	one := estimateUpgradeSpace(stats, []schemaMigration{additive(41)})
	written := (stats.livePages()*upgradeAdditiveStepPercent + 99) / 100
	require.Equal(t, walFileHeaderBytes+written*(stats.pageSize+walFrameHeaderBytes), one.WALPeak)
	require.Equal(t, written*stats.pageSize, one.Growth, "an empty free list absorbs nothing")
	require.Equal(t, uint64(upgradeSpaceHeadroom), one.Headroom)
	require.Equal(t, one.WALPeak+one.Growth+one.Headroom, one.Need)

	three := estimateUpgradeSpace(stats, []schemaMigration{additive(41), additive(42), additive(43)})
	full := estimateUpgradeSpace(stats, []schemaMigration{reshaping})
	require.Less(t, one.Need, three.Need, "each additive step adds its share")
	require.LessOrEqual(t, three.Need, full.Need)
	require.Equal(t, full, estimateUpgradeSpace(stats, []schemaMigration{additive(25), reshaping}),
		"one reshaping step makes the whole transition a rewrite")
	many := make([]schemaMigration, 0, 10)
	for v := range 10 {
		many = append(many, additive(50+v))
	}
	require.Equal(t, full, estimateUpgradeSpace(stats, many), "additive shares are capped at a full rewrite")

	// Free pages absorb main-file growth but not WAL frames.
	freed := stats
	freed.freelistPages = 4_000
	withFree := estimateUpgradeSpace(freed, []schemaMigration{reshaping})
	live := freed.livePages()
	require.Equal(t, walFileHeaderBytes+live*(freed.pageSize+walFrameHeaderBytes), withFree.WALPeak)
	require.Equal(t, (live-freed.freelistPages)*freed.pageSize, withFree.Growth)
	mostlyFree := stats
	mostlyFree.freelistPages = 8_000
	require.Zero(t, estimateUpgradeSpace(mostlyFree, []schemaMigration{reshaping}).Growth)

	// The estimate grows with the live store.
	bigger := stats
	bigger.pageCount *= 2
	require.Less(t, full.Need, estimateUpgradeSpace(bigger, []schemaMigration{reshaping}).Need)
	require.Equal(t, uint64(upgradeSpaceHeadroom), estimateUpgradeSpace(storeSpaceStats{pageSize: 4096}, []schemaMigration{reshaping}).Need)
}

func TestUpgradeFreeSpaceCheckSkipsFreshMemoryAndCurrentStores(t *testing.T) {
	var calls atomic.Int32
	noSpace := withDiskAvail(func(string) (uint64, error) {
		calls.Add(1)
		return 0, nil
	})

	fresh := filepath.Join(t.TempDir(), "fresh.sqlite")
	s, err := Open(fresh, noSpace)
	require.NoError(t, err, "a new store has nothing to upgrade")
	require.NoError(t, s.Close())

	s, err = Open(fresh, noSpace)
	require.NoError(t, err, "a current store has nothing to upgrade")
	require.NoError(t, s.Close())

	s, err = Open(":memory:", noSpace)
	require.NoError(t, err, "an in-memory store has no volume")
	require.NoError(t, s.Close())

	require.Zero(t, calls.Load(), "no pending upgrade means no free-space probe")
}

func TestUpgradeFreeSpaceCheckLetsRoomyOrUnmeasuredVolumesProceed(t *testing.T) {
	for name, avail := range map[string]func(string) (uint64, error){
		"fits":       fixedDiskAvail(1 << 50),
		"unmeasured": func(string) (uint64, error) { return 0, errors.New("statfs: not supported") },
	} {
		t.Run(name, func(t *testing.T) {
			path := seedOlderStore(t, 40, 50)
			var probed string
			s, err := Open(path, withDiskAvail(func(dir string) (uint64, error) {
				probed = dir
				return avail(dir)
			}))
			require.NoError(t, err)
			require.NoError(t, s.Close())
			require.Equal(t, filepath.Dir(path), probed, "free space is read for the store's own directory")
			require.Equal(t, currentSchemaVersion, rawUserVersion(t, path))
		})
	}
}
