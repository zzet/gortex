package store_sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Upgrade fixtures.
//
// testdata/upgrade/schema-v<N>.sql holds the exact DDL (sqlite_master) of a
// store a released or current build wrote at user_version N. The upgrade
// tests replay it into a fresh file and fill nodes and edges with synthetic
// rows, which gives a store with the old release's real shape at gortex
// scale without committing anyone's indexed code.
//
// To add a fixture, index any repository with the build in question, stop
// its daemon, and run
//
//	GORTEX_TEST_UPGRADE_CAPTURE_FROM=<store.sqlite> \
//	GORTEX_TEST_UPGRADE_CAPTURE_TO=testdata/upgrade/schema-v<N>.sql \
//	GORTEX_TEST_UPGRADE_CAPTURE_SOURCE="<which build wrote the store>" \
//	go test -run '^TestHelperCaptureUpgradeSchema$' ./internal/graph/store_sqlite/
//
// Every fixture older than currentSchemaVersion is upgraded by the kill
// tests automatically; the one at the current version becomes an upgrade
// fixture as soon as the next schema step is appended.

const upgradeFixtureDir = "testdata/upgrade"

// upgradeFixtureScale is how many synthetic rows a fixture store gets. The
// default is about a sixth of the graph the daemon builds for this repository
// today: deep enough for every index rebuild to do real work, small enough
// for the crash matrix to fit a CI budget. Race and -short runs, the Linux CI
// leg among them, build a tenth of it; every kill point is a seam, so a
// smaller store moves no kill off its boundary. GORTEX_TEST_UPGRADE_STORE
// runs the same matrix on a full-size real store.
type upgradeFixtureScale struct {
	nodes, edges int
}

var (
	upgradeFixtureGortexScale = upgradeFixtureScale{nodes: 31_000, edges: 206_000}
	upgradeFixtureSmallScale  = upgradeFixtureScale{nodes: 600, edges: 4_000}
)

type upgradeFixture struct {
	version    int
	file       string
	statements []string
}

var upgradeFixtureVersionLine = regexp.MustCompile(`(?m)^-- user_version: (\d+)$`)

// loadUpgradeSchemaFixtures reads every committed schema fixture, oldest
// first.
func loadUpgradeSchemaFixtures(t testing.TB) []upgradeFixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(upgradeFixtureDir, "schema-v*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no schema fixtures under %s", upgradeFixtureDir)
	fixtures := make([]upgradeFixture, 0, len(files))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		text := strings.ReplaceAll(string(raw), "\r\n", "\n")
		m := upgradeFixtureVersionLine.FindStringSubmatch(text)
		require.NotNil(t, m, "%s has no user_version header", file)
		version, err := strconv.Atoi(m[1])
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("schema-v%d.sql", version), filepath.Base(file), "file name and header disagree")
		fixtures = append(fixtures, upgradeFixture{version: version, file: file, statements: splitUpgradeFixtureSQL(text)})
	}
	slices.SortFunc(fixtures, func(a, b upgradeFixture) int { return a.version - b.version })
	return fixtures
}

// splitUpgradeFixtureSQL splits a fixture into statements. Statements end at
// a line holding only ";", which DDL text from sqlite_master never contains
// (trigger bodies carry their own semicolons mid-line).
func splitUpgradeFixtureSQL(text string) []string {
	var statements []string
	var current []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == ";" {
			if stmt := strings.TrimSpace(strings.Join(current, "\n")); stmt != "" {
				statements = append(statements, stmt)
			}
			current = current[:0]
			continue
		}
		if len(current) == 0 && strings.HasPrefix(line, "--") {
			continue // header comments between statements
		}
		current = append(current, line)
	}
	return statements
}

// TestHelperCaptureUpgradeSchema writes the schema of an existing store as a
// fixture. It is a tool rather than a test and skips unless pointed at a
// store; see the comment at the top of this file.
func TestHelperCaptureUpgradeSchema(t *testing.T) {
	from, to := os.Getenv("GORTEX_TEST_UPGRADE_CAPTURE_FROM"), os.Getenv("GORTEX_TEST_UPGRADE_CAPTURE_TO")
	if from == "" || to == "" {
		t.Skip("set GORTEX_TEST_UPGRADE_CAPTURE_FROM and GORTEX_TEST_UPGRADE_CAPTURE_TO to capture a schema fixture")
	}
	source := os.Getenv("GORTEX_TEST_UPGRADE_CAPTURE_SOURCE")
	require.NotEmpty(t, source, "name the build that wrote the store in GORTEX_TEST_UPGRADE_CAPTURE_SOURCE")
	// Read-only, through plain database/sql: never through Open, which would
	// reconcile the very schema being captured.
	db, err := sql.Open("sqlite", sqliteDSN(from, "mode=ro"))
	require.NoError(t, err)
	defer db.Close()
	version, err := readUserVersion(db)
	require.NoError(t, err)
	require.Positive(t, version)

	shadow := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_list WHERE schema = 'main' AND type = 'shadow'`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		shadow[name] = true
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())

	// Tables before the indexes, views and triggers that depend on them; each
	// group in creation order.
	rows, err = db.Query(`SELECT type, name, sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 WHEN 'view' THEN 2 ELSE 3 END, rowid`)
	require.NoError(t, err)
	var b strings.Builder
	fmt.Fprintf(&b, "-- Schema of a gortex graph store, captured by TestHelperCaptureUpgradeSchema\n")
	fmt.Fprintf(&b, "-- from a store written by %s.\n", source)
	fmt.Fprintf(&b, "-- user_version: %d\n", version)
	objects := map[string]int{}
	for rows.Next() {
		var kind, name, ddl string
		require.NoError(t, rows.Scan(&kind, &name, &ddl))
		if shadow[name] {
			continue // recreated by its virtual table
		}
		objects[kind]++
		fmt.Fprintf(&b, "\n%s\n;\n", strings.TrimSpace(strings.ReplaceAll(ddl, "\r\n", "\n")))
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NoError(t, os.WriteFile(to, []byte(b.String()), 0o644))
	t.Logf("captured v%d: %v", version, objects)
	for _, table := range []string{"nodes", "edges"} {
		var n int
		require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
		t.Logf("source %s rows: %d", table, n)
	}
}

// upgradeFixtureNodeID is the SQL id of synthetic node number x.
func upgradeFixtureNodeID(x string) string {
	return fmt.Sprintf(`'repo' || ((%[1]s) %% 4) || '/pkg' || ((%[1]s) %% 300) || '/f' || ((%[1]s) %% 2700) || '.go::S' || (%[1]s)`, x)
}

// upgradeFixtureViewGen spreads about one row in ten over non-base view
// generations, the way worktree views populate a real store.
const upgradeFixtureViewGen = `CASE WHEN i % 10 = 9 THEN 1 + i % 3 ELSE 0 END`

// upgradeFixtureColumns gives the SQL expression (over the row number i) for
// every synthetic column the loader fills. A column absent here is left to
// its default; a NOT NULL column without one fails the load loudly, so a
// fixture with a new required column cannot silently lose rows.
func upgradeFixtureColumns(table string, scale upgradeFixtureScale) map[string]string {
	n := strconv.Itoa(scale.nodes)
	filePath := `'pkg' || (i % 300) || '/f' || (i % 2700) || '.go'`
	switch table {
	case "nodes":
		return map[string]string{
			"id":          upgradeFixtureNodeID("i"),
			"view_gen":    upgradeFixtureViewGen,
			"kind":        `CASE i % 5 WHEN 0 THEN 'function' WHEN 1 THEN 'method' WHEN 2 THEN 'type' WHEN 3 THEN 'variable' ELSE 'field' END`,
			"name":        `'S' || i`,
			"qual_name":   `'pkg' || (i % 300) || '.S' || i`,
			"file_path":   filePath,
			"start_line":  `i % 400 + 1`,
			"end_line":    `i % 400 + 12`,
			"language":    `'go'`,
			"repo_prefix": `'repo' || (i % 4)`,
			"signature":   `'func S' || i || '(ctx context.Context) error'`,
			"visibility":  `CASE WHEN i % 2 = 0 THEN 'public' ELSE 'private' END`,
			"is_exported": `CASE WHEN i % 2 = 0 THEN 1 ELSE 0 END`,
			"updated_at":  `1761000000 + i`,
		}
	case "edges":
		return map[string]string{
			"from_id": upgradeFixtureNodeID("i % " + n),
			// (from, kind, to) stays unique: rows sharing a source and a kind
			// differ in i / n, which shifts the target.
			"to_id":     upgradeFixtureNodeID("(31 * (i % " + n + ") + i / " + n + " + 1) % " + n),
			"kind":      `CASE (i / ` + n + `) % 6 WHEN 0 THEN 'calls' WHEN 1 THEN 'references' WHEN 2 THEN 'defines' WHEN 3 THEN 'imports' WHEN 4 THEN 'member_of' ELSE 'implements' END`,
			"file_path": filePath,
			"line":      `i % 400 + 1`,
			"origin":    `'ast'`,
			"view_gen":  upgradeFixtureViewGen,
		}
	}
	return nil
}

// buildUpgradeFixtureStore replays fixture into dir/store.sqlite, fills it at
// scale, stamps the fixture's version and leaves it checkpointed in WAL mode,
// as a cleanly stopped daemon of that release would.
func buildUpgradeFixtureStore(t testing.TB, fixture upgradeFixture, scale upgradeFixtureScale, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "store.sqlite")
	db, err := sql.Open("sqlite", sqliteDSN(path, "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"))
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, stmt := range fixture.statements {
		_, err := db.Exec(stmt)
		require.NoError(t, err, "%s: replay %q", fixture.file, stmt)
	}
	for _, fill := range []struct {
		table string
		rows  int
	}{{"nodes", scale.nodes}, {"edges", scale.edges}} {
		insert := upgradeFixtureInsert(t, db, fixture, fill.table, upgradeFixtureColumns(fill.table, scale))
		_, err := db.Exec(insert, fill.rows-1)
		require.NoError(t, err, "%s: fill %s", fixture.file, fill.table)
	}
	_, err = db.Exec(fmt.Sprintf("PRAGMA user_version = %d", fixture.version))
	require.NoError(t, err)
	_, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
	return path
}

// upgradeFixtureInsert builds the INSERT that fills table with synthetic
// rows, from the columns the replayed schema actually has.
func upgradeFixtureInsert(t testing.TB, db *sql.DB, fixture upgradeFixture, table string, rules map[string]string) string {
	t.Helper()
	rows, err := db.Query(`SELECT name, "notnull", dflt_value IS NOT NULL, pk, hidden FROM pragma_table_xinfo(?)`, table)
	require.NoError(t, err)
	defer rows.Close()
	var columns, values []string
	for rows.Next() {
		var name string
		var notNull, hasDefault bool
		var pk, hidden int
		require.NoError(t, rows.Scan(&name, &notNull, &hasDefault, &pk, &hidden))
		if hidden != 0 {
			continue // generated or hidden: SQLite fills it
		}
		if expr, ok := rules[name]; ok {
			columns = append(columns, name)
			values = append(values, expr)
			continue
		}
		integerKey := pk == 1 && name == "id" && table == "edges"
		require.False(t, notNull && !hasDefault && !integerKey,
			"%s: %s.%s is NOT NULL without a default; give the fixture loader a value for it", fixture.file, table, name)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, columns, "%s: no %s table to fill", fixture.file, table)
	return fmt.Sprintf(`WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i < ?)
		INSERT INTO %s (%s) SELECT %s FROM seq`, table, strings.Join(columns, ", "), strings.Join(values, ", "))
}

func TestUpgradeFixturesReplayTheirSchema(t *testing.T) {
	for _, fixture := range loadUpgradeSchemaFixtures(t) {
		t.Run(fmt.Sprintf("v%d", fixture.version), func(t *testing.T) {
			require.LessOrEqual(t, fixture.version, currentSchemaVersion, "a fixture newer than this build cannot be upgraded")
			path := buildUpgradeFixtureStore(t, fixture, upgradeFixtureSmallScale, t.TempDir())
			db, err := sql.Open("sqlite", sqliteDSN(path, "mode=ro"))
			require.NoError(t, err)
			defer db.Close()
			version, err := readUserVersion(db)
			require.NoError(t, err)
			require.Equal(t, fixture.version, version)
			var integrity string
			require.NoError(t, db.QueryRow("PRAGMA integrity_check").Scan(&integrity))
			require.Equal(t, "ok", integrity)
			var nodes, edges, viewNodes int
			require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&nodes))
			require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM edges").Scan(&edges))
			require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM nodes WHERE view_gen <> 0").Scan(&viewNodes))
			require.Equal(t, upgradeFixtureSmallScale.nodes, nodes)
			require.Equal(t, upgradeFixtureSmallScale.edges, edges)
			require.Equal(t, upgradeFixtureSmallScale.nodes/10, viewNodes, "about one row in ten lives in a view generation")
			var objects int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\' AND sql IS NOT NULL
				AND name NOT IN (SELECT name FROM pragma_table_list WHERE type = 'shadow')`).Scan(&objects))
			require.Len(t, fixture.statements, objects, "the replayed store carries exactly the captured objects")
		})
	}
}
