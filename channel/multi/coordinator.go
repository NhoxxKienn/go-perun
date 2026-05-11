package multi

import (
	"context"

	"perun.network/go-perun/channel"
)

// CoordinatorNotifier is an optional interface for notifying an external coordinator
// to start watching a channel.
// This is only invoked when a channel has a non-nil coordinator.
type CoordinatorNotifier interface {
	NotifyWatch(ctx context.Context, signedState channel.SignedState) error
}
