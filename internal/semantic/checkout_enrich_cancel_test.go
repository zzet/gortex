package semantic

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// blockingContextProvider blocks in its pass until the pass context ends,
// the way a go/packages load observes cancellation.
type blockingContextProvider struct {
	started chan struct{}
}

func (p *blockingContextProvider) Name() string        { return "test-ctx-go" }
func (p *blockingContextProvider) Languages() []string { return []string{"go"} }
func (p *blockingContextProvider) Available() bool     { return true }
func (p *blockingContextProvider) Close() error        { return nil }
func (p *blockingContextProvider) Enrich(graph.Store, string) (*EnrichResult, error) {
	return nil, errors.New("not a context pass")
}
func (p *blockingContextProvider) EnrichFile(graph.Store, string, string) (*EnrichResult, error) {
	return nil, nil
}
func (p *blockingContextProvider) EnrichRepoContext(ctx context.Context, _ graph.Store, _, _ string, _ EnrichDeadlinePolicy) (*EnrichResult, error) {
	close(p.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestEnrichCheckoutContextCancelReachesTheProvider: the caller's cancel
// (a yielding compaction) ends a provider pass already in flight, and the
// checkout pass returns the context error; an already-cancelled context
// dispatches nothing.
func TestEnrichCheckoutContextCancelReachesTheProvider(t *testing.T) {
	provider := &blockingContextProvider{started: make(chan struct{})}
	mgr := NewManager(Config{
		Enabled:                  true,
		CheckoutLSPMaxWorkspaces: 2,
		Providers: []ProviderConfig{
			{Name: "test-ctx-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}, zap.NewNop())
	mgr.RegisterProvider(provider)
	var _ ContextEnricher = provider
	g := checkoutStore(t)
	req := CheckoutEnrichRequest{RepoPrefix: checkoutRepo, CheckoutID: "checkout-a", Root: "/family/a", Fingerprint: "fp"}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := mgr.EnrichCheckoutContext(cancelled, g, req)
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-provider.started:
		t.Fatal("an already-cancelled pass dispatched the provider")
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-provider.started
		cancel()
	}()
	start := time.Now()
	_, err = mgr.EnrichCheckoutContext(ctx, g, req)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 10*time.Second, "the cancel must end the in-flight provider pass")
}
