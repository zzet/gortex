package mcp

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

func lowerWorkspaceEditPrediction(t *testing.T, srv *Server, raw string) *prediction {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"workspace_edit": raw}
	p, err := srv.lowerEditSource(context.Background(), req)
	require.NoError(t, err)
	return p
}

func workspaceEditJSON(t *testing.T, files map[string]string) string {
	t.Helper()
	edit := lsp.WorkspaceEdit{Changes: make(map[string][]lsp.TextEdit, len(files))}
	for file, content := range files {
		edit.Changes[file] = []lsp.TextEdit{{
			Range: lsp.Range{
				Start: lsp.Position{Line: 0, Character: 0},
				End:   lsp.Position{Line: 1 << 20, Character: 0},
			},
			NewText: content,
		}}
	}
	raw, err := json.Marshal(edit)
	require.NoError(t, err)
	return string(raw)
}

func nativeFileURI(path string) string {
	slashPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	return (&url.URL{Scheme: "file", Path: slashPath}).String()
}

func TestLowerEditSourceNativeSeparatorCanonicalizesAbsoluteAndFileURI(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	platform := "POSIX"
	if runtime.GOOS == "windows" {
		platform = "Windows"
	}

	for _, tt := range []struct {
		name string
		path string
	}{
		{name: platform + " absolute path", path: targetFile},
		{name: platform + " file URI", path: nativeFileURI(targetFile)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := lowerWorkspaceEditPrediction(t, srv, workspaceEditJSON(t, map[string]string{
				tt.path: "package main\n\nfunc Target() {}\n",
			}))
			require.Equal(t, []verificationFile{{path: "target.go"}}, p.verificationFiles)

			cmd, err := buildVerificationCommand(p)
			require.NoError(t, err)
			require.Equal(t, "go build ./... && go test -race ./...", cmd)
			require.NotContains(t, filepath.ToSlash(cmd), filepath.ToSlash(filepath.Dir(targetFile)))
			if volume := filepath.VolumeName(targetFile); volume != "" {
				require.NotContains(t, cmd, volume)
			}
		})
	}
}

func setupContractMultiRepoServer(t *testing.T, names ...string) (*Server, *graph.Graph) {
	t.Helper()
	workspace := t.TempDir()
	entries := make([]config.RepoEntry, 0, len(names))
	for _, name := range names {
		root := filepath.Join(workspace, name)
		require.NoError(t, os.MkdirAll(root, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc Existing() {}\n"), 0o644))
		entries = append(entries, config.RepoEntry{Path: root, Name: name})
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	global := &config.GlobalConfig{Repos: entries}
	global.SetConfigPath(configPath)
	require.NoError(t, global.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)

	g := graph.New()
	multi := indexer.NewMultiIndexer(g, testRegistry(), search.NewNull(), manager, zap.NewNop())
	_, err = multi.IndexAll()
	require.NoError(t, err)

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	srv.multiIndexer = multi
	srv.RunAnalysis()
	return srv, g
}

func TestLowerEditSourceCanonicalizesBrandNewGraphQualifiedFile(t *testing.T) {
	srv, g := setupContractMultiRepoServer(t, "api")
	file := "api/newpkg/foo.go"
	require.Empty(t, g.GetFileNodes(file), "the regression requires no base graph node")

	p := lowerWorkspaceEditPrediction(t, srv, workspaceEditJSON(t, map[string]string{
		file: "package newpkg\n\nfunc Added() {}\n",
	}))
	require.Equal(t, []verificationFile{{repoPrefix: "api", path: "newpkg/foo.go"}}, p.verificationFiles)

	cmd, err := buildVerificationCommand(p)
	require.NoError(t, err)
	require.Equal(t, "go build ./newpkg/... && go test -race ./newpkg/...", cmd)
}

func TestLowerEditSourceRefusesTwoRepoNewFilesActionably(t *testing.T) {
	srv, g := setupContractMultiRepoServer(t, "api", "worker")
	apiFile := "api/newpkg/foo.go"
	workerFile := "worker/newpkg/job.go"
	require.Empty(t, g.GetFileNodes(apiFile))
	require.Empty(t, g.GetFileNodes(workerFile))

	p := lowerWorkspaceEditPrediction(t, srv, workspaceEditJSON(t, map[string]string{
		apiFile:    "package newpkg\n\nfunc Added() {}\n",
		workerFile: "package newpkg\n\nfunc Run() {}\n",
	}))
	require.Equal(t, []verificationFile{
		{repoPrefix: "api", path: "newpkg/foo.go"},
		{repoPrefix: "worker", path: "newpkg/job.go"},
	}, p.verificationFiles)

	env := srv.assembleEnvelope(context.Background(), p, nil)
	require.Equal(t, verdictWarn, env.Verdict)
	require.Empty(t, env.VerificationCommand)
	require.Contains(t, env.StopCondition, "repository-scoped verification commands")
	require.Contains(t, env.StopCondition, "all commands exit 0")

	var verificationReason *changeReason
	for i := range env.Reasons {
		if env.Reasons[i].Family == "verification" {
			verificationReason = &env.Reasons[i]
			break
		}
	}
	require.NotNil(t, verificationReason)
	require.Equal(t, "cannot synthesize one verification command for multiple repositories: api, worker", verificationReason.Message)
}
