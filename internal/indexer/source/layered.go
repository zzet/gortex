package source

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
)

// LayeredSource composes two sources into one namespace: an upper
// source that owns a subset of the paths, and a lower source that
// answers for everything else.
//
// Ownership is a predicate over the path, not a lookup — the upper
// source is asked exactly for the paths it claims, and the lower source
// for the rest. That keeps the composition honest about what it is: a
// routing rule, not a merge policy. In particular a path the upper
// layer claims but does not hold is simply absent; deciding that a
// claimed-and-absent path means "deleted" is masking logic, and it
// belongs to the caller that knows what the upper layer represents.
type LayeredSource struct {
	upper     ContentSource
	lower     ContentSource
	upperOwns func(path string) bool
}

// compile-time check that the source satisfies the interface.
var _ ContentSource = (*LayeredSource)(nil)

// NewLayeredSource routes every path that upperOwns accepts to upper
// and every other path to lower. Both sources must be non-nil; a nil
// upperOwns claims nothing, which routes everything to lower.
func NewLayeredSource(upper ContentSource, upperOwns func(path string) bool, lower ContentSource) *LayeredSource {
	if upperOwns == nil {
		upperOwns = func(string) bool { return false }
	}
	return &LayeredSource{upper: upper, lower: lower, upperOwns: upperOwns}
}

// Identity returns a stable description naming both layers in routing
// order.
func (s *LayeredSource) Identity() string {
	return "layered:" + s.upper.Identity() + "|" + s.lower.Identity()
}

// Close closes both layers and joins their errors, so one failing close
// does not hide the other layer's.
func (s *LayeredSource) Close() error {
	return errors.Join(s.upper.Close(), s.lower.Close())
}

// route returns the layer that answers for path.
func (s *LayeredSource) route(rel string) ContentSource {
	if s.upperOwns(rel) {
		return s.upper
	}
	return s.lower
}

// Stat reports metadata from whichever layer owns the path. An owned
// path is never looked up in the lower layer, even when the lower layer
// has it.
func (s *LayeredSource) Stat(p string) (FileMeta, error) {
	rel, err := normalizePath(p)
	if err != nil {
		return FileMeta{}, err
	}
	return s.route(rel).Stat(rel)
}

// Open serves content from whichever layer owns the path.
func (s *LayeredSource) Open(p string) (io.ReadCloser, FileMeta, error) {
	rel, err := normalizePath(p)
	if err != nil {
		return nil, FileMeta{}, err
	}
	return s.route(rel).Open(rel)
}

// Walk visits the union of both layers in the package's canonical path
// order: every entry the upper layer holds and owns, plus every lower
// entry the upper layer does not claim.
//
// The upper layer's owned entries are buffered — it is the small,
// selective layer by construction — and the lower layer streams, with
// buffered entries emitted at the position their path takes in the
// merged order. The result is the same sequence a single sorted source
// would produce.
func (s *LayeredSource) Walk(ctx context.Context, fn func(FileMeta) error) error {
	if fn == nil {
		return errors.New("layered source: nil walk function")
	}
	var owned []FileMeta
	err := s.upper.Walk(ctx, func(m FileMeta) error {
		if s.upperOwns(m.Path) {
			owned = append(owned, m)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortFunc(owned, func(a, b FileMeta) int { return strings.Compare(a.Path, b.Path) })

	next := 0
	// drainBefore emits every buffered upper entry that sorts before
	// limit, so the merged stream stays in path order.
	drainBefore := func(limit string) error {
		for next < len(owned) && owned[next].Path < limit {
			if err := fn(owned[next]); err != nil {
				return err
			}
			next++
		}
		return nil
	}

	err = s.lower.Walk(ctx, func(m FileMeta) error {
		if err := drainBefore(m.Path); err != nil {
			return err
		}
		if s.upperOwns(m.Path) {
			// The upper layer answers for this path. Its own entry, if
			// it has one, takes the slot here.
			if next < len(owned) && owned[next].Path == m.Path {
				if err := fn(owned[next]); err != nil {
					return err
				}
				next++
			}
			return nil
		}
		return fn(m)
	})
	if err != nil {
		return err
	}
	for ; next < len(owned); next++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(owned[next]); err != nil {
			return err
		}
	}
	return nil
}
