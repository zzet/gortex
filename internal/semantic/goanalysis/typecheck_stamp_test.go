package goanalysis

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatStampDetectsReplacementWithEqualSizeAndTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.go")
	replacement := filepath.Join(dir, "replacement.go")
	mtime := time.Unix(1700000000, 0)
	for _, name := range []string{path, replacement} {
		if err := os.WriteFile(name, []byte("package p\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	before, ok := statStamp(path)
	if !ok {
		t.Fatal("could not stamp source")
	}
	if again, ok := statStamp(path); !ok || again != before {
		t.Fatalf("unchanged file stamp = %+v, %v; want %+v", again, ok, before)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	after, ok := statStamp(path)
	if !ok || after == before {
		t.Fatalf("replacement stamp = %+v, %v; must differ from %+v", after, ok, before)
	}
	if after.size != before.size || after.mtime != before.mtime {
		t.Fatalf("fixture changed size/time: before %+v, after %+v", before, after)
	}
}

func TestStatStampSharesHardlinkIdentityAndRejectsMissingOrDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.go")
	alias := filepath.Join(dir, "alias.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	stamp, ok := statStamp(path)
	if !ok {
		t.Fatal("could not stamp source")
	}
	if linked, ok := statStamp(alias); !ok || linked != stamp {
		t.Fatalf("hardlink stamp = %+v, %v; want %+v", linked, ok, stamp)
	}
	for _, name := range []string{dir, filepath.Join(dir, "missing.go")} {
		if stamp, ok := statStamp(name); ok {
			t.Fatalf("stamped non-file %s: %+v", name, stamp)
		}
	}
}
