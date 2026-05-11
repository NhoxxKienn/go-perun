package multi

import (
	"context"
	"fmt"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/wallet"
)

// Coordinator is a multi-ledger coordinator.
type Coordinator struct {
	coordinators map[LedgerBackendKey]channel.CoordinatorSubscriber
}

// NewCoordinator creates a new coordinator.
func NewCoordinator() *Coordinator {
	return &Coordinator{
		coordinators: make(map[LedgerBackendKey]channel.CoordinatorSubscriber),
	}
}

// RegisterCoordinator registers a coordinator for a given ledger.
func (a *Coordinator) RegisterCoordinator(l LedgerBackendID, la channel.CoordinatorSubscriber) {
	key := LedgerBackendKey{BackendID: l.BackendID(), LedgerID: string(l.LedgerID().MapKey())}
	a.coordinators[key] = la
}

// LedgerCoordinator returns the coordinator for a given ledger.
func (a *Coordinator) LedgerCoordinator(l LedgerBackendID) (channel.CoordinatorSubscriber, bool) {
	key := LedgerBackendKey{BackendID: l.BackendID(), LedgerID: string(l.LedgerID().MapKey())}
	coord, ok := a.coordinators[key]
	return coord, ok
}

// Coordinate coordinates the state of a multi-ledger channel. It dispatches
// Coordinate calls to all relevant coordinators. If any of the calls fails, the
// method returns an error.
func (a *Coordinator) Coordinate(ctx context.Context, req channel.AdjudicatorReq, subStates []channel.SignedState, coordSigs []wallet.Sig) error {
	ledgerIDs, err := assets(req.Tx.Assets).LedgerIDs()
	if err != nil {
		return err
	}

	err = a.dispatch(ledgerIDs, func(lc channel.CoordinatorSubscriber) error {
		return lc.Coordinate(ctx, req, subStates, coordSigs)
	})
	return err
}

// dispatch dispatches an adjudicator call on all given ledgers.
func (a *Coordinator) dispatch(assetIds []LedgerBackendID, f func(channel.CoordinatorSubscriber) error) error {
	n := len(assetIds)
	errs := make(chan error, n)

	for _, l := range assetIds {
		go func(l LedgerBackendID) {
			err := func() error {
				key := LedgerBackendKey{BackendID: l.BackendID(), LedgerID: string(l.LedgerID().MapKey())}

				coordinator, ok := a.coordinators[key]
				if !ok {
					return fmt.Errorf("coordinator not found for id %v", l)
				}

				// Call the provided function f with the CoordinatorSubscriber
				err := f(coordinator)
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
