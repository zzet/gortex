package search

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProjectNameDeWeight proves project-name detection (go.mod / package.json /
// dir, with the length floor) and that the detected name is stripped from the
// INDEXED path so it no longer contributes a uniform path-field boost — the
// de-weighting that composes with the rerank rather than flattening it.
func TestProjectNameDeWeight(t *testing.T) {
	t.Run("detect_from_go_mod", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "go.mod"), "module github.com/acme/gortex\n\ngo 1.24\n")
		if got := DetectProjectName(dir); got != "gortex" {
			t.Errorf("DetectProjectName=%q, want gortex", got)
		}
	})

	t.Run("detect_from_package_json", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "package.json"), `{"name": "@acme/widgets", "version": "1.0.0"}`)
		if got := DetectProjectName(dir); got != "widgets" {
			t.Errorf("DetectProjectName=%q, want widgets (scoped name de-scoped)", got)
		}
	})

	t.Run("short_name_disabled", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "go.mod"), "module github.com/x/cli\n")
		if got := DetectProjectName(dir); got != "" {
			t.Errorf("DetectProjectName=%q, want \"\" (below the %d-char floor)", got, projectNameMinLen)
		}
	})

	t.Run("strip_project_name_segment", func(t *testing.T) {
		// The repo-root segment is dropped; the rest of the path is intact.
		if got := StripProjectNameFromPath("gortex/internal/auth/token.go", "gortex"); got != "internal/auth/token.go" {
			t.Errorf("StripProjectNameFromPath=%q, want internal/auth/token.go", got)
		}
		// A path that does not contain the project name is unchanged.
		if got := StripProjectNameFromPath("internal/auth/token.go", "gortex"); got != "internal/auth/token.go" {
			t.Errorf("unchanged path mutated: %q", got)
		}
		// A short / empty project name never strips.
		if got := StripProjectNameFromPath("cli/main.go", "cli"); got != "cli/main.go" {
			t.Errorf("short project name should not strip: got %q", got)
		}
	})
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// DetectProjectNameWith takes every root manifest it reads through the reader
// it is given — go.mod, and package.json only when go.mod names nothing — so
// a caller that accounts for its working-copy reads sees each one.
func TestDetectProjectNameWithReadsThroughTheReader(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "package.json"), `{"name": "@acme/widgets"}`)
	var read []string
	got := DetectProjectNameWith(dir, func(name string) ([]byte, error) {
		read = append(read, filepath.Base(name))
		return os.ReadFile(name)
	})
	if got != "widgets" || len(read) != 2 || read[0] != "go.mod" || read[1] != "package.json" {
		t.Fatalf("DetectProjectNameWith = %q reading %v, want widgets reading go.mod then package.json", got, read)
	}
}
