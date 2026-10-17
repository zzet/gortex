package indexer

import (
	"context"

	"go.uber.org/zap"
)

// excludeColdFailedFiles stages a failure envelope for every failed file of a
// cold namespace pass, whatever the failure was. The cold reset dropped each
// file's core, so a prior accepted receipt would describe nothing; the
// envelope excludes the file from the namespace the door then accepts. When
// the envelopes cannot be staged the namespace header is held pending, so the
// door accepts no input state that still names a stale receipt and the
// baseline certifies the namespace instead.
func (idx *Indexer) excludeColdFailedFiles(ctx context.Context, failed []string) {
	journal := idx.contractCoreInputs
	if journal == nil || len(failed) == 0 || !journal.coldNamespace() {
		return
	}
	paths := make([]string, 0, len(failed))
	for _, absPath := range failed {
		if !contractFailureEnvelopeEligible(idx.fileIndexFailureError(absPath), true) {
			continue
		}
		paths = append(paths, idx.prefixPath(idx.relKey(absPath)))
	}
	if err := journal.stageFailureEnvelopes(ctx, paths); err != nil {
		if backend, ok := journal.backend.(*contractCoreStorageBackend); ok {
			backend.holdNamespacePending()
		}
		if idx.logger != nil {
			idx.logger.Warn("contract failure envelopes not staged; namespace left pending",
				zap.String("repo", idx.repoPrefix), zap.Int("failed_files", len(paths)), zap.Error(err))
		}
	}
}

// stageContractFailureEnvelope excludes one incrementally failed file when its
// failure is a deterministic content failure. A file whose own receipt this
// pass already rewrote is left alone: its failure came after the core write,
// so its receipt stays unaccepted for the stuck-receipt repair instead.
func (idx *Indexer) stageContractFailureEnvelope(absPath string, err error) {
	journal := idx.contractCoreInputs
	if journal == nil || !contractFailureEnvelopeEligible(err, false) {
		return
	}
	path := idx.prefixPath(idx.relKey(absPath))
	if journal.hasBegun(path) {
		return
	}
	if stageErr := journal.stageFailureEnvelopes(journal.ctx, []string{path}); stageErr != nil && idx.logger != nil {
		idx.logger.Debug("contract failure envelope not staged", zap.String("path", path), zap.Error(stageErr))
	}
}
