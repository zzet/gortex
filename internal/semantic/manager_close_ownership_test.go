package semantic

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

type managerCloseProbe struct {
	name         string
	started      chan struct{}
	peer, rescue <-chan struct{}
	err          error
	calls        atomic.Int32
}

func (p *managerCloseProbe) Name() string                                    { return p.name }
func (*managerCloseProbe) Languages() []string                               { return nil }
func (*managerCloseProbe) Available() bool                                   { return true }
func (*managerCloseProbe) Enrich(graph.Store, string) (*EnrichResult, error) { return nil, nil }
func (*managerCloseProbe) EnrichFile(graph.Store, string, string) (*EnrichResult, error) {
	return nil, nil
}
func (p *managerCloseProbe) Close() error {
	p.calls.Add(1)
	close(p.started)
	select {
	case <-p.peer:
	case <-p.rescue:
	}
	return p.err
}

type managerRouterCloseProbe struct {
	LSPRouter    // No discovery methods are used by this close-only fixture.
	started      chan struct{}
	peer, rescue <-chan struct{}
	err          error
	calls        atomic.Int32
}

func (p *managerRouterCloseProbe) Close() error {
	p.calls.Add(1)
	close(p.started)
	select {
	case <-p.peer:
	case <-p.rescue:
	}
	return p.err
}

func TestManagerCloseStartsAllOwnersAndCachesTypedErrors(t *testing.T) {
	rescue := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(rescue) }) }
	m := NewManager(Config{Enabled: true}, zap.NewNop())
	t.Cleanup(func() { unblock(); _ = m.Close() })
	aErr, bErr, rErr := errors.New("provider-a close"), errors.New("provider-b close"), errors.New("router close")
	a := &managerCloseProbe{name: "a", started: make(chan struct{}), rescue: rescue, err: aErr}
	b := &managerCloseProbe{name: "b", started: make(chan struct{}), rescue: rescue, err: bErr}
	r := &managerRouterCloseProbe{started: make(chan struct{}), rescue: rescue, err: rErr}
	// Every Close starts before waiting on a peer. Any sequential manager
	// close deadlocks at a, and the rescue is failure cleanup only.
	a.peer = r.started
	r.peer = b.started
	b.peer = a.started
	m.RegisterProvider(a)
	m.RegisterProvider(b)
	m.SetLSPRouter(r)
	begun := make(chan (<-chan struct{}), 1)
	go func() { begun <- m.BeginClose() }()
	var done <-chan struct{}
	select {
	case done = <-begun:
	case <-time.After(3 * time.Second):
		t.Fatal("BeginClose joined a resource owner")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("not every provider/router close started before join")
	}
	first := m.Close()
	require.ErrorIs(t, first, aErr)
	require.ErrorIs(t, first, bErr)
	require.ErrorIs(t, first, rErr)
	require.Same(t, first, m.Close())
	require.Equal(t, int32(1), a.calls.Load())
	require.Equal(t, int32(1), b.calls.Load())
	require.Equal(t, int32(1), r.calls.Load())
	require.False(t, m.beginPass(), "closed manager admitted another terminal writer")
}
