//go:build windows

package pathguard

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mustJunction creates a directory junction. Unlike a symlink it needs no
// privilege, so these tests run on every Windows host. mklink does not check
// the target, which is what makes a dangling junction or a cycle possible.
func mustJunction(t *testing.T, target, link string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinkEscapesJunction(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "pkg"))

	mustJunction(t, outside, filepath.Join(root, "out"))
	mustJunction(t, filepath.Join(root, "pkg"), filepath.Join(root, "in"))
	mustJunction(t, filepath.Join(root, "out"), filepath.Join(root, "hop"))
	mustJunction(t, filepath.Join(outside, "does-not-exist"), filepath.Join(root, "dangling"))
	mustJunction(t, filepath.Join(root, "cycle-b"), filepath.Join(root, "cycle-a"))
	mustJunction(t, filepath.Join(root, "cycle-a"), filepath.Join(root, "cycle-b"))

	cases := []struct {
		name  string
		entry string
		want  bool
	}{
		{"a junction out of the root escapes", "out", true},
		{"a junction within the root does not escape", "in", false},
		{"a junction chained out of the root escapes", "hop", true},
		{"a dangling junction is refused", "dangling", true},
		{"a junction cycle is refused", "cycle-a", true},
		{"an ordinary directory never escapes", "pkg", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.entry)
			if got := SymlinkEscapes(path, root); got != tc.want {
				t.Fatalf("SymlinkEscapes(%q) = %v, want %v", path, got, tc.want)
			}
			if got := EscapesResolvedRoot(path, ResolveRoot(root)); got != tc.want {
				t.Fatalf("EscapesResolvedRoot(%q) = %v, want %v", path, got, tc.want)
			}
		})
	}
}

// TestSymlinkEscapesUnderJunctionedRoot is the junction form of the macOS
// /var -> /private/var shape. ResolveRoot leaves a junction in the root's own
// spelling, so an in-repo junction would look like an escape if only its
// target were resolved.
func TestSymlinkEscapesUnderJunctionedRoot(t *testing.T) {
	outside := t.TempDir()
	base := t.TempDir()
	realRoot := filepath.Join(base, "real", "repo")
	mustMkdir(t, filepath.Join(realRoot, "pkg"))
	mustJunction(t, filepath.Join(realRoot, "pkg"), filepath.Join(realRoot, "in"))
	mustJunction(t, outside, filepath.Join(realRoot, "out"))

	mustJunction(t, filepath.Join(base, "real"), filepath.Join(base, "alias"))
	mustJunction(t, realRoot, filepath.Join(base, "direct"))

	roots := []struct {
		name string
		root string
	}{
		{"a root beneath a junction", filepath.Join(base, "alias", "repo")},
		{"a root that is a junction", filepath.Join(base, "direct")},
	}
	for _, tc := range roots {
		t.Run(tc.name, func(t *testing.T) {
			if SymlinkEscapes(filepath.Join(tc.root, "in"), tc.root) {
				t.Fatal("an in-repo junction must not be treated as an escape")
			}
			if !SymlinkEscapes(filepath.Join(tc.root, "out"), tc.root) {
				t.Fatal("a junction out of the root must be treated as an escape")
			}
			if EscapesResolvedRoot(filepath.Join(tc.root, "in"), ResolveRoot(tc.root)) {
				t.Fatal("an in-repo junction must not be treated as an escape under a resolved root")
			}
		})
	}
}

func TestIsLink(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.go")
	mustWriteFile(t, file)
	dir := filepath.Join(root, "pkg")
	mustMkdir(t, dir)
	junction := filepath.Join(root, "junction")
	mustJunction(t, dir, junction)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"a junction", junction, true},
		{"a directory", dir, false},
		{"a regular file", file, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := os.Lstat(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if got := IsLink(tc.path, info); got != tc.want {
				t.Fatalf("IsLink(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestIsLinkIgnoresOtherReparsePoints covers why ModeIrregular alone is not
// enough: Go also uses it for reparse points that redirect nothing a path walk
// can follow, such as the app execution aliases Windows keeps for Store apps.
func TestIsLinkIgnoresOtherReparsePoints(t *testing.T) {
	aliases, err := filepath.Glob(filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WindowsApps", "*.exe"))
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range aliases {
		info, err := os.Lstat(alias)
		if err != nil || info.Mode()&os.ModeIrregular == 0 {
			continue
		}
		if IsLink(alias, info) {
			t.Fatalf("IsLink(%q) = true for an app execution alias", alias)
		}
		return
	}
	t.Skip("no app execution alias on this Windows host")
}

func TestEvalLinksFollowsJunctions(t *testing.T) {
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "secret.txt"))
	mustMkdir(t, filepath.Join(outside, "nested"))
	mustJunction(t, filepath.Join(outside, "nested"), filepath.Join(outside, "back"))

	root := t.TempDir()
	mustJunction(t, outside, filepath.Join(root, "out"))
	mustJunction(t, filepath.Join(root, "out"), filepath.Join(root, "hop"))

	// outside has no junction above it, so the standard library can spell the
	// expected location independently of the code under test.
	realOutside, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(realOutside, "secret.txt")

	cases := []struct {
		name string
		path string
		want string
	}{
		{"the junction itself", filepath.Join(root, "out"), realOutside},
		{"a file beneath a junction", filepath.Join(root, "out", "secret.txt"), secret},
		{"a chain of junctions", filepath.Join(root, "hop", "secret.txt"), secret},
		{"a junction beneath a junction", filepath.Join(root, "out", "back"), filepath.Join(realOutside, "nested")},
		{"another spelling of a path through a junction", strings.ToUpper(filepath.Join(root, "out", "secret.txt")), secret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalLinks(tc.path)
			if err != nil {
				t.Fatalf("EvalLinks(%q): %v", tc.path, err)
			}
			if got != tc.want {
				t.Fatalf("EvalLinks(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestEvalLinksFollowsSymlinkToJunction(t *testing.T) {
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "secret.txt"))
	root := t.TempDir()
	mustJunction(t, outside, filepath.Join(root, "out"))
	mustSymlink(t, filepath.Join(root, "out"), filepath.Join(root, "sym"))

	realOutside, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EvalLinks(filepath.Join(root, "sym", "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(realOutside, "secret.txt"); got != want {
		t.Fatalf("EvalLinks through a symlink to a junction = %q, want %q", got, want)
	}
}

func TestEvalLinksReportsWhatDoesNotExist(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	mustJunction(t, outside, filepath.Join(root, "out"))
	mustJunction(t, filepath.Join(outside, "does-not-exist"), filepath.Join(root, "dangling"))

	for _, path := range []string{
		filepath.Join(root, "out", "absent.txt"),
		filepath.Join(root, "dangling"),
		filepath.Join(root, "dangling", "absent.txt"),
	} {
		if got, err := EvalLinks(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("EvalLinks(%q) = %q, %v, want a not-exist error", path, got, err)
		}
	}
}

func TestEvalLinksRefusesJunctionCycle(t *testing.T) {
	root := t.TempDir()
	mustJunction(t, filepath.Join(root, "b"), filepath.Join(root, "a"))
	mustJunction(t, filepath.Join(root, "a"), filepath.Join(root, "b"))

	if got, err := EvalLinks(filepath.Join(root, "a", "file.txt")); err == nil {
		t.Fatalf("EvalLinks through a junction cycle = %q, want an error", got)
	}
}

func TestLinkDestination(t *testing.T) {
	cases := []struct {
		name   string
		link   string
		target string
		want   string
	}{
		{"an absolute target", `C:\repo\link`, `D:\elsewhere`, `D:\elsewhere`},
		{"a UNC target", `C:\repo\link`, `\\host\share\dir`, `\\host\share\dir`},
		{"a drive-relative target", `C:\repo\link`, `D:elsewhere`, `D:elsewhere`},
		{"a target rooted on the link's volume", `D:\repo\link`, `\Windows`, `D:\Windows`},
		{"a target beside the link", `C:\repo\pkg\link`, `other`, `C:\repo\pkg\other`},
		{"a target above the link", `C:\repo\pkg\link`, `..\other`, `C:\repo\other`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkDestination(tc.link, tc.target); got != tc.want {
				t.Fatalf("linkDestination(%q, %q) = %q, want %q", tc.link, tc.target, got, tc.want)
			}
		})
	}
}
