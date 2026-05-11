package channel

import "perun.network/go-perun/wallet"

// IsCoordinated returns whether the channel parameters contain a coordinator.
func IsCoordinated(coordinator map[wallet.BackendID]wallet.Address) bool {
	if coordinator == nil {
		return false
	}
	return len(coordinator) > 0
}

// IsValidCoordinatorWithBackend checks if the coordinator is valid and if its backend is among the backends of the channel participants.
func IsValidCoordinatorWithBackend(coordinator map[wallet.BackendID]wallet.Address, backends []wallet.BackendID) bool {
	if !IsCoordinated(coordinator) {
		return false
	}
	// Check if the coordinator's backend is among the backends of the channel participants.
	for backend := range coordinator {
		for _, id := range backends {
			if id == backend {
				return true
			}
		}
	}
	return false
}
