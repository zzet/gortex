package mcp

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPendingTextVerificationBufferRemovesPerFileScratchAllocation(t *testing.T) {
	var files []*os.File
	root := t.TempDir()
	for i := range 4 {
		path := filepath.Join(root, fmt.Sprintf("source-%d.go", i))
		require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("package fixture\n"), 4096), 0o600))
		file, err := openPhysicalEvidenceFile(path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = file.Close() })
		files = append(files, file)
	}
	scratch := make([]byte, 32<<10)
	// Isolate the production descriptor reader's verification-buffer cost.
	// Re-resolving full paths adds independent filesystem/pool allocations to
	// each variant; the path/hash/identity oracles are tested separately below.
	measure := func(buffer []byte) float64 {
		return testing.AllocsPerRun(3, func() {
			for _, file := range files {
				_, err := file.Seek(0, io.SeekStart)
				require.NoError(t, err)
				_, _, _, err = readPhysicalEvidenceDescriptor(file, nil, 1<<20, buffer)
				require.NoError(t, err)
			}
		})
	}
	legacy, buffered := measure(nil), measure(scratch)
	t.Logf("actual_descriptor_allocations legacy=%g request_buffer=%g files=%d", legacy, buffered, len(files))
	require.GreaterOrEqual(t, legacy-buffered, float64(len(files)), "one new streaming verification buffer per file returned")
}

func TestPendingTextVerificationBufferPreservesPhysicalEvidence(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		name := "legacy"
		if buffered {
			name = "request_buffer"
		}
		t.Run(name, func(t *testing.T) {
			var scratch []byte
			if buffered {
				scratch = make([]byte, 32<<10)
			}
			read := func(path string, after func(), limit int64) ([]byte, physicalReadEvidence, error) {
				if buffered {
					return readPhysicalFileEvidenceObservedBoundedBuffer(path, after, limit, scratch)
				}
				return readPhysicalFileEvidenceObservedBounded(path, after, limit)
			}
			t.Run("content_does_not_alias_verification_workspace", func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "source.go")
				first := bytes.Repeat([]byte("first source\n"), 4096)
				require.NoError(t, os.WriteFile(path, first, 0o600))
				raw, evidence, err := read(path, nil, 1<<20)
				require.NoError(t, err)
				require.Equal(t, len(first), evidence.byteCount)
				require.Equal(t, first, raw)
				second := bytes.Repeat([]byte("second bytes\n"), 4096)
				require.NoError(t, os.WriteFile(path, second, 0o600))
				next, _, err := read(path, nil, 1<<20)
				require.NoError(t, err)
				require.Equal(t, second, next)
				require.Equal(t, first, raw, "a later physical verification cannot overwrite earlier returned content")
			})
			t.Run("same_size_restored_mtime_still_requires_second_hash", func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "source.go")
				require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("a"), 8192), 0o600))
				before, err := os.Stat(path)
				require.NoError(t, err)
				_, _, err = read(path, func() {
					require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("b"), 8192), 0o600))
					require.NoError(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
				}, 1<<20)
				require.ErrorIs(t, err, errPhysicalFileMoved)
			})
			t.Run("same_content_new_inode_is_refused", func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "source.go")
				raw := []byte("package fixture\nfunc Same() {}\n")
				require.NoError(t, os.WriteFile(path, raw, 0o600))
				var rename pendingTextTestRename
				current, _, err := read(path, func() {
					if !rename.try(t, path, path+".old") {
						return
					}
					require.NoError(t, os.WriteFile(path, raw, 0o600))
				}, 1<<20)
				if rename.blocked {
					require.NoError(t, err)
					require.Equal(t, raw, current)
					rename.afterClose(t)
					return
				}
				require.ErrorIs(t, err, errPhysicalFileMoved)
			})
			t.Run("byte_budget_still_refuses", func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "source.go")
				require.NoError(t, os.WriteFile(path, []byte("package fixture\n"), 0o600))
				_, _, err := read(path, nil, 2)
				require.Error(t, err)
				require.Contains(t, err.Error(), "byte bound")
			})
		})
	}
}
