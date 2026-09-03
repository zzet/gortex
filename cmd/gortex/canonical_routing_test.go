package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func statusWithTrackedPath(t *testing.T, path string) daemon.StatusResponse {
	t.Helper()
	var status daemon.StatusResponse
	tracked := reflect.ValueOf(&status).Elem().FieldByName("TrackedRepos")
	if !tracked.IsValid() || tracked.Kind() != reflect.Slice {
		t.Fatal("daemon.StatusResponse.TrackedRepos is not a slice")
	}
	repo := reflect.New(tracked.Type().Elem()).Elem()
	pathField := repo.FieldByName("Path")
	if !pathField.IsValid() || !pathField.CanSet() || pathField.Kind() != reflect.String {
		t.Fatal("daemon tracked-repository status has no settable Path field")
	}
	pathField.SetString(path)
	tracked.Set(reflect.Append(tracked, repo))
	return status
}

func TestTrackedReposReachCanonicalRootAndCWDAliases(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	realRepo := filepath.Join(realRoot, "repo")
	realCWD := filepath.Join(realRepo, "nested")
	if err := os.MkdirAll(realCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasRepo := filepath.Join(aliasRoot, "repo")
	aliasCWD := filepath.Join(aliasRepo, "nested")

	for _, tc := range []struct {
		name string
		root string
		cwd  string
	}{
		{name: "canonical root canonical cwd", root: realRepo, cwd: realCWD},
		{name: "canonical root alias cwd", root: realRepo, cwd: aliasCWD},
		{name: "alias root canonical cwd", root: aliasRepo, cwd: realCWD},
		{name: "alias root alias cwd", root: aliasRepo, cwd: aliasCWD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := statusWithTrackedPath(t, tc.root)
			if !trackedRootContains(status, tc.cwd) {
				t.Fatalf("trackedRootContains rejected cwd %q under root %q", tc.cwd, tc.root)
			}
			if !trackedReposReach(status, tc.cwd) {
				t.Fatalf("trackedReposReach rejected cwd %q under root %q", tc.cwd, tc.root)
			}
		})
	}
}
