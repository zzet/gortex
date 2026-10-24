package indexer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkWalkAllowList(b *testing.B) {
	root := b.TempDir()
	dir := filepath.Join(root, "src/a/b/c/d/e")
	if err := os.MkdirAll(dir, 0755); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%d.go", i)), []byte("package fixture\n"), 0644); err != nil {
			b.Fatal(err)
		}
	}
	idx := newAdmissionTestIndexer(b, root, "/*", "!/src/")
	check := func() {
		b.Helper()
		count := 0
		err := idx.walkDisk(root, root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if idx.admitFileInOpenDir(root, path, -1, d.Type()).admit {
				count++
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
		if count != 500 {
			b.Fatalf("admitted %d files, want 500", count)
		}
	}
	check()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		check()
	}
}
