package embedding

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sharedStatic memoises a single process-wide StaticProvider. The baked
// GloVe vectors are ~3.7MB compressed and decompress into a ~20k-entry
// map that is safe for concurrent reads, so one instance serves every
// rerank call. Constructed lazily on first use.
var (
	sharedStaticOnce sync.Once
	sharedStaticInst *StaticProvider
)

// SharedStatic returns the process-wide static word-vector provider,
// constructing it on first call. Returns nil only when the baked
// vectors fail to load (a corrupt build); callers treat nil as "no
// semantic-cosine channel". Safe for concurrent use.
func SharedStatic() *StaticProvider {
	sharedStaticOnce.Do(func() {
		p, err := NewStaticProvider()
		if err != nil {
			return
		}
		sharedStaticInst = p
	})
	return sharedStaticInst
}

// EmbedTextFunc adapts a provider into the plain func the rerank
// Context wants: text -> normalised vector, errors and nil providers
// collapsing to a nil result the signal reads as "cannot embed".
func EmbedTextFunc(p Provider) func(string) []float32 {
	if p == nil {
		return nil
	}
	return func(text string) []float32 {
		vec, err := p.Embed(context.Background(), text)
		if err != nil {
			return nil
		}
		return vec
	}
}

// sharedCode holds the process-wide code-embedding provider used by the
// rerank's semantic-cosine channel: the bundled static code model (potion)
// when its files resolve — explicit dir, exec-adjacent sidecar, per-user
// models dir, or a checksum-verified first-use download — and the baked
// GloVe word vectors otherwise, so an offline install without the sidecar
// still gets a semantic channel.
//
// It is deliberately NOT a sync.Once. The potion matrix is tens of MiB and
// a daemon that stops searching should not hold it for the rest of its
// life: the loader records when it was last used and a reaper drops it
// after an idle period, so the next search pays a reload instead of the
// process paying rent forever.
var (
	sharedCodeMu     sync.Mutex
	sharedCodeInst   Provider
	sharedCodeLoaded bool
	sharedCodeEnable = true
	sharedCodeReaper bool
	sharedCodeReady  atomic.Pointer[codeEmbedderReady]
)

// Each publication is immutable. Refreshing usage replaces the snapshot,
// allowing idle retirement to lose its CAS to a concurrent active reader.
type codeEmbedderReady struct {
	provider Provider
	usedAt   time.Time
}

// codeEmbedderIdleTTL is how long the code embedder survives with no
// rerank traffic. Long enough that a working session never reloads,
// short enough that an idle daemon gives the memory back.
const codeEmbedderIdleTTL = 30 * time.Minute

// SetCodeEmbedderEnabled turns the rerank semantic-cosine channel on or
// off process-wide, and drops an already-loaded model when turning it off.
// The daemon calls this from its resolved configuration; the channel is on
// by default, which is the historical behaviour.
func SetCodeEmbedderEnabled(enabled bool) {
	sharedCodeMu.Lock()
	defer sharedCodeMu.Unlock()
	sharedCodeEnable = enabled
	if !enabled {
		// Clearing publication is the disable boundary for loaded-only readers.
		// Already admitted requests retain their provider, as eager readers do.
		sharedCodeReady.Store(nil)
		sharedCodeInst = nil
		sharedCodeLoaded = false
	}
}

// SharedCodeEmbedder returns the process-wide code embedder for the
// rerank's semantic-cosine channel, loading it on first use and reloading
// it after an idle eviction. Never returns an error — the fallback chain
// ends at the baked static provider; nil only when even that failed to
// load, or when the channel is disabled. Safe for concurrent use.
// GORTEX_POTION=0 pins the GloVe fallback (diagnostic escape hatch).
func SharedCodeEmbedder() Provider {
	sharedCodeMu.Lock()
	defer sharedCodeMu.Unlock()
	if !sharedCodeEnable {
		return nil
	}
	if !sharedCodeLoaded {
		sharedCodeInst = buildCodeEmbedder()
		sharedCodeLoaded = true
		sharedCodeReady.Store(&codeEmbedderReady{provider: sharedCodeInst, usedAt: time.Now()})
		startCodeEmbedderReaperLocked()
	} else {
		refreshCodeEmbedderReady()
	}
	return sharedCodeInst
}

// LoadedSharedCodeEmbedder returns only an already initialized provider. It
// never loads a model, downloads files, starts a reaper, or waits for the load
// mutex, including when another request is cold-loading the eager provider.
// Warm usage refreshes idle expiry without dropping scoring under contention.
func LoadedSharedCodeEmbedder() Provider {
	return refreshCodeEmbedderReady()
}

func refreshCodeEmbedderReady() Provider {
	for {
		ready := sharedCodeReady.Load()
		if ready == nil {
			return nil
		}
		now := time.Now()
		if now.Before(ready.usedAt) {
			now = ready.usedAt
		}
		if sharedCodeReady.CompareAndSwap(ready, &codeEmbedderReady{provider: ready.provider, usedAt: now}) {
			return ready.provider
		}
		// A warm refresh retries; disable/retirement leaves nil and exits.
	}
}

func buildCodeEmbedder() Provider {
	if v := strings.TrimSpace(os.Getenv("GORTEX_POTION")); v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off") {
		return staticOrNil()
	}
	dir := resolvePotionDir()
	if dir == "" {
		if d, err := downloadPotion(); err == nil {
			dir = d
		}
	}
	if dir != "" {
		if p, err := NewPotionProviderFromDir(dir); err == nil {
			return p
		}
	}
	return staticOrNil()
}

// startCodeEmbedderReaperLocked launches the idle reaper once. It drops
// the instance and exits; a later load starts a fresh one. The caller must
// hold sharedCodeMu.
func startCodeEmbedderReaperLocked() {
	if sharedCodeReaper {
		return
	}
	sharedCodeReaper = true
	go func() {
		ticker := time.NewTicker(codeEmbedderIdleTTL / 4)
		defer ticker.Stop()
		for range ticker.C {
			sharedCodeMu.Lock()
			idle := retireCodeEmbedderLocked(sharedCodeReady.Load(), time.Now())
			sharedCodeMu.Unlock()
			if idle {
				return
			}
		}
	}()
}

// retireCodeEmbedderLocked closes only a snapshot that remains idle and
// unchanged. The caller holds sharedCodeMu to serialize eager load/disable.
func retireCodeEmbedderLocked(ready *codeEmbedderReady, now time.Time) bool {
	if ready == nil || now.Sub(ready.usedAt) < codeEmbedderIdleTTL || !sharedCodeReady.CompareAndSwap(ready, nil) {
		return false
	}
	if ready.provider != nil {
		_ = ready.provider.Close()
	}
	sharedCodeInst = nil
	sharedCodeLoaded = false
	sharedCodeReaper = false
	return true
}

// staticOrNil adapts SharedStatic's concrete return into the Provider
// interface without wrapping a typed nil.
func staticOrNil() Provider {
	if p := SharedStatic(); p != nil {
		return p
	}
	return nil
}
