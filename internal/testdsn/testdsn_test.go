package testdsn

import (
	"path/filepath"
	"testing"
)

// The Windows rows are the whole point of this test: they are the only proof
// available on a POSIX developer machine that the helper does not hand SQLite
// a drive letter in the URI authority. The exact strings are asserted rather
// than a HasPrefix, because "starts with file:///" is also true of a DSN whose
// query string or escaping is wrong.
func TestFileURIRendersBothPlatformSpellings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		query string
		want  string
	}{
		{
			name:  "windows drive letter",
			path:  `C:\Users\x\store.sqlite`,
			query: "mode=ro",
			want:  "file:///C:/Users/x/store.sqlite?mode=ro",
		},
		{
			name: "windows drive letter without a query",
			path: `D:\work\gortex\store.sqlite`,
			want: "file:///D:/work/gortex/store.sqlite",
		},
		{
			name:  "windows path with a space",
			path:  `C:\Users\RUNNER~1\App Data\store.sqlite`,
			query: "mode=ro",
			want:  "file:///C:/Users/RUNNER~1/App%20Data/store.sqlite?mode=ro",
		},
		{
			name:  "posix path",
			path:  "/tmp/gortex/store.sqlite",
			query: "mode=ro",
			want:  "file:///tmp/gortex/store.sqlite?mode=ro",
		},
		{
			name: "posix path without a query",
			path: "/tmp/gortex/store.sqlite",
			want: "file:///tmp/gortex/store.sqlite",
		},
		{
			name:  "posix path with a space",
			path:  "/tmp/gortex fixture/store.sqlite",
			query: "mode=ro",
			want:  "file:///tmp/gortex%20fixture/store.sqlite?mode=ro",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileURI(tc.path, tc.query); got != tc.want {
				t.Fatalf("fileURI(%q, %q) = %q; want %q", tc.path, tc.query, got, tc.want)
			}
		})
	}
}

// FileURI is the exported half: it makes the path absolute for the running OS
// and then renders it. An already-absolute path for this platform must survive
// that round trip unchanged.
func TestFileURIAbsolutePathForThisPlatform(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	if !filepath.IsAbs(path) {
		t.Fatalf("t.TempDir() is not absolute: %q", path)
	}
	want := fileURI(path, "mode=ro")
	if got := FileURI(path, "mode=ro"); got != want {
		t.Fatalf("FileURI(%q) = %q; want %q", path, got, want)
	}
}

// A relative path becomes absolute before rendering, so SQLite never resolves
// it against whatever directory the test binary happens to run in.
func TestFileURIMakesRelativePathsAbsolute(t *testing.T) {
	got := FileURI(filepath.Join("relative", "store.sqlite"), "")
	absolute, err := filepath.Abs(filepath.Join("relative", "store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if want := fileURI(absolute, ""); got != want {
		t.Fatalf("FileURI(relative) = %q; want %q", got, want)
	}
}

// An explicit URI or the in-memory sentinel carries settings of its own, so it
// is passed through with the query appended rather than re-rendered.
func TestFileURIPassesThroughExplicitDSNs(t *testing.T) {
	for _, tc := range []struct {
		path  string
		query string
		want  string
	}{
		{path: ":memory:", want: ":memory:"},
		{path: ":memory:", query: "mode=ro", want: ":memory:?mode=ro"},
		{path: "file:/tmp/store.sqlite", query: "mode=ro", want: "file:/tmp/store.sqlite?mode=ro"},
		{path: "file:/tmp/store.sqlite?cache=shared", query: "mode=ro", want: "file:/tmp/store.sqlite?cache=shared&mode=ro"},
	} {
		if got := FileURI(tc.path, tc.query); got != tc.want {
			t.Fatalf("FileURI(%q, %q) = %q; want %q", tc.path, tc.query, got, tc.want)
		}
	}
}
