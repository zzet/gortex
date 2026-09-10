package store_sqlite

import (
	"database/sql"
	"fmt"
)

// addDependencyRevisionColumns is an additive metadata migration. Empty marks
// legacy/unproven dependency provenance; it does not certify existing output.
// The fixed statements tolerate both an upgraded old store and fresh schemaSQL
// that already includes the columns. Version allocation belongs to the registry.
func addDependencyRevisionColumns(tx *sql.Tx) error {
	for _, step := range []struct {
		table, probe, alter string
	}{
		{
			table: "view_generations",
			probe: `SELECT COUNT(*) FROM pragma_table_xinfo('view_generations') WHERE name = 'dependency_revision'`,
			alter: `ALTER TABLE view_generations ADD COLUMN dependency_revision TEXT NOT NULL DEFAULT ''`,
		},
		{
			table: "dedicated_base_publications",
			probe: `SELECT COUNT(*) FROM pragma_table_xinfo('dedicated_base_publications') WHERE name = 'dependency_revision'`,
			alter: `ALTER TABLE dedicated_base_publications ADD COLUMN dependency_revision TEXT NOT NULL DEFAULT ''`,
		},
	} {
		var count int
		if err := tx.QueryRow(step.probe).Scan(&count); err != nil {
			return fmt.Errorf("inspect %s dependency revision: %w", step.table, err)
		}
		if count == 0 {
			if _, err := tx.Exec(step.alter); err != nil {
				return fmt.Errorf("add %s dependency revision: %w", step.table, err)
			}
		}
	}
	return nil
}
