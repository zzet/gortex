package semantic

import "time"

// CheckoutCompilerWarmer is implemented by a compiler-backed provider that
// can warm a checkout's retained compiler state in the background, so the
// first pass over a package the checkout has not built yet finds its
// dependency closure already listed. Warming never writes facts and never
// runs on a pass's path; a pass that arrives first proceeds as if nothing
// were warming (and the warming yields to it).
type CheckoutCompilerWarmer interface {
	// WarmCheckoutCompiler starts warming the checkout rooted at root for
	// passes with the given scope, unless it is already warm or warming for
	// the current module manifests. It returns at once with an outcome code
	// (started, running, warm, disabled, or ineligible_<reason>).
	WarmCheckoutCompiler(root string, scope CheckoutCompilerScope) string
}

// ForegroundActivity is the daemon's view of the foreground work background
// compiler warm-ups yield to. A warm-up is a whole-module `go list -export`:
// on a small machine it competes for the same cores an edit's build and a
// fresh query need, so it starts only after the daemon has been idle for a
// while and is cancelled the moment foreground work appears.
type ForegroundActivity interface {
	// ForegroundWork names foreground work in flight — an edit, a refresh
	// ticket, a checkout's build cycle — or returns "" when there is none,
	// together with the latest instant foreground work was known to happen
	// (zero when unknown).
	ForegroundWork() (busy string, last time.Time)
	// CheckoutTouched reports whether the checkout rooted at root has served
	// foreground work (an edit or a fresh request) since the daemon started.
	// A checkout nobody has touched is warmed only when the daemon is idle.
	CheckoutTouched(root string) bool
}

// ForegroundActivityAware is implemented by a provider whose background
// work yields to foreground work.
type ForegroundActivityAware interface {
	SetForegroundActivity(activity ForegroundActivity)
}

// SetForegroundActivity installs the foreground-activity view on every
// provider that yields to it. Installing the same view again is a no-op.
func (m *Manager) SetForegroundActivity(activity ForegroundActivity) {
	if m == nil || activity == nil {
		return
	}
	for _, provider := range m.AllProviders() {
		if aware, ok := provider.(ForegroundActivityAware); ok {
			aware.SetForegroundActivity(activity)
		}
	}
}

// WarmCheckoutCompiler asks every provider that can warm a checkout's
// compiler state to start doing so for root. It returns each asked
// provider's outcome by provider name; nil when checkout enrichment is off
// or no provider warms.
func (m *Manager) WarmCheckoutCompiler(root string, scope CheckoutCompilerScope) map[string]string {
	if m == nil || !m.config.Enabled || !m.config.checkoutLSPEnabled() || root == "" {
		return nil
	}
	var out map[string]string
	for _, provider := range m.AllProviders() {
		warmer, ok := provider.(CheckoutCompilerWarmer)
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[provider.Name()] = warmer.WarmCheckoutCompiler(root, scope)
	}
	return out
}
