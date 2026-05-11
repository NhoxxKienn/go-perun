package multi

import (
	"context"

	"perun.network/go-perun/channel"
)

// CoordinatorNotifier is an optional interface for notifying an external coordinator
// to start watching a channel.
// This is only invoked when a channel has a non-nil coordinator.
type CoordinatorNotifier interface {
	NotifyWatchLedgerChannel(ctx context.Context, signedState channel.SignedState) error
	NotifyWatchSubChannel(_ context.Context, parent channel.ID, signedState channel.SignedState) error
	NotifyStopWatch(ctx context.Context, id channel.ID) error
}
