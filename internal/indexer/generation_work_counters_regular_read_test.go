package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/indexer/source"
)

// plainContentSource offers only the ContentSource methods, never the bounded
// regular-file read.
type plainContentSource struct{ source.ContentSource }

// TestWorkCountingSourceKeepsBoundedRegularReads pins that counting reads does
// not change what can be read. Go package ownership reads go.mod and go.work
// with source.ReadRegularFile through the build's target, which the builder
// wraps in the admission counter; a wrapper that dropped the read made every
// manifest look unreadable, certified no package, and let a sparse generation
// bind an import or call to a same-named package in another directory.
func TestWorkCountingSourceKeepsBoundedRegularReads(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := source.NewFilesystemSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close() //nolint:errcheck // test source
	ctx := context.Background()

	work := newGenerationWorkCounters(BuildRequest{})
	admission := work.admissionSource(fs)
	data, _, err := source.ReadRegularFile(ctx, admission, "go.mod", 64<<10)
	if err != nil || string(data) != "module example.test/m\n" {
		t.Fatalf("regular read through the admission wrapper = %q, %v", data, err)
	}
	if work.AdmissionOpens != 1 || work.AdmissionOpenBytes != int64(len(data)) {
		t.Fatalf("admission account = %d opens / %d bytes, want 1 / %d", work.AdmissionOpens, work.AdmissionOpenBytes, len(data))
	}
	if _, _, err := source.ReadRegularFile(ctx, admission, "go.work", 64<<10); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent go.work through the wrapper = %v, want not-exist (ownership reads anything else as an unknown workspace)", err)
	}

	// A source that never offered the read still refuses it wrapped.
	if _, _, err := source.ReadRegularFile(ctx, work.admissionSource(plainContentSource{fs}), "go.mod", 64<<10); !errors.Is(err, source.ErrRegularReadUnsupported) {
		t.Fatalf("wrapped plain source = %v, want ErrRegularReadUnsupported", err)
	}

	// The extraction wrapper forwards without charging admission or parse.
	extraction := work.extractionSource(fs)
	if _, _, err := source.ReadRegularFile(ctx, extraction, "go.mod", 64<<10); err != nil {
		t.Fatalf("regular read through the extraction wrapper: %v", err)
	}
	if work.AdmissionOpens != 1 || len(work.parsed) != 0 {
		t.Fatalf("extraction-wrapper regular read charged an account: opens=%d parsed=%d", work.AdmissionOpens, len(work.parsed))
	}
}
