// Copyright 2025 - See NOTICE file for copyright holders.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/client"
)

// TestMultiLedgerVirtualCoordinate runs the coordinator-assisted dispute
// scenario for a multi-ledger virtual channel.  Parent ledger channels are
// opened with a designated TTP coordinator.  A dispute is triggered by
// registering the parent channels (with the virtual sub-channel state
// included); the coordinator co-signs the resolution; and all parties settle.
//
//nolint:revive // test.Test... stutters but this is OK in this special case.
func TestMultiLedgerVirtualCoordinate( //nolint:cyclop
	ctx context.Context,
	t *testing.T,
	mls MultiLedgerVirtualSetup,
	_ uint64,
) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)

	// Record on-chain balances before any channel is funded.
	balancesBefore := channel.Balances{
		{
			mls.Alice.BalanceReader1.Balance(mls.Asset1),
			mls.Bob.BalanceReader1.Balance(mls.Asset1),
		},
		{
			mls.Alice.BalanceReader2.Balance(mls.Asset2),
			mls.Bob.BalanceReader2.Balance(mls.Asset2),
		},
	}

	// Open channels with coordinator enabled on both parent ledger channels.
	mlvc, errs := openMultiLedgerVirtualChannels(ctx, t, mls, true)

	// Start parent channel watchers first so they register with the watcher
	// before the virtual sub-channel watchers call StartWatchingSubChannel
	// (which requires the parent to already be in the watcher registry).
	//nolint:contextcheck
	go func() { errs <- mlvc.chAliceHub.Watch(mls.Alice) }()
	//nolint:contextcheck
	go func() { errs <- mlvc.chBobHub.Watch(mls.Bob) }()
	// Hub's parent channels also need watchers so their machine phases get
	// updated to Coordinated before Settle is called.
	//nolint:contextcheck
	go func() { errs <- mlvc.chHubAlice.Watch(mls.Hub) }()
	//nolint:contextcheck
	go func() { errs <- mlvc.chHubBob.Watch(mls.Hub) }()

	// Sleep to ensure all parent watchers have called StartWatchingLedgerChannel
	// and are registered before the sub-channel watchers start.
	time.Sleep(mls.WaitWatcherTimeout)

	// Now start sub-channel (virtual channel) watchers.  The watcher's
	// retrieveLatestSubStates uses the registry to find sub-channel states; the
	// virtual channel must be in the registry before any RegisteredEvent fires.
	//nolint:contextcheck
	go func() { errs <- mlvc.chAliceBob.Watch(mls.Alice) }()
	//nolint:contextcheck
	go func() { errs <- mlvc.chBobAlice.Watch(mls.Bob) }()

	// Hub may have the virtual channel in its client registry (it co-signed the
	// sub-channel funding update).  If so, watch it so Hub's watcher can include
	// the sub-state when Hub's parent channels get a RegisteredEvent.
	if hubVirtual, err := mls.Hub.Channel(mlvc.chAliceBob.ID()); err == nil {
		//nolint:contextcheck
		go func() { errs <- hubVirtual.Watch(mls.Hub) }()
	}

	// Wait for all sub-channel watchers to complete StartWatchingSubChannel.
	time.Sleep(mls.WaitWatcherTimeout)

	// Build the virtual channel's signed state to include in Coordinate calls.
	// Each parent channel (Alice-Hub, Bob-Hub) has the virtual channel locked as
	// a sub-channel; Coordinate must receive its signed state so the coordinator
	// can co-sign the full channel tree.
	vtcReq := client.NewTestChannel(mlvc.chAliceBob).AdjudicatorReq()
	virtualSignedState := channel.SignedState{
		Params: vtcReq.Params,
		State:  vtcReq.Tx.State,
		Sigs:   vtcReq.Tx.Sigs,
	}
	virtualSubStates := []channel.SignedState{virtualSignedState}

	// Alice registers her parent channel on both ledgers.  The watcher detects
	// the event and re-registers with the virtual sub-channel state to
	// synchronise L1 and L2.
	reqAlice := client.NewTestChannel(mlvc.chAliceHub).AdjudicatorReq()
	err := mls.Alice.Adjudicator1.Register(ctx, reqAlice, nil)
	require.NoError(err, "Alice: registering Alice-Hub on L1")
	err = mls.Alice.Adjudicator2.Register(ctx, reqAlice, nil)
	require.NoError(err, "Alice: registering Alice-Hub on L2")

	// Bob registers his parent channel on both ledgers.
	reqBob := client.NewTestChannel(mlvc.chBobHub).AdjudicatorReq()
	err = mls.Bob.Adjudicator1.Register(ctx, reqBob, nil)
	require.NoError(err, "Bob: registering Bob-Hub on L1")
	err = mls.Bob.Adjudicator2.Register(ctx, reqBob, nil)
	require.NoError(err, "Bob: registering Bob-Hub on L2")

	// Wait for RegisteredEvent on Alice and Bob (watcher → Events channel).
	eAlice := <-mls.Alice.Events
	require.IsType(&channel.RegisteredEvent{}, eAlice, "Alice: expected RegisteredEvent")
	err = eAlice.(*channel.RegisteredEvent).TimeoutV.Wait(ctx)
	require.NoError(err, "Alice: waiting for challenge timeout")

	eBob := <-mls.Bob.Events
	require.IsType(&channel.RegisteredEvent{}, eBob, "Bob: expected RegisteredEvent")

	// Allow watchers to finish processing before coordinating.
	time.Sleep(mls.WaitWatcherTimeout)

	// Coordinator co-signs the resolution for Alice-Hub with the virtual
	// sub-channel's signed state.  multi.Coordinator.Coordinate dispatches to
	// all ledgers backing the channel's assets (L1 and L2).
	err = mls.Coordinator.Coordinate(ctx, reqAlice, virtualSubStates, mlvc.bID1)
	require.NoError(err, "coordinator: coordinating Alice-Hub")

	// Coordinator co-signs the resolution for Bob-Hub with the same virtual
	// sub-channel state (the virtual channel is also locked in Bob-Hub).
	err = mls.Coordinator.Coordinate(ctx, reqBob, virtualSubStates, mlvc.bID1)
	require.NoError(err, "coordinator: coordinating Bob-Hub")

	// Allow watchers to process CoordinatedEvent and update machine phases.
	time.Sleep(mls.WaitWatcherTimeout)

	// Settle all four parent channel views.  Machine phases have been updated
	// to Coordinated by the watchers, so ensureCoordinated returns immediately.
	parentChs := []*client.Channel{
		mlvc.chAliceHub, mlvc.chHubAlice,
		mlvc.chBobHub, mlvc.chHubBob,
	}
	isSecondary := [2]bool{false, false}
	perm := rand.Perm(len(parentChs))
	t.Logf("settle order = %v", perm)
	for _, i := range perm {
		var e error
		if i < 2 { //nolint:mnd
			e = parentChs[i].Settle(ctx, isSecondary[0])
			isSecondary[0] = true
		} else {
			e = parentChs[i].Settle(ctx, isSecondary[1])
			isSecondary[1] = true
		}
		assert.NoErrorf(e, "settling parent channel %d", i)
	}

	// Verify final on-chain balances.
	balancesAfter := channel.Balances{
		{
			mls.Alice.BalanceReader1.Balance(mls.Asset1),
			mls.Bob.BalanceReader1.Balance(mls.Asset1),
		},
		{
			mls.Alice.BalanceReader2.Balance(mls.Asset2),
			mls.Bob.BalanceReader2.Balance(mls.Asset2),
		},
	}

	diff := balancesAfter.Sub(balancesBefore)
	expectedDiff := mls.Balances.UpdateBalsVirtual.Sub(mls.Balances.InitBalsVirtual)
	assert.True(
		EqualBalancesWithDelta(expectedDiff, diff, mls.BalanceDelta),
		"final on-chain balances incorrect: expected diff %v ±%v, got %v",
		expectedDiff, mls.BalanceDelta, diff,
	)
}
