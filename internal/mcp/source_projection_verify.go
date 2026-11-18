package mcp

import (
	"context"
	"fmt"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/pathkey"
	"path/filepath"
)

// verifyPreparedProjection authenticates an after-image while its original
// physical path remains held. It performs bounded I/O before cache admission;
// the cohort registry itself never performs I/O or supplies disk authority.
// expectedSHA256 is the durable journal's after-image SHA, when applicable.
func (s *Server) verifyPreparedProjection(ctx context.Context, prepared *preparedFileProjection, sequence uint64, expectedSHA256 string) (*projectionEntry, error) {
	if prepared == nil || prepared.entry == nil {
		return nil, fmt.Errorf("projection preparation unavailable")
	}
	entry := prepared.entry
	if sequence == 0 || !mutationPathAdmitted(ctx, prepared.key) || pathkey.MutationKey(entry.absPath) != prepared.key {
		return nil, fmt.Errorf("projection held-path identity unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := statDiskStamp(entry.absPath)
	if err != nil {
		return nil, err
	}
	if entry.tombstone {
		if before.info != nil || expectedSHA256 != "" {
			return nil, fmt.Errorf("projection removal after-image changed")
		}
		return entry.committedCopy(sequence, before), nil
	}
	if expectedSHA256 != "" && prepared.contentSHA256 != expectedSHA256 {
		return nil, fmt.Errorf("projection journal after-image mismatch")
	}
	if before.info == nil || !before.info.Mode().IsRegular() || before.info.Size() > 1<<20 {
		return nil, fmt.Errorf("projection after-image is not a bounded regular file")
	}
	files, err := source.NewFilesystemSource(filepath.Dir(entry.absPath))
	if err != nil {
		return nil, err
	}
	raw, _, readErr := files.ReadRegularFile(ctx, filepath.Base(entry.absPath), 1<<20)
	files.Close()
	after, err := statDiskStamp(entry.absPath)
	if readErr != nil {
		return nil, readErr
	}
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !before.matches(after) || blake3.Sum256(raw) != prepared.rawHash || pathkey.MutationKey(entry.absPath) != prepared.key || !mutationPathAdmitted(ctx, prepared.key) {
		return nil, fmt.Errorf("projection after-image changed")
	}
	return entry.committedCopy(sequence, after), nil
}
