package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/indexer/source"
)

func TestSetTrackContentSourceBorrowsAnImmutableSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := source.NewFilesystemSource(root)
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close() //nolint:errcheck // test cleanup

	idx := &Indexer{}
	setTrackContentSource(idx, nil)
	if got := idx.contentSource(); got != nil {
		t.Fatalf("nil source changed the legacy selection: %T", got)
	}

	setTrackContentSource(idx, content)
	if got := idx.contentSource(); got != content {
		t.Fatalf("content source = %T, want the borrowed source", got)
	}

	// Nil means legacy selection, not clear an explicitly installed source.
	setTrackContentSource(idx, nil)
	if got := idx.contentSource(); got != content {
		t.Fatalf("nil cleared the borrowed source: %T", got)
	}
	if _, err := content.Stat("fixture.go"); err != nil {
		t.Fatalf("borrowed source was unexpectedly closed: %v", err)
	}
}
