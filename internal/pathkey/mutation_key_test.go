package pathkey

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMutationKeyJoinsAliasSpellings(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(base, "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	same := []struct {
		name string
		a, b string
	}{
		{name: "symlinked directory", a: filepath.Join(realDir, "a.go"), b: filepath.Join(aliasDir, "a.go")},
		{name: "file not created yet", a: filepath.Join(realDir, "new.go"), b: filepath.Join(aliasDir, "new.go")},
		{name: "dot segments", a: filepath.Join(realDir, "a.go"), b: realDir + string(filepath.Separator) + "." + string(filepath.Separator) + "a.go"},
	}
	if runtime.GOOS == "darwin" {
		same = append(same, struct {
			name string
			a, b string
		}{name: "tmp alias", a: "/tmp/gortex-mutation-key/a.go", b: "/private/tmp/gortex-mutation-key/a.go"})
	}
	for _, tc := range same {
		t.Run(tc.name, func(t *testing.T) {
			if ka, kb := MutationKey(tc.a), MutationKey(tc.b); ka != kb {
				t.Fatalf("MutationKey(%q) = %q, MutationKey(%q) = %q; want one key", tc.a, ka, tc.b, kb)
			}
		})
	}

	t.Run("case variant on a case-insensitive volume", func(t *testing.T) {
		prev := CaseInsensitivePaths
		CaseInsensitivePaths = true
		t.Cleanup(func() { CaseInsensitivePaths = prev })
		a := filepath.Join(realDir, "a.go")
		for _, b := range []string{filepath.Join(realDir, "A.go"), filepath.Join(base, "REAL", "a.go"), filepath.Join(aliasDir, "A.GO")} {
			if ka, kb := MutationKey(a), MutationKey(b); ka != kb {
				t.Fatalf("MutationKey(%q) = %q, MutationKey(%q) = %q; want one key", a, ka, b, kb)
			}
		}
	})

	t.Run("leaf symlink is its own file", func(t *testing.T) {
		link := filepath.Join(realDir, "link.go")
		if err := os.Symlink(filepath.Join(realDir, "a.go"), link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if MutationKey(link) == MutationKey(filepath.Join(realDir, "a.go")) {
			t.Fatalf("MutationKey followed the leaf symlink %q", link)
		}
	})
}
