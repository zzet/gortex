package graph

import "context"

// ConstantValueReadStatus lets a caller reject an equality proof when a legacy
// extractor swallowed a checked lookup error. The status is per delta/build.
type ConstantValueReadStatus interface{ ConstantValueReadError() error }

func (dw *DeltaWriter) ConstantValueReadError() error {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	return dw.constantReadErr
}
func (dw *DeltaWriter) recordConstantError(err error) error {
	if err != nil && dw.constantReadErr == nil {
		dw.constantReadErr = err
	}
	return err
}

func (dw *DeltaWriter) readConstantProjection(ctx context.Context, ids []string, files []ConstantFileKey) (ConstantValueProjection, error) {
	base, err := readConstantProjection(ctx, dw.below, ids, files)
	if err != nil {
		dw.writeMu.Lock()
		_ = dw.recordConstantError(err)
		dw.writeMu.Unlock()
		return ConstantValueProjection{}, err
	}
	keys := append([]ConstantFileKey(nil), files...)
	for _, row := range base.Rows {
		keys = append(keys, ConstantFileKey{RepoPrefix: row.RepoPrefix, FilePath: row.FilePath})
	}
	own := newConstantValueProjection()
	if dw.sidecar != nil {
		reader, ok := dw.sidecar.(ConstantValueProjectionReader)
		if !ok {
			err = ErrConstantProjectionUnsupported
		} else {
			own, err = reader.ReadConstantValueProjectionContext(ctx, ids, keys)
		}
	}
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	if err != nil {
		return ConstantValueProjection{}, dw.recordConstantError(err)
	}
	own.Nodes = make(map[string]string)
	for _, id := range ids {
		if node := dw.work.GetNode(id); node != nil {
			own.Nodes[id] = node.FilePath
		}
	}
	// Active deltas can cover a file merely to update one node's metadata.
	// Only an explicit successful constant delete owns its empty sidecar here.
	out, err := composeConstantProjection(ctx, dw.layer, base, own, dw.constantOwnedFiles, false)
	return out, dw.recordConstantError(err)
}

// BulkSetConstantValues is a partial overwrite. It never claims an entire
// file or hides inherited constants absent from rows.
func (dw *DeltaWriter) BulkSetConstantValues(repo string, rows []ConstantValueRow) error {
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	writer, ok := sidecarAs[ConstantValueWriter](dw)
	if !ok {
		return dw.recordConstantError(ErrConstantProjectionUnsupported)
	}
	return dw.recordConstantError(writer.BulkSetConstantValues(repo, rows))
}

// DeleteConstantValuesByFiles is authoritative. It requires actual accepted
// inventory, materializes the file's existing graph claim when necessary, and
// retains that claim through equal-graph payload pruning. Missing legacy input
// refuses the write; accepted metadata is never invented for it.
func (dw *DeltaWriter) DeleteConstantValuesByFiles(repo string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	dw.writeMu.Lock()
	defer dw.writeMu.Unlock()
	writer, ok := sidecarAs[ConstantValueWriter](dw)
	reader, readOK := sidecarAs[ConstantValueProjectionReader](dw)
	metadata, metaOK := sidecarAs[FileMetaWriter](dw)
	if !ok || !readOK || !metaOK {
		return dw.recordConstantError(ErrConstantProjectionUnsupported)
	}
	var keys []ConstantFileKey
	for _, path := range UniqueRecordingPaths(paths) {
		keys = append(keys, ConstantFileKey{RepoPrefix: repo, FilePath: path})
	}
	own, err := reader.ReadConstantValueProjectionContext(context.Background(), nil, keys)
	if err != nil {
		return dw.recordConstantError(err)
	}
	var missing []ConstantFileKey
	for _, key := range keys {
		// A deleted file already has authoritative graph ownership and must
		// retain absent inventory, even when metadata was deleted first.
		if dw.layer.IsTombstone(key.FilePath) {
			continue
		}
		if row, ok := own.Files[key]; !ok || row.ContentHash == "" {
			missing = append(missing, key)
		}
	}
	var copyRows []FileMetaRow
	if len(missing) > 0 {
		below, err := readConstantProjection(context.Background(), dw.below, nil, missing)
		if err != nil {
			return dw.recordConstantError(err)
		}
		for _, key := range missing {
			row, ok := below.Files[key]
			if !ok || row.ContentHash == "" {
				return dw.recordConstantError(ErrConstantProjectionIncomplete)
			}
			copyRows = append(copyRows, row)
		}
	}
	if len(copyRows) > 0 {
		if err := metadata.SetFileMetas(repo, copyRows); err != nil {
			return dw.recordConstantError(err)
		}
	}
	if err := writer.DeleteConstantValuesByFiles(repo, paths); err != nil {
		return dw.recordConstantError(err)
	}
	dw.coverPaths(paths)
	if dw.constantOwnedFiles == nil {
		dw.constantOwnedFiles = make(map[ConstantFileKey]bool)
	}
	for _, key := range keys {
		dw.constantOwnedFiles[key] = true
	}
	return nil
}
