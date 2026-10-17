package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
)

// Released generation references, reported to the retirement sweep.
//
// A retirement the catalog refuses because something still references the
// generation can only succeed once that reference goes, so the sweep parks it
// until a release may have freed it. The catalog writes that drop a reference
// report it here, after their transaction commits: a removed generation
// releases the "based" reference it held on its base, a released ref view the
// generation it pointed at, an adopted dedicated base the head it replaced, a
// re-installed or repointed route (UpsertCheckoutRoute, FlipCheckoutRoute) and
// a dedicated graph's moved active pointer what they named before. The writes
// that cannot cheaply name what they freed — a withdrawn route, a deleted ref
// view or dedicated graph, a failed or cleaned-up dedicated publication —
// report a release of any generation.
//
// Some references no write here names: a contract attachment stays referenced
// while another generation's contract input state or work rows match it, and
// those go with that generation's sweep or a base-state update. A refusal says
// which kinds held the generation (GenerationReferencedError), so the sweep
// can wait for a named release only when nothing but a generation built on it
// did, and take any release as its cue otherwise.

// GenerationReferenceRelease is one committed catalog write that may have
// released generation references.
type GenerationReferenceRelease struct {
	// Removed is the generation whose catalog row this write deleted (0 for
	// none).
	Removed int64
	// Released names the generations whose references may have been
	// released. Empty with Any false means none.
	Released []int64
	// Any reports a release the write cannot name: any generation's reference
	// may have gone.
	Any bool
}

var generationReferenceReleaseObserver atomic.Pointer[func(GenerationReferenceRelease)]

// OnGenerationReferencesReleased installs the process-wide observer of
// released generation references (nil removes it). It runs after the
// releasing transaction commits, on the writer's goroutine, and must not block.
func OnGenerationReferencesReleased(observer func(GenerationReferenceRelease)) {
	if observer == nil {
		generationReferenceReleaseObserver.Store(nil)
		return
	}
	generationReferenceReleaseObserver.Store(&observer)
}

// noteGenerationReferencesReleased reports a committed release.
func noteGenerationReferencesReleased(release GenerationReferenceRelease) {
	if observer := generationReferenceReleaseObserver.Load(); observer != nil {
		(*observer)(release)
	}
}

// noteAnyGenerationReferenceReleased reports a committed release the write
// cannot name.
func noteAnyGenerationReferenceReleased() {
	noteGenerationReferencesReleased(GenerationReferenceRelease{Any: true})
}

// noteGenerationReferenceReleased reports a committed release of one named
// generation's reference.
func noteGenerationReferenceReleased(generationID int64) {
	if generationID > 0 {
		noteGenerationReferencesReleased(GenerationReferenceRelease{Released: []int64{generationID}})
	}
}

// positiveIDs is ids without the zero (none) values.
func positiveIDs(ids ...int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// GenerationReferencedError is a retirement refused for the references the
// generation still had, when the refusal knows them. It matches
// ErrCatalogGenerationReferenced.
type GenerationReferencedError struct {
	GenerationID int64
	Refs         ViewGenerationReferences
}

func (e *GenerationReferencedError) Error() string {
	return fmt.Sprintf("%v: generation %d", ErrCatalogGenerationReferenced, e.GenerationID)
}

func (e *GenerationReferencedError) Unwrap() error { return ErrCatalogGenerationReferenced }

// OnlyBased reports references that are nothing but generations built on
// this one: the one kind a release can always name, since only removing or
// re-basing the generation above releases it.
func (r ViewGenerationReferences) OnlyBased() bool {
	return r.Based && !r.Routed && !r.RefViewed && !r.GraphActive && !r.DedicatedPublication && !r.ContractAttached
}

// routeGenerationsTx reads the generations a checkout's route names now (none
// when it has no row), so a write that repoints it can name what it released.
func routeGenerationsTx(ctx context.Context, tx *sql.Tx, checkoutID string) ([]int64, error) {
	var commit, dirty sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT commit_generation_id, dirty_generation_id FROM checkout_routes WHERE checkout_id = ?`,
		checkoutID).Scan(&commit, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return positiveIDs(commit.Int64, dirty.Int64), nil
}

// releasedRouteGenerations is what a route that named previous no longer
// names once it names commit and dirty.
func releasedRouteGenerations(previous []int64, commit, dirty int64) []int64 {
	var out []int64
	for _, id := range previous {
		if id != commit && id != dirty {
			out = append(out, id)
		}
	}
	return out
}
