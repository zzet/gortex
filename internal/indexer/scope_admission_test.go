package indexer

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const scopeTestBudget = 5 * time.Second

// mustScope fails when the walk could not settle the scope.
func mustScope(t *testing.T, idx *Indexer, rel string) bool {
	t.Helper()
	indexable, walked := idx.ScopeIndexability(rel, scopeTestBudget)
	if !walked {
		t.Fatalf("ScopeIndexability(%q) did not finish; expected a verdict", rel)
	}
	return indexable
}

// The finding this verb exists for. Every one of these is a language with a
// first-class extractor and none carries an extension a hardcoded list would
// recognise, so a scope holding one must never read as source-free.
func TestScopeIndexability_ClaimsUnrecognisedSourceNames(t *testing.T) {
	for _, name := range []string{"main.tf", "Chart.yaml", "playbook.yml", "App.csproj"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			idx := newAdmissionTestIndexer(t, root)
			writeExcludeFixture(t, filepath.Join(root, "infra", name), "x\n")

			if !mustScope(t, idx, "infra") {
				t.Fatalf("a scope holding %s must not read as source-free", name)
			}
		})
	}
}

// The dotfile bias a lexical sample had: the walk must reach the .tf file past
// the dotfiles that sort before it.
func TestScopeIndexability_DotfilesDoNotDecideTheScope(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	for _, name := range []string{".gitattributes", ".gitignore", ".terraform.lock.hcl"} {
		writeExcludeFixture(t, filepath.Join(root, name), "x\n")
	}
	writeExcludeFixture(t, filepath.Join(root, "main.tf"), "resource \"x\" \"y\" {}\n")

	if !mustScope(t, idx, "") {
		t.Fatal("dotfiles sorting first must not settle the repository root as source-free")
	}
}

// The complement: a tree the walk really would claim nothing in is settled.
func TestScopeIndexability_ExcludedTreeIsSourceFree(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	writeExcludeFixture(t, filepath.Join(root, "node_modules", "dpack", "index.js"), "module.exports = {}\n")

	if mustScope(t, idx, "node_modules/dpack") {
		t.Fatal("an excluded tree holds nothing the walk would claim")
	}
}

func TestScopeIndexability_UnclaimedNamesAreSourceFree(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	writeExcludeFixture(t, filepath.Join(root, "docs", "a.bin"), "\x00\x01\n")

	if mustScope(t, idx, "docs") {
		t.Fatal("a tree of files no extractor claims holds no source")
	}
}

// A subtree the walk cannot read may hold source it never saw, so the scope is
// unsettled rather than empty. This is the failure a sampled witness reported
// as proof.
func TestScopeIndexability_UnreadableSubtreeIsNotProof(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not gate the walk here")
	}
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	locked := filepath.Join(root, "scope", "src")
	writeExcludeFixture(t, filepath.Join(locked, "a.go"), "package a\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	indexable, walked := idx.ScopeIndexability("scope", scopeTestBudget)
	if indexable || walked {
		t.Fatalf("an unreadable subtree leaves the scope unsettled, got (indexable=%v, walked=%v)", indexable, walked)
	}
}

// A missing or non-directory scope has no verdict either.
func TestScopeIndexability_AbstainsOffTheWalk(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	writeExcludeFixture(t, filepath.Join(root, "a.go"), "package a\n")

	for _, rel := range []string{"gone", "a.go", "../outside", "/etc"} {
		t.Run(rel, func(t *testing.T) {
			if indexable, walked := idx.ScopeIndexability(rel, scopeTestBudget); indexable || walked {
				t.Errorf("ScopeIndexability(%q) = (%v, %v), want an abstention", rel, indexable, walked)
			}
		})
	}
}

// An exhausted budget is reported as unfinished, never as an empty scope.
func TestScopeIndexability_ExhaustedBudgetIsNotProof(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	for i := range 64 {
		writeExcludeFixture(t, filepath.Join(root, "docs", string(rune('a'+i%26)), "f.bin"), "\x00\n")
	}

	indexable, walked := idx.ScopeIndexability("docs", 0)
	if indexable || walked {
		t.Fatalf("a walk that ran out of budget proves nothing, got (indexable=%v, walked=%v)", indexable, walked)
	}
}

// One claimed file settles the scope however the walk ended, so a budget that
// expires after the hit still yields a verdict.
func TestScopeIndexability_FoundFileSettlesATruncatedWalk(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	writeExcludeFixture(t, filepath.Join(root, "pkg", "a.go"), "package a\n")

	if indexable, walked := idx.ScopeIndexability("pkg", scopeTestBudget); !indexable || !walked {
		t.Fatalf("a claimed file settles the scope, got (indexable=%v, walked=%v)", indexable, walked)
	}
}
