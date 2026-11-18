package indexer

import (
	"fmt"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Cold parsing may involve tens of thousands of files. Keep compact sidecar
// memory and SQLite statement sizes bounded while still amortising the old
// delete+insert transaction pair paid once per file.
const (
	parseSidecarBatchFiles     = 256
	parseSidecarBatchConstRows = 2048
	shadowSidecarTransferRows  = 2048
	shadowSidecarTransferBytes = 4 << 20
)

type compactSidecarPayload struct {
	fileRows   []graph.FileMetaRow
	constRows  []graph.ConstantValueRow
	constFiles []string
}

type parseSidecarBatch struct {
	idx    *Indexer
	target graph.Store

	mu         sync.Mutex
	fileRows   []graph.FileMetaRow
	constRows  []graph.ConstantValueRow
	constFiles []string
}

func newParseSidecarBatch(idx *Indexer) *parseSidecarBatch {
	return &parseSidecarBatch{idx: idx, target: idx.graph}
}

func (b *parseSidecarBatch) add(relPath string, src []byte, result *parser.ExtractionResult) {
	row, hasFileRow := b.idx.prepareFileMeta(relPath, src, result)
	constRows, constFiles := b.idx.prepareConstValues(result)
	b.addPrepared(row, hasFileRow, constRows, constFiles)
}

func (b *parseSidecarBatch) addConstValues(result *parser.ExtractionResult) {
	rows, files := b.idx.prepareConstValues(result)
	b.addPrepared(graph.FileMetaRow{}, false, rows, files)
}

func (b *parseSidecarBatch) addPrepared(
	fileRow graph.FileMetaRow,
	hasFileRow bool,
	constRows []graph.ConstantValueRow,
	constFiles []string,
) {
	if !hasFileRow && len(constRows) == 0 {
		return
	}
	var flush compactSidecarPayload
	b.mu.Lock()
	if hasFileRow {
		b.fileRows = append(b.fileRows, fileRow)
	}
	b.constRows = append(b.constRows, constRows...)
	b.constFiles = append(b.constFiles, constFiles...)
	if len(b.fileRows) >= parseSidecarBatchFiles || len(b.constRows) >= parseSidecarBatchConstRows {
		flush = b.takeLocked()
	}
	b.mu.Unlock()
	b.persist(flush)
}

func (b *parseSidecarBatch) flush() {
	b.mu.Lock()
	flush := b.takeLocked()
	b.mu.Unlock()
	b.persist(flush)
}

func (b *parseSidecarBatch) takeLocked() compactSidecarPayload {
	payload := compactSidecarPayload{
		fileRows:   b.fileRows,
		constRows:  b.constRows,
		constFiles: b.constFiles,
	}
	b.fileRows = nil
	b.constRows = nil
	b.constFiles = nil
	return payload
}

func (b *parseSidecarBatch) persist(payload compactSidecarPayload) {
	persistFileMetaRows(b.target, b.idx.repoPrefix, payload.fileRows)
	persistConstantValueRows(b.target, b.idx.repoPrefix, payload.constFiles, payload.constRows)
}

// resetShadowCloneCorpus runs before the first node-drain write. SQLite's
// AddBatch extracts pending raw clone rows from those nodes; resetting after
// that drain would delete them when the shadow deferred clone finalization.
func resetShadowCloneCorpus(disk graph.Store, repoPrefix string) error {
	if writer, ok := disk.(graph.CloneCorpusRepoReplacer); ok {
		if err := writer.ReplaceCloneCorpus(repoPrefix, nil); err != nil {
			return fmt.Errorf("reset clone corpus: %w", err)
		}
	}
	return nil
}

// persistShadowCompactSidecars transfers authoritative compact projections in
// bounded batches after the graph is durable. The caller has already reset
// clone rows before the node drain; append finalized shadow rows without
// removing pending rows that the node writer just installed.
func persistShadowCompactSidecars(
	shadow *graph.Graph,
	disk graph.Store,
	repoPrefix string,
) error {
	if writer, ok := disk.(graph.FileMetaRepoReplacer); ok {
		if err := writer.ReplaceFileMetas(repoPrefix, nil); err != nil {
			return fmt.Errorf("reset file metadata: %w", err)
		}
	}
	if writer, ok := disk.(graph.ConstantValueRepoReplacer); ok {
		if err := writer.ReplaceConstantValues(repoPrefix, nil); err != nil {
			return fmt.Errorf("reset constant values: %w", err)
		}
	}
	return appendShadowCompactSidecars(shadow, disk, repoPrefix)
}

// persistShadowCompactSidecarChunk appends one bounded streaming shadow to the
// disk projection. It intentionally does not use repo-wide replacement: prior
// streaming chunks from the same full index are already durable and must stay.
func persistShadowCompactSidecarChunk(
	shadow *graph.Graph,
	disk graph.Store,
	repoPrefix string,
) error {
	return appendShadowCompactSidecars(shadow, disk, repoPrefix)
}

func appendShadowCompactSidecars(shadow *graph.Graph, disk graph.Store, repoPrefix string) error {
	if writer, ok := disk.(graph.CloneCorpusWriter); ok {
		for rows := range shadow.DrainCloneCorpusBatches(
			repoPrefix, shadowSidecarTransferRows, shadowSidecarTransferBytes,
		) {
			if err := writer.BulkSetCloneCorpus(repoPrefix, rows); err != nil {
				return fmt.Errorf("append clone-corpus chunk: %w", err)
			}
		}
	}
	if writer, ok := disk.(graph.FileMetaWriter); ok {
		var writeErr error
		for rows := range shadow.DrainFileMetaBatches(
			repoPrefix, shadowSidecarTransferRows, shadowSidecarTransferBytes,
		) {
			if err := writer.SetFileMetas(repoPrefix, rows); err != nil {
				writeErr = fmt.Errorf("append file metadata chunk: %w", err)
				break
			}
		}
		if writeErr != nil {
			return writeErr
		}
	}
	if writer, ok := disk.(graph.ConstantValueWriter); ok {
		var writeErr error
		for rows := range shadow.DrainConstantValueBatches(
			repoPrefix, shadowSidecarTransferRows, shadowSidecarTransferBytes,
		) {
			if err := writer.BulkSetConstantValues(repoPrefix, rows); err != nil {
				writeErr = fmt.Errorf("append constant-value chunk: %w", err)
				break
			}
		}
		if writeErr != nil {
			return writeErr
		}
	}
	return nil
}
