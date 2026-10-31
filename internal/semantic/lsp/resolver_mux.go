package lsp

import (
	"errors"
	"fmt"
	"sync"

	"github.com/zzet/gortex/internal/resolver"
)

// ResolverHelperMux composes multiple resolve-time helpers for one
// workspace. Each child helper keeps its own extension set, so a repo can
// route .ts/.tsx definitions to tsserver and .py definitions to pyright
// without registering multiple top-level repo-prefix entries.
type ResolverHelperMux struct {
	helpers   []resolver.LSPHelper
	closeOnce sync.Once
	closeErr  error
}

// NewResolverHelperMux returns nil for an empty helper set, the sole helper
// unchanged for a singleton, or a mux for multiple helpers.
func NewResolverHelperMux(helpers ...resolver.LSPHelper) resolver.LSPHelper {
	filtered := make([]resolver.LSPHelper, 0, len(helpers))
	for _, h := range helpers {
		if h != nil {
			filtered = append(filtered, h)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return &ResolverHelperMux{helpers: filtered}
	}
}

// SupportsPath implements resolver.LSPHelper.
func (m *ResolverHelperMux) SupportsPath(relPath string) bool {
	if m == nil {
		return false
	}
	for _, h := range m.helpers {
		if h != nil && h.SupportsPath(relPath) {
			return true
		}
	}
	return false
}

// Definition implements resolver.LSPHelper.
func (m *ResolverHelperMux) Definition(relPath string, oneBasedLine int, name string) (string, int, bool) {
	if m == nil {
		return "", 0, false
	}
	for _, h := range m.helpers {
		if h == nil || !h.SupportsPath(relPath) {
			continue
		}
		if defPath, defLine, ok := h.Definition(relPath, oneBasedLine, name); ok {
			return defPath, defLine, true
		}
	}
	return "", 0, false
}

// Close shuts down every child helper that owns closeable state.
func (m *ResolverHelperMux) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		var closing sync.WaitGroup
		errs := make([]error, len(m.helpers))
		for i, helper := range m.helpers {
			if closer, ok := helper.(interface{ Close() error }); ok && closer != nil {
				closing.Add(1)
				go func() {
					defer closing.Done()
					if err := closer.Close(); err != nil {
						errs[i] = fmt.Errorf("resolver mux helper %d: %w", i, err)
					}
				}()
			}
		}
		closing.Wait()
		m.closeErr = errors.Join(errs...)
	})
	return m.closeErr
}
