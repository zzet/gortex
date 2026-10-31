package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type lifetimeHelper struct{ trace, entered, release string }

func lifetimeSpec(t *testing.T, name, mode string) (*ServerSpec, lifetimeHelper) {
	t.Helper()
	dir := t.TempDir()
	h := lifetimeHelper{trace: filepath.Join(dir, "methods"), entered: filepath.Join(dir, "initialize"), release: filepath.Join(dir, "release")}
	return &ServerSpec{Name: name, Command: helperCommand(t), Args: helperArgs, Languages: []string{"go"}, Env: []string{
		helperModeEnv + "=" + mode, helperTranscriptEnv + "=" + h.trace, helperInitEnteredEnv + "=" + h.entered, helperInitReleaseEnv + "=" + h.release,
		"GORACE=" + os.Getenv("GORACE") + " atexit_sleep_ms=0",
	}}, h
}

func awaitLifetimeFile(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool { _, err := os.Stat(path); return err == nil }, 3*time.Second, time.Millisecond)
}

func beginLifetimeClose(t *testing.T, begin func() <-chan struct{}) <-chan struct{} {
	t.Helper()
	result := make(chan (<-chan struct{}), 1)
	go func() { result <- begin() }()
	select {
	case done := <-result:
		return done
	case <-time.After(3 * time.Second):
		t.Fatal("nonjoining close blocked on an owned callback")
		return nil
	}
}

// Observe only: calling Shutdown here would conceal a missing owner close.
func awaitOwnedClientClose(t *testing.T, client *Client) error {
	t.Helper()
	require.Eventually(t, func() bool { client.mu.Lock(); defer client.mu.Unlock(); return client.closing }, 3*time.Second, time.Millisecond)
	client.mu.Lock()
	done := client.shutdownDone
	client.mu.Unlock()
	select {
	case <-done:
		return client.shutdownErr
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not actually close/reap its captured client")
		return nil
	}
}

func ownedLifetimeClient(t *testing.T, p *Provider) *Client {
	t.Helper()
	p.lifetimeMu.Lock()
	client := p.client
	if p.starting != nil && p.starting.client != nil {
		client = p.starting.client
	}
	p.lifetimeMu.Unlock()
	require.NotNil(t, client)
	tr, ok := client.transport.(*SpawnTransport)
	require.True(t, ok)
	t.Logf("owned lifetime helper pid=%d", tr.cmd.Process.Pid)
	t.Cleanup(func() { _ = client.Shutdown() }) // Failure cleanup, never a passing oracle.
	return client
}

func assertLifetimeReaped(t *testing.T, client *Client) {
	t.Helper()
	tr := client.transport.(*SpawnTransport)
	require.NotNil(t, tr.cmd.ProcessState, "completion must include actual child Wait")
	require.Equal(t, tr.cmd.Process.Pid, tr.cmd.ProcessState.Pid())
	t.Logf("reaped lifetime helper pid=%d success=%t", tr.cmd.ProcessState.Pid(), tr.cmd.ProcessState.Success())
}

func TestProviderConcurrentInitializeSharesOneOwnedClient(t *testing.T) {
	spec, h := lifetimeSpec(t, "shared-initialize", helperLSPInitBarrier)
	p := NewProviderFromSpec(spec, zap.NewNop())
	t.Cleanup(func() { _ = p.Close() })
	results := make(chan error, 8)
	root := t.TempDir()
	for range 8 {
		go func() { results <- p.EnsureClient(root) }()
	}
	awaitLifetimeFile(t, h.entered)
	client := ownedLifetimeClient(t, p)
	require.NoError(t, os.WriteFile(h.release, []byte("release"), 0o600))
	for range 8 {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-time.After(3 * time.Second):
			t.Fatal("shared initialization did not finish")
		}
	}
	require.Same(t, client, p.Client())
	require.NoError(t, p.Close())
	assertLifetimeReaped(t, client)
	methods, err := os.ReadFile(h.trace)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(methods), "initialize\n"))
	require.ErrorIs(t, p.EnsureClient(t.TempDir()), errProviderClosed)
}

func TestProviderCloseInterruptsInitializingChildWithoutLatePublication(t *testing.T) {
	spec, h := lifetimeSpec(t, "close-initialize", helperLSPInitBarrier)
	p := NewProviderFromSpec(spec, zap.NewNop())
	t.Cleanup(func() { _ = p.Close() })
	started := make(chan error, 1)
	go func() { started <- p.EnsureClient(t.TempDir()) }()
	awaitLifetimeFile(t, h.entered)
	client := ownedLifetimeClient(t, p)
	beginLifetimeClose(t, p.BeginClose)
	// The actual server replies after close admission shut, so successful
	// initialization cannot publish a late client into the closed provider.
	require.NoError(t, os.WriteFile(h.release, []byte("release"), 0o600))
	select {
	case err := <-started:
		require.ErrorIs(t, err, errProviderClosed)
	case <-time.After(3 * time.Second):
		t.Fatal("initializing owner was not interrupted/joined")
	}
	require.NoError(t, p.Close())
	require.Nil(t, p.Client())
	assertLifetimeReaped(t, client)
}

func TestProviderCloseCancelsUnansweredInitializeAndReapsActualChild(t *testing.T) {
	spec, h := lifetimeSpec(t, "unanswered-initialize", helperLSPInitBarrier)
	p := NewProviderFromSpec(spec, zap.NewNop())
	t.Cleanup(func() { _ = p.Close() })
	result := make(chan error, 1)
	root := t.TempDir()
	go func() { result <- p.EnsureClient(root) }()
	awaitLifetimeFile(t, h.entered)
	client := ownedLifetimeClient(t, p)
	beginLifetimeClose(t, p.BeginClose)
	// No release is sent: only owner-driven transport interruption can end
	// the real initialize RPC. Observing shutdown must not initiate it here.
	require.ErrorIs(t, awaitOwnedClientClose(t, client), errLSPStopBudget)
	assertLifetimeReaped(t, client)
	select {
	case err := <-result:
		require.ErrorIs(t, err, errProviderClosed)
	case <-time.After(3 * time.Second):
		t.Fatal("unanswered constructor outlived owned child reap")
	}
	require.ErrorIs(t, p.Close(), errLSPStopBudget)
	_, err := os.Stat(h.release)
	require.True(t, os.IsNotExist(err), "test accidentally released the unanswered initialize")
}

func TestProviderCloseJoinsConstructorCallbackAfterActualChildExit(t *testing.T) {
	spec, _ := lifetimeSpec(t, "close-callback", helperLSPGraceful)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	p := NewProviderFromSpec(spec, zap.NewNop())
	p.altInitOptionsFunc = func(string) json.RawMessage { close(entered); <-release; return nil }
	t.Cleanup(func() { unblock(); _ = p.Close() })
	started := make(chan error, 1)
	go func() { started <- p.EnsureClient(t.TempDir()) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("initializer callback did not start")
	}
	client := ownedLifetimeClient(t, p)
	closed := beginLifetimeClose(t, p.BeginClose) // Must not block behind the callback's owner.
	require.NoError(t, awaitOwnedClientClose(t, client))
	assertLifetimeReaped(t, client)
	select {
	case <-closed:
		t.Fatal("provider dropped its still-owned constructor callback")
	default:
	}
	unblock()
	select {
	case err := <-started:
		require.ErrorIs(t, err, errProviderClosed)
	case <-time.After(3 * time.Second):
		t.Fatal("constructor did not finish")
	}
	require.NoError(t, p.Close())
}

func TestProviderReconnectAndCloseKeepStableClientOwnership(t *testing.T) {
	spec, h := lifetimeSpec(t, "reconnect-close", helperLSPInitBarrier)
	require.NoError(t, os.WriteFile(h.release, []byte("initial release"), 0o600))
	p := NewProviderFromSpec(spec, zap.NewNop())
	t.Cleanup(func() { _ = p.Close() })
	root := t.TempDir()
	require.NoError(t, p.EnsureClient(root))
	first := ownedLifetimeClient(t, p)
	require.NoError(t, first.Shutdown()) // Genuine EOF of the first connection.
	assertLifetimeReaped(t, first)
	require.NoError(t, os.Remove(h.release))
	require.NoError(t, os.Remove(h.entered))
	reconnected := make(chan error, 1)
	go func() { reconnected <- p.EnsureClient(root) }()
	awaitLifetimeFile(t, h.entered)
	second := ownedLifetimeClient(t, p)
	require.NotSame(t, first.transport, second.transport, "EOF reconnect must construct a fresh transport")
	readers := make(chan struct{})
	go func() {
		defer close(readers)
		for range 50 {
			_, _ = p.FindDefinition(root, "main.go", 0, 0, time.Millisecond)
		}
	}()
	beginLifetimeClose(t, p.BeginClose)
	require.NoError(t, os.WriteFile(h.release, []byte("late release"), 0o600))
	select {
	case err := <-reconnected:
		require.ErrorIs(t, err, errProviderClosed)
	case <-time.After(3 * time.Second):
		t.Fatal("reconnect escaped close")
	}
	require.NoError(t, p.Close())
	select {
	case <-readers:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent request snapshots did not finish")
	}
	assertLifetimeReaped(t, second)
	require.ErrorIs(t, p.EnsureClient(root), errProviderClosed)
}

func TestRouterCloseOwnsInitializingProviderAndRefusesLatePublish(t *testing.T) {
	spec, h := lifetimeSpec(t, "router-initialize", helperLSPInitBarrier)
	r := NewRouter(t.TempDir(), zap.NewNop())
	t.Cleanup(func() { _ = r.Close() })
	result := make(chan error, 1)
	go func() {
		p, err := r.ForSpecWorkspace(spec, r.defaultWorkspace)
		if p != nil {
			result <- fmt.Errorf("late provider published")
			return
		}
		result <- err
	}()
	awaitLifetimeFile(t, h.entered)
	r.mu.Lock()
	var starting *Provider
	for p := range r.starting {
		starting = p
	}
	r.mu.Unlock()
	require.NotNil(t, starting)
	client := ownedLifetimeClient(t, starting)
	beginLifetimeClose(t, r.BeginClose)
	require.NoError(t, os.WriteFile(h.release, []byte("release"), 0o600))
	select {
	case err := <-result:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("router constructor did not join")
	}
	require.NoError(t, r.Close())
	assertLifetimeReaped(t, client)
	_, err := r.ForSpecWorkspace(spec, r.defaultWorkspace)
	require.ErrorIs(t, err, errRouterClosed)
	r.mu.Lock()
	require.Empty(t, r.providers)
	require.Empty(t, r.starting)
	r.mu.Unlock()
}

func TestRouterCloseJoinsDetachedEvictionAndReaperOwners(t *testing.T) {
	for _, mode := range []string{"lru", "reaper", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var blocked sync.Once
			core, _ := observer.New(zap.InfoLevel)
			logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
				if mode == "lru" && entry.Message == "LSP router evicted LRU provider" || mode != "lru" && entry.Message == "LSP router provider close starting" {
					blocked.Do(func() { close(entered); <-release })
				}
				return nil
			}))
			r := NewRouter(t.TempDir(), logger)
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(func() { unblock(); _ = r.Close() })
			var providers []*Provider
			if mode == "duplicate" {
				spec, h := lifetimeSpec(t, "duplicate", helperLSPInitBarrier)
				results := make(chan *Provider, 2)
				errs := make(chan error, 2)
				for range 2 {
					go func() { p, err := r.ForSpecWorkspace(spec, r.defaultWorkspace); results <- p; errs <- err }()
				}
				require.Eventually(t, func() bool {
					r.mu.Lock()
					pending := make([]*Provider, 0, len(r.starting))
					for p := range r.starting {
						pending = append(pending, p)
					}
					r.mu.Unlock()
					if len(pending) != 2 {
						return false
					}
					for _, p := range pending {
						p.lifetimeMu.Lock()
						ready := p.starting != nil && p.starting.client != nil
						p.lifetimeMu.Unlock()
						if !ready {
							return false
						}
					}
					providers = pending
					return true
				}, 3*time.Second, time.Millisecond)
				require.NoError(t, os.WriteFile(h.release, []byte("release"), 0o600))
				var published []*Provider
				for range 2 {
					select {
					case p := <-results:
						published = append(published, p)
					case <-time.After(3 * time.Second):
						t.Fatal("duplicate constructor did not complete")
					}
					require.NoError(t, <-errs)
				}
				require.Same(t, published[0], published[1])
			} else {
				spec, _ := lifetimeSpec(t, "first", helperLSPGraceful)
				first, err := r.ForSpecWorkspace(spec, r.defaultWorkspace)
				require.NoError(t, err)
				providers = append(providers, first)
				if mode == "lru" {
					r.SetMaxAlive(1)
					other, _ := lifetimeSpec(t, "second", helperLSPGraceful)
					second, err := r.ForSpecWorkspace(other, r.defaultWorkspace)
					require.NoError(t, err)
					providers = append(providers, second)
				} else {
					r.idleTimeout = time.Nanosecond
					r.WithReaperInterval(time.Millisecond)
				}
			}
			var clients []*Client
			for _, p := range providers {
				clients = append(clients, ownedLifetimeClient(t, p))
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("detached close did not enter real logging callback")
			}
			closed := beginLifetimeClose(t, r.BeginClose) // No router lock may be held across that callback.
			for _, client := range clients {
				require.NoError(t, awaitOwnedClientClose(t, client))
				assertLifetimeReaped(t, client)
			}
			select {
			case <-closed:
				t.Fatal("router dropped a detached close/reaper while its real callback still ran")
			default:
			}
			unblock()
			require.NoError(t, r.Close())
			r.WithReaperInterval(time.Millisecond)
			r.mu.Lock()
			require.Nil(t, r.stopReaper, "closed router restarted its reaper")
			require.Empty(t, r.closeOwners)
			r.mu.Unlock()
		})
	}
}
