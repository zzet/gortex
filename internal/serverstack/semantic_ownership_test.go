package serverstack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/lsp"
	"go.uber.org/zap"
)

func semanticOwnershipStack(t *testing.T) *SharedServer {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(key, home)
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, filepath.Join(home, key))
	}
	t.Setenv("GORTEX_GO_TYPES", "0")
	t.Setenv("GORTEX_LSP_RESOLVER", "1")
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "fixture.go"), []byte("package fixture\nfunc Fixture() {}\n"), 0o600))
	cfg := config.Default()
	cfg.Semantic.Enabled = true
	s, err := NewSharedServer(SharedServerConfig{
		Lifecycle:         LifecycleOneshot,
		Index:             repo,
		BackendPath:       filepath.Join(t.TempDir(), "graph.sqlite"),
		Config:            cfg,
		Global:            &config.GlobalConfig{},
		Logger:            zap.NewNop(),
		Version:           "test",
		Embedder:          EmbedderRequest{FlagChanged: true, FlagEnabled: false},
		SideStores:        SideStores{NotesDir: t.TempDir(), NotesRepo: "test"},
		SavingsPath:       filepath.Join(t.TempDir(), "savings.sqlite"),
		SavingsLegacyJSON: filepath.Join(t.TempDir(), "savings.json"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NotNil(t, s.semanticOwner)
	require.Same(t, s.semanticOwner, s.Indexer.SemanticManager(), "constructor must register the actual shared manager")
	return s
}

type stackSemanticProbe struct {
	name                                        string
	entered, interrupted, closeStarted, written chan struct{}
	terminal, peer                              <-chan struct{}
	once                                        sync.Once
	closeCalls, passCalls                       atomic.Int32
	closeErr                                    error
}

func (p *stackSemanticProbe) Name() string        { return p.name }
func (p *stackSemanticProbe) Languages() []string { return []string{p.name} }
func (*stackSemanticProbe) Available() bool       { return true }
func (*stackSemanticProbe) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	return nil, errors.New("fixture requires exact file pass")
}
func (p *stackSemanticProbe) EnrichFile(g graph.Store, _, _ string) (*semantic.EnrichResult, error) {
	p.passCalls.Add(1)
	close(p.entered)
	<-p.closeStarted // A legacy call is interrupted by real Close, not context.
	close(p.interrupted)
	<-p.terminal
	g.AddNode(&graph.Node{ID: p.name, Kind: graph.KindFunction, Name: p.name, Language: p.name})
	close(p.written)
	return &semantic.EnrichResult{}, nil
}
func (p *stackSemanticProbe) Close() error {
	p.closeCalls.Add(1)
	p.once.Do(func() { close(p.closeStarted) })
	if p.peer != nil {
		select {
		case <-p.peer:
		case <-p.terminal:
		} // terminal also rescues a failing sequential-close fixture
	}
	return p.closeErr
}

type stackContextProbe struct{ *stackSemanticProbe }

func (p *stackContextProbe) EnrichFilesContext(ctx context.Context, g graph.Store, _, _ string, _ []string) (*semantic.EnrichResult, error) {
	p.passCalls.Add(1)
	close(p.entered)
	<-ctx.Done()
	close(p.interrupted)
	<-p.terminal
	g.AddNode(&graph.Node{ID: p.name, Kind: graph.KindFunction, Name: p.name, Language: p.name})
	close(p.written)
	return nil, ctx.Err()
}

func TestSharedSemanticOwnerJoinsTerminalWritesBeforeActualBackendClose(t *testing.T) {
	s := semanticOwnershipStack(t)
	terminal := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(terminal) }) }
	t.Cleanup(unblock)
	newProbe := func(name string) *stackSemanticProbe {
		return &stackSemanticProbe{name: name, entered: make(chan struct{}), interrupted: make(chan struct{}), closeStarted: make(chan struct{}), written: make(chan struct{}), terminal: terminal}
	}
	legacy := newProbe("shutdown-legacy")
	cooperative := &stackContextProbe{newProbe("shutdown-context")}
	providerErr := errors.New("cooperative provider cleanup error")
	cooperative.closeErr = providerErr
	legacy.peer = cooperative.closeStarted // Sequential Close would strand legacy.
	m := s.semanticOwner
	// Fixture boot registration finishes before its first pass; no dynamic
	// production provider-registration contract is introduced by this test.
	m.RegisterProvider(legacy)
	m.RegisterProvider(cooperative)
	passes := make(chan error, 2)
	for _, p := range []*stackSemanticProbe{legacy, cooperative.stackSemanticProbe} {
		go func() {
			_, err := m.EnrichFilesContext(context.Background(), s.Graph, "fixture", t.TempDir(), p.name, []string{"fixture.go"})
			passes <- err
		}()
		select {
		case <-p.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("exact provider pass not admitted")
		}
	}
	var backendCloses atomic.Int32
	for i := range s.cleanup {
		if s.cleanup[i].name == "backend" {
			original := s.cleanup[i].close
			s.cleanup[i].close = func() error {
				backendCloses.Add(1)
				for _, p := range []*stackSemanticProbe{legacy, cooperative.stackSemanticProbe} {
					select {
					case <-p.written:
					default:
						t.Errorf("backend reached before terminal writer %s", p.name)
					}
				}
				return original() // Actually close the production SQLite backend.
			}
		}
	}
	done := make(chan error, 8)
	for range 8 {
		go func() { done <- s.Close() }()
	}
	for _, p := range []*stackSemanticProbe{legacy, cooperative.stackSemanticProbe} {
		select {
		case <-p.interrupted:
		case <-time.After(3 * time.Second):
			t.Fatal("semantic cancellation/legacy interruption did not precede join")
		}
		select {
		case <-p.closeStarted:
		case <-time.After(3 * time.Second):
			t.Fatal("not every provider close started before waiting on its peer")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("shared close dropped terminal writer: %v", err)
	default:
	}
	_, err := m.EnrichFilesContext(context.Background(), s.Graph, "fixture", t.TempDir(), legacy.name, []string{"late.go"})
	require.Error(t, err)
	require.Equal(t, int32(1), legacy.passCalls.Load(), "closed manager admitted late pass")
	unblock()
	var first error
	for i := range 8 {
		select {
		case err := <-done:
			require.ErrorIs(t, err, providerErr)
			if i == 0 {
				first = err
			} else {
				require.Same(t, first, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("shared semantic owner did not finish after terminal writers")
		}
	}
	for range 2 {
		select {
		case <-passes:
		case <-time.After(3 * time.Second):
			t.Fatal("manager-owned pass did not return")
		}
	}
	require.Equal(t, int32(1), backendCloses.Load())
	require.Equal(t, int32(1), legacy.closeCalls.Load())
	require.Equal(t, int32(1), cooperative.closeCalls.Load())
	// Persisted markers prove actual final writes reached SQLite before the
	// real backend closer, rather than a channel merely claiming completion.
	reopened, err := store_sqlite.Open(s.StorePath)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	require.NotNil(t, reopened.GetNode(legacy.name))
	require.NotNil(t, reopened.GetNode(cooperative.name))
}

type countedLateResolver struct {
	*lsp.ResolverHelper
	closes atomic.Int32
	err    error
}

func (h *countedLateResolver) Close() error {
	h.closes.Add(1)
	return errors.Join(h.ResolverHelper.Close(), h.err)
}

func TestSharedSemanticCloseKeepsLateRouterBackedHelperForFinalRegistryJoin(t *testing.T) {
	s := semanticOwnershipStack(t)
	require.NotNil(t, s.ResolverLSPRegistry)
	s.BeginSemanticClose() // Warmup/lifecycle registration has not joined yet.
	var lookups atomic.Int32
	helperErr := errors.New("late registered helper cleanup")
	helper := &countedLateResolver{ResolverHelper: lsp.NewLazyResolverHelper(func() (*lsp.Provider, error) { lookups.Add(1); return nil, errors.New("must not borrow during close") }, t.TempDir(), []string{".go"}, time.Second, zap.NewNop()), err: helperErr}
	s.ResolverLSPRegistry.Register("late-warmup", helper)
	require.Contains(t, s.ResolverLSPRegistry.Helpers(), helper)
	first := s.Close()
	require.ErrorIs(t, first, helperErr)
	require.Same(t, first, s.Close(), "registered helper error must survive later shared closes")
	require.Equal(t, int32(1), helper.closes.Load(), "final registry snapshot lost late warmup registration")
	require.Zero(t, lookups.Load(), "router-backed Close must not borrow or spawn a provider")
	require.Empty(t, s.ResolverLSPRegistry.Helpers())
}
