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
	"polycry.pt/poly-go/sync"
)

// TestMultiLedgerVirtualHappy runs the optimistic (no dispute) multi-ledger
// virtual channel scenario.  It opens parent ledger channels spanning two
// ledgers, creates a virtual channel on top, performs an off-chain update,
// settles the virtual channel cooperatively, and then settles the parent
// channels.
//
//nolint:revive // test.Test... stutters but this is OK in this special case.
func TestMultiLedgerVirtualHappy(
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

	mlvc, errs := openMultiLedgerVirtualChannels(ctx, t, mls, false)

	// Mark virtual channel as final and settle it cooperatively (off-chain).
	err := mlvc.chAliceBob.Update(ctx, func(s *channel.State) {
		s.IsFinal = true
	})
	require.NoError(err, "marking virtual channel final")

	var vcSettled sync.WaitGroup
	vcSettled.Add(2) //nolint:mnd
	for _, ch := range []*client.Channel{mlvc.chAliceBob, mlvc.chBobAlice} {
		go func(ch *client.Channel) {
			if err := ch.Settle(ctx, false); err != nil {
				errs <- err
				return
			}
			vcSettled.Done()
		}(ch)
	}
	select {
	case <-vcSettled.WaitCh():
	case e := <-errs:
		t.Fatalf("virtual channel settlement error: %v", e)
	}

	// After virtual settlement the parent channels carry the updated balances.
	assert.NoError(
		mlvc.chAliceHub.State().AssertEqual(channel.Balances{
			mls.Balances.FinalBalsAlice[0],
			mls.Balances.FinalBalsAlice[1],
		}),
		"Alice-Hub final state",
	)
	assert.NoError(
		mlvc.chBobHub.State().AssertEqual(channel.Balances{
			mls.Balances.FinalBalsBob[0],
			mls.Balances.FinalBalsBob[1],
		}),
		"Bob-Hub final state",
	)

	// Hub updates both parent channels to final.
	err = mlvc.chHubAlice.Update(ctx, func(s *channel.State) { s.IsFinal = true })
	require.NoError(err, "finalising Alice-Hub via Hub")
	err = mlvc.chHubBob.Update(ctx, func(s *channel.State) { s.IsFinal = true })
	require.NoError(err, "finalising Bob-Hub via Hub")

	// Settle all four parent channel views in a random order.
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

	// Check final on-chain balances.
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
