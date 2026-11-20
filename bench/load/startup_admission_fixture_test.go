package main

import (
	"context"
	"testing"
)

// A re-run into the same trial dir must succeed: the fixture clears any stale
// committed corpus before rebuilding, otherwise `git commit` has nothing to
// commit and fails. Regression for the prepared-arm re-run that errored on
// trials 0-8 because earlier trial directories were still present.
func TestStartupAdmissionFixtureRerunIntoSameDir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	first, err := buildStartupAdmissionFixture(ctx, root, "alpha", 2, 1, 1)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := buildStartupAdmissionFixture(ctx, root, "alpha", 2, 1, 1)
	if err != nil {
		t.Fatalf("re-run into same dir: %v", err)
	}
	if first.Marker != second.Marker {
		t.Fatalf("marker changed across re-run: %q vs %q", first.Marker, second.Marker)
	}
	if second.Corpus.Commit == "" {
		t.Fatal("re-run produced no commit")
	}
}
