package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/testenv"
)

func TestResolveEmbeddedIndexCanonicalizesExplicitAlias(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	want, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Clean(want)

	plan := resolveEmbeddedIndex(aliasRoot, "", nil)
	if plan.Index != want {
		t.Fatalf("index root = %q, want canonical %q", plan.Index, want)
	}
	if plan.Notebook != want {
		t.Fatalf("notebook root = %q, want canonical %q", plan.Notebook, want)
	}
}

func TestRunTrackCanonicalizesAliasBeforePersistenceAndRegistration(t *testing.T) {
	testenv.Sandbox(t)
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	want := pathkey.CanonicalExistingRoot(aliasRoot)

	oldEnsure := trackEnsureDaemonReadyFn
	oldNotify := trackNotifyDaemonTrackFn
	oldWait, oldName, oldAsWorktree := trackWait, trackName, trackAsWorktree
	t.Cleanup(func() {
		trackEnsureDaemonReadyFn = oldEnsure
		trackNotifyDaemonTrackFn = oldNotify
		trackWait, trackName, trackAsWorktree = oldWait, oldName, oldAsWorktree
	})
	trackWait, trackName, trackAsWorktree = false, "", false
	ensureSlot := reflect.ValueOf(&trackEnsureDaemonReadyFn).Elem()
	ensureSlot.Set(reflect.MakeFunc(ensureSlot.Type(), func([]reflect.Value) []reflect.Value {
		return []reflect.Value{reflect.ValueOf(daemonReady)}
	}))
	var registered []string
	trackNotifyDaemonTrackFn = func(path string, _ time.Time) error {
		registered = append(registered, path)
		return nil
	}

	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	for _, root := range []string{aliasRoot, realRoot} {
		if err := runTrack(cmd, []string{root}); err != nil {
			t.Fatal(err)
		}
	}
	if len(registered) != 2 {
		t.Fatalf("daemon registrations = %d, want 2", len(registered))
	}
	for _, got := range registered {
		if got != want {
			t.Fatalf("daemon registration path = %q, want canonical %q", got, want)
		}
	}
	global, err := config.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	if len(global.Repos) != 1 {
		t.Fatalf("persisted repos after alias and canonical registration = %d, want 1", len(global.Repos))
	}
	if got := global.Repos[0].Path; got != want {
		t.Fatalf("persisted repo path = %q, want canonical %q", got, want)
	}
}
