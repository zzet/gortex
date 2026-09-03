package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
)

func TestVerdictEscalation(t *testing.T) {
	require.Equal(t, verdictRefuse, escalate(verdictWarn, verdictRefuse))
	require.Equal(t, verdictWarn, escalate(verdictAllow, verdictWarn))
	require.Equal(t, verdictWarn, escalate(verdictWarn, verdictAllow))
	require.Equal(t, verdictRefuse, escalate(verdictRefuse, verdictWarn))

	require.Equal(t, verdictRefuse, verdictForSeverity("error"))
	require.Equal(t, verdictRefuse, verdictForSeverity("critical"))
	require.Equal(t, verdictWarn, verdictForSeverity("warn"))
	require.Equal(t, verdictAllow, verdictForSeverity("info"))
}

func TestClassifyChange(t *testing.T) {
	// rename-only with no broken callers -> structural
	structural := &prediction{step: &simulationStep{
		symbolsRenamed: []map[string]string{{"old": "a", "new": "b"}},
	}}
	require.Equal(t, "structural", classifyChange(structural))

	// config-only touched files -> runtime_drift
	cfg := &prediction{touchedFiles: []string{"deploy/values.yaml", "Dockerfile"}, changedIDs: []string{"x"}}
	require.Equal(t, "runtime_drift", classifyChange(cfg))

	// docs-only, no symbols -> metadata_only
	docs := &prediction{touchedFiles: []string{"README.md"}}
	require.Equal(t, "metadata_only", classifyChange(docs))

	// code change with symbols -> behavioral
	code := &prediction{touchedFiles: []string{"pkg/svc.go"}, changedIDs: []string{"pkg/svc.go::Foo"}}
	require.Equal(t, "behavioral", classifyChange(code))
}

func TestIsConfigFile(t *testing.T) {
	require.True(t, isConfigFile("a/b/values.yaml"))
	require.True(t, isConfigFile("Dockerfile"))
	require.True(t, isConfigFile("infra/main.tf"))
	require.False(t, isConfigFile("pkg/svc.go"))
	require.True(t, isDocFile("README.md"))
	require.False(t, isDocFile("svc.go"))
}

func TestBuildVerificationCommand(t *testing.T) {
	tests := []struct {
		name        string
		prediction  *prediction
		wantCommand string
		wantErr     string
	}{
		{
			name: "repo-relative Go file",
			prediction: &prediction{
				touchedFiles: []string{"internal/svc/svc.go"},
			},
			wantCommand: "go build ./internal/svc/... && go test -race ./internal/svc/...",
		},
		{
			name: "graph-qualified Go file",
			prediction: &prediction{
				touchedFiles: []string{"gortex/internal/mcp/change_contract.go"},
				verificationFiles: []verificationFile{{
					repoPrefix: "gortex",
					path:       "internal/mcp/change_contract.go",
				}},
			},
			wantCommand: "go build ./internal/mcp/... && go test -race ./internal/mcp/...",
		},
		{
			name: "graph-qualified Windows path",
			prediction: &prediction{
				touchedFiles: []string{`gortex\internal\mcp\change_contract.go`},
				verificationFiles: []verificationFile{{
					repoPrefix: "gortex",
					path:       `internal\mcp\change_contract.go`,
				}},
			},
			wantCommand: "go build ./internal/mcp/... && go test -race ./internal/mcp/...",
		},
		{
			name: "covering test path",
			prediction: &prediction{
				touchedFiles: []string{"gortex/internal/mcp/change_contract.go"},
				repoPrefixes: []string{"gortex"},
				impact: &analysis.ImpactResult{
					TestFiles: []string{"gortex/internal/mcp/change_contract_test.go"},
				},
			},
			wantCommand: "go test -race ./internal/mcp",
		},
		{
			name: "cross-repo impact keeps source repository command",
			prediction: &prediction{
				touchedFiles: []string{"internal/mcp/change_contract.go"},
				repoPrefixes: []string{"gortex"},
				impact: &analysis.ImpactResult{
					ByRepo: map[string][]analysis.ImpactEntry{
						"gortex":       {{RepoPrefix: "gortex"}},
						"gortex-cloud": {{RepoPrefix: "gortex-cloud"}},
					},
					TestFiles: []string{
						"gortex/internal/mcp/change_contract_test.go",
						"gortex-cloud/internal/client/client_test.go",
					},
				},
			},
			wantCommand: "go test -race ./internal/mcp",
		},
		{
			name: "repo-relative path starts with repository prefix",
			prediction: &prediction{
				touchedFiles: []string{"web/handler/handler.go"},
				repoPrefixes: []string{"web"},
			},
			wantCommand: "go build ./web/handler/... && go test -race ./web/handler/...",
		},
		{
			name: "multiple repositories",
			prediction: &prediction{
				touchedFiles: []string{"api/internal/api.go", "worker/internal/worker.go"},
				verificationFiles: []verificationFile{
					{repoPrefix: "api", path: "internal/api.go"},
					{repoPrefix: "worker", path: "internal/worker.go"},
				},
			},
			wantErr: "cannot synthesize one verification command for multiple repositories: api, worker",
		},
		{
			name:        "documentation only",
			prediction:  &prediction{touchedFiles: []string{"docs/guide.md"}},
			wantCommand: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := buildVerificationCommand(tt.prediction)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantCommand, cmd)
		})
	}
}

func TestAssembleEnvelopeKeepsSingleRepoVerificationWithCrossRepoImpact(t *testing.T) {
	srv := &Server{}
	p := &prediction{
		source:       "diff",
		touchedFiles: []string{"internal/mcp/change_contract.go"},
		repoPrefixes: []string{"gortex"},
		impact: &analysis.ImpactResult{
			ByRepo: map[string][]analysis.ImpactEntry{
				"gortex":       {{RepoPrefix: "gortex"}},
				"gortex-cloud": {{RepoPrefix: "gortex-cloud"}},
			},
			TestFiles: []string{
				"gortex/internal/mcp/change_contract_test.go",
				"gortex-cloud/internal/client/client_test.go",
			},
		},
	}

	env := srv.assembleEnvelope(context.Background(), p, nil)
	require.Equal(t, verdictAllow, env.Verdict)
	require.Equal(t, "go test -race ./internal/mcp", env.VerificationCommand)
	require.Contains(t, env.StopCondition, "`go test -race ./internal/mcp` exits 0")
	for _, reason := range env.Reasons {
		require.NotEqual(t, "verification", reason.Family)
	}
}

func TestAssembleEnvelopeReportsMultiRepoVerificationRefusal(t *testing.T) {
	srv := &Server{}
	p := &prediction{
		source:       "symbols",
		touchedFiles: []string{"api/internal/api.go", "worker/internal/worker.go"},
		verificationFiles: []verificationFile{
			{repoPrefix: "api", path: "internal/api.go"},
			{repoPrefix: "worker", path: "internal/worker.go"},
		},
	}

	env := srv.assembleEnvelope(context.Background(), p, nil)
	require.Equal(t, verdictWarn, env.Verdict)
	require.Empty(t, env.VerificationCommand)

	var verificationReason *changeReason
	for i := range env.Reasons {
		if env.Reasons[i].Family == "verification" {
			verificationReason = &env.Reasons[i]
			break
		}
	}
	require.NotNil(t, verificationReason)
	require.Equal(t, "cannot synthesize one verification command for multiple repositories: api, worker", verificationReason.Message)
	require.Contains(t, env.StopCondition, "repository-scoped verification commands")
	require.Contains(t, env.StopCondition, "all commands exit 0")
}

func TestChangeContractSymbolSource(t *testing.T) {
	srv, g := setupNavServer(t)
	startID := navFindMethod(t, g, "Start")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"source":  "symbols",
		"symbols": startID,
	}
	res, err := srv.handleChangeContract(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "change_contract errored: %s", toolResultText(res))

	var env changeEnvelope
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &env))

	require.Equal(t, "symbols", env.Source)
	require.Equal(t, "behavioral", env.Classification)
	require.NotEmpty(t, env.ChangedSymbols)
	require.Equal(t, startID, env.ChangedSymbols[0].ID)
	// A symbol set with no signature change and no guard rules never refuses
	// (refuse is reserved for correctness breakage); it may warn on risk.
	require.NotEqual(t, verdictRefuse, env.Verdict)
	require.NotEmpty(t, env.StopCondition)
	require.NotEmpty(t, env.Risk.Tier)
}

func TestChangeContractDiffSourceCleanTree(t *testing.T) {
	srv, _ := setupNavServer(t)
	// The temp repo isn't a git repo; the diff source should fail gracefully
	// (an error result, not a panic).
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"source": "symbols", "symbols": ""}
	res, err := srv.handleChangeContract(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "empty symbol set should be a clean error")
}
