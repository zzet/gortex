package semantic

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type preparationSupplemental struct{ *mockProvider }

func (*preparationSupplemental) Supplemental() bool { return true }

func TestCheckoutPreparationProvidersMatchesDispatchWithoutSpawning(t *testing.T) {
	m := NewManager(Config{Enabled: true, EagerLSP: true}, zap.NewNop())
	goProvider := &mockProvider{name: "go", languages: []string{"go"}, available: true}
	jsSupplemental := &preparationSupplemental{&mockProvider{name: "js-extra", languages: []string{"javascript"}, available: true}}
	m.RegisterProvider(goProvider)
	m.RegisterProvider(jsSupplemental)
	router := &fakeRouter{specs: []string{"gopls", "python"}, languages: map[string][]string{"gopls": {"go"}, "python": {"python"}}}
	m.SetLSPRouter(router)
	providers, known := m.CheckoutPreparationProviders("go")
	require.True(t, known)
	require.Equal(t, []Provider{goProvider}, providers, "unrelated supplemental and shadowed router do not execute")
	providers, known = m.CheckoutPreparationProviders("javascript")
	require.True(t, known)
	require.Equal(t, []Provider{jsSupplemental}, providers, "supplemental dispatch is real work even without arbitration winner")
	_, known = m.CheckoutPreparationProviders("python")
	require.False(t, known, "router-only runtime provider has unknown admission")
	providers, known = m.CheckoutPreparationProviders("ruby")
	require.True(t, known)
	require.Empty(t, providers)
	for _, call := range router.calls {
		require.NotContains(t, call, "ProviderForSpec", "eligibility never instantiates a server")
	}
	m.config.EagerLSP = false
	providers, known = m.CheckoutPreparationProviders("python")
	require.True(t, known)
	require.Empty(t, providers, "lazy router is not checkout enrichment work")
	m.config.CheckoutLSP = "off"
	providers, known = m.CheckoutPreparationProviders("go")
	require.True(t, known)
	require.Empty(t, providers)
}
