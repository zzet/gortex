package indexer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckoutRootFileInfoPinsIdentityBeforePathReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "checkout")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	original, err := checkoutRootFileInfo(root)
	if err != nil {
		t.Fatal(err)
	}
	// Do not call SameFile before replacement: that would eagerly populate
	// Windows' lazy os.Stat identity and conceal the original bug.
	moved := filepath.Join(parent, "moved-checkout")
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("capturing identity must not retain a directory handle: %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	replacement, err := checkoutRootFileInfo(root)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(original, replacement) {
		t.Fatal("captured identity adopted a replacement at the reused pathname")
	}
	renamedOriginal, err := checkoutRootFileInfo(moved)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(original, renamedOriginal) {
		t.Fatal("captured identity no longer identifies the original directory after rename")
	}
}

func TestCheckoutRootFileInfoRejectsMissingOrRegularFile(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "source.go")
	if err := os.WriteFile(file, []byte("package fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "missing"), file} {
		if info, err := checkoutRootFileInfo(path); err == nil || info != nil {
			t.Fatalf("accepted non-directory root %q: info=%v err=%v", path, info, err)
		}
	}
}

func BenchmarkCheckoutRootFileInfo(b *testing.B) {
	root := b.TempDir()
	for _, mode := range []string{"pathname_stat", "captured_identity"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var info os.FileInfo
				var err error
				if mode == "pathname_stat" {
					info, err = os.Stat(root)
				} else {
					info, err = checkoutRootFileInfo(root)
				}
				if err != nil || !info.IsDir() {
					b.Fatalf("capture root identity: info=%v err=%v", info, err)
				}
			}
		})
	}
}
