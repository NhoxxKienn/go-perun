package channel

import "perun.network/go-perun/wallet"

// IsCoordinated returns whether the channel parameters contain a coordinator.
func IsCoordinated(coordinator map[wallet.BackendID]wallet.Address) bool {
	if coordinator == nil {
		return false
	}
	return len(coordinator) > 0
}

// IsValidCoordinatorWithBackend checks if the coordinator is valid and if every
// coordinator entry's backend is among the channel's backends.  All coordinator
// keys must be present in backends; a coordinator entry for an unknown backend
// would never be verified by any chain's contract.
func IsValidCoordinatorWithBackend(coordinator map[wallet.BackendID]wallet.Address, backends []wallet.BackendID) bool {
	if !IsCoordinated(coordinator) {
		return false
	}
	for coordBackend := range coordinator {
		found := false
		for _, id := range backends {
			if id == coordBackend {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
