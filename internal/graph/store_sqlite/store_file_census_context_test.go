package store_sqlite

import (
	"context"
	"errors"
	"github.com/zzet/gortex/internal/graph"
	"path/filepath"
	"testing"
)

func TestFileMetadataCensusContextExactNamespaceAndCancellation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "files.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "repo/a.go", ContentHash: "accepted-a", Size: 12, NodeCount: 1}}); err != nil {
		t.Fatal(err)
	}
	other := s.AtGeneration(7)
	if err := other.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "repo/a.go", ContentHash: "accepted-b", Size: 13, NodeCount: 2}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		store *Store
		hash  string
	}{{s, "accepted-a"}, {other, "accepted-b"}} {
		rows, err := test.store.FileMetasForRepoContext(context.Background(), "repo")
		if err != nil || len(rows) != 1 || rows[0].ContentHash != test.hash {
			t.Fatalf("physical file census wrong=%#v %v", rows, err)
		}
	}
	empty, err := s.FileMetasForRepoContext(context.Background(), "")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty namespace lost=%#v %v", empty, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows, err := s.FileMetasForRepoContext(ctx, "repo")
	if !errors.Is(err, context.Canceled) || rows != nil {
		t.Fatalf("canceled census usable=%#v %v", rows, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err = s.FileMetasForRepoContext(context.Background(), "repo")
	if err == nil || rows != nil {
		t.Fatalf("closed census usable=%#v %v", rows, err)
	}
}
