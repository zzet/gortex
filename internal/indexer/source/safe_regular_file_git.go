package source

import (
	"context"
	"fmt"
	"io/fs"
	"path"
)

var _ RegularFileReader = (*GitTreeSource)(nil)

// ReadRegularFile validates immutable metadata before requesting a bounded blob.
func (s *GitTreeSource) ReadRegularFile(ctx context.Context, name string, maxBytes int64) ([]byte, FileMeta, error) {
	rel, err := regularReadArguments(ctx, name, maxBytes)
	if err != nil {
		return nil, FileMeta{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, FileMeta{}, err
	}
	if s.closed {
		return nil, FileMeta{}, fmt.Errorf("git tree source: %w", fs.ErrClosed)
	}
	if mode, found := s.nonContent[rel]; found {
		meta := FileMeta{Path: rel, Mode: mode}
		return nil, meta, fmt.Errorf("%s: %w", rel, ErrNotRegularFile)
	}
	entry, found := s.entries[rel]
	if !found {
		// A flat tree inventory cannot establish a missing manifest below a
		// symlink, gitlink, or regular file. Retain that unsupported boundary.
		for parent := path.Dir(rel); parent != "."; parent = path.Dir(parent) {
			_, blob := s.entries[parent]
			mode, known := s.nonContent[parent]
			if blob || (known && !mode.IsDir()) {
				return nil, FileMeta{}, fmt.Errorf("%s: ancestor %s: %w", rel, parent, ErrNotRegularFile)
			}
		}
		if !s.inventoryComplete {
			return nil, FileMeta{}, fmt.Errorf("%s: incomplete entry inventory: %w", rel, ErrRegularReadUnsupported)
		}
		return nil, FileMeta{}, regularFileAbsent(rel, nil)
	}
	meta := entry.meta
	if err := regularMetadataError(meta, maxBytes); err != nil {
		return nil, meta, err
	}
	data, err := s.readRegularBlobLocked(ctx, entry.oid, maxBytes)
	if err != nil {
		return nil, meta, fmt.Errorf("%s: %w", rel, err)
	}
	if int64(len(data)) != meta.Size {
		return nil, meta, fmt.Errorf("%s: Git blob size changed from %d to %d", rel, meta.Size, len(data))
	}
	if err := ctx.Err(); err != nil {
		return nil, meta, err
	}
	return data, meta, nil
}
func (s *GitTreeSource) readRegularBlobLocked(ctx context.Context, oid string, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.batch == nil {
		batch, err := startGitBatch(ctx, s.repoDir)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			batch.close()
			return nil, err
		}
		s.batch = batch
		s.spawns++
	}
	data, err := s.batch.readBounded(oid, maxBytes)
	if s.batch.broken != nil {
		s.batch.close()
		s.batch = nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return data, err
}
