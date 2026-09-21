package store_sqlite

import (
	"context"
	"fmt"
)

// WaitPayloadBuildFlights waits only for already-existing physical owners. It
// never creates a flight, recovers a building generation, writes SQL, or starts
// a goroutine. Cleanup must close/join graph-owned producer admission first;
// this snapshot wait is not an independent admission fence. Retirement still
// performs its atomic pre/post-fence ownership checks afterwards.
func (s *Store) WaitPayloadBuildFlights(ctx context.Context, ids ...int64) error {
	if ctx == nil || s == nil || s.storeCore == nil {
		return fmt.Errorf("%w: cleanup flight wait needs context and store", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if id <= 0 {
			return fmt.Errorf("%w: cleanup flight generation %d", ErrCatalogInvalidValue, id)
		}
		value, active := s.payloadBuildFlights.Load(id)
		if !active {
			continue
		}
		state := value.(*payloadBuildFlightState)
		select {
		case <-state.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
