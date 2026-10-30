package store_sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zzet/gortex/internal/graph"
)

// BindAnalysisWriteContext keeps the selected generation/revision and core,
// while binding only this non-owning handle's analysis writes. Legacy methods
// and the original handle retain their existing Background behavior.
func (s *Store) BindAnalysisWriteContext(ctx context.Context) graph.AnalysisGenerationStore {
	if s == nil {
		return nil
	}
	bound := *s
	bound.ownsCore = false
	bound.analysisWriteCtx = ctx
	return &bound
}

func (s *Store) analysisWriteContext() context.Context {
	if s.analysisWriteCtx != nil {
		return s.analysisWriteCtx
	}
	return context.Background()
}

// ownedWriteTx retains its exact physical connection until the real
// transaction ends. A canceled database/sql lifetime would let a second
// Rollback return ErrTxDone before its automatic rollback physically finished.
type ownedWriteTx struct {
	tx              *sql.Tx
	release         func() error
	committed       bool
	commitAttempted bool
}

// The caller holds writeMu. Connection admission observes ctx, including the
// ordinary pool path; BEGIN uses a detached owned lifetime. Native immediate
// BEGIN contention can still wait for busy_timeout after it starts. This is
// not an assertion that a requested cleanup budget bounds the whole BEGIN.
func (s *Store) beginOwnedAnalysisWrite(ctx context.Context) (*ownedWriteTx, error) {
	return s.atBase().beginOwnedWrite(ctx)
}

// beginOwnedWrite uses the selected payload handle, including its managed
// generation checks. The caller owns writeMu until finish has really returned.
func (s *Store) beginOwnedWrite(ctx context.Context) (*ownedWriteTx, error) {
	conn, release, err := s.acquireWriteConnContext(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := s.beginWriteOnWithLifetimeContext(ctx, context.WithoutCancel(ctx), conn)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	return &ownedWriteTx{tx: tx, release: release}, nil
}

func (s *Store) acquireWriteConnContext(ctx context.Context) (*sql.Conn, func() error, error) {
	conn := s.bulkConn
	release := func() error { return nil }
	if conn == nil {
		var err error
		conn, err = s.writerDB.Conn(ctx)
		if err != nil {
			return nil, nil, err
		}
		release = conn.Close
	}
	return conn, release, nil
}

func (o *ownedWriteTx) commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.commitAttempted = true
	err := o.tx.Commit()
	o.committed = err == nil
	return err
}

func (o *ownedWriteTx) finish(err *error) {
	if !o.committed {
		rollbackErr := o.tx.Rollback()
		// Commit always marks sql.Tx done; modernc's Commit synchronously
		// attempts driver rollback before returning its original error.
		// Preserve that original error, without disguising an unexpected
		// pre-Commit ErrTxDone as proof of physical cleanup.
		if o.commitAttempted && errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		*err = errors.Join(*err, rollbackErr)
	}
	*err = errors.Join(*err, o.release())
}
