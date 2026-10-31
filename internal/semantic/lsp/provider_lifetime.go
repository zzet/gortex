package lsp

import (
	"context"
	"errors"
	"sync"
)

var errProviderClosed = errors.New("LSP provider is closed")
var errClientNotInitialized = errors.New("LSP client not initialized")

type providerClientStart struct {
	done   chan struct{}
	client *Client
	err    error
}

func (p *Provider) contextLocked() context.Context {
	if p.lifetimeCtx == nil {
		p.lifetimeCtx, p.lifetimeCancel = context.WithCancel(context.Background())
	}
	return p.lifetimeCtx
}

func (p *Provider) providerContext() context.Context {
	p.lifetimeMu.Lock()
	defer p.lifetimeMu.Unlock()
	return p.contextLocked()
}

// requestClient captures one published client for one operation. Reconnect can
// replace the publication after this snapshot, never the pointer an RPC uses.
func (p *Provider) requestClient() (*Client, error) {
	p.lifetimeMu.Lock()
	defer p.lifetimeMu.Unlock()
	if p.closed {
		return nil, errProviderClosed
	}
	if p.client == nil {
		return nil, errClientNotInitialized
	}
	return p.client, nil
}

func (p *Provider) callCurrentClient(method string, params, result any) error {
	client, err := p.requestClient()
	if err != nil {
		return err
	}
	return client.Call(method, params, result)
}

func (p *Provider) notifyCurrentClient(method string, params any) error {
	client, err := p.requestClient()
	if err != nil {
		return err
	}
	return client.Notify(method, params)
}

// ensureClient shares one admitted initialization. Waiters own no separate
// subprocess; closure cancels/detaches that same start before joining it.
func (p *Provider) ensureClient(workspaceRoot string) error {
	p.lifetimeMu.Lock()
	p.contextLocked()
	if p.closed {
		p.lifetimeMu.Unlock()
		return errProviderClosed
	}
	if start := p.starting; start != nil {
		p.lifetimeMu.Unlock()
		<-start.done
		return start.err
	}
	if client := p.client; client != nil {
		select {
		case <-client.Done():
		default:
			p.lifetimeMu.Unlock()
			return nil
		}
	}
	start := &providerClientStart{done: make(chan struct{})}
	p.starting = start
	p.starts.Add(1)
	p.lifetimeMu.Unlock()

	err := p.initializeClient(workspaceRoot, start)
	p.lifetimeMu.Lock()
	if p.closed && err != nil {
		err = errors.Join(errProviderClosed, err)
	}
	start.err = err
	p.starting = nil
	close(start.done)
	p.lifetimeMu.Unlock()
	p.starts.Done()
	return err
}

// captureStartingClient runs before initialize can block. A child whose
// construction raced closure is immediately joined by its admitted constructor.
func (p *Provider) captureStartingClient(start *providerClientStart, client *Client) error {
	p.lifetimeMu.Lock()
	start.client = client
	closed := p.closed
	p.lifetimeMu.Unlock()
	if closed {
		return errors.Join(errProviderClosed, p.closeClient(client))
	}
	return nil
}

func (p *Provider) publishInitializedClient(client *Client) error {
	p.lifetimeMu.Lock()
	if !p.closed {
		p.client = client
		p.lifetimeMu.Unlock()
		return nil
	}
	p.lifetimeMu.Unlock()
	return errors.Join(errProviderClosed, p.closeClient(client))
}

func (p *Provider) closeClient(client *Client) error {
	err := client.Shutdown()
	p.lifetimeMu.Lock()
	if p.closed && err != nil {
		if p.closeErrors == nil {
			p.closeErrors = make(map[*Client]error)
		}
		p.closeErrors[client] = err
	}
	p.lifetimeMu.Unlock()
	return err
}

// BeginClose permanently refuses new initialization/reconnect, interrupts all
// captured clients and returns one actual completion. No caller may substitute
// this nonjoining admission fence for the completion returned by Close.
func (p *Provider) BeginClose() <-chan struct{} {
	p.closeOnce.Do(func() {
		p.lifetimeMu.Lock()
		p.contextLocked()
		p.closed = true
		p.closeDone = make(chan struct{})
		cancel := p.lifetimeCancel
		clients := make(map[*Client]struct{})
		if p.client != nil {
			clients[p.client] = struct{}{}
		}
		if p.starting != nil && p.starting.client != nil {
			clients[p.starting.client] = struct{}{}
		}
		p.lifetimeMu.Unlock()
		cancel()
		var closing sync.WaitGroup
		for client := range clients {
			closing.Add(1)
			go func() { defer closing.Done(); _ = p.closeClient(client) }()
		}
		go func() {
			defer close(p.closeDone)
			// An admitted constructor can still return a new transport after
			// the snapshot; captureStartingClient joins it before starts.Done.
			p.starts.Wait()
			closing.Wait()
			p.lifetimeMu.Lock()
			var errs []error
			for _, err := range p.closeErrors {
				errs = append(errs, err)
			}
			p.closeErr = errors.Join(errs...)
			p.lifetimeMu.Unlock()
		}()
	})
	return p.closeDone
}
