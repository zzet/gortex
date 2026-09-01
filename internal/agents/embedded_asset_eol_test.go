package agents

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// embeddedTextAssets are the text files this tree hands to a user
// verbatim: go:embed compiles them into the binary and the adapters write
// them into a project unchanged. Their bytes are therefore a shipping
// artefact, not a source detail, and a checkout that rewrites their line
// endings changes what users receive.
var embeddedTextAssets = []string{
	"internal/agents/opencode/plugin/gortex.js",
	"internal/agents/pi/extension/index.ts",
}

// TestEmbeddedTextAssetsArePinnedToLF guards the `* text=auto eol=lf` line
// in .gitattributes from both directions.
//
// The byte half catches a checkout that already converted: Git for Windows
// installs with core.autocrlf=true, so without the attribute these files
// arrive CRLF and the binary ships CRLF. The attribute half catches the
// removal itself, and is the half that still binds on a runner configured
// not to convert — where a byte assertion would pass no matter what
// .gitattributes says.
//
// TestPluginFailsOpen detects the same corruption, but only as a side
// effect of splitting the plugin source on "\n}\n"; this states the
// contract directly and covers the pi extension too.
func TestEmbeddedTextAssetsArePinnedToLF(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, err := moduleRootFrom(wd)
	if err != nil {
		t.Fatalf("locating the module root: %v", err)
	}

	// The attribute half needs the assets to belong to the checkout whose
	// attributes it is about to read. A copied, vendored or nested tree
	// can sit inside some *other* work tree, and asking that one about
	// these paths answers a different question — or, as this test did
	// before, addresses files that are not there at all.
	gitRoot, gitErr := gitTopLevel(root)
	attributesApply := gitErr == nil && sameDir(gitRoot, root)

	for _, rel := range embeddedTextAssets {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if i := strings.Index(string(body), "\r\n"); i >= 0 {
			t.Errorf("%s: embedded asset has a CRLF line ending at byte %d — "+
				"this checkout converted it and the binary would ship it that way; "+
				"check the `* text=auto eol=lf` line in .gitattributes", rel, i)
		}

		if !attributesApply {
			continue
		}
		out, err := exec.Command("git", "-C", root, "check-attr", "eol", "--", rel).Output()
		if err != nil {
			t.Errorf("%s: git check-attr: %v", rel, err)
			continue
		}
		// `<path>: eol: <value>`; unset attributes report "unspecified".
		line := string(out)
		if got := strings.TrimSpace(line[strings.LastIndex(line, ":")+1:]); got != "lf" {
			t.Errorf("%s: eol attribute is %q, want \"lf\" — a Windows checkout "+
				"will convert this embedded asset to CRLF", rel, got)
		}
	}

	if !attributesApply {
		t.Logf("module root %s is not its own git work tree (%v); "+
			"asserted the embedded bytes only, skipped the attribute half", root, gitErr)
	}
}

// TestModuleRootFromIgnoresAnEnclosingCheckout is the regression case for
// the layout that broke this file: the source placed under some other
// repository, as a vendored copy, a monorepo subdirectory or a packaging
// staging tree. `git rev-parse --show-toplevel` answers for the enclosing
// work tree, which is the wrong tree and often does not contain these
// paths at all, so the root has to come from the module instead.
func TestModuleRootFromIgnoresAnEnclosingCheckout(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "src")
	start := filepath.Join(inner, "internal", "agents")

	// An enclosing work tree *and* an enclosing module, so neither a git
	// lookup nor a sloppy upward walk can accidentally pass.
	for _, dir := range []string{filepath.Join(outer, ".git"), start} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(outer, "go.mod"),
		filepath.Join(inner, "go.mod"),
	} {
		if err := os.WriteFile(f, []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := moduleRootFrom(start)
	if err != nil {
		t.Fatalf("moduleRootFrom(%s): %v", start, err)
	}
	if !sameDir(got, inner) {
		t.Errorf("moduleRootFrom(%s) = %s, want the nearest module root %s", start, got, inner)
	}
}

// moduleRootFrom walks up from dir to the nearest directory holding a
// go.mod. That is the module this test belongs to, whatever repository
// happens to contain it.
func moduleRootFrom(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod at or above %s", dir)
		}
		dir = parent
	}
}

// gitTopLevel reports the work tree dir owns, if any.
func gitTopLevel(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sameDir compares two directories by identity rather than by spelling.
// git answers with forward slashes on Windows while filepath produces
// backslashes, and either side may arrive through a symlink or a
// different case, so string equality is the wrong test.
func sameDir(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}
