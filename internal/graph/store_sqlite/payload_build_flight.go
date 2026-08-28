package store_sqlite

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// payloadBuildFlightState is one process-local writer rendezvous for a payload
// generation. The state belongs to storeCore, so every Store handle derived
// from the same open database observes the same writer. It is deliberately not
// persisted: a process restart has no live writer to wait for and a later
// caller must recover a catalog generation still left building.
type payloadBuildFlightState struct {
	done    chan struct{}
	once    sync.Once
	err     error
	waiters atomic.Int64
}

// PayloadBuildFlight is a process-local lease on one generation's physical
// build. Exactly one joined caller is the leader; followers wait for its result
// without sharing or canceling its context.
type PayloadBuildFlight struct {
	core         *storeCore
	generationID int64
	state        *payloadBuildFlightState
	leader       bool
}

// PayloadBuildFlightStart is the atomic allocation/adoption and flight-join
// verdict for one physical payload build.
type PayloadBuildFlightStart struct {
	GenerationID int64
	Handle       *Store
	Adopted      bool
	Flight       *PayloadBuildFlight
	Leader       bool
	Ready        bool
}

// BeginPayloadBuildFlight keeps generation adoption and process-local flight
// installation in one lifecycle critical section. beforeJoin is an observer
// invoked while that section is held; it must not start another payload
// lifecycle transition on this store.
func (s *Store) BeginPayloadBuildFlight(
	ctx context.Context,
	req PayloadGenerationRequest,
	beforeJoin func(generationID int64, adopted bool),
) (start PayloadBuildFlightStart, err error) {
	if ctx == nil {
		return start, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if s == nil || s.storeCore == nil {
		return start, fmt.Errorf("%w: payload build flight needs an open store", ErrCatalogInvalidValue)
	}

	s.payloadLifecycleMu.Lock()
	defer s.payloadLifecycleMu.Unlock()
	start.GenerationID, start.Handle, start.Adopted, err = s.beginPayloadGenerationWithStatus(ctx, req)
	if err != nil {
		return start, err
	}
	if beforeJoin != nil {
		beforeJoin(start.GenerationID, start.Adopted)
	}
	start.Flight, start.Leader, start.Ready, err = s.joinPayloadBuildFlightLocked(
		ctx, start.GenerationID, start.Adopted,
	)
	return start, err
}

// JoinPayloadBuildFlight coalesces physical payload construction by catalog
// generation ID. The returned booleans are mutually exclusive:
//
//   - leader means the caller owns the physical build and must call Complete;
//   - ready means an adopted generation became ready before this caller could
//     join its former flight and can be reused immediately;
//   - otherwise the caller is a follower and must call Wait.
//
// If an adopted generation is still building but its former process-local
// flight is gone, this caller becomes the recovery leader. No lock is persisted,
// so a process restart naturally takes the same recovery path.
func (s *Store) JoinPayloadBuildFlight(
	ctx context.Context,
	generationID int64,
	adopted bool,
) (flight *PayloadBuildFlight, leader, ready bool, err error) {
	if ctx == nil {
		return nil, false, false, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if s == nil || s.storeCore == nil {
		return nil, false, false, fmt.Errorf("%w: payload build flight needs an open store", ErrCatalogInvalidValue)
	}
	if generationID <= 0 {
		return nil, false, false, fmt.Errorf("%w: payload build flight generation %d", ErrCatalogInvalidValue, generationID)
	}

	s.payloadLifecycleMu.Lock()
	defer s.payloadLifecycleMu.Unlock()
	return s.joinPayloadBuildFlightLocked(ctx, generationID, adopted)
}

// joinPayloadBuildFlightLocked runs with payloadLifecycleMu held, so retirement
// cannot claim an adopted generation between flight installation and state
// validation.
func (s *Store) joinPayloadBuildFlightLocked(
	ctx context.Context,
	generationID int64,
	adopted bool,
) (flight *PayloadBuildFlight, leader, ready bool, err error) {
	candidate := &payloadBuildFlightState{done: make(chan struct{})}
	actual, loaded := s.payloadBuildFlights.LoadOrStore(generationID, candidate)
	state := actual.(*payloadBuildFlightState)
	flight = &PayloadBuildFlight{
		core:         s.storeCore,
		generationID: generationID,
		state:        state,
		leader:       !loaded,
	}
	if loaded {
		return flight, false, false, nil
	}
	if !adopted {
		return flight, true, false, nil
	}

	generation, found, readErr := s.Catalog().GetViewGeneration(ctx, generationID)
	if readErr != nil {
		flight.Complete(readErr)
		return nil, false, false, readErr
	}
	if !found {
		readErr = fmt.Errorf("%w: adopted payload generation %d disappeared", ErrCatalogInvalidValue, generationID)
		flight.Complete(readErr)
		return nil, false, false, readErr
	}
	switch generation.State {
	case ViewGenerationReady:
		flight.Complete(nil)
		return nil, false, true, nil
	case ViewGenerationBuilding:
		return flight, true, false, nil
	default:
		readErr = fmt.Errorf("%w: adopted payload generation %d is %q", ErrCatalogInvalidValue, generationID, generation.State)
		flight.Complete(readErr)
		return nil, false, false, readErr
	}
}

// Wait waits for the physical writer without transferring cancellation to it.
// A canceled follower leaves the flight intact for the leader and other
// followers.
func (f *PayloadBuildFlight) Wait(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if f == nil || f.state == nil {
		return fmt.Errorf("%w: nil payload build flight", ErrCatalogInvalidValue)
	}
	f.state.waiters.Add(1)
	defer f.state.waiters.Add(-1)
	select {
	case <-f.state.done:
		return f.state.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Complete publishes the leader's process-local result to its followers and
// removes the rendezvous. It is idempotent; a follower calling it is a no-op.
func (f *PayloadBuildFlight) Complete(err error) {
	if f == nil || f.state == nil || f.core == nil || !f.leader {
		return
	}
	f.state.once.Do(func() {
		f.state.err = err
		close(f.state.done)
		f.core.payloadBuildFlights.CompareAndDelete(f.generationID, f.state)
	})
}

// PayloadBuildFlightWaiters reports how many followers currently wait on a
// generation's in-process writer. It is lock-free diagnostic state and returns
// zero after the flight completes or when the generation has no live writer.
func (s *Store) PayloadBuildFlightWaiters(generationID int64) int64 {
	if s == nil || s.storeCore == nil {
		return 0
	}
	value, ok := s.payloadBuildFlights.Load(generationID)
	if !ok {
		return 0
	}
	return value.(*payloadBuildFlightState).waiters.Load()
}

// PayloadBuildFlightActive reports whether this process currently owns the
// physical writer rendezvous for a generation. Retirement uses it as a
// process-local lease: a catalog row may be old because it was adopted after a
// restart, but it is not abandoned while a recovery leader is actively
// building it.
func (s *Store) PayloadBuildFlightActive(generationID int64) bool {
	if s == nil || s.storeCore == nil || generationID <= 0 {
		return false
	}
	return s.payloadBuildFlightActiveLocked(generationID)
}

// payloadBuildFlightActiveLocked is the lifecycle-decision form. Callers that
// use the answer to mutate durable state hold payloadLifecycleMu across this
// load and their catalog claim.
func (s *Store) payloadBuildFlightActiveLocked(generationID int64) bool {
	_, ok := s.payloadBuildFlights.Load(generationID)
	return ok
}
