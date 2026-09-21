package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

var _ RegularFileReader = (*FilesystemSource)(nil)

// ReadRegularFile validates a confined nonblocking descriptor before reading.
func (s *FilesystemSource) ReadRegularFile(ctx context.Context, name string, maxBytes int64) ([]byte, FileMeta, error) {
	return s.readRegularFileWithOpen(ctx, name, maxBytes, openRootRegularFile)
}
func (s *FilesystemSource) readRegularFileWithOpen(ctx context.Context, name string, maxBytes int64, openFile func(*os.Root, string) (*os.File, error)) ([]byte, FileMeta, error) {
	rel, err := regularReadArguments(ctx, name, maxBytes)
	if err != nil {
		return nil, FileMeta{}, err
	}
	if !regularFilesystemReadSupported {
		// Without a safe descriptor opener, do not claim even absence from
		// platform metadata errors (e.g. path-not-found below a regular file).
		return nil, FileMeta{}, fmt.Errorf("%s: %w", rel, ErrRegularReadUnsupported)
	}
	info, err := s.root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, FileMeta{}, regularFilesystemError(rel, err)
	}
	meta := regularDescriptorMeta(rel, info)
	if err := regularMetadataError(meta, maxBytes); err != nil {
		return nil, meta, err
	}
	if err := ctx.Err(); err != nil {
		return nil, meta, err
	}
	file, err := openFile(s.root, filepath.FromSlash(rel))
	if err != nil {
		return nil, meta, regularFilesystemError(rel, err)
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, FileMeta{}, regularFilesystemError(rel, err)
	}
	meta = regularDescriptorMeta(rel, info)
	if err := regularMetadataError(meta, maxBytes); err != nil {
		return nil, meta, err
	}
	data, err := io.ReadAll(io.LimitReader(regularContextReader{ctx: ctx, reader: file}, maxBytes+1))
	if err != nil {
		return nil, meta, err
	}
	if err := ctx.Err(); err != nil {
		return nil, meta, err
	}
	if int64(len(data)) > maxBytes {
		return nil, meta, fmt.Errorf("%s: %w", rel, ErrRegularFileTooLarge)
	}
	return data, meta, nil
}
func regularDescriptorMeta(name string, info fs.FileInfo) FileMeta {
	return FileMeta{Path: name, Size: info.Size(), Mode: info.Mode(), Symlink: info.Mode()&fs.ModeSymlink != 0}
}
func regularFilesystemError(name string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return regularFileAbsent(name, err)
	}
	return mapRootError(name, err)
}

type regularContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r regularContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
