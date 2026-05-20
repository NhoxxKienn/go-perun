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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/client"
)

// TestMultiLedgerVirtualDispute runs the on-chain dispute scenario for a
// multi-ledger virtual channel.  It opens parent ledger channels and a virtual
// channel, then triggers a dispute by registering all parent channels directly
// via the adjudicator.  The virtual channel's state is included as a
// sub-channel during registration.  After the challenge period the parent
// channels are settled on-chain and the final balances are verified.
//
//nolint:revive // test.Test... stutters but this is OK in this special case.
func TestMultiLedgerVirtualDispute(
	ctx context.Context,
	t *testing.T,
	mls MultiLedgerVirtualSetup,
	challengeDuration uint64,
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

	mlvc, _ := openMultiLedgerVirtualChannels(ctx, t, mls, false)

	// Register all four parent channel views.  Each registration includes the
	// virtual sub-channel's latest state via registerDispute→gatherSubChannelStates.
	parentChs := []*client.Channel{
		mlvc.chAliceHub, mlvc.chHubAlice,
		mlvc.chBobHub, mlvc.chHubBob,
	}
	perm := rand.Perm(len(parentChs))
	t.Logf("register order = %v", perm)
	for _, i := range perm {
		err := client.NewTestChannel(parentChs[i]).Register(ctx)
		require.NoErrorf(err, "registering parent channel %d", i)
	}

	// Settle all four parent channel views in the same random order.
	// The virtual channel's funds are distributed as part of the parent
	// channel's on-chain withdrawal (outcomeRecursive in the mock backend).
	isSecondary := [2]bool{false, false}
	perm = rand.Perm(len(parentChs))
	t.Logf("settle order = %v", perm)
	for _, i := range perm {
		var err error
		if i < 2 { //nolint:mnd
			err = parentChs[i].Settle(ctx, isSecondary[0])
			isSecondary[0] = true
		} else {
			err = parentChs[i].Settle(ctx, isSecondary[1])
			isSecondary[1] = true
		}
		assert.NoErrorf(err, "settling parent channel %d", i)
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
