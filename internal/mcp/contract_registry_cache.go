package mcp

import (
	"context"
	"sync"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// contractRegistryCacheSlots bounds the retained repository registries. A slot
// holds one repository namespace's latest load: an older identity there names
// an earlier attachment or clocks that have since moved on.
const contractRegistryCacheSlots = 16

type contractRegistryCacheKey struct{ repo, checkout string }

type contractRegistryCacheEntry struct {
	key      contractRegistryCacheKey
	identity graphview.ContractRegistryIdentity
	registry *contracts.Registry
}

// contractRegistryCache reuses a repository registry loaded through a bound
// contract analysis across requests, only while every projection input keeps
// the identity it had around the load. A view without such an identity always
// loads. Cached registries are read-only: loadRegistry copies their records
// into the request's own registry, but each copy shares its record's Meta map
// with the cached registry and with every other request served from it, so
// consumers must never write record Meta (only the indexer mutates contract
// metadata, on registries of its own). The cache holds no lease, reader or
// binding state.
type contractRegistryCache struct {
	mu      sync.Mutex
	entries []contractRegistryCacheEntry // most recently used first
	// load is the checked loader; tests observe it.
	load func(context.Context, graph.Reader, contracts.RegistryLoadOptions) (*contracts.Registry, error)
}

func (c *contractRegistryCache) loadRepo(ctx context.Context, view *graphview.ContractAnalysisView, repo string) (*contracts.Registry, error) {
	load := contracts.LoadRegistryFromGraphChecked
	if c != nil && c.load != nil {
		load = c.load
	}
	opts := contracts.RegistryLoadOptions{RepoPrefix: repo}
	if c == nil {
		return load(ctx, view.RegistryReader, opts)
	}
	// A cancelled request fails exactly as the checked loader would.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := contractRegistryCacheKey{repo: repo, checkout: view.Attachment.CheckoutID}
	before, known := view.RegistryIdentity(ctx)
	if known {
		if registry, hit := c.lookup(key, before); hit {
			return registry, nil
		}
	}
	registry, err := load(ctx, view.RegistryReader, opts)
	if err != nil || !known {
		return registry, err
	}
	// A write committed during the load moves a clock; that result stays
	// request-local.
	if after, ok := view.RegistryIdentity(ctx); ok && after.Equal(before) {
		c.store(key, before, registry)
	}
	return registry, nil
}

func (c *contractRegistryCache) lookup(key contractRegistryCacheKey, identity graphview.ContractRegistryIdentity) (*contracts.Registry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, entry := range c.entries {
		if entry.key != key {
			continue
		}
		if !entry.identity.Equal(identity) {
			return nil, false
		}
		copy(c.entries[1:i+1], c.entries[:i])
		c.entries[0] = entry
		return entry.registry, true
	}
	return nil, false
}

func (c *contractRegistryCache) store(key contractRegistryCacheKey, identity graphview.ContractRegistryIdentity, registry *contracts.Registry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := make([]contractRegistryCacheEntry, 0, min(len(c.entries)+1, contractRegistryCacheSlots))
	entries = append(entries, contractRegistryCacheEntry{key: key, identity: identity, registry: registry})
	for _, entry := range c.entries {
		if entry.key != key && len(entries) < contractRegistryCacheSlots {
			entries = append(entries, entry)
		}
	}
	c.entries = entries
}
