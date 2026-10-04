package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ErrPayloadInputChanged refuses publication when a selected input changed.
var ErrPayloadInputChanged = fmt.Errorf("%w: payload input changed", ErrCatalogStaleGuard)

// PayloadInputWitness binds selected ancestor rows to one live store core.
// It is process-local proof, not persisted cache authority.
type PayloadInputWitness struct {
	core      *storeCore
	revisions []payloadInputRevision
}
type payloadInputRevision struct {
	generation          int64
	analysis, constants uint64
}

// CapturePayloadInputWitness must precede the checked projection. It includes
// exactly the supplied generations, holds no gate after return, and excludes no supplied
// ancestor. Callers must omit the generation they are about to build.
func (s *Store) CapturePayloadInputWitness(ctx context.Context, generationIDs []int64) (*PayloadInputWitness, error) {
	if ctx == nil || s == nil || s.coreless() {
		return nil, fmt.Errorf("%w: invalid input witness", ErrCatalogInvalidValue)
	}
	if len(generationIDs) == 0 {
		return nil, fmt.Errorf("%w: empty input witness", ErrCatalogInvalidValue)
	}
	ids := map[int64]bool{}
	for _, g := range generationIDs {
		if g < 0 {
			return nil, fmt.Errorf("%w: negative input generation", ErrCatalogInvalidValue)
		}
		ids[g] = true
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer s.writeMu.Unlock()
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}
	w := &PayloadInputWitness{core: s.storeCore}
	for g := range ids {
		w.revisions = append(w.revisions, payloadInputRevision{g, s.analysisViewCounter(g).Load(), s.constantInputCounter(g).Load()})
	}
	sort.Slice(w.revisions, func(i, j int) bool { return w.revisions[i].generation < w.revisions[j].generation })
	return w, nil
}

func (w *PayloadInputWitness) validateLocked(s *Store) error {
	if w.core != s.storeCore || len(w.revisions) == 0 {
		return ErrPayloadInputChanged
	}
	for _, r := range w.revisions {
		if r.analysis != s.analysisViewCounter(r.generation).Load() || r.constants != s.constantInputCounter(r.generation).Load() {
			return ErrPayloadInputChanged
		}
	}
	return nil
}

// PublishPayloadGenerationWithInputWitness leaves existing publication callers
// unchanged. Ancestor validation and the ready transition share one write gate.
func (s *Store) PublishPayloadGenerationWithInputWitness(ctx context.Context, generationID, publishedAt int64, witness *PayloadInputWitness) error {
	if witness == nil {
		return ErrPayloadInputChanged
	}
	p, err := s.PreparePayloadGenerationPublication(ctx, generationID)
	if err != nil {
		return err
	}
	return p.PublishWithInputWitness(ctx, publishedAt, witness)
}

func (p *PreparedPayloadGenerationPublication) PublishWithInputWitness(ctx context.Context, publishedAt int64, witness *PayloadInputWitness) error {
	if witness == nil {
		return ErrPayloadInputChanged
	}
	return p.publishWithInputWitness(ctx, publishedAt, true, witness)
}

func (c *Catalog) execGuardedWithInputWitness(ctx context.Context, w *PayloadInputWitness, subject, query string, args ...any) error {
	if w == nil {
		return c.execGuarded(ctx, subject, query, args...)
	}
	if ctx == nil {
		return errors.New("nil publication context")
	}
	if err := c.store.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer c.store.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.validateLocked(c.store); err != nil {
		return err
	}
	result, err := c.store.execActiveWriteLocked(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrCatalogStaleGuard, subject)
	}
	return nil
}
