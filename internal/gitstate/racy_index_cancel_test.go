package gitstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRefreshRacyIndexPreservesInjectedRunnerAndCancellation(t *testing.T) {
	repo := makeRacyIndexRepo(t, 800)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := false
	sampler := newDirtySampler(repo, "", "", func(context.Context, string, ...string) ([]byte, error) {
		called = true
		// A lock created while the injected operation is active must never be
		// mistaken for a successful refresh or removed by the application.
		if err := os.WriteFile(filepath.Join(repo, ".git", "index.lock"), []byte("external lock witness"), 0600); err != nil {
			t.Fatal(err)
		}
		cancel()
		return nil, errors.New("runner interrupted")
	})
	_, _, ran, err := sampler.RefreshRacyIndex(ctx)
	if !called || !ran || !errors.Is(err, context.Canceled) {
		t.Fatalf("injected cancellation lost: called=%t ran=%t err=%v", called, ran, err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, ".git", "index.lock")); err != nil || string(data) != "external lock witness" {
		t.Fatalf("application touched unknown lock: %q %v", data, err)
	}
}
