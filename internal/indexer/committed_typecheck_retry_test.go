package indexer

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

type preemptOnceCommittedProvider struct{ calls int }

func (p *preemptOnceCommittedProvider) Name() string             { return "preempt-once-go" }
func (p *preemptOnceCommittedProvider) Languages() []string      { return []string{"go"} }
func (p *preemptOnceCommittedProvider) Available() bool          { return true }
func (p *preemptOnceCommittedProvider) Close() error             { return nil }
func (p *preemptOnceCommittedProvider) ReadsCommittedTree() bool { return true }
func (p *preemptOnceCommittedProvider) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	return nil, fmt.Errorf("unexpected legacy pass")
}
func (p *preemptOnceCommittedProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	return nil, nil
}
func (p *preemptOnceCommittedProvider) EnrichRepoContext(ctx context.Context, g graph.Store, _, _ string, _ semantic.EnrichDeadlinePolicy) (*semantic.EnrichResult, error) {
	p.calls++
	if p.calls == 1 {
		scope, ok := semantic.CheckoutCompilerScopeFrom(ctx)
		if !ok || scope.Preempt == nil {
			return nil, fmt.Errorf("missing preempt scope")
		}
		scope.Preempt()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &semantic.EnrichResult{Provider: p.Name(), Language: "go"}, nil
}
func TestCommittedTypecheckRetryClearsEarlierOutcome(t *testing.T) {
	builderIsolateGit(t)
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "10000")
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "1")
	repo := builderTempDir(t, "retry-review")
	builderGit(t, repo, "init", "--initial-branch=main")
	builderWriteTree(t, repo, map[string]string{"go.mod": "module example.com/retryreview\n\ngo 1.22\n", "core.go": "package fixture\nfunc Pivot() int { return 1 }\n"})
	builderGit(t, repo, "add", "-A")
	builderGit(t, repo, "commit", "-m", "base")
	base := builderGit(t, repo, "rev-parse", "HEAD^{tree}")
	store := builderOpenStore(t, "retry-review")
	builderIndex(t, store, repo)
	builderWriteFile(t, repo, "core.go", "package fixture\nfunc Pivot() int { return 2 }\n")
	builderGit(t, repo, "add", "-A")
	builderGit(t, repo, "commit", "-m", "target")
	target := builderGit(t, repo, "rev-parse", "HEAD^{tree}")
	provider := &preemptOnceCommittedProvider{}
	manager := semantic.NewManager(semantic.Config{Enabled: true, Providers: []semantic.ProviderConfig{{Name: provider.Name(), Languages: []string{"go"}, Priority: 1, Enabled: true}}}, zap.NewNop())
	defer manager.Close()
	manager.RegisterProvider(provider)
	b := builderNewBuilder(store)
	b.Semantic = manager
	_, report, e := b.BuildCommitLayer(context.Background(), CommitLayerRequest{Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: "graph-fixture", LayerID: "retry-target", CheckoutID: "retry-checkout"}, Base: store, RepoDir: repo, RootPath: repo, BaseTreeOID: base, TargetTreeOID: target, RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix})
	require.NoError(t, e)
	require.Equal(t, 2, provider.calls)
	require.True(t, report.CommittedTypecheck.Ran)
	require.False(t, report.CommittedTypecheck.Preempted)
	require.Empty(t, report.CommittedTypecheck.Reason)
	t.Logf("calls=%d final-outcome=%+v", provider.calls, report.CommittedTypecheck)
}
