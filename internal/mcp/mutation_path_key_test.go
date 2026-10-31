package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/pathkey"
)

// Two spellings of one file must share one mutation lock, or two edits of the
// file interleave and one of them is lost.
func TestMutationPathLockSerializesAliasSpellings(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(base, "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	realPath := filepath.Join(realDir, "a.go")
	if err := os.WriteFile(realPath, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	aliases := []string{filepath.Join(aliasDir, "a.go")}
	if pathkey.CaseInsensitivePaths {
		aliases = append(aliases, filepath.Join(realDir, "A.go"))
	}
	for _, alias := range aliases {
		t.Run(filepath.Base(filepath.Dir(alias))+"/"+filepath.Base(alias), func(t *testing.T) {
			release, err := acquireMutationPath(context.Background(), realPath)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			aliasRelease, err := acquireMutationPath(ctx, alias)
			if err == nil {
				aliasRelease()
				release()
				t.Fatalf("%s acquired the mutation lock while %s held it", alias, realPath)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				release()
				t.Fatalf("alias acquire error = %v, want it to wait for the holder", err)
			}
			release()

			// One request naming both spellings takes the lock once.
			ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			both, err := acquireMutationPaths(ctx, []string{realPath, alias})
			if err != nil {
				t.Fatalf("acquiring both spellings in one request: %v", err)
			}
			both()
		})
	}
}
