package lsp

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type resolverCloseProbe struct {
	started      chan struct{}
	peer, rescue <-chan struct{}
	err          error
	calls        atomic.Int32
}

func (*resolverCloseProbe) SupportsPath(string) bool                           { return false }
func (*resolverCloseProbe) Definition(string, int, string) (string, int, bool) { return "", 0, false }
func (p *resolverCloseProbe) Close() error {
	p.calls.Add(1)
	close(p.started)
	select {
	case <-p.peer:
	case <-p.rescue:
	}
	return p.err
}

func TestResolverRegistryAndMuxCloseAllOwnedHelpersAndRetainErrors(t *testing.T) {
	for _, kind := range []string{"registry", "mux"} {
		t.Run(kind, func(t *testing.T) {
			rescue := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(rescue) }) }
			t.Cleanup(unblock)
			aErr, bErr := errors.New("first owned helper close"), errors.New("second owned helper close")
			a := &resolverCloseProbe{started: make(chan struct{}), rescue: rescue, err: aErr}
			b := &resolverCloseProbe{started: make(chan struct{}), rescue: rescue, err: bErr}
			a.peer = b.started
			b.peer = a.started
			var closeOwner func() error
			if kind == "registry" {
				r := NewResolverHelperRegistry()
				r.Register("first", a)
				r.Register("second", b)
				closeOwner = r.Close
			} else {
				m := NewResolverHelperMux(a, b).(*ResolverHelperMux)
				closeOwner = m.Close
			}
			t.Cleanup(func() { unblock(); _ = closeOwner() })
			done := make(chan error, 1)
			go func() { done <- closeOwner() }()
			var first error
			select {
			case first = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("one helper close prevented another helper from starting")
			}
			require.ErrorIs(t, first, aErr)
			require.ErrorIs(t, first, bErr)
			require.Same(t, first, closeOwner(), "later callers lost close error")
			require.Equal(t, int32(1), a.calls.Load())
			require.Equal(t, int32(1), b.calls.Load())
		})
	}
}

func TestOwnedResolverHelperRetainsEveryActualProviderCloseError(t *testing.T) {
	var providers []*Provider
	var clients []*Client
	for _, name := range []string{"first-owned", "second-owned"} {
		spec, _ := lifetimeSpec(t, name, helperLSPIgnore)
		p := NewProviderFromSpec(spec, zap.NewNop())
		t.Cleanup(func() { _ = p.Close() })
		require.NoError(t, p.EnsureClient(t.TempDir()))
		providers = append(providers, p)
		clients = append(clients, ownedLifetimeClient(t, p))
	}
	h := &ResolverHelper{providers: providers}
	err := h.Close()
	require.ErrorIs(t, err, errLSPStopBudget)
	for i, p := range providers {
		assertLifetimeReaped(t, clients[i]) // Observe actual reaping before reading cached Close errors.
		require.ErrorIs(t, err, p.Close(), "one owned provider's error was discarded")
	}
}
