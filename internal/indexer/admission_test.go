package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// newAdmissionTestIndexer mirrors production on the two axes a fixture can
// silently diverge on: the LAYERED exclude list (config.Default().Index.Exclude
// is intentionally empty, so passing it asserts a branch production never
// takes) and the full registry (a hand-picked one manufactures unindexable
// verdicts for .sql / .sh / .parquet that production never produces).
func newAdmissionTestIndexer(t testing.TB, root string, extra ...string) *Indexer {
	t.Helper()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default().Index
	cfg.Exclude = append(append([]string{}, excludes.Builtin...), extra...)
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	idx.storeRootPath(root)
	return idx
}

// writeExcludeFixture is writeFile plus parent directories.
func writeExcludeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, body)
}

// mustIndexability fails when the indexer cannot answer at all.
func mustIndexability(t *testing.T, idx *Indexer, path string) PathSkip {
	t.Helper()
	skip, ok := idx.PathIndexability(path)
	if !ok {
		t.Fatalf("PathIndexability(%q) could not answer; expected a verdict", path)
	}
	return skip
}

// The walk prunes a vendored directory; the hook probes a file underneath it,
// so both the builtin list and a repo-local `exclude:` must reach the file.
func TestPathIndexability_ExcludeRules(t *testing.T) {
	repo := t.TempDir()
	writeExcludeFixture(t, filepath.Join(repo, "main.go"), "package app\n")
	writeExcludeFixture(t, filepath.Join(repo, "node_modules", "dpack", "lib", "Block.js"), "module.exports = 1\n")
	writeExcludeFixture(t, filepath.Join(repo, "generated", "api.go"), "package gen\n")

	idx := newAdmissionTestIndexer(t, repo, "generated/")

	for _, tc := range []struct {
		path string
		want PathSkip
	}{
		{"main.go", PathSkip{}},
		{"node_modules/dpack/lib/Block.js", PathSkip{Skipped: true, ByRule: true}},
		{"generated/api.go", PathSkip{Skipped: true, ByRule: true}},
	} {
		if got := mustIndexability(t, idx, tc.path); got != tc.want {
			t.Errorf("PathIndexability(%q) = %+v, want %+v", tc.path, got, tc.want)
		}
	}
}

// .gortexignore / .ignore / .rgignore: a second source the flat list never sees.
func TestPathIndexability_DirectoryIgnoreFile(t *testing.T) {
	repo := t.TempDir()
	writeExcludeFixture(t, filepath.Join(repo, "tools", ".gortexignore"), "scratch/\n")
	writeExcludeFixture(t, filepath.Join(repo, "tools", "scratch", "hack.go"), "package scratch\n")
	writeExcludeFixture(t, filepath.Join(repo, "tools", "real.go"), "package tools\n")

	idx := newAdmissionTestIndexer(t, repo)

	if got := mustIndexability(t, idx, "tools/scratch/hack.go"); !got.Skipped || !got.ByRule {
		t.Errorf("a .gortexignore in an ancestor directory must exclude the file, got %+v", got)
	}
	if got := mustIndexability(t, idx, "tools/real.go"); got.Skipped {
		t.Errorf("a sibling not matched by the ignore file must not be excluded, got %+v", got)
	}
}

// A language production claims must be indexable. With a hand-picked registry a
// .sql migration reads as unclaimed — the verdict that silences the read door.
func TestPathIndexability_ProductionClaimedLanguages(t *testing.T) {
	repo := t.TempDir()
	for _, f := range []struct{ path, body string }{
		{"main.go", "package app\n"},
		{"db/0042.sql", "select 1;\n"},
		{"scripts/deploy.sh", "#!/bin/sh\necho hi\n"},
		{"web/app.ts", "export const a = 1\n"},
	} {
		writeExcludeFixture(t, filepath.Join(repo, filepath.FromSlash(f.path)), f.body)
	}

	idx := newAdmissionTestIndexer(t, repo)

	for _, path := range []string{"main.go", "db/0042.sql", "scripts/deploy.sh", "web/app.ts"} {
		if got := mustIndexability(t, idx, path); got.Skipped {
			t.Errorf("PathIndexability(%q) = %+v, want indexable — production registers an extractor for it", path, got)
		}
	}
}

// Unclaimed extension: skipped, but not by rule — the guidance renders both.
func TestPathIndexability_UnclaimedLanguage(t *testing.T) {
	repo := t.TempDir()
	writeExcludeFixture(t, filepath.Join(repo, "assets", "logo.sketch"), "binary-ish\n")

	idx := newAdmissionTestIndexer(t, repo)

	got := mustIndexability(t, idx, "assets/logo.sketch")
	if !got.Skipped || got.ByRule {
		t.Errorf("PathIndexability of an unclaimed extension = %+v, want skipped but not by rule", got)
	}
}

// MaxFileSize defaults to 0, so the cap is the one opt-in admission rule.
func TestPathIndexability_SizeCap(t *testing.T) {
	repo := t.TempDir()
	writeExcludeFixture(t, filepath.Join(repo, "big.go"), "package app\n"+strings.Repeat("x", 4096))

	idx := newAdmissionTestIndexer(t, repo)
	if got := mustIndexability(t, idx, "big.go"); got.Skipped {
		t.Fatalf("with no cap configured the file must be indexable, got %+v", got)
	}

	idx.config.MaxFileSize = 64
	got := mustIndexability(t, idx, "big.go")
	if !got.Skipped || got.ByRule {
		t.Errorf("PathIndexability over the size cap = %+v, want skipped but not by rule", got)
	}
}

// The walk drops data assets unless index_data is on; the probe used to stop
// after the size cap and call them indexable.
func TestPathIndexability_ContentGate_DataAsset(t *testing.T) {
	repo := t.TempDir()
	writeExcludeFixture(t, filepath.Join(repo, "data", "embeddings.parquet"), "PAR1....\n")

	idx := newAdmissionTestIndexer(t, repo)
	if !idx.config.Content.IndexData {
		got := mustIndexability(t, idx, "data/embeddings.parquet")
		if !got.Skipped || got.ByRule {
			t.Errorf("a data asset with index_data off = %+v, want skipped but not by rule", got)
		}
	}

	idx.config.Content.IndexData = true
	if got := mustIndexability(t, idx, "data/embeddings.parquet"); got.Skipped {
		t.Errorf("with index_data on the data asset must be admitted, got %+v", got)
	}
}

// Size-driven, so it also proves the probe feeds the gate a real size.
func TestPathIndexability_ContentGate_DocumentCap(t *testing.T) {
	repo := t.TempDir()
	writeExcludeFixture(t, filepath.Join(repo, "docs", "handbook.txt"), strings.Repeat("a", 4096))

	idx := newAdmissionTestIndexer(t, repo)
	if got := mustIndexability(t, idx, "docs/handbook.txt"); got.Skipped {
		t.Skipf("no document extractor claims .txt in this build (%+v); nothing to cap", got)
	}

	idx.config.Content.MaxDocumentBytes = 64
	if got := mustIndexability(t, idx, "docs/handbook.txt"); !got.Skipped || got.ByRule {
		t.Errorf("a document over the cap = %+v, want skipped but not by rule", got)
	}
}

// SkipUntrackedAssets governs the cold full-index walk alone; the incremental
// watcher keeps only the size / class caps. So the probe must not apply it —
// reporting "never indexable" for a file the watcher indexes on the next save
// is the silence this PR exists to remove.
func TestPathIndexability_IgnoresTheUntrackedAssetGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "t@example.com")
	runGit(t, repo, "config", "user.name", "T")
	writeExcludeFixture(t, filepath.Join(repo, "docs", "tracked.txt"), "committed\n")
	runGit(t, repo, "add", "docs/tracked.txt")
	runGit(t, repo, "commit", "-m", "seed")
	writeExcludeFixture(t, filepath.Join(repo, "docs", "untracked.txt"), "scratch\n")

	idx := newAdmissionTestIndexer(t, repo)
	if got := mustIndexability(t, idx, "docs/untracked.txt"); got.Skipped {
		t.Skipf("no asset extractor claims .txt in this build (%+v); nothing to gate", got)
	}

	idx.config.SkipUntrackedAssets = true
	if got := mustIndexability(t, idx, "docs/untracked.txt"); got.Skipped {
		t.Errorf("an untracked asset must stay admitted for the probe, got %+v", got)
	}
	if got := mustIndexability(t, idx, "docs/tracked.txt"); got.Skipped {
		t.Errorf("a git-tracked asset must stay admitted, got %+v", got)
	}
}

// The exclude stage must run before language detection: effectiveLanguage
// falls through to readSniffPrefix, which opens the file, and shouldExclude is
// what refuses a symlink pointing out of the repo. Language-first sniffed the
// target first.
func TestAdmitWalkEntry_SymlinkEscapeRefusedBeforeSniff(t *testing.T) {
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.conf")
	writeFile(t, outside, "#!/usr/bin/env python3\nSECRET = 1\n")
	link := filepath.Join(repo, "pwn.unclaimedext")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	idx := newAdmissionTestIndexer(t, repo)

	adm := idx.admitWalkEntry(repo, link, 1, false)
	if adm.admit {
		t.Fatal("an escaping symlink must never be admitted")
	}
	if !adm.excluded {
		t.Error("the confinement guard should be what rejects it, so excluded must be set")
	}
	if adm.lang != "" {
		t.Errorf("the escaping symlink's TARGET was opened and sniffed (language %q) before the "+
			"confinement guard refused the link; the exclude rules must run first", adm.lang)
	}

	if got := mustIndexability(t, idx, "pwn.unclaimedext"); got != (PathSkip{Skipped: true, ByRule: true}) {
		t.Errorf("PathIndexability of an escaping symlink = %+v, want skipped by rule", got)
	}
}
