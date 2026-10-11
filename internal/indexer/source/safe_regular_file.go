package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
)

// RegularFileReader guarantees a bounded, non-symlink regular final component.
// Cancellation is cooperative, not a hard mutex/syscall/pipe deadline.
type RegularFileReader interface {
	ReadRegularFile(context.Context, string, int64) ([]byte, FileMeta, error)
}

var (
	ErrRegularReadUnsupported = errors.New("safe regular-file reads are unavailable")
	ErrNotRegularFile         = errors.New("entry is not a non-symlink regular file")
	ErrRegularFileTooLarge    = errors.New("regular file exceeds the read limit")
	ErrRegularFileSizeUnknown = errors.New("regular file size is unknown")
)

// Leave room for the overflow-probe byte in a Go slice on every architecture.
const maxRegularReadBytes = int64(math.MaxInt) - 1

// ReadRegularFile never falls back to unsafe generic Stat/Open.
func ReadRegularFile(ctx context.Context, src ContentSource, name string, maxBytes int64) ([]byte, FileMeta, error) {
	if _, err := regularReadArguments(ctx, name, maxBytes); err != nil {
		return nil, FileMeta{}, err
	}
	reader, ok := src.(RegularFileReader)
	if !ok {
		return nil, FileMeta{}, fmt.Errorf("%s: %w", name, ErrRegularReadUnsupported)
	}
	data, meta, err := reader.ReadRegularFile(ctx, name, maxBytes)
	if err != nil {
		return nil, meta, err
	}
	if err := ctx.Err(); err != nil {
		return nil, meta, err
	}
	if err := regularMetadataError(meta, maxBytes); err != nil {
		return nil, meta, err
	}
	if int64(len(data)) > maxBytes {
		return nil, meta, fmt.Errorf("%s: %w", name, ErrRegularFileTooLarge)
	}
	return data, meta, nil
}
func regularReadArguments(ctx context.Context, name string, maxBytes int64) (string, error) {
	if ctx == nil {
		return "", errors.New("safe regular-file read: nil context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if maxBytes < 0 || maxBytes > maxRegularReadBytes {
		return "", errors.New("safe regular-file read: invalid byte limit")
	}
	return normalizePath(name)
}
func regularMetadataError(meta FileMeta, maxBytes int64) error {
	switch {
	case meta.Symlink || !meta.Mode.IsRegular():
		return fmt.Errorf("%s: %w", meta.Path, ErrNotRegularFile)
	case meta.Size < 0:
		return fmt.Errorf("%s: %w", meta.Path, ErrRegularFileSizeUnknown)
	case meta.Size > maxBytes:
		return fmt.Errorf("%s: %w", meta.Path, ErrRegularFileTooLarge)
	default:
		return nil
	}
}
func regularFileAbsent(name string, cause error) error {
	return fmt.Errorf("%s: %w", name, errors.Join(ErrNotInSource, fs.ErrNotExist, cause))
}

var _ RegularFileReader = (*LayeredSource)(nil)

// ReadRegularFile respects authoritative upper ownership, including errors.
func (s *LayeredSource) ReadRegularFile(ctx context.Context, name string, maxBytes int64) ([]byte, FileMeta, error) {
	rel, err := regularReadArguments(ctx, name, maxBytes)
	if err != nil {
		return nil, FileMeta{}, err
	}
	return ReadRegularFile(ctx, s.route(rel), rel, maxBytes)
}
