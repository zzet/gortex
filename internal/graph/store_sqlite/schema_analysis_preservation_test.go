package store_sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func seedIndexMigrationAnalysis(t *testing.T, version int) (string, graph.AnalysisGenerationHeader) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index-upgrade.db")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "kept-node", Kind: graph.KindFunction, Name: "kept", RepoPrefix: "repo", Meta: map[string]any{"doc": "persisted"}}}, nil))
	buildMinimalAnalysisGeneration(t, s, "kept", 2, true)
	header, found, err := s.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, s.Close())
	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`DROP INDEX nodes_deadcode_eligible_workspace`)
		require.NoError(t, err)
		if version < 41 {
			_, err = db.Exec(`DROP INDEX nodes_by_effective_workspace`)
			require.NoError(t, err)
		}
		require.NoError(t, setUserVersion(db, version))
	})
	return path, header
}

func requireKeptIndexMigrationAnalysis(t *testing.T, s *Store, want graph.AnalysisGenerationHeader) {
	t.Helper()
	header, found, err := s.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	require.True(t, found, "index-only upgrade must retain active analysis")
	var persistedRevision uint64
	require.NoError(t, s.db.QueryRow(`SELECT build_revision FROM analysis_generations WHERE generation_id = ?`, want.GenerationID).Scan(&persistedRevision))
	require.Equal(t, want.GraphRevision, persistedRevision, "index-only upgrade must not rewrite the persisted build revision")
	// The public load receipt intentionally rebases to the current process;
	// the stored build revision above remains the original publication's.
	want.GraphRevision = s.AnalysisMutationRevision()
	require.Equal(t, want, header, "all persisted header fields survive; the returned receipt uses the current process revision")
	blob, found, err := s.LoadAnalysisBlob(header.GenerationID, graph.AnalysisBlobAdjacency)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []byte("adjacency-kept"), blob)
	nodes, err := s.AnalysisNodeMetrics(header.GenerationID, []string{"kept-node"})
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Equal(t, "kept-community", nodes[0].CommunityID)
	require.Equal(t, "persisted", s.GetNode("kept-node").Meta["doc"])
	version, err := readUserVersion(s.db)
	require.NoError(t, err)
	require.Equal(t, currentSchemaVersion, version)
	require.True(t, indexNames(t, s.db)["nodes_by_effective_workspace"])
	require.True(t, indexNames(t, s.db)["nodes_deadcode_eligible_workspace"])
}

func TestIndexOnlyMigrationPreservesActiveAnalysis(t *testing.T) {
	for _, version := range []int{40, 41} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path, header := seedIndexMigrationAnalysis(t, version)
			s, err := Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() })
			requireKeptIndexMigrationAnalysis(t, s, header)
		})
	}
}

func TestMixedIndexMigrationStillInvalidatesAnalysis(t *testing.T) {
	path, header := seedIndexMigrationAnalysis(t, 39)
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, found, err := s.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	require.False(t, found, "default v40 plus preserving v41 is still a conservative mixed transition")
	var state int
	require.NoError(t, s.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, header.GenerationID).Scan(&state))
	require.Equal(t, analysisGenerationStale, state)
	require.Equal(t, "persisted", s.GetNode("kept-node").Meta["doc"])
}

func TestPreservingMigrationDoesNotStampBeforeIndexPostconditions(t *testing.T) {
	path, header := seedIndexMigrationAnalysis(t, 40)
	migrations := append([]schemaMigration(nil), schemaMigrations...)
	for i := range migrations {
		if migrations[i].version == 41 {
			migrations[i].inPlace = func(tx *sql.Tx) error {
				if err := createEffectiveWorkspaceIndex(tx); err != nil {
					return err
				}
				// The step commits, then the mandatory core-index tail must fail. This
				// tests the final completion stamp, not only migration transaction rollback.
				_, err := tx.Exec(`DROP INDEX nodes_by_kind; CREATE VIEW nodes_by_kind AS SELECT 1 AS blocker`)
				return err
			}
		}
	}
	opened, err := openWith(path, 41, migrations, false)
	require.ErrorContains(t, err, "sqlite core index")
	require.Nil(t, opened)
	withRawDB(t, path, func(db *sql.DB) {
		version, err := readUserVersion(db)
		require.NoError(t, err)
		require.Equal(t, 40, version, "postcondition failure must remain retryable")
		var active, ready, index int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM analysis_active_generation WHERE generation_id = ?`, header.GenerationID).Scan(&active))
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM analysis_generations WHERE generation_id = ? AND state = 1`, header.GenerationID).Scan(&ready))
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'nodes_by_effective_workspace'`).Scan(&index))
		require.Equal(t, 1, active)
		require.Equal(t, 1, ready)
		require.Equal(t, 1, index, "actual index migration committed before tail refusal")
		_, err = db.Exec(`DROP VIEW nodes_by_kind`)
		require.NoError(t, err)
	})
	retried, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = retried.Close() })
	requireKeptIndexMigrationAnalysis(t, retried, header)
}

func TestAnalysisPreservationRequiresEveryKnownAppliedStep(t *testing.T) {
	neutral := func(version int) schemaMigration {
		return schemaMigration{version: version, inPlace: func(*sql.Tx) error { return nil }, preserveAnalysis: true}
	}
	cases := []struct {
		name              string
		stored, current   int
		steps             []schemaMigration
		wipe, stamp, want bool
	}{
		{name: "known preserving", stored: 40, current: 41, steps: []schemaMigration{neutral(41)}, stamp: true, want: true},
		{name: "fresh unknown", stored: 0, current: 1, steps: []schemaMigration{neutral(1)}, stamp: true},
		{name: "missing registry step", stored: 39, current: 41, steps: []schemaMigration{neutral(41)}, stamp: true},
		{name: "mixed default", stored: 39, current: 41, steps: []schemaMigration{{version: 40, inPlace: func(*sql.Tx) error { return nil }}, neutral(41)}, stamp: true},
		{name: "rebuild", stored: 40, current: 41, steps: []schemaMigration{neutral(41)}, wipe: true, stamp: true},
		{name: "not applied", stored: 40, current: 41, steps: []schemaMigration{neutral(41)}},
		{name: "empty unknown transition", stored: 40, current: 41, stamp: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := schemaPlan{inPlace: tc.steps, wipe: tc.wipe, stamp: tc.stamp}
			require.Equal(t, tc.want, plan.preservesAnalysis(tc.stored, tc.current))
		})
	}
	require.Error(t, validateSchemaMigrations(2, []schemaMigration{{version: 2, rebuild: true, preserveAnalysis: true}}))
}
