package config

import (
	"reflect"
	"testing"
)

func TestConfiguredRepoRootsIncludesUnloadedProjectDeclarations(t *testing.T) {
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: "/first"}, {Path: ""}, {Path: "/first"}}, Projects: map[string]ProjectConfig{
		"not-active": {Repos: []RepoEntry{{Path: "/unregistered"}, {Path: "/first"}}},
	}}
	want := []string{"/first", "/unregistered"}
	got := gc.ConfiguredRepoRoots()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("configured inventory omitted unloaded roots: %v", got)
	}
	got[0] = "/changed-copy"
	if !reflect.DeepEqual(gc.ConfiguredRepoRoots(), want) {
		t.Fatal("snapshot mutated live configuration")
	}
	var absent *GlobalConfig
	if absent.ConfiguredRepoRoots() != nil {
		t.Fatal("nil configuration has roots")
	}
}
