package indexer

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/indexer/source"
)

type admissionOpenRecorder struct {
	source.ContentSource
	opens int
}

func (s *admissionOpenRecorder) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	s.opens++
	return s.ContentSource.Open(path)
}

func TestAdmitWalkFileKnownType_ExclusionPrecedesSniff(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"assets/script.unknown", "node_modules/pkg/script.unknown", "generated/script.unknown", ".claude/state.unknown"} {
		writeExcludeFixture(t, filepath.Join(root, filepath.FromSlash(rel)), "#!/usr/bin/env python3\npass\n")
	}
	idx := newAdmissionTestIndexer(t, root, "generated/")
	fsSource, err := source.NewFilesystemSource(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsSource.Close() })
	recorder := &admissionOpenRecorder{ContentSource: fsSource}
	idx.SetContentSource(recorder)

	for _, tc := range []struct {
		rel      string
		excluded bool
	}{
		{"assets/script.unknown", false},
		{"node_modules/pkg/script.unknown", true},
		{"generated/script.unknown", true},
		{".claude/state.unknown", true},
	} {
		t.Run(tc.rel, func(t *testing.T) {
			path := filepath.Join(root, filepath.FromSlash(tc.rel))
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			recorder.opens = 0
			adm := idx.admitWalkFileKnownType(root, path, info.Size(), info.Mode())
			if tc.excluded {
				if adm != (walkAdmission{excluded: true}) || recorder.opens != 0 {
					t.Fatalf("excluded file: admission=%+v, content opens=%d", adm, recorder.opens)
				}
				if got := mustIndexability(t, idx, tc.rel); got != (PathSkip{Skipped: true, ByRule: true}) {
					t.Fatalf("excluded file lost rule attribution: %+v", got)
				}
			} else if !adm.admit || adm.lang != "python" || recorder.opens != 1 {
				t.Fatalf("included shebang file: admission=%+v, content opens=%d", adm, recorder.opens)
			}
		})
	}
}

func TestAdmitWalkFileKnownType_EscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.unknown")
	writeFile(t, outside, "#!/usr/bin/env python3\nSECRET = 1\n")
	link := filepath.Join(root, "escape.unknown")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	idx := newAdmissionTestIndexer(t, root)
	fsSource, err := source.NewFilesystemSource(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsSource.Close() })
	recorder := &admissionOpenRecorder{ContentSource: fsSource}
	idx.SetContentSource(recorder)
	// Windows reparse points can report ModeIrregular instead of ModeSymlink.
	// Every nonregular mode must retain the original confinement check.
	for _, mode := range []os.FileMode{info.Mode(), os.ModeIrregular} {
		adm := idx.admitWalkFileKnownType(root, link, info.Size(), mode)
		if adm != (walkAdmission{excluded: true}) || recorder.opens != 0 {
			t.Fatalf("escaping link mode=%v: admission=%+v, content opens=%d", mode, adm, recorder.opens)
		}
	}
}

func TestPathIndexability_UnclaimedExcludedLanguage(t *testing.T) {
	root := t.TempDir()
	writeExcludeFixture(t, filepath.Join(root, "scratch", "asset.unknown"), "binary-ish\n")
	writeExcludeFixture(t, filepath.Join(root, "scratch", ".gortexignore"), "asset.unknown\n")
	idx := newAdmissionTestIndexer(t, root)
	if got := mustIndexability(t, idx, "scratch/asset.unknown"); got != (PathSkip{Skipped: true, ByRule: true}) {
		t.Fatalf("unclaimed file excluded by a local ignore rule: %+v", got)
	}
}
