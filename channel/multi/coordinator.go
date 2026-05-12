package multi

import (
	"context"
	"fmt"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/wallet"
)

var _ channel.CoordinatorSubscriber = (*Coordinator)(nil)

// Coordinator is the multi-ledger coordinator.
type Coordinator struct {
	coordinators map[LedgerBackendKey]channel.CoordinatorSubscriber
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

	err = c.dispatch(ledgerIDs, func(lc channel.Coordinator) error {
		return lc.Coordinate(ctx, req, signedStates, coordSigs)
	})
	return err
}

// Subscribe creates a new multi-ledger AdjudicatorSubscription.
func (c *Coordinator) Subscribe(ctx context.Context, chID channel.ID) (channel.AdjudicatorSubscription, error) {
	asub := &AdjudicatorSubscription{
		events: make(chan channel.AdjudicatorEvent),
		errors: make(chan error),
		subs:   []channel.AdjudicatorSubscription{},
		done:   make(chan struct{}),
	}

	for _, lc := range c.coordinators {
		sub, err := lc.Subscribe(ctx, chID)
		if err != nil {
			asub.Close()
			return nil, err
		}
		asub.subs = append(asub.subs, sub)

		go func() {
			for {
				select {
				case asub.events <- sub.Next():
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
func (c *Coordinator) dispatch(assetIDs []LedgerBackendID, f func(channel.Coordinator) error) error {
	n := len(assetIDs)
	errs := make(chan error, n)

	for _, l := range assetIDs {
		go func(l LedgerBackendID) {
			err := func() error {
				key := LedgerBackendKey{BackendID: l.BackendID(), LedgerID: string(l.LedgerID().MapKey())}

				coord, ok := c.coordinators[key]
				if !ok {
					return fmt.Errorf("coordinator not found for id %v", l)
				}

				// Call the provided function f with the Coordinator
				err := f(coord)
				return err
			}()
			errs <- err
		}(l)
	}

	for range n {
		err := <-errs
		if err != nil {
			return err
		}
	}

	return nil
}
