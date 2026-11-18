package lsp

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/resolver"
)

// ResolverHelperRegistry composes per-repo resolver.LSPHelper
// instances into a single resolver.LSPHelper. The cross-file
// resolver works against a shared graph whose file paths are
// repo-prefixed in multi-repo mode; the registry strips the prefix,
// dispatches the question to the matching per-repo helper, and
// re-prefixes the response.
//
// Single-repo daemons register one entry under an empty prefix and
// the registry degenerates to a thin pass-through.
//
// The registry holds no LSP state itself — every helper owns its own
// underlying *Provider lifecycle. Safe for concurrent access from
// the parallel resolver workers.
type ResolverHelperRegistry struct {
	mu        sync.RWMutex
	entries   []registryEntry
	closeOnce sync.Once
	closeErr  error
}

type registryEntry struct {
	prefix string // e.g. "myrepo" (no leading or trailing slash); "" for single-repo mode
	helper resolver.LSPHelper
}

// NewResolverHelperRegistry constructs an empty registry.
func NewResolverHelperRegistry() *ResolverHelperRegistry {
	return &ResolverHelperRegistry{}
}

// Register adds (or replaces) the helper for the given repo prefix.
// Pass "" for single-repo mode. A nil helper deregisters the entry.
//
// The registry sorts entries by prefix length descending so
// longest-prefix-wins for nested registrations — useful when a
// monorepo's outer workspace is registered as a parent of an
// inner per-project workspace.
//
// Accepts any resolver.LSPHelper implementation so tests can install
// scripted stubs; production callers register *ResolverHelper or its
// lazy-wrapped variant.
func (r *ResolverHelperRegistry) Register(repoPrefix string, helper resolver.LSPHelper) {
	r.mu.Lock()
	defer r.mu.Unlock()
	repoPrefix = strings.Trim(repoPrefix, "/")
	// Remove any existing entry with the same prefix.
	kept := r.entries[:0]
	for _, e := range r.entries {
		if e.prefix != repoPrefix {
			kept = append(kept, e)
		}
	}
	r.entries = kept
	if helper != nil {
		r.entries = append(r.entries, registryEntry{prefix: repoPrefix, helper: helper})
	}
	// Sort by prefix length descending so the longest match wins.
	for i := 1; i < len(r.entries); i++ {
		for j := i; j > 0 && len(r.entries[j].prefix) > len(r.entries[j-1].prefix); j-- {
			r.entries[j], r.entries[j-1] = r.entries[j-1], r.entries[j]
		}
	}
}

// Unregister removes the helper for the given repo prefix (no-op
// when no entry exists).
func (r *ResolverHelperRegistry) Unregister(repoPrefix string) {
	r.Register(repoPrefix, nil)
}

// Helpers returns a snapshot of the registered helpers. Order matches
// the longest-prefix-first dispatch order.
func (r *ResolverHelperRegistry) Helpers() []resolver.LSPHelper {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]resolver.LSPHelper, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.helper)
	}
	return out
}

// Close shuts down every registered helper's underlying provider.
// Registration/resolver producers must already be joined (the shared stack
// enforces this); Close snapshots their final entries. The result is cached.
// Helpers that don't expose a Close method (e.g. test stubs implementing only
// resolver.LSPHelper) are skipped silently.
func (r *ResolverHelperRegistry) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		entries := r.entries
		r.entries = nil
		r.mu.Unlock()
		var closing sync.WaitGroup
		errs := make([]error, len(entries))
		for i, entry := range entries {
			if closer, ok := entry.helper.(interface{ Close() error }); ok && closer != nil {
				closing.Add(1)
				go func() {
					defer closing.Done()
					if err := closer.Close(); err != nil {
						errs[i] = fmt.Errorf("resolver helper %q: %w", entry.prefix, err)
					}
				}()
			}
		}
		closing.Wait()
		r.closeErr = errors.Join(errs...)
	})
	return r.closeErr
}

// SupportsPath implements resolver.LSPHelper.
func (r *ResolverHelperRegistry) SupportsPath(relPath string) bool {
	helper, _ := r.helperFor(relPath)
	if helper == nil {
		return false
	}
	stripped, _ := r.stripPrefix(relPath)
	return helper.SupportsPath(stripped)
}

// Definition implements resolver.LSPHelper.
func (r *ResolverHelperRegistry) Definition(relPath string, oneBasedLine int, name string) (string, int, bool) {
	helper, prefix := r.helperFor(relPath)
	if helper == nil {
		return "", 0, false
	}
	stripped, _ := r.stripPrefix(relPath)
	defRel, defLine, ok := helper.Definition(stripped, oneBasedLine, name)
	if !ok {
		return "", 0, false
	}
	// Re-prefix the returned path so it matches the graph's
	// node ID format. defRel is already forward-slash normalised
	// (the helper called filepath.ToSlash before returning).
	if prefix == "" {
		return defRel, defLine, true
	}
	return prefix + "/" + defRel, defLine, true
}

// helperFor returns the helper whose prefix best matches relPath.
// Returns (nil, "") when no entry matches.
func (r *ResolverHelperRegistry) helperFor(relPath string) (resolver.LSPHelper, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.entries) == 0 {
		return nil, ""
	}
	normalised := filepath.ToSlash(relPath)
	for _, e := range r.entries {
		if e.prefix == "" {
			continue
		}
		if normalised == e.prefix || strings.HasPrefix(normalised, e.prefix+"/") {
			return e.helper, e.prefix
		}
	}
	// Fall back to an empty-prefix entry (single-repo).
	for _, e := range r.entries {
		if e.prefix == "" {
			return e.helper, ""
		}
	}
	return nil, ""
}

// stripPrefix returns relPath with its registered prefix removed.
// When no prefix matches, returns relPath unchanged.
func (r *ResolverHelperRegistry) stripPrefix(relPath string) (string, string) {
	_, prefix := r.helperFor(relPath)
	if prefix == "" {
		return relPath, ""
	}
	normalised := filepath.ToSlash(relPath)
	stripped := strings.TrimPrefix(normalised, prefix+"/")
	if stripped == normalised {
		// Edge case: relPath equals the prefix exactly.
		stripped = ""
	}
	return stripped, prefix
}
