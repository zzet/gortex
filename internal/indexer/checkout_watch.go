package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
)

// The file watcher of one automatic checkout.
//
// A linked worktree is not a tracked repository: nothing watches its files, so
// before this a plain save there (no query, no MCP edit) reached its
// coordinator only through the coordinator's 15-second poll. The watcher closes
// that gap. It does nothing but report which paths changed: the coordinator's
// quiet window coalesces the reports, and its admission decides when the
// working tree has stopped moving long enough to be worth a build
// (checkout_motion.go). The poll stays as the fallback for whatever a watcher
// misses.
//
// One watcher per checkout root, owned by the checkout's coordinator and closed
// with it, so no other lifecycle has to remember it.

// checkoutWatchDisableEnv turns the per-checkout file watcher off ("0", "off",
// "false"): automatic checkouts are then woken only by queries, MCP edits and
// the coordinator's poll.
const checkoutWatchDisableEnv = "GORTEX_CHECKOUT_WATCH"

// checkoutWatchReadyTimeout bounds how long starting a watcher may wait for the
// backend to report its streams live. A watcher that is not live in time is
// abandoned: the poll still covers the checkout.
const checkoutWatchReadyTimeout = 5 * time.Second

// checkoutWatchEnabled reports whether automatic checkouts get a file watcher.
func checkoutWatchEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(checkoutWatchDisableEnv))) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

// checkoutWatch is a started watcher; close stops it and waits for its loop.
type checkoutWatch struct {
	fsw    fswatcher.Watcher
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// checkoutWatchFilter drops, inside the backend, events no build of the
// checkout reads: excluded trees (.git, dependency and build directories, the
// repository's ignore rules) and the guarded editor's own atomic temp files.
type checkoutWatchFilter struct {
	excludes *excludes.Matcher
	roots    []string
}

func (f *checkoutWatchFilter) ShouldInclude(path string) bool {
	if isGortexAtomicTemp(path) {
		return false
	}
	if f == nil || f.excludes == nil {
		return true
	}
	for _, root := range f.roots {
		rel, ok := checkoutRelPath(root, path)
		if !ok {
			continue
		}
		if rel == "." {
			return true
		}
		return !f.excludes.MatchRel(rel)
	}
	return true
}

// checkoutRelPath returns path relative to root, slash-separated, and whether
// path is inside root at all.
func checkoutRelPath(root, path string) (string, bool) {
	if root == "" || path == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// startCheckoutWatch watches root recursively and calls onChange with the
// changed paths (absolute, as the backend reports them) of each batch of
// events. A nil slice means changes were lost (a queue overflow or a dropped
// event) and the paths are unknown. patterns are the repository's exclude
// patterns; nil takes the built-in list.
func startCheckoutWatch(root string, patterns []string, logger *zap.Logger, onChange func([]string)) (*checkoutWatch, error) {
	if root == "" || onChange == nil {
		return nil, errors.New("indexer: checkout watch needs a root and a consumer")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if slowWatchMount(abs) {
		return nil, errors.New("indexer: checkout is on a mount native file events do not reach")
	}
	if len(patterns) == 0 {
		patterns = excludes.Builtin
	}
	registered := backendWatchPath(abs)
	roots := []string{registered}
	if registered != abs {
		roots = append(roots, abs)
	}
	filter := &checkoutWatchFilter{excludes: excludes.New(patterns), roots: roots}

	ready := make(chan struct{})
	// Our own channels, for the reason Watcher.Start owns its: the library
	// never closes them on teardown, so a late flush cannot panic.
	droppedSize := max(fswatcher.DefaultBufferSize/fswatcher.MaxDroppedBufferRatio, fswatcher.MinDroppedBuffer)
	events := make(chan fswatcher.WatchEvent, fswatcher.DefaultBufferSize)
	dropped := make(chan fswatcher.WatchEvent, droppedSize)
	fsw, err := fswatcher.New(
		fswatcher.WithCooldown(0),
		fswatcher.WithSeverity(fswatcher.SeverityError),
		fswatcher.WithReadyChannel(ready),
		fswatcher.WithCustomChannels(events, dropped),
		fswatcher.WithPath(registered, fswatcher.WithPathFilter(filter)),
	)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	watchErr := make(chan error, 1)
	go func() { watchErr <- fsw.Watch(ctx) }()
	select {
	case <-ready:
	case err := <-watchErr:
		cancel()
		if err == nil {
			err = errors.New("indexer: checkout watch stopped before it was ready")
		}
		return nil, err
	case <-time.After(checkoutWatchReadyTimeout):
		cancel()
		fsw.Close()
		return nil, errors.New("indexer: checkout watch did not become ready")
	}
	w := &checkoutWatch{fsw: fsw, cancel: cancel, done: make(chan struct{})}
	go w.loop(ctx, events, dropped, watchErr, logger, onChange)
	return w, nil
}

// loop forwards events in batches: whatever is already queued when one event
// arrives is delivered with it, so an editor's save burst is one report.
func (w *checkoutWatch) loop(
	ctx context.Context,
	events <-chan fswatcher.WatchEvent,
	dropped <-chan fswatcher.WatchEvent,
	watchErr <-chan error,
	logger *zap.Logger,
	onChange func([]string),
) {
	defer close(w.done)
	for {
		var batch []string
		lost := false
		add := func(event fswatcher.WatchEvent) {
			for _, t := range event.Types {
				if t == fswatcher.EventOverflow {
					lost = true
					return
				}
			}
			if event.Path == "" {
				lost = true
				return
			}
			batch = append(batch, event.Path)
		}
		select {
		case <-ctx.Done():
			return
		case err := <-watchErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn("checkout watch: the file watcher stopped; the checkout is refreshed by its poll",
					zap.Error(err))
			}
			return
		case event := <-events:
			add(event)
		case <-dropped:
			lost = true
		}
	drain:
		for {
			select {
			case event := <-events:
				add(event)
			case <-dropped:
				lost = true
			default:
				break drain
			}
		}
		if lost {
			onChange(nil)
			continue
		}
		if len(batch) > 0 {
			onChange(batch)
		}
	}
}

// close stops the watcher and waits for its loop. Safe on nil and repeatable.
func (w *checkoutWatch) close() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}
		if w.fsw != nil {
			w.fsw.Close()
		}
		if w.done != nil {
			<-w.done
		}
	})
}
