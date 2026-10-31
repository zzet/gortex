package lsp

import "errors"

var errRouterClosed = errors.New("LSP router is closed")

type routerCloseOwner struct {
	provider                *Provider
	spec, workspace, reason string
}

// registerCloseLocked retains an owner after it leaves providers. Add is
// fenced with lazy-start/reaper closure by the same router admission mutex.
func (r *Router) registerCloseLocked(p *Provider, spec, workspace, reason string) (routerCloseOwner, bool) {
	if owner, ok := r.closeOwners[p]; ok {
		return owner, false
	}
	if r.closeOwners == nil {
		r.closeOwners = make(map[*Provider]routerCloseOwner)
	}
	owner := routerCloseOwner{provider: p, spec: spec, workspace: workspace, reason: reason}
	r.closeOwners[p] = owner
	r.closeWorkers.Add(1)
	return owner, true
}

func (r *Router) finishClose(owner routerCloseOwner) {
	defer r.closeWorkers.Done()
	err := r.closeTimedProvider(owner.provider, owner.spec, owner.workspace, owner.reason)
	r.mu.Lock()
	delete(r.closeOwners, owner.provider)
	if r.closed && err != nil {
		if r.closeErrors == nil {
			r.closeErrors = make(map[*Provider]error)
		}
		r.closeErrors[owner.provider] = err
	}
	r.mu.Unlock()
}

// BeginClose revokes lazy construction and starts closure of EVERY captured
// provider before joining any. The returned completion includes constructors
// which were admitted before this snapshot but cannot publish after it.
func (r *Router) BeginClose() <-chan struct{} {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.closeDone = make(chan struct{})
		if r.stopReaper != nil {
			close(r.stopReaper)
			r.stopReaper = nil
		}
		providers := make(map[*Provider]routerCloseOwner)
		for _, rp := range r.providers {
			providers[rp.provider] = routerCloseOwner{provider: rp.provider, spec: rp.spec.Name, workspace: rp.workspace}
		}
		for p, rp := range r.starting {
			providers[p] = routerCloseOwner{provider: p, spec: rp.spec.Name, workspace: rp.workspace}
		}
		for p, owner := range r.closeOwners {
			providers[p] = owner
		}
		var newOwners []routerCloseOwner
		for p, provider := range providers {
			if owner, ok := r.registerCloseLocked(p, provider.spec, provider.workspace, "shutdown"); ok {
				newOwners = append(newOwners, owner)
			}
		}
		r.providers = make(map[providerKey]*routedProvider)
		r.mu.Unlock()
		for p := range providers {
			p.BeginClose()
		}
		for _, owner := range newOwners {
			go r.finishClose(owner)
		}
		go func() {
			defer close(r.closeDone)
			// Late constructors close any unaccepted client themselves before
			// Done; no detached close can be added after that final fence.
			r.starts.Wait()
			r.closeWorkers.Wait()
			r.reapers.Wait()
			r.mu.Lock()
			var errs []error
			for _, err := range r.closeErrors {
				errs = append(errs, err)
			}
			r.closeErr = errors.Join(errs...)
			r.mu.Unlock()
		}()
	})
	return r.closeDone
}
