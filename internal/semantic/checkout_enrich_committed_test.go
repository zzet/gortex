package semantic

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// committedTreeProvider reads a committed tree when committed is set. Its
// pass records the scope it was handed and, when preempt is set, ends itself
// through the scope's preemption the way the compiler admission does.
type committedTreeProvider struct {
	committed bool
	preempt   bool
	scope     CheckoutCompilerScope
	ran       bool
}

func (p *committedTreeProvider) Name() string        { return "test-committed-go" }
func (p *committedTreeProvider) Languages() []string { return []string{"go"} }
func (p *committedTreeProvider) Available() bool     { return true }
func (p *committedTreeProvider) Close() error        { return nil }
func (p *committedTreeProvider) ReadsCommittedTree() bool {
	return p.committed
}
func (p *committedTreeProvider) Enrich(graph.Store, string) (*EnrichResult, error) {
	return nil, errors.New("not a context pass")
}
func (p *committedTreeProvider) EnrichFile(graph.Store, string, string) (*EnrichResult, error) {
	return nil, nil
}
func (p *committedTreeProvider) EnrichRepoContext(ctx context.Context, _ graph.Store, _, _ string, _ EnrichDeadlinePolicy) (*EnrichResult, error) {
	p.ran = true
	p.scope, _ = CheckoutCompilerScopeFrom(ctx)
	if p.preempt && p.scope.Preempt != nil {
		p.scope.Preempt()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &EnrichResult{Provider: p.Name(), Language: "go"}, nil
}

func committedManager(t *testing.T, provider *committedTreeProvider) *Manager {
	t.Helper()
	mgr := NewManager(Config{
		Enabled:                  true,
		CheckoutLSPMaxWorkspaces: 2,
		Providers: []ProviderConfig{
			{Name: provider.Name(), Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}, zap.NewNop())
	mgr.RegisterProvider(provider)
	return mgr
}

func committedRequest() CheckoutEnrichRequest {
	return CheckoutEnrichRequest{RepoPrefix: checkoutRepo, CheckoutID: "checkout-a", Root: "/family/a",
		Compiler: CheckoutCompilerScope{Committed: true, Overlay: map[string][]byte{"/family/a/x.go": []byte("package x\n")}}}
}

// A committed pass reaches a provider that reads a committed tree with the
// overlay and a preemption, and reports what ran.
func TestCommittedPassCarriesTheOverlay(t *testing.T) {
	provider := &committedTreeProvider{committed: true}
	report, err := committedManager(t, provider).EnrichCheckoutContext(context.Background(), checkoutStore(t), committedRequest())
	require.NoError(t, err)
	require.Equal(t, []string{"go"}, report.Ran)
	require.True(t, provider.scope.Committed)
	require.Contains(t, provider.scope.Overlay, "/family/a/x.go")
	require.NotNil(t, provider.scope.Preempt)
}

// A provider that reads the working copy never runs in a committed pass.
func TestCommittedPassSkipsAWorkingCopyProvider(t *testing.T) {
	provider := &committedTreeProvider{}
	report, err := committedManager(t, provider).EnrichCheckoutContext(context.Background(), checkoutStore(t), committedRequest())
	require.NoError(t, err)
	require.False(t, provider.ran, "a provider that reads the working copy ran over a committed tree")
	require.Empty(t, report.Ran)
	require.NotEmpty(t, report.Reason)
}

// A preempted committed pass is reported as preempted, not as an error.
func TestPreemptedCommittedPassIsReported(t *testing.T) {
	provider := &committedTreeProvider{committed: true, preempt: true}
	report, err := committedManager(t, provider).EnrichCheckoutContext(context.Background(), checkoutStore(t), committedRequest())
	require.NoError(t, err)
	require.True(t, report.Preempted)
	require.Empty(t, report.Ran)
	require.Equal(t, ErrCommittedPassPreempted.Error(), report.Reason)
}
