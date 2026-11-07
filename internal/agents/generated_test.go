package agents

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeLister []OwnedFile

func (f fakeLister) OwnedFiles(Env) []OwnedFile { return f }

// owned renders "new" for any copy and recognises "old" and "new" as
// shipped renders, the way an adapter vouches for a pre-manifest file. A
// copy it cannot read back ("garbage") renders as itself.
func owned(path string) OwnedFile {
	return OwnedFile{Path: path, Render: func(existing []byte) (string, bool) {
		if string(existing) == "garbage" {
			return string(existing), false
		}
		return "new", string(existing) == "old" || string(existing) == "new"
	}}
}

func TestRefreshOwnedFilesRewritesOnlyWhatGortexProvablyWrote(t *testing.T) {
	dir := t.TempDir()
	path := func(name string) string { return filepath.Join(dir, name) }
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(path(name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("recorded", "v1 render")   // in the manifest, untouched since
	write("edited", "user changes")  // in the manifest, hash no longer matches
	write("legacy", "old")           // no record, but a shipped render
	write("foreign", "someone else") // no record, not a shipped render
	write("current", "new")          // already what this binary writes
	write("garbage", "garbage")      // a copy the adapter cannot read back
	if err := os.Symlink(path("recorded"), path("linked")); err != nil {
		t.Fatal(err)
	}
	prev := &GeneratedManifest{Version: "v1", Files: map[string]string{
		path("recorded"): GeneratedHash([]byte("v1 render")),
		path("edited"):   GeneratedHash([]byte("v1 render")),
	}}
	var files fakeLister
	for _, n := range []string{"recorded", "edited", "legacy", "foreign", "current", "garbage", "linked", "absent"} {
		files = append(files, owned(path(n)))
	}
	listers := []NamedLister{{Name: "fake", Lister: files}}
	env := Env{BinaryVersion: "v2"}

	entries, _, err := RefreshOwnedFiles(listers, env, prev, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]RefreshStatus{
		"recorded": RefreshStale, "edited": RefreshModified, "legacy": RefreshStale,
		"foreign": RefreshUnrecognized, "current": RefreshCurrent,
		"garbage": RefreshUnrecognized, "linked": RefreshUnrecognized,
	}
	assertStatuses(t, entries, want, path)
	if got, _ := os.ReadFile(path("recorded")); string(got) != "v1 render" {
		t.Fatalf("apply=false rewrote a file: %q", got)
	}

	entries, next, err := RefreshOwnedFiles(listers, env, prev, true)
	if err != nil {
		t.Fatal(err)
	}
	want["recorded"], want["legacy"] = RefreshRewritten, RefreshRewritten
	assertStatuses(t, entries, want, path)
	for name, body := range map[string]string{"recorded": "new", "legacy": "new", "edited": "user changes", "foreign": "someone else"} {
		if got, _ := os.ReadFile(path(name)); string(got) != body {
			t.Errorf("%s = %q, want %q", name, got, body)
		}
	}
	if _, err := os.Stat(path("absent")); !os.IsNotExist(err) {
		t.Fatalf("an absent file was created: %v", err)
	}
	if _, isLink := next.Files[path("linked")]; isLink {
		t.Fatal("a symlink was recorded as a generated file")
	}
	if fi, _ := os.Lstat(path("linked")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the refresh replaced a symlink")
	}
	if _, recorded := next.Files[path("garbage")]; recorded {
		t.Fatal("a copy the adapter cannot read back was recorded as generated")
	}
	if next.Version != "v2" || next.Files[path("recorded")] != GeneratedHash([]byte("new")) ||
		next.Files[path("edited")] != prev.Files[path("edited")] {
		t.Fatalf("manifest after refresh = %+v", next)
	}
}

func assertStatuses(t *testing.T, entries []RefreshEntry, want map[string]RefreshStatus, path func(string) string) {
	t.Helper()
	got := map[string]RefreshStatus{}
	for _, e := range entries {
		got[e.Path] = e.Status
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %d", entries, len(want))
	}
	for name, status := range want {
		if got[path(name)] != status {
			t.Errorf("%s: status %q, want %q", name, got[path(name)], status)
		}
	}
}
