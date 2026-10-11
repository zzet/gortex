package contracts

import (
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSpringConfigAcceptedSourceDoesNotReadCurrentFilesystem(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo/application.yml", Kind: graph.KindFile, Name: "application.yml", FilePath: "repo/application.yml", Language: "yaml", RepoPrefix: "repo", WorkspaceID: "workspace"})
	calls := 0
	scope := SpringConfigScope{RepoPrefix: "repo", RepoRoot: "/unavailable/accepted-snapshot", WorkspaceID: "workspace", ReadSource: func(path string) ([]byte, error) {
		calls++
		if path != "repo/application.yml" {
			t.Fatalf("source path=%q", path)
		}
		return []byte("server:\n  port: 8080\n"), nil
	}}
	BindSpringConfig(g, scope)
	if calls != 1 || g.GetNode(scopedSpringConfigKeyID(scope, "server.port")) == nil {
		t.Fatal("accepted config source was not materialized")
	}
	failed := graph.New()
	failed.AddNode(&graph.Node{ID: "repo/application.yml", Kind: graph.KindFile, Name: "application.yml", FilePath: "repo/application.yml", Language: "yaml", RepoPrefix: "repo", WorkspaceID: "workspace"})
	scope.ReadSource = func(string) ([]byte, error) { return nil, errors.New("accepted bytes unavailable") }
	BindSpringConfig(failed, scope)
	if failed.GetNode(scopedSpringConfigKeyID(scope, "server.port")) != nil {
		t.Fatal("failed accepted source invented config key")
	}
}
