package store_sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	sqlite "modernc.org/sqlite"
)

// Keep the old length() implementation local to the fixture's driver. A
// process-global function override would silently change every other test.
type legacyNameLengthConnector struct {
	driver *sqlite.Driver
	path   string
}

func (c legacyNameLengthConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.path)
}

func (c legacyNameLengthConnector) Driver() driver.Driver { return c.driver }

func TestNameCandidateSQLite354UpgradeRepairsPersistedLengthKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sqlite353.sqlite")
	s, err := Open(path)
	require.NoError(t, err)
	s.AddNode(&graph.Node{ID: "repo/f.go::raw", Name: "placeholder", Kind: graph.KindFunction, FilePath: "repo/f.go", RepoPrefix: "repo"})
	require.NoError(t, s.Close())

	d := &sqlite.Driver{}
	require.NoError(t, d.RegisterFunction("length", &sqlite.FunctionImpl{
		NArgs: 1, Deterministic: true,
		Scalar: func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			text := args[0].(string)
			var n int64
			for i := 0; i < len(text) && text[i] != 0; n++ {
				lead := text[i]
				i++
				if lead >= 0xc0 {
					for i < len(text) && text[i]&0xc0 == 0x80 {
						i++
					}
				}
			}
			return n, nil
		},
	}))
	legacy := sql.OpenDB(legacyNameLengthConnector{driver: d, path: path})
	t.Cleanup(func() { _ = legacy.Close() })
	_, err = legacy.Exec(`UPDATE nodes SET name=CAST(? AS TEXT) WHERE id='repo/f.go::raw'`, []byte{0x80, 0x80})
	require.NoError(t, err)
	_, err = legacy.Exec(`REINDEX nodes_name_candidates; PRAGMA user_version=39`)
	require.NoError(t, err)
	require.NoError(t, legacy.Close())

	// Prove that this is a stale expression key with the CURRENT index DDL,
	// not a missing-index fixture that the ordinary shape repair would fix.
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	var stored, computed int
	require.NoError(t, raw.QueryRow(`SELECT length(name), length(name || '') FROM nodes INDEXED BY nodes_name_candidates WHERE name <> '' AND id='repo/f.go::raw'`).Scan(&stored, &computed))
	require.Equal(t, 2, stored)
	require.Equal(t, 1, computed)
	var integrity string
	require.NoError(t, raw.QueryRow(`PRAGMA integrity_check`).Scan(&integrity))
	require.Contains(t, integrity, "nodes_name_candidates")
	require.NoError(t, raw.Close())

	s, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity))
	require.Equal(t, "ok", integrity)
	var version int
	require.NoError(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	require.Equal(t, currentSchemaVersion, version)
	var name string
	require.NoError(t, s.db.QueryRow(`SELECT name FROM nodes WHERE id='repo/f.go::raw'`).Scan(&name))
	require.Equal(t, "\x80\x80", name, "migration must preserve the raw stored payload")
	got, err := s.FindNodesByNameContainingFilteredContext(t.Context(), name, 1, graph.NameSearchFilter{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "repo/f.go::raw", got[0].ID)
}

func TestNameCandidateSQLite354UpgradeSkipsCurrentKeys(t *testing.T) {
	s := openNodeSearchKeyTestStore(t)
	s.AddNode(&graph.Node{ID: "repo/f.go::unicode", Name: "日本語", Kind: graph.KindFunction, FilePath: "repo/f.go", RepoPrefix: "repo"})
	conn, err := s.writerDB.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.Raw(func(dc any) error {
		return dc.(sqlite.AuthorizerRegisterer).RegisterAuthorizer(func(action sqlite.AuthorizerActionCode, _, _, _, _ string) sqlite.AuthorizerReturnCode {
			if action == sqlite.AuthReindex {
				return sqlite.AuthorizerDeny
			}
			return sqlite.AuthorizerOK
		})
	}))
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, reconcileNameCandidateLengths(tx), "current keys must not pay for REINDEX")
	require.NoError(t, tx.Commit())
}
