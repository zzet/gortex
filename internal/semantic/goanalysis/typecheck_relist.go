package goanalysis

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/tools/go/packages"
)

// Export relist after an edit.
//
// A package listed while it (or a dependency) did not compile — a root
// listed during its own edit, or a closure listed while an edit elsewhere
// was in flight — is retained without export data. Once the edit is undone
// or finished, every pass over a root importing it checks it (and its
// importers between it and the root) from source: correct, but 50-120 ms
// on every pass until some listing brings its export data back. Nothing
// else would: the warm-up lists a package once, and a cached pass never
// lists while its closure validates.
//
// So a cached pass that type-checked without hard errors (its roots'
// handle files whole, every other checked file's declarations: a body-only
// error in a stripped file is not seen, and then the relist finds it and
// the signature bound below keeps that to one listing) and whose working
// set holds a mutable package without export data schedules one background
// relist of its roots (with their dependencies): at the warm-up's lowest
// priority, after the module directory's compiler loads have been quiet
// for the quiet period, preempted (and retried, at most
// maxRelistAttempts times) by a new load over the same directory. It is
// bounded: one relist per checkout at a time, and never twice for the same
// export-less packages with the same files (relistSignature), so a package
// that still does not compile is not listed again until its files change.

// maxRelistAttempts bounds the listings one scheduled relist starts
// (preempted ones included).
const maxRelistAttempts = 3

// exportlessWorkingSet returns, sorted, the mutable packages of the working
// set that are retained without export data (and are not export-stale: a
// stale package has export data a relist of its dependency refreshes
// anyway). The caller holds st.mu.
func (st *checkoutTypecheckState) exportlessWorkingSet(ws workingSet) []string {
	var out []string
	for path := range ws.packages {
		meta := st.meta[path]
		if meta != nil && mutablePackage(meta) && meta.ExportFile == "" && !st.exportStale[path] {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// relistSignature identifies the export-less packages and their files'
// current stamps.
func (st *checkoutTypecheckState) relistSignature(paths []string) string {
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(path)
		b.WriteByte('\x00')
		meta := st.meta[path]
		if meta == nil || meta.Dir == "" {
			continue
		}
		names, _ := listGoSources(meta.Dir)
		for _, name := range names {
			stamp, _ := statStamp(filepath.Join(meta.Dir, name))
			fmt.Fprintf(&b, "%s:%d:%d:%d:%d\x00", name, stamp.size, stamp.mtime, stamp.identity[0], stamp.identity[1])
		}
	}
	return b.String()
}

// scheduleExportRelist starts the background relist of the roots when the
// export-less packages are not already being relisted and were not
// relisted with the same files before. The caller holds st.mu. It reports
// whether a relist was scheduled.
func (p *Provider) scheduleExportRelist(st *checkoutTypecheckState, roots []*packages.Package, exportless []string) bool {
	if len(roots) == 0 || len(exportless) == 0 || st.relistRunning {
		return false
	}
	sig := st.relistSignature(exportless)
	if sig == st.relistSig {
		return false
	}
	st.relistSig, st.relistRunning = sig, true
	patterns := make([]string, 0, len(roots))
	for _, root := range roots {
		patterns = append(patterns, root.PkgPath)
	}
	r := &p.warm
	r.mu.Lock()
	if r.ctx == nil {
		r.ctx, r.stop = context.WithCancel(context.Background())
		r.since = time.Now()
	}
	parent := r.ctx
	r.relists.Add(1)
	r.mu.Unlock()
	go p.runExportRelist(parent, st, patterns, exportless)
	return true
}

// runExportRelist is the scheduled relist (see the top of this file).
func (p *Provider) runExportRelist(parent context.Context, st *checkoutTypecheckState, patterns, exportless []string) {
	r := &p.warm
	defer r.relists.Done()
	defer func() {
		st.mu.Lock()
		st.relistRunning = false
		st.mu.Unlock()
	}()
	dir := filepath.Clean(st.loadDir)
	scheduled := time.Now()
	// Start only once the scheduling pass released the state: its apply
	// is on the edit's critical path.
	st.mu.Lock()
	st.mu.Unlock() //nolint:staticcheck // a barrier, not a critical section
	for attempt := 1; attempt <= maxRelistAttempts; attempt++ {
		var (
			ctx    context.Context
			cancel context.CancelFunc
		)
		for ctx == nil {
			r.mu.Lock()
			if parent.Err() != nil {
				r.mu.Unlock()
				return
			}
			wait := r.loadWaitLocked(dir)
			if wait == 0 {
				ctx, cancel = context.WithCancel(parent)
				if r.relistCancel == nil {
					r.relistCancel = map[string]context.CancelFunc{}
				}
				r.relistCancel[dir] = cancel
			}
			r.mu.Unlock()
			if ctx == nil {
				timer := time.NewTimer(wait)
				select {
				case <-parent.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}
		listing, err := p.runListing(ctx, dir, patterns, warmupBuildFlags())
		preempted := ctx.Err() != nil
		cancel()
		r.mu.Lock()
		delete(r.relistCancel, dir)
		r.mu.Unlock()
		if parent.Err() != nil {
			return
		}
		if preempted {
			continue
		}
		if err != nil {
			if p.logger != nil {
				p.logger.Warn("go-types: export relist after an edit failed", zap.String("module_dir", dir), zap.Error(err))
			}
			return
		}
		p.tcMu.Lock()
		current := p.tcStates[st.loadDir] == st
		p.tcMu.Unlock()
		if !current {
			return
		}
		st.mu.Lock()
		taken := st.mergeListing(listing)
		still := 0
		for _, path := range exportless {
			if meta := st.meta[path]; meta == nil || meta.ExportFile == "" {
				still++
			}
		}
		st.mu.Unlock()
		if p.logger != nil {
			p.logger.Info("go-types: export data relisted after an edit",
				zap.String("module_dir", dir),
				zap.Strings("roots", firstPaths(patterns, 4)),
				zap.Strings("exportless", firstPaths(exportless, 8)),
				zap.Int("still_exportless", still),
				zap.Int("taken", taken),
				zap.Int("attempt", attempt),
				zap.Duration("listing", listing.elapsed),
				zap.Duration("since_scheduled", time.Since(scheduled)))
		}
		return
	}
}

// waitExportRelists blocks until no scheduled relist runs. Tests and
// measurement only.
func (p *Provider) waitExportRelists() {
	p.warm.relists.Wait()
}
