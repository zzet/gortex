package indexer

import (
	"context"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

type readyCommitLayer struct {
	generationID int64
	leaseToken   string
	cacheKey     store_sqlite.ReadyGenerationCacheKey
	built        bool
	reused       bool
}

func (c *CheckoutCoordinator) readyCommitCacheKey(
	base primaryBase,
	targetTree string,
) store_sqlite.ReadyGenerationCacheKey {
	return commitLayerReadyGenerationKey(
		base.graphID,
		base.generationID,
		targetTree,
		c.configHash,
		c.extractors,
	)
}

// resolveReadyCommitLayer adopts the graph-scoped canonical ready generation
// before paying build cost. On a miss it builds once, offers the candidate to
// the cache, and adopts the winner of any concurrent race. Every successful
// return owns a short-lived handoff lease which publication must consume or
// release.
func (c *CheckoutCoordinator) resolveReadyCommitLayer(
	ctx context.Context,
	base primaryBase,
	targetTree string,
) (readyCommitLayer, error) {
	key := c.readyCommitCacheKey(base, targetTree)
	required := commitLayerRequiredCapabilities()
	claim, found, err := c.catalog.ClaimReadyGeneration(ctx, store_sqlite.ClaimReadyGenerationRequest{
		Key:                  key,
		RequiredCapabilities: required,
	})
	if err != nil {
		return readyCommitLayer{}, err
	}
	if found {
		return readyCommitLayer{
			generationID: claim.WinnerGenerationID,
			leaseToken:   claim.LeaseToken,
			cacheKey:     key,
			reused:       true,
		}, nil
	}

	candidate, retained, err := c.resolveCommitLayer(ctx, base, targetTree)
	if err != nil {
		return readyCommitLayer{}, err
	}
	claim, found, err = c.catalog.ClaimReadyGeneration(ctx, store_sqlite.ClaimReadyGenerationRequest{
		Key:                   key,
		CandidateGenerationID: candidate,
		RequiredCapabilities:  required,
	})
	if err != nil {
		if !retained {
			c.supersede(ctx, candidate)
			c.offerRetire(ctx, candidate)
		}
		return readyCommitLayer{}, err
	}
	if !found {
		if !retained {
			c.supersede(ctx, candidate)
			c.offerRetire(ctx, candidate)
		}
		return readyCommitLayer{}, fmt.Errorf("indexer: ready commit candidate %d disappeared before claim", candidate)
	}
	return readyCommitLayer{
		generationID: claim.WinnerGenerationID,
		leaseToken:   claim.LeaseToken,
		cacheKey:     key,
		built:        !retained,
		reused:       retained || claim.Reused,
	}, nil
}

func (c *CheckoutCoordinator) releaseReadyCommitLease(leaseToken string) {
	if leaseToken == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.catalog.ReleaseReadyGenerationLease(ctx, leaseToken); err != nil {
		c.logger.Warn("checkout coordinator: release ready generation lease",
			zap.String("checkout", c.checkoutID),
			zap.Error(err))
	}
}

// moveReadyCommitSlot validates and consumes the canonical cache lease in the
// same catalog transaction that advances the route. This prevents collection,
// stale-cache adoption, and route races from opening a handoff gap.
func (c *CheckoutCoordinator) moveReadyCommitSlot(
	ctx context.Context,
	route *store_sqlite.CheckoutRoute,
	resolved readyCommitLayer,
) error {
	state := store_sqlite.RouteActive
	if route.DirtyGenerationID > 0 {
		state = store_sqlite.RoutePending
	}
	err := c.catalog.BindReadyGenerationLeaseToCheckout(ctx,
		store_sqlite.BindReadyGenerationLeaseToCheckoutRequest{
			Key:                resolved.cacheKey,
			LeaseToken:         resolved.leaseToken,
			CheckoutID:         c.checkoutID,
			ExpectedRouteEpoch: route.RouteEpoch,
			GenerationID:       resolved.generationID,
			State:              state,
		})
	if err != nil {
		c.releaseReadyCommitLease(resolved.leaseToken)
		return err
	}
	dropped := route.DirtyGenerationID
	route.RouteEpoch++
	route.State = state
	route.CommitGenerationID = resolved.generationID
	route.DirtyGenerationID = 0
	c.rememberRoutedDirty(0)
	c.offerRetire(ctx, dropped)
	return nil
}
