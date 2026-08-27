package telemetry

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// consentFile is the persisted explicit-choice record under the telemetry dir.
const consentFile = "consent.json"

// PersistedConsent is the on-disk record of an explicit user choice (via
// `gortex telemetry on|off` or the installer prompt). Its presence is also the
// signal the opt-in-once notice uses to fire at most once.
type PersistedConsent struct {
	Enabled   bool   `json:"enabled"`
	Source    string `json:"source"`     // "cli" | "installer"
	UpdatedAt string `json:"updated_at"` // RFC3339 UTC, informational
}

// LoadConsentConfig reads the persisted choice into a ConsentConfig for the
// resolver's third rung. A missing or unreadable file yields an unset
// (nil-pointer) value, so the resolver falls through to the opt-in default
// rather than treating "never chosen" as off.
func LoadConsentConfig(dir string) ConsentConfig {
	b, err := os.ReadFile(filepath.Join(dir, consentFile))
	if err != nil {
		return ConsentConfig{}
	}
	var pc PersistedConsent
	if json.Unmarshal(b, &pc) != nil {
		return ConsentConfig{}
	}
	enabled := pc.Enabled
	return ConsentConfig{Enabled: &enabled}
}

// HasPersistedConsent reports whether the user has made an explicit choice yet.
func HasPersistedConsent(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, consentFile))
	return err == nil
}

// SaveConsent persists an explicit choice. Disabling also clears any buffered,
// unsent telemetry — off means off — best-effort. now defaults to time.Now.
func SaveConsent(dir string, enabled bool, source string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	pc := PersistedConsent{
		Enabled:   enabled,
		Source:    source,
		UpdatedAt: now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, consentFile), b, 0o644); err != nil {
		return err
	}
	if !enabled {
		clearBuffer(dir)
	}
	return nil
}

// clearBuffer deletes every buffered rollup and the last-send marker, so a
// disable leaves nothing to transmit. The anonymous install id is left in place
// (it is not telemetry data and is never sent while disabled). Best-effort.
func clearBuffer(dir string) {
	store := NewStore(dir)
	if days, err := store.Days(); err == nil {
		for _, d := range days {
			_ = store.Delete(d)
		}
	}
	_ = os.Remove(filepath.Join(dir, lastSendFile))
}

// CachedConsentResolver returns a consent check that re-resolves at most once
// per ttl, so a hot record path observes a `gortex telemetry on|off` toggle
// within ttl without an os.ReadFile per event. It is the live resolver a
// long-lived process hands to NewRecorderFunc. Safe for concurrent use.
func CachedConsentResolver(dir string, ttl time.Duration) func() bool {
	return cachedConsentResolver(dir, ttl, time.Now)
}

// cachedConsentResolver is CachedConsentResolver with an injectable clock, so a
// test can step across the TTL boundary instead of racing a real deadline
// against a loaded machine's scheduler and disk.
func cachedConsentResolver(dir string, ttl time.Duration, clock func() time.Time) func() bool {
	var (
		mu        sync.Mutex
		enabled   bool
		checkedAt time.Time
		valid     bool
	)
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		now := clock()
		if !valid || now.Sub(checkedAt) >= ttl {
			enabled = ResolveConsent(LoadConsentConfig(dir), os.Getenv).Enabled
			checkedAt = now
			valid = true
		}
		return enabled
	}
}

// MaybeFirstRunNotice prints a one-time, opt-in notice to w when the user has
// not yet made an explicit choice, and records a default (off) choice so it
// never fires again. Telemetry is opt-in, so this is an informational notice —
// it never enables anything. Returns whether it printed. Fail-soft: a write
// failure still suppresses future notices if the choice was recorded.
func MaybeFirstRunNotice(dir string, w io.Writer) bool {
	if HasPersistedConsent(dir) {
		return false
	}
	// Record the default (off) choice up front so the notice is one-time even
	// if the process exits immediately after.
	_ = SaveConsent(dir, false, "installer", time.Now)
	if w != nil {
		_, _ = io.WriteString(w,
			"Gortex can collect anonymous usage stats (tool/command counts only — no code, "+
				"paths, or names). It is OFF by default; enable with `gortex telemetry on`. "+
				"See `gortex telemetry status`.\n")
	}
	return true
}
