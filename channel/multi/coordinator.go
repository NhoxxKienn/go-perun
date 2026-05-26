package multi

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/wallet"
)

var _ channel.CoordinatorSubscriber = (*Coordinator)(nil)

// Coordinator is the multi-ledger coordinator.
type Coordinator struct {
	coordinators map[LedgerBackendKey]channel.CoordinatorSubscriber
}

// NewCoordinator creates a new multi-ledger coordinator.
func NewCoordinator() *Coordinator {
	return &Coordinator{
		coordinators: make(map[LedgerBackendKey]channel.CoordinatorSubscriber),
	}
}

// RegisterCoordinator registers a coordinator for a given ledger.
func (c *Coordinator) RegisterCoordinator(l LedgerBackendID, lc channel.CoordinatorSubscriber) {
	key := LedgerBackendKey{BackendID: l.BackendID(), LedgerID: string(l.LedgerID().MapKey())}
	c.coordinators[key] = lc
}

// LedgerCoordinator returns the coordinator for a given ledger.
func (c *Coordinator) LedgerCoordinator(l LedgerBackendID) (channel.CoordinatorSubscriber, bool) {
	key := LedgerBackendKey{BackendID: l.BackendID(), LedgerID: string(l.LedgerID().MapKey())}
	coord, ok := c.coordinators[key]
	return coord, ok
}

// Coordinate coordinates a multi-ledger channel. It dispatches Coordinate calls to
// all relevant coordinators. If any of the calls fails, the method returns an
// error.
func (c *Coordinator) Coordinate(ctx context.Context, req channel.AdjudicatorReq, signedStates []channel.SignedState, coordSigs []wallet.Sig) error {
	ledgerIDs, err := assets(req.Tx.Assets).LedgerIDs()
	if err != nil {
		return err
	}

	return c.dispatch(ctx, ledgerIDs, func(lc channel.Coordinator) error {
		return lc.Coordinate(ctx, req, signedStates, coordSigs)
	})
}

// Subscribe creates a new multi-ledger AdjudicatorSubscription.
func (c *Coordinator) Subscribe(ctx context.Context, chID channel.ID) (channel.AdjudicatorSubscription, error) {
	asub := &AdjudicatorSubscription{
		events: make(chan LedgerAdjudicatorEvent),
		errors: make(chan error),
		subs:   []channel.AdjudicatorSubscription{},
		done:   make(chan struct{}),
	}

	for key, la := range c.coordinators {
		sub, err := la.Subscribe(ctx, chID)
		if err != nil {
			asub.Close()
			return nil, err
		}
		asub.subs = append(asub.subs, sub)

		go func() {
			for {
				e := sub.Next()
				select {
				case asub.events <- LedgerAdjudicatorEvent{LedgerKey: key, AdjudicatorEvent: e}:
				case <-asub.done:
					return
				}
			}
		}()

		go func() {
			asub.errors <- sub.Err()
		}()
	}

	return asub, nil
}

// dispatch dispatches an adjudicator call on all given ledgers.
func (c *Coordinator) dispatch(ctx context.Context, assetIDs []LedgerBackendID, f func(channel.Coordinator) error) error {
	g, _ := errgroup.WithContext(ctx)

	for _, l := range assetIDs {
		l := l
		g.Go(func() error {
			key := LedgerBackendKey{BackendID: l.BackendID(), LedgerID: string(l.LedgerID().MapKey())}

			coord, ok := c.coordinators[key]
			if !ok {
				return fmt.Errorf("coordinator not found for id %v", l)
			}

			// Call the provided function f with the Coordinator. If the
			// underlying implementation respects context cancellation, use
			// gctx via closures passed to f.
			return f(coord)
		})
	}

	return g.Wait()
}
