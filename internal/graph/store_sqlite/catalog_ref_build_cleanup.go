package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
)

// Generic upsert is a creation/identity-replacement surface, including rows
// with no generation yet. It must not open a new attempt or move an existing
// closing graph's attempt to a healthy graph. Actual admitted workers drain via
// token-guarded CompleteRefViewBuild and TouchRefViewBuild, left unchanged.
func validateRefBuildUpsertAdmissionTx(ctx context.Context, tx *sql.Tx, build RefViewBuild) error {
	if err := validateRefGraphAdmissionTx(ctx, tx, build.RefViewID); err != nil {
		return err
	}
	var previousRef string
	err := tx.QueryRowContext(ctx, `SELECT ref_view_id FROM ref_view_builds WHERE build_id = ?`, build.BuildID).Scan(&previousRef)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if previousRef != build.RefViewID {
		return validateRefGraphAdmissionTx(ctx, tx, previousRef)
	}
	return nil
}
