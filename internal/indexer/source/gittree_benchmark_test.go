package source

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkFullTreeContentSourceOpen(b *testing.B) {
	root := b.TempDir()
	content := []byte("package fixture\n\nvar Payload = `" + strings.Repeat("immutable-tree-bytes-", 1024) + "`\n")
	if err := os.WriteFile(filepath.Join(root, "fixture.go"), content, 0o600); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	if _, err := runGit(ctx, root, "init", "--quiet"); err != nil {
		b.Fatal(err)
	}
	if _, err := runGit(ctx, root, "add", "--", "fixture.go"); err != nil {
		b.Fatal(err)
	}
	treeBytes, err := runGit(ctx, root, "write-tree")
	if err != nil {
		b.Fatal(err)
	}

	gitTree, err := NewGitTreeSource(ctx, root, strings.TrimSpace(string(treeBytes)))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = gitTree.Close() })
	filesystem, err := NewFilesystemSource(root)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = filesystem.Close() })

	benchmarkContentSourceOpen(b, "git_tree", gitTree, int64(len(content)))
	benchmarkContentSourceOpen(b, "filesystem", filesystem, int64(len(content)))
}

func benchmarkContentSourceOpen(b *testing.B, name string, content ContentSource, size int64) {
	b.Helper()
	b.Run(name, func(b *testing.B) {
		// Warm the Git batch child and the filesystem page cache before measuring.
		reader, _, err := content.Open("fixture.go")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			b.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			b.Fatal(err)
		}

		b.ReportAllocs()
		b.SetBytes(size)
		b.ResetTimer()
		for range b.N {
			reader, _, err := content.Open("fixture.go")
			if err != nil {
				b.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, reader); err != nil {
				_ = reader.Close()
				b.Fatal(err)
			}
			if err := reader.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
